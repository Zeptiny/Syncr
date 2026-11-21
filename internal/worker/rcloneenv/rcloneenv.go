// Package rcloneenv turns typed remote definitions into the environment of
// an rclone child process, and isolates that process from the host's rclone
// configuration.
//
// Remotes are never strings: each syncrv1.RemoteDefinition becomes
// RCLONE_CONFIG_<ALIAS>_TYPE and RCLONE_CONFIG_<ALIAS>_<OPTION> variables,
// which rclone reads with higher priority than any config file. Values are
// passed through unchanged (they are never parsed as connection strings),
// except for:
//
//   - secrets listed in obscure_keys, which are obscured in-process the way
//     "rclone obscure" does (see Obscure);
//   - union upstreams, which are generated from typed UnionUpstream entries
//     and quoted the way rclone parses its space-separated lists;
//   - the "remote" option of a crypt remote, which is generated from the
//     typed wrapped location (another remote of the same job, never a
//     local path), and its "strict_names", which is always "true".
//
// Validation is strict, because the definitions come from the network:
// aliases and option keys have a fixed charset, environment names must not
// collide, only the backend types the worker supports are accepted (see
// SupportedBackends; never "local", which would bypass the worker's
// local_roots, nor backends that wrap free-form remote strings), options
// that make rclone run local commands, read local files or use the worker
// host's credentials are rejected, and so are remotes that carry too few
// credentials of their own, which rclone would complete with the worker
// host's (see checkCredentials).
//
// Every error message names aliases and keys only, never values, so errors
// can be shown to users.
//
// Allowed dependencies: the standard library and the generated protobuf code.
// The rclone and restic runners both use it.
package rcloneenv

