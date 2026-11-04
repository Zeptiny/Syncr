package rcloneenv

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// weird contains every character that could matter to a connection-string,
// shell, CSV or INI parser.
const weird = "a'b\"c,d:e f\n g=h;i\\j`k$l{m}n#o[p]q\tr"

// reveal is the inverse of Obscure (rclone's fs/config/obscure.Reveal).
func reveal(t *testing.T, s string) string {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(data) < aes.BlockSize {
		t.Fatalf("reveal(%q): not an obscured value: %v", s, err)
	}
	block, err := aes.NewCipher([]byte(obscureKey))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(data)-aes.BlockSize)
	cipher.NewCTR(block, data[:aes.BlockSize]).XORKeyStream(out, data[aes.BlockSize:])
	return string(out)
}

func remote(alias, backend string, opts ...func(*syncrv1.RemoteDefinition)) *syncrv1.RemoteDefinition {
	d := &syncrv1.RemoteDefinition{Alias: alias, BackendType: backend}
	for _, o := range opts {
		o(d)
	}
	return d
}

func withOptions(kv ...string) func(*syncrv1.RemoteDefinition) {
	return func(d *syncrv1.RemoteDefinition) {
		d.Options = map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			d.Options[kv[i]] = kv[i+1]
		}
	}
}

func withSecrets(kv ...string) func(*syncrv1.RemoteDefinition) {
	return func(d *syncrv1.RemoteDefinition) {
		d.Secrets = map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			d.Secrets[kv[i]] = kv[i+1]
		}
	}
}

func withObscure(keys ...string) func(*syncrv1.RemoteDefinition) {
	return func(d *syncrv1.RemoteDefinition) { d.ObscureKeys = keys }
}

func withUpstreams(ups ...*syncrv1.UnionUpstream) func(*syncrv1.RemoteDefinition) {
	return func(d *syncrv1.RemoteDefinition) { d.UnionUpstreams = ups }
}

func upstream(alias, path string, mode syncrv1.UnionUpstreamMode) *syncrv1.UnionUpstream {
	return &syncrv1.UnionUpstream{Location: &syncrv1.Location{Remote: alias, Path: path}, Mode: mode}
}

// fakeResolver accepts paths under allow and returns them with a marker
// prefix, so tests can see that the resolved path was used.
type fakeResolver struct{ allow string }

var errOutside = errors.New("outside local_roots")

func (f fakeResolver) Resolve(path string) (string, error) {
	if !strings.HasPrefix(path, f.allow) {
		return "", errOutside
	}
	return "/resolved" + path, nil
}

