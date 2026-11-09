package sftp_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/sftp"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, sftp.Type{})
}

func TestValidateAndMap(t *testing.T) {
	t.Parallel()
	base := map[string]string{"host": "h", "user": "u"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{"host": "h", "user": "u"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tt := range []struct {
		name    string
		opts    map[string]string
		secrets map[string]string
		wantErr bool
	}{
		{"password", base, map[string]string{"pass": "p"}, false},
		{"key", base, map[string]string{"key_pem": "k"}, false},
		{"neither password nor key", base, map[string]string{"key_file_pass": "x"}, true},
		{"port out of range", with("port", "70000"), map[string]string{"pass": "p"}, true},
		{"port zero", with("port", "0"), map[string]string{"pass": "p"}, true},
		{"dot-dot path", with("path", "/srv/../etc"), map[string]string{"pass": "p"}, true},
		{"key escaped onto one line", base, map[string]string{"key_pem": `-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----`}, true},
	} {
		if err := (sftp.Type{}).Validate(backend.Config{Options: tt.opts, Secrets: tt.secrets}); (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate = %v, want error %v", tt.name, err, tt.wantErr)
		}
	}

	r, err := sftp.Type{}.ToRclone(backend.Config{Options: with("path", "/srv//data/"), Secrets: map[string]string{"pass": "p", "key_file_pass": "k"}})
	if err != nil || r.Root != "/srv/data" || r.Options["port"] != "22" || !slices.Equal(r.ObscureKeys, []string{"pass", "key_file_pass"}) {
		t.Errorf("ToRclone = %+v, %v", r, err)
	}
}

// unquoteLikeRclone parses a key_pem value the way rclone's sftp backend
// does before it decodes the PEM.
func unquoteLikeRclone(t *testing.T, v string) string {
	t.Helper()
	s, err := strconv.Unquote(`"` + v + `"`)
	if err != nil {
		t.Fatalf("rclone would reject key_pem %q (pem key not formatted properly): %v", v, err)
	}
	return s
}

func TestToRcloneEscapesKeyPem(t *testing.T) {
	t.Parallel()
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nBG5vbmU=\n-----END OPENSSH PRIVATE KEY-----\n"
	for _, key := range []string{
		pem,
		strings.ReplaceAll(pem, "\n", "\r\n"), // pasted on Windows
		strings.TrimSuffix(pem, "\n"),
		"-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00FF\n\nAAAA\n-----END RSA PRIVATE KEY-----\n",
		// Not PEM, but rclone must still read back exactly the input.
		`a\b"c` + "\t\x00\x7fé \xff\xfe end",
	} {
		r, err := sftp.Type{}.ToRclone(backend.Config{
			Options: map[string]string{"host": "h", "user": "u"},
			Secrets: map[string]string{"key_pem": key, "key_file_pass": "k"},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := r.Secrets["key_pem"]
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("key_pem %q is not escaped onto one line: %q", key, got)
		}
		if back := unquoteLikeRclone(t, got); back != key {
			t.Errorf("rclone would read key_pem %q as %q", key, back)
		}
		if r.Secrets["key_file_pass"] != "k" {
			t.Errorf("key_file_pass changed to %q", r.Secrets["key_file_pass"])
		}
	}

	r, err := sftp.Type{}.ToRclone(backend.Config{Options: map[string]string{"host": "h", "user": "u"}, Secrets: map[string]string{"pass": "p"}})
	if _, ok := r.Secrets["key_pem"]; err != nil || ok {
		t.Errorf("ToRclone without a key = %+v, %v; want no key_pem", r, err)
	}
}