import (
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Isolation variables set by Isolate. Isolate also points the system temp
// variables (TempVars) at the job's temp directory.
const (
	EnvConfig      = "RCLONE_CONFIG"
	EnvCacheDir    = "RCLONE_CACHE_DIR"
	EnvTempDir     = "RCLONE_TEMP_DIR"
	EnvAskPassword = "RCLONE_ASK_PASSWORD" //nolint:gosec // G101 false positive: a variable name, not a credential
)

// TempVars are the variables that name the system temp directory: TMPDIR
// on Unix, TEMP and TMP on Windows. Isolate sets all three.
func TempVars() []string { return []string{"TMPDIR", "TEMP", "TMP"} }

// Sentinel errors, matched with errors.Is.
var (
	// ErrInvalidRemote reports a remote definition that failed validation.
	ErrInvalidRemote = errors.New("invalid remote definition")

	// ErrLocalPath reports a union upstream whose local path was rejected
	// by the LocalResolver (e.g. outside local_roots). It is always joined
	// with ErrInvalidRemote and the resolver's error.
	ErrLocalPath = errors.New("local path rejected")
)

// RemoteError describes an invalid remote definition. It names the alias and
// option key involved, never a value.
type RemoteError struct {
	// Alias of the offending remote; may be empty or invalid itself.
	Alias string
	Err   error
}

// Error implements error.
func (e *RemoteError) Error() string {
	return fmt.Sprintf("remote %q: %v", e.Alias, e.Err)
}

// Unwrap returns the underlying errors, including ErrInvalidRemote.
func (e *RemoteError) Unwrap() []error { return []error{ErrInvalidRemote, e.Err} }

// LocalResolver checks local paths used by union upstreams. It returns the
// path to use, or an error if the path must not be used.
// *localfs.Policy implements it.
type LocalResolver interface {
	Resolve(path string) (string, error)
}

// ReservedHolder is what a LocalResolver also implements when the worker
// keeps folders for its own use that no job may read or write
// (*localfs.Policy, which refuses those folders and what lies inside
// them). HoldsReserved returns the reserved folder that a local path holds,
// such as "/" holding <data_dir>/sources, if any. A local union upstream
// that holds one is refused: rclone would read and write that folder
// through the union, and a union's paths cannot be kept out of it with
// filters.
type ReservedHolder interface {
	HoldsReserved(path string) (string, bool)
}

// Builder validates remote definitions and builds child environments. It
// holds no per-job state and is safe for concurrent use.
type Builder struct {
	local LocalResolver
}

// NewBuilder returns a Builder that checks local union upstreams with local.
// A nil local rejects every local upstream.
func NewBuilder(local LocalResolver) *Builder {
	return &Builder{local: local}
}

// Env is the environment of one rclone child process.
type Env struct {
	vars    map[string]string
	types   map[string]string // alias -> backend type
	secrets []string          // raw secret values, plain and obscured
	// local are the local folders of each union (alias -> resolved
	// upstream paths, without their mode suffix).
	local map[string][]string
	// wrapped are the locations where wrapper remotes (crypt) store their
	// data (alias -> another remote's alias and a path on it).
	wrapped map[string]*syncrv1.Location
}

// Vars returns the variables to add to the child's environment (proc.Spec.Env).
// The map is a copy and may be modified.
func (e *Env) Vars() map[string]string {
	out := make(map[string]string, len(e.vars))
	for k, v := range e.vars {
		out[k] = v
	}
	return out
}

// BackendType returns the backend type of the remote alias and whether such
// a remote is defined.
func (e *Env) BackendType(alias string) (string, bool) {
	t, ok := e.types[alias]
	return t, ok
}

// LocalPaths returns the local folders that the remote alias reads and
// writes: the resolved paths of a union's local upstreams, joined with
// path, a location on the remote. rclone gives each upstream the same
// location and keeps it inside the upstream. Remotes without local
// upstreams return nil, crypt remotes included: the remote a crypt remote
// stores its data in is never local, and never a union.
func (e *Env) LocalPaths(alias, path string) []string {
	ups := e.local[alias]
	if len(ups) == 0 {
		return nil
	}
	// rclone remote paths separate with "/" only (its local backend
	// encodes a backslash in a name on Windows).
	rel := strings.TrimPrefix(pathpkg.Clean("/"+path), "/")
	out := make([]string, len(ups))
	for i, up := range ups {
		out[i] = filepath.Join(up, filepath.FromSlash(rel))
	}
	return out
}

// Storage returns where the data at path on the remote alias is stored,
// for checks of nested locations: for a crypt remote, the remote it wraps
// and the wrapped path joined with path; for any other remote, alias and
// path unchanged. The wrapped remote is never a crypt remote itself, so
// one step is enough. With encrypted names the folder on the wrapped
// remote has another name than path, but it is inside the wrapped path all
// the same; this approximates it by path, as the controller does.
func (e *Env) Storage(alias, path string) (string, string) {
	w, ok := e.wrapped[alias]
	if !ok {
		return alias, path
	}
	return w.GetRemote(), joinPath(w.GetPath(), path)
}

// joinPath joins two paths on a remote with "/", the only separator of
// rclone remote paths, with one slash where they meet; an empty part adds
// nothing. Nothing is cleaned.
func joinPath(base, path string) string {
	switch {
	case path == "":
		return base
	case base == "":
		return path
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// Secrets returns the secret values of all remotes, in the forms they could
// take in tool output (plain, obscured, quoted, URL-escaped), longest first.
// Use it to scrub tool output before showing it to users (see
// internal/worker/runner/scrub).
func (e *Env) Secrets() []string { return secretForms(e.secrets) }

// Build prepares dir (see Isolate) and returns the environment that defines
// remotes for an rclone child confined to dir. dir must be a private,
// per-job directory that the caller removes after the child exits.
func (b *Builder) Build(dir string, remotes []*syncrv1.RemoteDefinition) (*Env, error) {
	env, err := b.Remotes(remotes)
	if err != nil {
		return nil, err
	}
	iso, err := Isolate(dir)
	if err != nil {
		return nil, err
	}
	for k, v := range iso {
		env.vars[k] = v
	}
	return env, nil
}

// Remotes validates remotes and returns only their RCLONE_CONFIG_* variables,
// without isolation variables. Most callers want Build.
func (b *Builder) Remotes(remotes []*syncrv1.RemoteDefinition) (*Env, error) {
	env := &Env{
		vars: make(map[string]string), types: make(map[string]string, len(remotes)),
		local: make(map[string][]string), wrapped: make(map[string]*syncrv1.Location),
	}
	if err := checkAliases(remotes); err != nil {
		return nil, err
	}
	for _, def := range remotes {
		env.types[def.GetAlias()] = def.GetBackendType()
	}
	var secrets []string
	for _, def := range remotes {
		s, err := b.addRemote(env, def)
		if err != nil {
			return nil, &RemoteError{Alias: def.GetAlias(), Err: err}
		}
		secrets = append(secrets, s...)
	}
	env.secrets = secrets
	return env, nil
}

// LocalPaths returns the local folders that the remote alias of remotes
// uses at path (Env.LocalPaths), after the same validation as Env. It
// lets the restic runner check a repository on a union with local
// upstreams like a local one (restic.LocalPather).
func (b *Builder) LocalPaths(remotes []*syncrv1.RemoteDefinition, alias, path string) ([]string, error) {
	env, err := b.Remotes(remotes)
	if err != nil {
		return nil, err
	}
	return env.LocalPaths(alias, path), nil
}

// Env returns the variables that define remotes (RCLONE_CONFIG_<ALIAS>_*
// only), after the same validation as Build. It adapts the Builder to the
// restic runner's RemoteEnv port: restic starts rclone itself, passes it
// "--config=" and its own scratch directory, so the isolation variables of
// Build are neither needed nor wanted there. Secrets are included in clear
// (or obscured, per ObscureKeys); the map is the caller's to modify.
//
// The restic package does not import this one and this one does not import
// restic: the composition root checks that *Builder satisfies the port.
func (b *Builder) Env(remotes []*syncrv1.RemoteDefinition) (map[string]string, error) {
	env, err := b.Remotes(remotes)
	if err != nil {
		return nil, err
	}
	return env.Vars(), nil
}

// Isolate creates an empty rclone config file, a cache directory and a temp
// directory inside dir and returns the variables that point rclone at them,
// so a host rclone.conf (e.g. a desktop user's
// ~/.config/rclone/rclone.conf) is never read, rclone never prompts for a
// config password, and nothing is cached or left as a temporary file
// outside dir: rclone's own temp directory (--temp-dir, RCLONE_TEMP_DIR)
// and the system temp variables (TempVars), for anything else the process
// writes there, are dir/tmp. Environment variables are used rather than
// flags so that they also reach rclone processes started by other tools
// (restic's rclone backend).
func Isolate(dir string) (map[string]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("rcloneenv: scratch dir %q is not absolute", dir)
	}
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, fmt.Errorf("rcloneenv: create cache dir: %w", err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("rcloneenv: create temp dir: %w", err)
	}
	conf := filepath.Join(dir, "rclone.conf")
	f, err := os.OpenFile(conf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is built from the caller's scratch dir
	if err != nil {
		return nil, fmt.Errorf("rcloneenv: create empty config: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("rcloneenv: create empty config: %w", err)
	}
	vars := map[string]string{
		EnvConfig:      conf,
		EnvCacheDir:    cache,
		EnvTempDir:     tmp,
		EnvAskPassword: "false",
	}
	for _, name := range TempVars() {
		vars[name] = tmp
	}
	return vars, nil
}

// RemotePath returns the rclone path of path on the remote alias
// ("alias:path"). rclone splits it at the first colon, and alias contains
// none, so nothing in path is parsed as a remote name or connection-string
// parameter.
func RemotePath(alias, path string) string { return alias + ":" + path }

// EnvName returns the variable rclone reads for option of remote alias:
// RCLONE_CONFIG_<ALIAS>_<OPTION>, uppercased with "-" replaced by "_" (as in
// rclone's fs.ConfigToEnv).
func EnvName(alias, option string) string {
	return "RCLONE_CONFIG_" + strings.ToUpper(alias+"_"+strings.ReplaceAll(option, "-", "_"))
}

var (
	// aliasRe: lowercase, starts with a letter, at least two characters so
	// it is never a Windows drive letter ("c:path").
	aliasRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)
	// backendRe matches rclone backend type names.
	backendRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,63}$`)
	// optionRe matches rclone option names (as written in config files or
	// as flags without the backend prefix).
	optionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// ValidateAlias reports whether alias is acceptable as a remote alias.
func ValidateAlias(alias string) error {
	if !aliasRe.MatchString(alias) {
		return errors.New("alias must match ^[a-z][a-z0-9_]{1,62}$")
	}
	return nil
}

// checkAliases validates aliases and their uniqueness. Aliases must not
// overlap in the environment: with aliases "s3" and "s3_eu",
// RCLONE_CONFIG_S3_EU_REGION would be both option "eu_region" of "s3" and
// option "region" of "s3_eu".
func checkAliases(remotes []*syncrv1.RemoteDefinition) error {
	seen := make(map[string]bool, len(remotes))
	for _, def := range remotes {
		if def == nil {
			return &RemoteError{Err: errors.New("nil definition")}
		}
		alias := def.GetAlias()
		if err := ValidateAlias(alias); err != nil {
			return &RemoteError{Alias: alias, Err: err}
		}
		if seen[alias] {
			return &RemoteError{Alias: alias, Err: errors.New("alias defined more than once")}
		}
		seen[alias] = true
	}
	for a := range seen {
		for b := range seen {
			if strings.HasPrefix(b, a+"_") {
				return &RemoteError{Alias: b, Err: fmt.Errorf("alias overlaps alias %q in environment variable names; rename one of them", a)}
			}
		}
	}
	return nil
}

// addRemote validates def and adds its variables to env. It returns the
// secret values it added (plain and obscured).
func (b *Builder) addRemote(env *Env, def *syncrv1.RemoteDefinition) ([]string, error) {
	alias, backend := def.GetAlias(), def.GetBackendType()
	if err := checkBackend(backend); err != nil {
		return nil, err
	}
	isUnion, isCrypt := backend == "union", backend == "crypt"
	if !isUnion && len(def.GetUnionUpstreams()) > 0 {
		return nil, errors.New("union_upstreams is only valid for backend type \"union\"")
	}
	if !isCrypt && def.GetWrapped() != nil {
		return nil, errors.New("wrapped is only valid for backend type \"crypt\"")
	}

	names := map[string]string{"TYPE": "backend_type"} // normalized key -> original
	vars := map[string]string{EnvName(alias, "type"): backend}
	add := func(key, value, kind string) error {
		if err := checkOption(backend, key); err != nil {
			return err
		}
		norm := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if prev, dup := names[norm]; dup {
			return fmt.Errorf("%s %q collides with %q", kind, key, prev)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("%s %q: value contains a NUL byte", kind, key)
		}
		names[norm] = key
		vars[EnvName(alias, key)] = value
		return nil
	}

	for _, key := range sortedKeys(def.GetOptions()) {
		if revealedSecret(backend, key) {
			return nil, fmt.Errorf("option %q must be a secret listed in obscure_keys", key)
		}
		if err := checkLine(backend, key, def.GetOptions()[key], "option"); err != nil {
			return nil, err
		}
		if err := add(key, def.GetOptions()[key], "option"); err != nil {
			return nil, err
		}
	}

	obscure := make(map[string]bool, len(def.GetObscureKeys()))
	for _, key := range def.GetObscureKeys() {
		if _, ok := def.GetSecrets()[key]; !ok {
			return nil, fmt.Errorf("obscure key %q is not a secret of this remote", key)
		}
		if obscure[key] {
			return nil, fmt.Errorf("obscure key %q listed more than once", key)
		}
		obscure[key] = true
	}
	var secrets []string
	for _, key := range sortedKeys(def.GetSecrets()) {
		value := def.GetSecrets()[key]
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("secret %q: value contains a NUL byte", key)
		}
		if err := checkLine(backend, key, value, "secret"); err != nil {
			return nil, err
		}
		if revealedSecret(backend, key) && !obscure[key] {
			// rclone reveals the value as it comes: a plain password would
			// fail, or (when it happens to decode) silently become another,
			// one that checkLine never saw.
			return nil, fmt.Errorf("secret %q must be listed in obscure_keys", key)
		}
		secrets = append(secrets, value)
		if obscure[key] {
			o, err := Obscure(value)
			if err != nil {
				return nil, fmt.Errorf("secret %q: %w", key, err)
			}
			value = o
			secrets = append(secrets, o)
		}
		if err := add(key, value, "secret"); err != nil {
			return nil, err
		}
	}
	if err := checkCredentials(alias, backend, vars); err != nil {
		return nil, err
	}

	if isUnion {
		upstreams, err := b.unionUpstreams(env, alias, def.GetUnionUpstreams())
		if err != nil {
			return nil, err
		}
		vars[EnvName(alias, "upstreams")] = upstreams
	}
	if isCrypt {
		wrapped, err := wrappedLocation(env, alias, def.GetWrapped())
		if err != nil {
			return nil, err
		}
		// checkOption refuses both keys from the definition, so neither
		// collides with an option or a secret.
		vars[EnvName(alias, "remote")] = RemotePath(wrapped.GetRemote(), wrapped.GetPath())
		vars[EnvName(alias, "strict_names")] = "true"
		env.wrapped[alias] = wrapped
	}

	for k, v := range vars {
		env.vars[k] = v
	}
	return secrets, nil
}

// SupportedBackends returns the rclone backend types the worker accepts,
// sorted: the remote types built into Syncr (internal/backend/builtin) and
// "memory", which keeps its data inside the rclone process (tests use it).
//
// It is an allowlist because most rclone backends must not be driven by
// the controller: "local" and the backends that wrap free-form remote
// strings ("alias", "archive", "combine", "chunker"...) reach the worker's
// files without the local_roots check, and many cloud backends can sign in
// as the worker host (Azure CLI and managed identities, cloud metadata
// servers). "union" is supported because its upstreams are generated from
// typed union_upstreams, and "crypt" because its "remote" is generated from
// the typed wrapped location (another remote of the job, never a local
// path, a union or another crypt remote). "azureblob" and "gcs" (rclone's
// "google cloud storage", named by its prefix) are supported because their
// options that sign in as the host are denied (deniedOptions) and a gcs
// remote must bring its own service account key (checkCredentials); "ftp"
// and "smb" because they must name their own user. A type is added here
// only after every one of its options in the pinned rclone has been
// reviewed: see deniedOptions and TestSupportedBackendOptionsReviewed.
func SupportedBackends() []string {
	return []string{"azureblob", "b2", "crypt", "ftp", "gcs", "memory", "s3", "sftp", "smb", "union", "webdav"}
}

// checkBackend rejects backend types that are malformed or not supported.
func checkBackend(backend string) error {
	if !backendRe.MatchString(backend) {
		return errors.New("backend type must match ^[a-z][a-z0-9]{0,63}$")
	}
	if backend == "local" {
		return errors.New(`backend type "local" is not allowed; use a location without a remote, which is checked against local_roots`)
	}
	if supported := SupportedBackends(); !slices.Contains(supported, backend) {
		return fmt.Errorf("backend type %q is not supported by this worker (supported: %s)", backend, strings.Join(supported, ", "))
	}
	return nil
}

// deniedOptions are rclone options that run commands on the worker, read or
// write arbitrary local files or sockets, or pick up the worker host's own
// credentials; checkOption also denies every "*_file" option. The
// controller's backend descriptors never emit them. They are denied for
// every backend type: the supported ones were reviewed against the pinned
// rclone (TestSupportedBackendOptionsReviewed), and the entries for other
// backends (Azure Files, the cache wrapper...) keep the list safe to
// extend.
//
// Two azureblob options are denied although they name no file or command:
// use_emulator points the remote at a storage emulator on the worker's own
// loopback (127.0.0.1:10000) with the emulator's published key, and
// msi_client_id with client_id and tenant (without use_msi) makes rclone
// 1.75.2 sign in with "workload identity": it asks the worker host's
// managed identity endpoint (169.254.169.254) for a token.
func deniedOptions() map[string]string {
	return map[string]string{ //nolint:gosec // G101 false positive: option names, not credentials
		"ssh":                     "runs a local command",
		"bearer_token_command":    "runs a local command",
		"use_az":                  "runs a local command (the Azure CLI) with the worker host's login",
		"unix_socket":             "connects to a local socket",
		"use_emulator":            "connects to a storage emulator on the worker host",
		"env_auth":                "uses the worker host's credentials",
		"key_use_agent":           "uses the worker host's ssh-agent",
		"use_msi":                 "uses the worker host's managed identity",
		"msi_client_id":           "uses the worker host's managed identity",
		"use_kerberos":            "uses the worker host's Kerberos tickets",
		"shared_credentials_file": "reads a local file",
		"config_file":             "reads a local file",
		"kerberos_ccache":         "reads a local file",
		"client_certificate_path": "reads a local file",
		"db_path":                 "writes local files",
		"chunk_path":              "writes local files",
		"tmp_upload_path":         "writes local files",
	}
}

// checkCredentials rejects a remote that rclone would complete with the
// worker host's own credentials or account name because it carries too few
// of its own, and a gcs service account key that would send its sign-in
// elsewhere than Google (checkGCSKey). vars are the remote's variables, as
// passed to rclone; a key counts as set when its value is not empty (rclone
// treats an empty value like a missing one). The messages name keys only,
// never values.
//
// azureblob needs no rule: with env_auth, use_msi, msi_client_id, use_az,
// use_emulator, service_principal_file and client_certificate_path denied,
// a remote without credentials of its own either fails ("no authentication
// method configured") or reads its account's endpoint anonymously.
func checkCredentials(alias, backend string, vars map[string]string) error {
	set := func(key string) bool { return vars[EnvName(alias, key)] != "" }
	switch backend {
	case "sftp":
		if !set("pass") && !set("key_pem") {
			// rclone then signs in with the host's ssh-agent (key_file and
			// ssh are denied). SSH_AUTH_SOCK never reaches children on
			// Unix, but on Windows rclone finds Pageant or the OpenSSH
			// agent pipe by itself.
			return errors.New(`an sftp remote needs a "pass" or a "key_pem"; the worker's ssh-agent is never used`)
		}
	case "gcs":
		if !set(gcsKeyOption) {
			// rclone then falls back to Google's Application Default
			// Credentials (env_auth and service_account_file are denied):
			// the gcloud file under the worker's HOME or APPDATA, or the
			// metadata server of a Google Cloud host. Later OAuth types
			// may accept a token here instead.
			return errors.New("a Google Cloud Storage remote needs a service account key; the worker's own Google credentials are never used")
		}
		return checkGCSKey(vars[EnvName(alias, gcsKeyOption)])
	case "ftp", "smb":
		if !set("user") {
			// rclone then sends the worker process's OS account name.
			return fmt.Errorf("an %s remote needs a user; the worker's account name is never sent", backend)
		}
	}
	return nil
}

