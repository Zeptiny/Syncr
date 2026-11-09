// Package sftp is the backend descriptor of SFTP servers (rclone backend
// "sftp").
//
// Host keys cannot be pinned yet: rclone reads them from a known_hosts file,
// and the worker refuses options that read local files.
package sftp

import (
	"strconv"
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the sftp backend type.
type Type struct{}

var _ backend.Exampler = Type{}

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
// private key, a key pasted as it is, a port in range and a well-formed
// base path.
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

// ToRclone implements backend.Type; the base path becomes the Root, and the
// private key is escaped onto one line (see escapeKeyPem).
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "sftp"
	r.Root, _ = backend.JoinRemote(cfg.Options["path"], "")
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

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"host": "nas.example.com", "user": "backup"}, Secrets: map[string]string{"pass": "hunter2"}},
		{
			Options: map[string]string{
				"host": "storage.example.com", "port": "2222", "user": "u123", "path": "/srv/backups",
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