func TestEnvName(t *testing.T) {
	t.Parallel()
	tests := map[[2]string]string{
		{"src", "type"}:              "RCLONE_CONFIG_SRC_TYPE",
		{"s3_eu", "access_key_id"}:   "RCLONE_CONFIG_S3_EU_ACCESS_KEY_ID",
		{"my_sftp", "key-use-agent"}: "RCLONE_CONFIG_MY_SFTP_KEY_USE_AGENT",
		{"web", "Bearer_Token"}:      "RCLONE_CONFIG_WEB_BEARER_TOKEN",
	}
	for in, want := range tests {
		if got := EnvName(in[0], in[1]); got != want {
			t.Errorf("EnvName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestRemotes(t *testing.T) {
	t.Parallel()
	env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
		remote("minio", "s3",
			withOptions("provider", "Minio", "endpoint", "http://127.0.0.1:9000", "force-path-style", "true"),
			withSecrets("access_key_id", "AKID", "secret_access_key", weird)),
		remote("mem2", "memory"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"RCLONE_CONFIG_MINIO_TYPE":              "s3",
		"RCLONE_CONFIG_MINIO_PROVIDER":          "Minio",
		"RCLONE_CONFIG_MINIO_ENDPOINT":          "http://127.0.0.1:9000",
		"RCLONE_CONFIG_MINIO_FORCE_PATH_STYLE":  "true",
		"RCLONE_CONFIG_MINIO_ACCESS_KEY_ID":     "AKID",
		"RCLONE_CONFIG_MINIO_SECRET_ACCESS_KEY": weird, // unchanged, byte for byte
		"RCLONE_CONFIG_MEM2_TYPE":               "memory",
	}
	if got := env.Vars(); !maps.Equal(got, want) {
		t.Errorf("Vars() =\n%q\nwant\n%q", got, want)
	}
	if bt, ok := env.BackendType("minio"); !ok || bt != "s3" {
		t.Errorf("BackendType(minio) = %q, %v", bt, ok)
	}
	if _, ok := env.BackendType("nope"); ok {
		t.Error("BackendType(nope) reported a remote")
	}

	// Vars returns a copy.
	env.Vars()["RCLONE_CONFIG_MINIO_TYPE"] = "changed"
	if env.Vars()["RCLONE_CONFIG_MINIO_TYPE"] != "s3" {
		t.Error("Vars() exposed internal state")
	}
}

func TestValuesAreNeverParsed(t *testing.T) {
	t.Parallel()
	// Values that look like connection strings, INI sections or CSV lists
	// pass through verbatim; only rclone's option parser for that one
	// option ever sees them.
	values := []string{weird, ":local,copy_links:/etc", "x\n[evil]\ntype = local", `"quoted" 'single'`, " lead and trail ", ""}
	for _, v := range values {
		env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
			remote("web", "webdav", withOptions("url", v, "user", v), withSecrets("bearer_token", v)),
		})
		if err != nil {
			t.Fatalf("value %q: %v", v, err)
		}
		vars := env.Vars()
		for _, k := range []string{"RCLONE_CONFIG_WEB_URL", "RCLONE_CONFIG_WEB_USER", "RCLONE_CONFIG_WEB_BEARER_TOKEN"} {
			if vars[k] != v {
				t.Errorf("%s = %q, want %q", k, vars[k], v)
			}
		}
	}
}

func TestObscureKeys(t *testing.T) {
	t.Parallel()
	env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
		remote("box", "sftp",
			withOptions("host", "example.com", "user", "syncr"),
			withSecrets("pass", weird, "key_pem", "-----BEGIN KEY-----"),
			withObscure("pass")),
	})
	if err != nil {
		t.Fatal(err)
	}
	vars := env.Vars()
	pass := vars["RCLONE_CONFIG_BOX_PASS"]
	if pass == weird || reveal(t, pass) != weird {
		t.Errorf("pass not obscured correctly: %q", pass)
	}
	if vars["RCLONE_CONFIG_BOX_KEY_PEM"] != "-----BEGIN KEY-----" {
		t.Error("a secret not listed in obscure_keys was changed")
	}
	// Both forms are scrubbed.
	secrets := env.Secrets()
	for _, s := range []string{weird, pass, "-----BEGIN KEY-----"} {
		if !contains(secrets, s) {
			t.Errorf("Secrets() lacks %q", s)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestObscureMatchesRclone(t *testing.T) {
	t.Parallel()
	// Produced by `rclone obscure 'pa:ss,w"o rd'` (rclone v1.75.2).
	if got := reveal(t, "HWnu4-iiEBw0YVJuNADGQ83Z1jLYlGWmmiYJAA"); got != `pa:ss,w"o rd` {
		t.Errorf("reveal(rclone output) = %q", got)
	}
	for _, plain := range []string{"", "x", weird, strings.Repeat("long", 1000)} {
		o, err := Obscure(plain)
		if err != nil {
			t.Fatal(err)
		}
		if got := reveal(t, o); got != plain {
			t.Errorf("reveal(Obscure(%q)) = %q", plain, got)
		}
	}
	a, _ := Obscure("same")
	b, _ := Obscure("same")
	if a == b {
		t.Error("Obscure is deterministic; it must use a random IV")
	}
	// A fixed IV gives a fixed output (IV || ciphertext); `rclone reveal`
	// prints "potato" for this value.
	iv := strings.NewReader(strings.Repeat("\x00", aes.BlockSize))
	if o, err := obscure("potato", iv); err != nil || o != "AAAAAAAAAAAAAAAAAAAAADS03QMhkA" {
		t.Errorf("obscure with zero IV = (%q, %v)", o, err)
	}
	if _, err := obscure("x", strings.NewReader("short")); err == nil {
		t.Error("obscure with a failing random source succeeded")
	}
}

func TestRemotesRejects(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-VALUE"
	ok := func(alias string) *syncrv1.RemoteDefinition { return remote(alias, "memory") }
	union := func(alias string, ups ...*syncrv1.UnionUpstream) *syncrv1.RemoteDefinition {
		return remote(alias, "union", withUpstreams(ups...), withSecrets("x", secret))
	}
	rw := syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE

	tests := []struct {
		name    string
		remotes []*syncrv1.RemoteDefinition
		local   bool // the error wraps ErrLocalPath
	}{
		{"nil definition", []*syncrv1.RemoteDefinition{nil}, false},
		{"empty alias", []*syncrv1.RemoteDefinition{ok("")}, false},
		{"single-letter alias (drive letter)", []*syncrv1.RemoteDefinition{ok("c")}, false},
		{"uppercase alias", []*syncrv1.RemoteDefinition{ok("Src")}, false},
		{"alias with dash", []*syncrv1.RemoteDefinition{ok("my-remote")}, false},
		{"alias with colon", []*syncrv1.RemoteDefinition{ok("a:b")}, false},
		{"alias starting with digit", []*syncrv1.RemoteDefinition{ok("1abc")}, false},
		{"alias too long", []*syncrv1.RemoteDefinition{ok("a" + strings.Repeat("b", 63))}, false},
		{"duplicate alias", []*syncrv1.RemoteDefinition{ok("src"), ok("src")}, false},
		{"overlapping aliases", []*syncrv1.RemoteDefinition{ok("s3_eu"), ok("s3")}, false},
		{"empty backend", []*syncrv1.RemoteDefinition{remote("src", "")}, false},
		{"backend with colon", []*syncrv1.RemoteDefinition{remote("src", "s3:")}, false},
		{"uppercase backend", []*syncrv1.RemoteDefinition{remote("src", "S3")}, false},
		{"local backend", []*syncrv1.RemoteDefinition{remote("src", "local")}, false},
		{"alias backend", []*syncrv1.RemoteDefinition{remote("src", "alias", withOptions("remote", "/etc"))}, false},
		{"crypt backend", []*syncrv1.RemoteDefinition{remote("src", "crypt")}, false},
		{"combine backend", []*syncrv1.RemoteDefinition{remote("src", "combine")}, false},
		{"empty option key", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("", "x"))}, false},
		{"option key with =", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("a=b", "x"))}, false},
		{"option key with space", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("a b", "x"))}, false},
		{"option key starting with dash", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("-x", "x"))}, false},
		{"option key non-ASCII", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("clé", "x"))}, false},
		{"type option", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("type", "local"))}, false},
		{"TYPE secret", []*syncrv1.RemoteDefinition{remote("src", "s3", withSecrets("Type", "local"))}, false},
		{"dash/underscore collision", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("foo-bar", "1", "foo_bar", "2"))}, false},
		{"case collision", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("Foo", "1", "foo", "2"))}, false},
		{"option/secret collision", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("key", "1"), withSecrets("key", secret))}, false},
		{"NUL in option", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("region", "a\x00b"))}, false},
		{"NUL in secret", []*syncrv1.RemoteDefinition{remote("src", "s3", withSecrets("k", secret+"\x00"))}, false},
		{"NUL in obscured secret", []*syncrv1.RemoteDefinition{remote("src", "sftp", withSecrets("pass", secret+"\x00"), withObscure("pass"))}, false},
		{"ssh command", []*syncrv1.RemoteDefinition{remote("src", "sftp", withOptions("ssh", "sh -c evil"))}, false},
		{"bearer token command", []*syncrv1.RemoteDefinition{remote("src", "webdav", withOptions("bearer-token-command", "evil"))}, false},
		{"local key file", []*syncrv1.RemoteDefinition{remote("src", "sftp", withOptions("key_file", "/etc/shadow"))}, false},
		{"local file, uppercase", []*syncrv1.RemoteDefinition{remote("src", "drive", withOptions("Service_Account_File", "/x"))}, false},
		{"env auth", []*syncrv1.RemoteDefinition{remote("src", "s3", withOptions("env_auth", "true"))}, false},
		{"unix socket", []*syncrv1.RemoteDefinition{remote("src", "webdav", withOptions("unix_socket", "/run/docker.sock"))}, false},
		{"obscure key not a secret", []*syncrv1.RemoteDefinition{remote("src", "sftp", withOptions("pass", "x"), withObscure("pass"))}, false},
		{"obscure key twice", []*syncrv1.RemoteDefinition{remote("src", "sftp", withSecrets("pass", secret), withObscure("pass", "pass"))}, false},
		{"upstreams on non-union", []*syncrv1.RemoteDefinition{ok("aa"), remote("src", "s3", withUpstreams(upstream("aa", "", rw), upstream("aa", "x", rw)))}, false},
		{"union upstreams option", []*syncrv1.RemoteDefinition{ok("aa"), remote("un", "union", withOptions("upstreams", "/etc /root"), withUpstreams(upstream("aa", "", rw), upstream("aa", "x", rw)))}, false},
		{"union remotes option", []*syncrv1.RemoteDefinition{ok("aa"), remote("un", "union", withOptions("remotes", "/etc"), withUpstreams(upstream("aa", "", rw), upstream("aa", "x", rw)))}, false},
		{"union without upstreams", []*syncrv1.RemoteDefinition{union("un")}, false},
		{"union with one upstream", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw))}, false},
		{"union of unknown alias", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("bb", "", rw))}, false},
		{"union of itself", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("un", "", rw))}, false},
		{"nested union", []*syncrv1.RemoteDefinition{ok("aa"), ok("bb"), union("u1", upstream("aa", "", rw), upstream("bb", "", rw)), union("u2", upstream("aa", "", rw), upstream("u1", "", rw))}, false},
		{"upstream without location", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), &syncrv1.UnionUpstream{})}, false},
		{"upstream path ending in :ro", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("aa", "dir:ro", rw))}, false},
		{"upstream path ending in :nc", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("aa", "dir:nc", 0))}, false},
		{"upstream path ending in :writeback", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("aa", "d:writeback", rw))}, false},
		{"upstream path with newline", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("aa", "a\nb", rw))}, false},
		{"upstream path with CR", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("aa", "a\rb", rw))}, false},
		{"upstream unknown mode", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("aa", "x", 42))}, false},
		{"local upstream outside roots", []*syncrv1.RemoteDefinition{ok("aa"), union("un", upstream("aa", "", rw), upstream("", "/etc", rw))}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewBuilder(fakeResolver{allow: "/data/"}).Remotes(tt.remotes)
			if err == nil {
				t.Fatal("Remotes() succeeded, want an error")
			}
			if !errors.Is(err, ErrInvalidRemote) {
				t.Errorf("err = %v, does not match ErrInvalidRemote", err)
			}
			if _, ok := errors.AsType[*RemoteError](err); !ok {
				t.Errorf("err = %T, want *RemoteError", err)
			}
			if got := errors.Is(err, ErrLocalPath); got != tt.local {
				t.Errorf("errors.Is(err, ErrLocalPath) = %v, want %v (err %v)", got, tt.local, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/etc/shadow") {
				t.Errorf("error message leaks a value: %v", err)
			}
		})
	}
}