// gcsKeyOption is the option of a gcs remote's service account key.
const gcsKeyOption = "service_account_credentials"

// gcsTokenURI is the only token_uri a gcs service account key may name:
// Google's OAuth 2.0 token endpoint, which rclone also uses when the key
// names none or an empty one. The controller's gcs descriptor pins the
// same address.
//
//nolint:gosec // G101: a public endpoint, not a credential
const gcsTokenURI = "https://oauth2.googleapis.com/token"

// checkGCSKey rejects a gcs service account key that is not one JSON
// object (rclone could not read it either), or whose token_uri names
// another address than gcsTokenURI: rclone posts the signed sign-in
// request to the key's token_uri, whatever it names. rclone reads the key
// with encoding/json, which matches a member to token_uri when their names
// are strings.EqualFold (any case, and the Kelvin sign for "k"), keeps the
// last of repeated ones and ignores null. So every such member must be
// null, empty or gcsTokenURI, not only the one encoding/json keeps: a
// repeat in another spelling cannot hide another address. The messages
// name the option, never any part of the key.
func checkGCSKey(key string) error {
	notKey := fmt.Errorf("%q must be a JSON key file (one JSON object)", gcsKeyOption)
	if !json.Valid([]byte(key)) {
		return notKey
	}
	dec := json.NewDecoder(strings.NewReader(key))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return notKey
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return notKey
		}
		name, _ := t.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return notKey
		}
		if !strings.EqualFold(name, "token_uri") {
			continue
		}
		var uri *string
		if err := json.Unmarshal(value, &uri); err != nil || (uri != nil && *uri != "" && *uri != gcsTokenURI) {
			return fmt.Errorf("%q: token_uri must be absent, empty or %s; the worker signs in to Google Cloud Storage only through Google", gcsKeyOption, gcsTokenURI)
		}
	}
	return nil
}

