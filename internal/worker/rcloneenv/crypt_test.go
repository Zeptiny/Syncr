package rcloneenv

import (
	"errors"
	"maps"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// cryptPath is a wrapped path full of characters that would matter to a
// connection-string or INI parser (no line break: those are refused).
const cryptPath = `bucket/a'b"c,d:e f g=h;i\j` + "`k$l{m}n#o[p]q\tr"

// cryptRemote is a crypt remote "vault" over the remote "store" at
// cryptPath, with both passwords, as the controller sends it.
func cryptRemote(opts ...func(*syncrv1.RemoteDefinition)) *syncrv1.RemoteDefinition {
	base := []func(*syncrv1.RemoteDefinition){
		withWrapped("store", cryptPath),
		withOptions("filename_encryption", "standard", "directory_name_encryption", "true",
			"filename_encoding", "base32", "suffix", ".bin"),
		withSecrets("password", weird, "password2", "salt "+weird),
		withObscure("password", "password2"),
	}
	return remote("vault", "crypt", append(base, opts...)...)
}

func TestCryptRemote(t *testing.T) {
	t.Parallel()
	store := remote("store", "s3", withOptions("provider", "Minio"), withSecrets("secret_access_key", "s3cret"))
	env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{cryptRemote(), store})
	if err != nil {
		t.Fatal(err)
	}
	vars := env.Vars()
	pass, salt := vars["RCLONE_CONFIG_VAULT_PASSWORD"], vars["RCLONE_CONFIG_VAULT_PASSWORD2"]
	if pass == weird || reveal(t, pass) != weird {
		t.Errorf("password not obscured correctly: %q", pass)
	}
	if salt == "salt "+weird || reveal(t, salt) != "salt "+weird {
		t.Errorf("password2 not obscured correctly: %q", salt)
	}
	want := map[string]string{
		"RCLONE_CONFIG_VAULT_TYPE":                      "crypt",
		"RCLONE_CONFIG_VAULT_REMOTE":                    "store:" + cryptPath, // byte for byte, after the alias
		"RCLONE_CONFIG_VAULT_STRICT_NAMES":              "true",
		"RCLONE_CONFIG_VAULT_FILENAME_ENCRYPTION":       "standard",
		"RCLONE_CONFIG_VAULT_DIRECTORY_NAME_ENCRYPTION": "true",
		"RCLONE_CONFIG_VAULT_FILENAME_ENCODING":         "base32",
		"RCLONE_CONFIG_VAULT_SUFFIX":                    ".bin",
		"RCLONE_CONFIG_VAULT_PASSWORD":                  pass,
		"RCLONE_CONFIG_VAULT_PASSWORD2":                 salt,
		"RCLONE_CONFIG_STORE_TYPE":                      "s3",
		"RCLONE_CONFIG_STORE_PROVIDER":                  "Minio",
		"RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY":         "s3cret",
	}
	if !maps.Equal(vars, want) {
		t.Errorf("Vars() =\n%q\nwant\n%q", vars, want)
	}
	if bt, ok := env.BackendType("vault"); !ok || bt != "crypt" {
		t.Errorf("BackendType(vault) = %q, %v", bt, ok)
	}

	// Both passwords are scrubbed in both forms.
	secrets := env.Secrets()
	for _, s := range []string{weird, pass, "salt " + weird, salt, "s3cret"} {
		if !contains(secrets, s) {
			t.Errorf("Secrets() lacks %q", s)
		}
	}

	// The wrapped remote is never local: no local folders to check.
	if got := env.LocalPaths("vault", "photos"); got != nil {
		t.Errorf("LocalPaths(vault) = %q, want none", got)
	}
}

// TestCryptWrappedPaths: the wrapped path reaches rclone after the alias
// unchanged, whatever it is ("" is the wrapped remote's root).
func TestCryptWrappedPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", "/", "bucket", "bucket/sub dir/", "/abs/on/sftp", "x:y", ",type=local:/etc"} {
		env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
			remote("mem", "memory"), cryptRemote(withWrapped("mem", path)),
		})
		if err != nil {
			t.Fatalf("wrapped path %q: %v", path, err)
		}
		if got := env.Vars()["RCLONE_CONFIG_VAULT_REMOTE"]; got != "mem:"+path {
			t.Errorf("wrapped path %q: REMOTE = %q", path, got)
		}
	}
}

func TestEnvStorage(t *testing.T) {
	t.Parallel()
	env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
		remote("mem", "memory"), remote("other", "memory"),
		cryptRemote(withWrapped("mem", "bucket/vault")),
		remote("atroot", "crypt", withWrapped("other", ""), withSecrets("password", "p"), withObscure("password")),
		remote("slashed", "crypt", withWrapped("other", "/"), withSecrets("password", "p"), withObscure("password")),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ alias, path, wantAlias, wantPath string }{
		{"vault", "", "mem", "bucket/vault"},
		{"vault", "photos/2024", "mem", "bucket/vault/photos/2024"},
		{"vault", "/photos/", "mem", "bucket/vault/photos/"},
		{"atroot", "photos", "other", "photos"},
		{"atroot", "", "other", ""},
		{"slashed", "photos", "other", "/photos"},
		{"mem", "bucket/x", "mem", "bucket/x"}, // not a crypt remote: unchanged
		{"nope", "x", "nope", "x"},
	} {
		if a, p := env.Storage(tt.alias, tt.path); a != tt.wantAlias || p != tt.wantPath {
			t.Errorf("Storage(%q, %q) = %q, %q; want %q, %q", tt.alias, tt.path, a, p, tt.wantAlias, tt.wantPath)
		}
	}
}