func TestLocalUpstreamWithoutResolver(t *testing.T) {
	t.Parallel()
	_, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
		remote("un", "union", withUpstreams(upstream("", "/data/a", 0), upstream("", "/data/b", 0))),
	})
	if !errors.Is(err, ErrLocalPath) {
		t.Errorf("err = %v, want ErrLocalPath", err)
	}
}

// parseSpaceSepList parses an upstreams value the way rclone does
// (fs.SpaceSepList: encoding/csv with a space separator, first record only).
func parseSpaceSepList(t *testing.T, s string) []string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = ' '
	rec, err := r.Read()
	if err != nil {
		t.Fatalf("rclone would fail to parse upstreams %q: %v", s, err)
	}
	return rec
}

func TestUnionUpstreams(t *testing.T) {
	t.Parallel()
	ro := syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY
	nc := syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE
	rw := syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE
	env, err := NewBuilder(fakeResolver{allow: "/data/"}).Remotes([]*syncrv1.RemoteDefinition{
		remote("mem_a", "memory"),
		remote("bucket", "s3", withOptions("provider", "Minio")),
		remote("un", "union",
			withOptions("action_policy", "all", "create_policy", "ff"),
			withUpstreams(
				upstream("mem_a", "", 0),
				upstream("bucket", `photos/my "best" shots, 2024: summer`, ro),
				upstream("bucket", " leading space/x:ro", nc), // the mode suffix is appended after
				upstream("", "/data/local dir", rw),
				upstream("mem_a", "ünïcödé/'single'", rw),
			)),
	})
	if err != nil {
		t.Fatal(err)
	}
	vars := env.Vars()
	if vars["RCLONE_CONFIG_UN_TYPE"] != "union" || vars["RCLONE_CONFIG_UN_ACTION_POLICY"] != "all" {
		t.Errorf("union vars = %q", vars)
	}
	got := parseSpaceSepList(t, vars["RCLONE_CONFIG_UN_UPSTREAMS"])
	want := []string{
		"mem_a:",
		`bucket:photos/my "best" shots, 2024: summer:ro`,
		"bucket: leading space/x:ro:nc",
		"/resolved/data/local dir",
		"mem_a:ünïcödé/'single'",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("upstreams parse as\n%q\nwant\n%q\n(raw %q)", got, want, vars["RCLONE_CONFIG_UN_UPSTREAMS"])
	}
}