// lineOptions are the options whose values rclone writes, as they are,
// into a line of a text protocol: ftp sends "USER <user>" and "PASS <pass>"
// on its control connection, and "host:port" in an HTTP proxy's CONNECT
// request. A line break in one would add lines of the remote's choosing,
// for whatever service host and port name. (rclone encodes line breaks in
// ftp paths itself.)
func lineOptions(backend string) []string {
	if backend == "ftp" {
		return []string{"host", "user", "pass"}
	}
	return nil
}

// checkLine rejects a line break in the value of an option or secret key
// that backend writes into a line of a text protocol (lineOptions). value
// is the plain value, before it is obscured; kind is "option" or "secret".
func checkLine(backend, key, value, kind string) error {
	name := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if slices.Contains(lineOptions(backend), name) && strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s %q must not contain a line break: an %s remote sends it as a line of a command", kind, key, backend)
	}
	return nil
}

// checkOption validates an option or secret key.
func checkOption(backend, key string) error {
	if !optionRe.MatchString(key) {
		return fmt.Errorf("option key %q must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$", key)
	}
	name := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if reason, ok := deniedOptions()[name]; ok {
		return fmt.Errorf("option %q is not allowed (%s)", key, reason)
	}
	if strings.HasSuffix(name, "_file") {
		return fmt.Errorf("option %q is not allowed (reads a local file); pass the content instead (e.g. key_pem)", key)
	}
	if backend == "union" && (name == "upstreams" || name == "remotes") {
		return fmt.Errorf("option %q is not allowed; use union_upstreams", key)
	}
	if backend == "crypt" {
		switch name {
		case "remote":
			return fmt.Errorf("option %q is not allowed; it is set by the worker from wrapped", key)
		case "strict_names":
			return fmt.Errorf("option %q is not allowed; the worker always sets it, so a wrong password fails instead of hiding every file", key)
		case "pass_bad_blocks":
			return fmt.Errorf("option %q is not allowed; with a wrong password it would read every file as zeros", key)
		case "no_data_encryption":
			return fmt.Errorf("option %q is not allowed; with a wrong password files would read without an error, so it could not be found", key)
		}
	}
	return nil
}

