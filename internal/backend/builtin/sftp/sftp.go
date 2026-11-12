// Package sftp is the backend descriptor of SFTP servers (rclone backend
// "sftp").
//
// The server's host keys are pinned with the host_keys field (rclone's
// host_keys option, full public keys rather than fingerprints): rclone then
// refuses a server that offers another key. Without them rclone accepts
// any key. rclone's known_hosts_file cannot be used, because the worker
// refuses options that read local files, and neither can pin_host_key,
// which writes into the per-job config file the worker discards.
package sftp

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the sftp backend type.
type Type struct{}

var (
	_ backend.Exampler        = Type{}
	_ backend.InvalidExampler = Type{}
)

// ID implements backend.Type.
func (Type) ID() string { return "sftp" }

// Label implements backend.Type.
func (Type) Label() string { return "SFTP" }

// Description implements backend.Type.
func (Type) Description() string {
	return "A server reachable over SSH, with a password or a private key."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "host", Label: "Host", Kind: backend.KindString, Required: true, Placeholder: "sftp.example.com"},
		{Name: "port", Label: "Port", Kind: backend.KindInt, Default: "22"},
		{Name: "user", Label: "User", Kind: backend.KindString, Required: true},
		{Name: "pass", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true,
			Help: "A password, a private key or both."},
		{Name: "key_pem", Label: "Private key", Kind: backend.KindText, Secret: true,
			Help: "PEM-encoded private key (-----BEGIN OPENSSH PRIVATE KEY----- ...)."},
		{Name: "key_file_pass", Label: "Private key passphrase", Kind: backend.KindString, Secret: true, Obscure: true, Advanced: true},
		{Name: "host_keys", Label: "Server host keys", Kind: backend.KindText,
			Placeholder: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...",
			Help: "The server's public host keys, one per line: \"<algorithm> <base64 key>\", as in a known_hosts line " +
				"without the host name. A server that offers another key is refused. Without them, the server is not verified."},
		{Name: "path", Label: "Base path", Kind: backend.KindString, Rclone: backend.NotRclone,
			Help: "Directory that job paths are relative to; empty means the login directory."},
		{Name: "shell_type", Label: "Remote shell", Kind: backend.KindEnum, Advanced: true, Choices: []backend.Choice{
			{Value: "unix", Label: "Unix shell"}, {Value: "powershell", Label: "PowerShell"},
			{Value: "cmd", Label: "Windows cmd"}, {Value: "none", Label: "No shell access"},
		}},
		{Name: "disable_hashcheck", Label: "Disable hash checks", Kind: backend.KindBool, Advanced: true},
		{Name: "md5sum_command", Label: "MD5 command", Kind: backend.KindString, Advanced: true, Help: "Run on the server."},
		{Name: "sha1sum_command", Label: "SHA-1 command", Kind: backend.KindString, Advanced: true, Help: "Run on the server."},
		{Name: "use_insecure_cipher", Label: "Allow insecure ciphers", Kind: backend.KindBool, Advanced: true},
	}
}

// Capabilities implements backend.Type.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true}
}