func TestIsolate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vars, err := Isolate(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		EnvConfig:      filepath.Join(dir, "rclone.conf"),
		EnvCacheDir:    filepath.Join(dir, "cache"),
		EnvAskPassword: "false",
	}
	if !maps.Equal(vars, want) {
		t.Errorf("Isolate() = %q, want %q", vars, want)
	}
	if fi, err := os.Stat(want[EnvConfig]); err != nil || fi.Size() != 0 || !fi.Mode().IsRegular() {
		t.Errorf("config file: %v, %v", fi, err)
	}
	if fi, err := os.Stat(want[EnvCacheDir]); err != nil || !fi.IsDir() {
		t.Errorf("cache dir: %v, %v", fi, err)
	}

	// An existing config file is never reused: it might not be empty.
	if _, err := Isolate(dir); err == nil {
		t.Error("Isolate() reused an existing config file")
	}
	if _, err := Isolate("relative"); err == nil {
		t.Error("Isolate(relative) succeeded")
	}
}

func TestBuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env, err := NewBuilder(nil).Build(dir, []*syncrv1.RemoteDefinition{remote("mem", "memory")})
	if err != nil {
		t.Fatal(err)
	}
	vars := env.Vars()
	if vars["RCLONE_CONFIG_MEM_TYPE"] != "memory" || vars[EnvConfig] != filepath.Join(dir, "rclone.conf") || vars[EnvAskPassword] != "false" {
		t.Errorf("Build() vars = %q", vars)
	}
	if _, err := NewBuilder(nil).Build(t.TempDir(), []*syncrv1.RemoteDefinition{remote("x", "memory")}); err == nil {
		t.Error("Build() with an invalid remote succeeded")
	}
}

func TestRemotePath(t *testing.T) {
	t.Parallel()
	if got := RemotePath("src", ",type=local:/etc"); got != "src:,type=local:/etc" {
		t.Errorf("RemotePath() = %q", got)
	}
}