// revealedSecrets are the keys of backend that rclone reads only in their
// obscured form (it reveals them as they come): crypt's passwords, and
// the ftp and smb passwords. Such a key must be a secret listed in
// obscure_keys, so the worker obscures it itself and its checks
// (checkLine) see the plain value rclone will use. Earlier types keep
// their behavior: their plain secrets are not refused here.
func revealedSecrets(backend string) []string {
	switch backend {
	case "crypt":
		return []string{"password", "password2"}
	case "ftp", "smb":
		return []string{"pass"}
	}
	return nil
}

// revealedSecret reports whether key (an option or secret key, in any case
// or dash spelling) is one of backend's revealedSecrets.
func revealedSecret(backend, key string) bool {
	name := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return slices.Contains(revealedSecrets(backend), name)
}

// wrappedLocation validates where the crypt remote self stores its data:
// another remote of the job, which is not a union or another crypt remote,
// plus a path on it. It returns the location to use.
func wrappedLocation(env *Env, self string, loc *syncrv1.Location) (*syncrv1.Location, error) {
	if loc == nil {
		return nil, errors.New("wrapped is required for backend type \"crypt\": the remote that stores its data")
	}
	// The same check as a union upstream's path: no value with a line
	// break or NUL byte reaches rclone's remote option.
	if strings.ContainsAny(loc.GetPath(), "\x00\r\n") {
		return nil, errors.New("wrapped path contains a line break or NUL byte")
	}
	alias := loc.GetRemote()
	backend, ok := env.types[alias]
	switch {
	case alias == "":
		return nil, errors.New("wrapped names no remote; a crypt remote cannot store its data in a local folder")
	case alias == self:
		return nil, errors.New("a crypt remote cannot store its data in itself")
	case !ok:
		return nil, fmt.Errorf("wrapped remote %q is not defined in this job", alias)
	case backend == "union":
		return nil, fmt.Errorf("wrapped remote %q is a union; nested wrappers are not supported", alias)
	case backend == "crypt":
		return nil, fmt.Errorf("wrapped remote %q is a crypt remote; nested wrappers are not supported", alias)
	}
	return &syncrv1.Location{Remote: alias, Path: loc.GetPath()}, nil
}