// Validate implements backend.Type: the field checks, a password or a
// private key, a key pasted as it is, a port in range, well-formed host
// keys (see checkHostKey; at most MaxHostKeys) and a well-formed base path.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if cfg.Secrets["pass"] == "" && cfg.Secrets["key_pem"] == "" {
		v.Add("secrets.pass", "a password or a private key is required")
	}
	// A PEM key never contains a backslash. One is most likely a key
	// already escaped onto one line ("...\n...", as rclone's own docs
	// suggest), which ToRclone would escape again.
	if strings.Contains(cfg.Secrets["key_pem"], `\`) {
		v.Add("secrets.key_pem", `paste the key file as it is, with its line breaks (not escaped with \n)`)
	}
	if port := cfg.Options["port"]; port != "" && !portInRange(port) {
		v.Add("options.port", "must be between 1 and 65535")
	}
	for _, msg := range checkHostKeys(cfg.Options["host_keys"]) {
		v.Add("options.host_keys", msg)
	}
	if _, err := backend.JoinRemote(cfg.Options["path"], ""); err != nil {
		v.Add("options.path", err.Error())
	}
	return v.Err()
}

// portInRange reports whether p is a port number; a non-number is reported
// by CheckFields.
func portInRange(p string) bool {
	n, err := strconv.Atoi(p)
	return err != nil || (n >= 1 && n <= 65535)
}

// ToRclone implements backend.Type; the base path becomes the Root, the
// private key is escaped onto one line (see escapeKeyPem), and the host keys
// are normalized to rclone's comma-separated form (normalizeHostKeys).
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "sftp"
	r.Root, _ = backend.JoinRemote(cfg.Options["path"], "")
	if keys, ok := r.Options["host_keys"]; ok {
		if keys = normalizeHostKeys(keys); keys != "" {
			r.Options["host_keys"] = keys
		} else {
			delete(r.Options, "host_keys")
		}
	}
	if pem, ok := r.Secrets["key_pem"]; ok {
		r.Secrets["key_pem"] = escapeKeyPem(pem)
	}
	return r, nil
}

// escapeKeyPem returns pem in the form rclone expects for its key_pem
// option. rclone does not take the value as it is: it reads it as the body
// of a Go string literal (strconv.Unquote of `"` + value + `"`). The raw
// line breaks of a pasted key make every connection fail with "pem key not
// formatted properly", and double quotes or backslashes would be misread
// too. The result is the body of strconv.Quote(pem): one line, which rclone
// unquotes back to exactly pem.
func escapeKeyPem(pem string) string {
	q := strconv.Quote(pem)
	return q[1 : len(q)-1]
}

// Sample host keys of the examples (generated for them; no server has
// them).
const (
	sampleEd25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMUiJpUbcNynH9rHhq4iK4jdXA7Qg/xNVqitUwiNweax"
	sampleECDSA   = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBH9xAeBCuXqzBfh9cf86Gd6fiPvC0DUhn7/y6t//sQHCSwUg+aU5NxZuU/ADvvNXRTI5S3AoUmxGN7kbZLiU6XQ="
	sampleRSA     = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQDUUl8WFrAIi3QuIprZ/el7Df+BIiZfUqpCn4jsxVNRFgQI0+lsFlN+O0BpDKxnlghPC8o1s41Y5mGzx3jRutTMC9RbgyJJsfdGn4dBPHvTX5onJXRj61ktX96ncl3rmuSJ/hgpk/GCi9i99vrUhUtNo3kTxvk7CBGebA4H95tvsQ=="
)

// Examples implements backend.Exampler: host keys one per line (with a
// Windows line break and blank lines) and comma-separated.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"host": "nas.example.com", "user": "backup"}, Secrets: map[string]string{"pass": "hunter2"}},
		{
			Options: map[string]string{"host": "nas.example.com", "user": "backup", "host_keys": sampleEd25519 + "," + sampleRSA},
			Secrets: map[string]string{"pass": "hunter2"},
		},
		{
			Options: map[string]string{
				"host": "storage.example.com", "port": "2222", "user": "u123", "path": "/srv/backups",
				"host_keys":  sampleEd25519 + "\r\n" + sampleECDSA + "\n\n  " + sampleRSA + "  \n",
				"shell_type": "unix", "disable_hashcheck": "false", "md5sum_command": "md5sum", "sha1sum_command": "sha1sum",
				"use_insecure_cipher": "false",
			},
			Secrets: map[string]string{
				"pass": "pw", "key_pem": "-----BEGIN OPENSSH PRIVATE KEY-----\nexample\n-----END OPENSSH PRIVATE KEY-----\n",
				"key_file_pass": "passphrase",
			},
		},
	}
}

// InvalidExamples implements backend.InvalidExampler: host keys that are
// malformed, of another algorithm than their name or too many.
func (Type) InvalidExamples() []backend.InvalidExample {
	cfg := func(hostKeys string) backend.Config {
		return backend.Config{
			Options: map[string]string{"host": "nas.example.com", "user": "backup", "host_keys": hostKeys},
			Secrets: map[string]string{"pass": "hunter2"},
		}
	}
	const field = "options.host_keys"
	ed25519Key := strings.Fields(sampleEd25519)[1]
	return []backend.InvalidExample{
		{Name: "algorithm only", Config: cfg("ssh-ed25519"), Field: field},
		{Name: "key only", Config: cfg(ed25519Key), Field: field},
		{Name: "known_hosts line with the host", Config: cfg("nas.example.com " + sampleEd25519), Field: field},
		{Name: "public key file with a comment", Config: cfg(sampleEd25519 + " root@nas"), Field: field},
		{Name: "wrong algorithm name", Config: cfg("ssh-rsa " + ed25519Key), Field: field},
		{Name: "bad base64", Config: cfg("ssh-ed25519 AAAA$$$$"), Field: field},
		{Name: "unpadded base64", Config: cfg("ssh-ed25519 " + strings.TrimRight(strings.Fields(sampleECDSA)[1], "=")), Field: field},
		{Name: "not a key blob", Config: cfg("ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("not a key"))), Field: field},
		{Name: "algorithm name without key data", Config: cfg("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5"), Field: field},
		{Name: "one bad entry among good ones", Config: cfg(sampleEd25519 + "\nssh-ed25519 AAAA$$$$\n" + sampleRSA), Field: field},
		{Name: "too many keys", Config: cfg(strings.Repeat(sampleEd25519+"\n", MaxHostKeys+1)), Field: field},
	}
}