// TestCryptRejects: every crypt remote, and every wrapped location, that the
// worker must refuse, with the reason, naming aliases and keys only.
func TestCryptRejects(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-VALUE"
	rw := syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE
	mem := func(alias string) *syncrv1.RemoteDefinition { return remote(alias, "memory") }
	// crypt is a valid crypt remote over aa (defined by the cases), changed
	// by opts.
	crypt := func(alias string, opts ...func(*syncrv1.RemoteDefinition)) *syncrv1.RemoteDefinition {
		base := []func(*syncrv1.RemoteDefinition){
			withWrapped("aa", "vault-"+secret), withSecrets("password", secret), withObscure("password"),
		}
		return remote(alias, "crypt", append(base, opts...)...)
	}
	union := func(alias string, ups ...*syncrv1.UnionUpstream) *syncrv1.RemoteDefinition {
		return remote(alias, "union", withUpstreams(ups...))
	}
	twoUps := withUpstreams(upstream("aa", "", rw), upstream("aa", "x", rw))

	tests := []struct {
		name    string
		remotes []*syncrv1.RemoteDefinition
		want    string
	}{
		{"no wrapped", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", func(d *syncrv1.RemoteDefinition) { d.Wrapped = nil })}, "wrapped is required"},
		{"a local folder", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withWrapped("", "/etc/shadow"))}, "cannot store its data in a local folder"},
		{"an undefined remote", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withWrapped("bb", secret))}, `wrapped remote "bb" is not defined`},
		{"itself", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withWrapped("cr", secret))}, "in itself"},
		{"a union", []*syncrv1.RemoteDefinition{mem("aa"), mem("bb"), union("un", upstream("aa", "", rw), upstream("bb", "", rw)), crypt("cr", withWrapped("un", secret))}, `wrapped remote "un" is a union; nested wrappers`},
		{"a crypt remote", []*syncrv1.RemoteDefinition{mem("aa"), crypt("c1"), crypt("c2", withWrapped("c1", secret))}, `wrapped remote "c1" is a crypt remote; nested wrappers`},
		{"a crypt remote defined later", []*syncrv1.RemoteDefinition{mem("aa"), crypt("c2", withWrapped("c1", secret)), crypt("c1")}, `wrapped remote "c1" is a crypt remote`},
		{"path with a newline", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withWrapped("aa", secret+"\n/etc/shadow"))}, "line break or NUL"},
		{"path with a CR", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withWrapped("aa", secret+"\r"))}, "line break or NUL"},
		{"path with a NUL", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withWrapped("aa", secret+"\x00"))}, "line break or NUL"},
		{"remote option", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("remote", "/etc/shadow"))}, `option "remote" is not allowed; it is set by the worker from wrapped`},
		{"Remote option", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("Remote", "/etc/shadow"))}, `option "Remote" is not allowed`},
		{"remote secret", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withSecrets("password", secret, "remote", "/etc/shadow"), withObscure("password"))}, `option "remote" is not allowed`},
		{"strict_names off", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("strict_names", "false"))}, `option "strict_names" is not allowed; the worker always sets it`},
		{"STRICT-NAMES off", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("STRICT-NAMES", "false"))}, `option "STRICT-NAMES" is not allowed`},
		{"pass_bad_blocks", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("pass_bad_blocks", "true"))}, `option "pass_bad_blocks" is not allowed`},
		{"password as an option", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("password", secret))}, `option "password" must be a secret`},
		{"password2 as an option", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withOptions("Password2", secret))}, `option "Password2" must be a secret`},
		{"password not obscured", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withObscure())}, `secret "password" must be listed in obscure_keys`},
		{"password2 not obscured", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", withSecrets("password", secret, "password2", secret+"2"))}, `secret "password2" must be listed in obscure_keys`},
		{"union upstreams", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr", twoUps)}, "union_upstreams is only valid"},
		{"a crypt union upstream", []*syncrv1.RemoteDefinition{mem("aa"), crypt("cr"), union("un", upstream("aa", "", rw), upstream("cr", secret, rw))}, `remote "cr" is a crypt remote; nested wrappers`},
		{"wrapped on s3", []*syncrv1.RemoteDefinition{mem("aa"), remote("src", "s3", withWrapped("aa", secret))}, `wrapped is only valid for backend type "crypt"`},
		{"wrapped on a union", []*syncrv1.RemoteDefinition{mem("aa"), remote("un", "union", twoUps, withWrapped("aa", secret))}, `wrapped is only valid`},
		{"an empty wrapped on memory", []*syncrv1.RemoteDefinition{mem("aa"), remote("src", "memory", func(d *syncrv1.RemoteDefinition) { d.Wrapped = &syncrv1.Location{} })}, `wrapped is only valid`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewBuilder(fakeResolver{allow: "/"}).Remotes(tt.remotes)
			if err == nil {
				t.Fatal("Remotes() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if !errors.Is(err, ErrInvalidRemote) || errors.Is(err, ErrLocalPath) {
				t.Errorf("err = %v, want ErrInvalidRemote only", err)
			}
			if _, ok := errors.AsType[*RemoteError](err); !ok {
				t.Errorf("err = %T, want *RemoteError", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/etc/shadow") {
				t.Errorf("error message leaks a value: %v", err)
			}
		})
	}
}