// unionSuffixes are the mode suffixes rclone strips from an upstream.
func unionSuffixes() []string { return []string{":ro", ":nc", ":writeback"} }

// unionUpstreams validates upstreams and encodes them as rclone's
// "upstreams" value: a space-separated list using encoding/csv quoting, which
// is exactly how rclone parses it (fs.SpaceSepList).
func (b *Builder) unionUpstreams(env *Env, self string, ups []*syncrv1.UnionUpstream) (string, error) {
	if len(ups) < 2 {
		return "", errors.New("a union needs at least two union_upstreams")
	}
	fields := make([]string, 0, len(ups))
	for i, up := range ups {
		f, err := b.unionUpstream(env, self, up)
		if err != nil {
			return "", fmt.Errorf("union upstream %d: %w", i, err)
		}
		fields = append(fields, f)
	}
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	w.Comma = ' '
	if err := w.Write(fields); err != nil {
		return "", fmt.Errorf("encode upstreams: %w", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("encode upstreams: %w", err)
	}
	return strings.TrimSuffix(sb.String(), "\n"), nil
}

func (b *Builder) unionUpstream(env *Env, self string, up *syncrv1.UnionUpstream) (string, error) {
	loc := up.GetLocation()
	if loc == nil {
		return "", errors.New("missing location")
	}
	path := loc.GetPath()
	// encoding/csv turns "\r\n" into "\n" inside quoted fields, so paths with
	// line breaks would not round-trip.
	if strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("path contains a line break or NUL byte")
	}

	var suffix string
	switch up.GetMode() {
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_UNSPECIFIED, syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE:
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY:
		suffix = ":ro"
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE:
		suffix = ":nc"
	default:
		return "", fmt.Errorf("unknown mode %d", up.GetMode())
	}

	upstream, err := b.upstreamPath(env, self, loc.GetRemote(), path)
	if err != nil {
		return "", err
	}
	if loc.GetRemote() == "" {
		env.local[self] = append(env.local[self], upstream)
	}
	// rclone strips one mode suffix from the end of the upstream as it is
	// passed, so a read-write one must not end with one. This checks the
	// path after resolving: cleaning a local path ("/data/x:ro/") can
	// expose a suffix the given path hid.
	if suffix == "" {
		for _, s := range unionSuffixes() {
			if strings.HasSuffix(upstream, s) {
				return "", fmt.Errorf("a read-write upstream path must not end with %q (rclone would read it as a mode)", s)
			}
		}
	}
	return upstream + suffix, nil
}

