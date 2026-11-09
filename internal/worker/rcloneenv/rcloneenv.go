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
//     and quoted the way rclone parses its space-separated lists.
//
// Validation is strict, because the definitions come from the network:
// aliases and option keys have a fixed charset, environment names must not
// collide, only the backend types the worker supports are accepted (see
// SupportedBackends; never "local", which would bypass the worker's
// local_roots, nor backends that wrap free-form remote strings), and
// options that make rclone run local commands or read local files are
// rejected.
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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Isolation variables set by Isolate.
const (
	EnvConfig      = "RCLONE_CONFIG"
	EnvCacheDir    = "RCLONE_CACHE_DIR"
	EnvAskPassword = "RCLONE_ASK_PASSWORD" //nolint:gosec // G101 false positive: a variable name, not a credential
)

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

// Secrets returns the secret values of all remotes, in the forms they could
// take in tool output (plain, obscured, quoted, URL-escaped), longest first.
// Use it to scrub tool output before showing it to users; see Scrubber.
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
	env := &Env{vars: make(map[string]string), types: make(map[string]string, len(remotes))}
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

// Isolate creates an empty rclone config file and a cache directory inside
// dir and returns the variables that point rclone at them, so a host
// rclone.conf (e.g. a desktop user's ~/.config/rclone/rclone.conf) is never
// read, rclone never prompts for a config password, and nothing is cached
// outside dir. Environment variables are used rather than flags so that they
// also reach rclone processes started by other tools (restic's rclone
// backend).
func Isolate(dir string) (map[string]string, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("rcloneenv: scratch dir %q is not absolute", dir)
	}
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, fmt.Errorf("rcloneenv: create cache dir: %w", err)
	}
	conf := filepath.Join(dir, "rclone.conf")
	f, err := os.OpenFile(conf, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is built from the caller's scratch dir
	if err != nil {
		return nil, fmt.Errorf("rcloneenv: create empty config: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("rcloneenv: create empty config: %w", err)
	}
	return map[string]string{
		EnvConfig:      conf,
		EnvCacheDir:    cache,
		EnvAskPassword: "false",
	}, nil
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
	isUnion := backend == "union"
	if !isUnion && len(def.GetUnionUpstreams()) > 0 {
		return nil, errors.New("union_upstreams is only valid for backend type \"union\"")
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

	if isUnion {
		upstreams, err := b.unionUpstreams(env, alias, def.GetUnionUpstreams())
		if err != nil {
			return nil, err
		}
		vars[EnvName(alias, "upstreams")] = upstreams
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
// strings ("alias", "archive", "crypt", "combine"...) reach the worker's
// files without the local_roots check, and many cloud backends can sign in
// as the worker host (Azure CLI and managed identities, cloud metadata
// servers). "union" is supported because its upstreams are generated from
// typed union_upstreams. A type is added here only after every one of its
// options in the pinned rclone has been reviewed (see deniedOptions).
func SupportedBackends() []string {
	return []string{"b2", "memory", "s3", "sftp", "union", "webdav"}
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
// credentials. The controller's backend descriptors never emit them; this is
// defense in depth.
func deniedOptions() map[string]string {
	return map[string]string{ //nolint:gosec // G101 false positive: option names, not credentials
		"ssh":                     "runs a local command",
		"bearer_token_command":    "runs a local command",
		"unix_socket":             "connects to a local socket",
		"env_auth":                "uses the worker host's credentials",
		"shared_credentials_file": "reads a local file",
		"config_file":             "reads a local file",
		"kerberos_ccache":         "reads a local file",
		"client_certificate_path": "reads a local file",
		"db_path":                 "writes local files",
		"chunk_path":              "writes local files",
		"tmp_upload_path":         "writes local files",
	}
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
	return nil
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
		for _, s := range unionSuffixes() {
			if strings.HasSuffix(path, s) {
				return "", fmt.Errorf("a read-write upstream path must not end with %q (rclone would read it as a mode)", s)
			}
		}
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY:
		suffix = ":ro"
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE:
		suffix = ":nc"
	default:
		return "", fmt.Errorf("unknown mode %d", up.GetMode())
	}

	alias := loc.GetRemote()
	if alias == "" {
		if b.local == nil {
			return "", fmt.Errorf("%w: local upstreams are not available", ErrLocalPath)
		}
		resolved, err := b.local.Resolve(path)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrLocalPath, err)
		}
		return resolved + suffix, nil
	}
	backend, ok := env.types[alias]
	switch {
	case alias == self:
		return "", errors.New("a union cannot contain itself")
	case !ok:
		return "", fmt.Errorf("remote %q is not defined in this job", alias)
	case backend == "union":
		return "", fmt.Errorf("remote %q is a union; nested unions are not supported", alias)
	}
	return RemotePath(alias, path) + suffix, nil
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