// upstreamPath returns a union upstream's location as rclone reads it: a
// local path resolved within local_roots (alias "") or alias:path.
func (b *Builder) upstreamPath(env *Env, self, alias, path string) (string, error) {
	if alias == "" {
		if b.local == nil {
			return "", fmt.Errorf("%w: local upstreams are not available", ErrLocalPath)
		}
		resolved, err := b.local.Resolve(path)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrLocalPath, err)
		}
		if h, ok := b.local.(ReservedHolder); ok {
			if dir, holds := h.HoldsReserved(resolved); holds {
				return "", fmt.Errorf("%w: local path %q holds %s, a folder the worker keeps for its own use", ErrLocalPath, resolved, dir)
			}
		}
		return resolved, nil
	}
	backend, ok := env.types[alias]
	switch {
	case alias == self:
		return "", errors.New("a union cannot contain itself")
	case !ok:
		return "", fmt.Errorf("remote %q is not defined in this job", alias)
	case backend == "union":
		return "", fmt.Errorf("remote %q is a union; nested unions are not supported", alias)
	case backend == "crypt":
		return "", fmt.Errorf("remote %q is a crypt remote; nested wrappers are not supported", alias)
	}
	return RemotePath(alias, path), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// longestFirst orders strings by decreasing length, then lexically.
func longestFirst(a, b string) int {
	if c := cmp.Compare(len(b), len(a)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}
