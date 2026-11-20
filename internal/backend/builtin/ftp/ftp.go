// Package ftp is the backend descriptor of FTP servers, with or without
// TLS (FTPS) (rclone backend "ftp").
//
// Three rclone facts shape it (checked with rclone 1.75.2):
//   - rclone writes the host, user and password, as they are, into lines
//     of the FTP control connection, so a line break in one would add
//     commands of the remote's choosing. Validate refuses them (the field
//     checks refuse line breaks in every single-line field), and so does
//     the worker.
//   - The user is always sent: left empty, rclone would sign in with the
//     worker's own account name (the worker refuses such a remote too).
//   - rclone reveals the password unconditionally, so a remote without one
//     fails before connecting ("input too short when revealing password").
//     ToRclone therefore always sends "pass" as an obscured secret, empty
//     for anonymous FTP.
//
// rclone keeps no checksums on FTP (and most servers keep no modification
// times), so the type is NoChecksum.
package ftp

import (
	"slices"
	"strconv"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the ftp backend type.
type Type struct{}

var (
	_ backend.Exampler        = Type{}
	_ backend.InvalidExampler = Type{}
)

// ID implements backend.Type.
func (Type) ID() string { return "ftp" }

// Label implements backend.Type.
func (Type) Label() string { return "FTP / FTPS" }

// Description implements backend.Type.
func (Type) Description() string {
	return "An FTP server, optionally over TLS (FTPS). Anonymous FTP works with the user \"anonymous\"."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "host", Label: "Host", Kind: backend.KindString, Required: true, Placeholder: "ftp.example.com"},
		{Name: "user", Label: "User", Kind: backend.KindString, Required: true,
			Help: `"anonymous" for anonymous FTP.`},
		{Name: "pass", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true,
			Help: "Without TLS the password crosses the network in clear."},
		{Name: "tls", Label: "Implicit FTPS", Kind: backend.KindBool,
			Help: "Implicit FTPS (usually port 990: set the port under advanced settings)."},
		{Name: "explicit_tls", Label: "Explicit FTPS", Kind: backend.KindBool,
			Help: "Explicit FTPS (STARTTLS on port 21)."},
		{Name: "path", Label: "Base path", Kind: backend.KindString, Rclone: backend.NotRclone,
			Help: "Folder that job paths are relative to; empty means the login folder."},
		{Name: "port", Label: "Port", Kind: backend.KindInt, Default: "21", Advanced: true},
		{Name: "no_check_certificate", Label: "Don't verify the server certificate", Kind: backend.KindBool, Advanced: true,
			Help: "Accepts any server certificate, so anyone between the worker and the server can read and change the traffic."},
		{Name: "concurrency", Label: "Connections", Kind: backend.KindInt, Advanced: true,
			Help: "Most connections at once; set it if the server refuses connections. 0 or empty means no limit."},
		{Name: "disable_epsv", Label: "Disable EPSV", Kind: backend.KindBool, Advanced: true,
			Help: "For servers that do not understand extended passive mode."},
		{Name: "disable_mlsd", Label: "Disable MLSD", Kind: backend.KindBool, Advanced: true,
			Help: "For servers whose MLSD listings are broken."},
		{Name: "writing_mdtm", Label: "Set times with MDTM", Kind: backend.KindBool, Advanced: true,
			Help: "For servers that set modification times with MDTM."},
	}
}

// Capabilities implements backend.Type: rclone keeps no checksums on FTP.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true, NoChecksum: true}
}

// Validate implements backend.Type: the field checks, not both kinds of
// TLS, a port in range, a connection count that is not negative and a
// well-formed base path.
//
// The field checks also refuse a line break (CR or LF) in the host, user
// and password, which rclone writes into lines of the control connection
// (the worker's rcloneenv.lineOptions refuses them too): all three are
// single-line string fields, which the package tests pin.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if cfg.Options["tls"] == "true" && cfg.Options["explicit_tls"] == "true" {
		v.Add("options.explicit_tls", "choose implicit or explicit FTPS, not both")
	}
	if port := cfg.Options["port"]; port != "" && !portInRange(port) {
		v.Add("options.port", "must be between 1 and 65535")
	}
	if n, err := strconv.Atoi(cfg.Options["concurrency"]); err == nil && n < 0 {
		v.Add("options.concurrency", "must not be negative")
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

// ToRclone implements backend.Type; the base path becomes the Root, and
// "pass" is always an obscured secret, empty when the remote has none
// (see the package documentation).
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "ftp"
	r.Root, _ = backend.JoinRemote(cfg.Options["path"], "")
	if _, ok := r.Secrets["pass"]; !ok {
		r.Secrets["pass"] = ""
	}
	if !slices.Contains(r.ObscureKeys, "pass") {
		r.ObscureKeys = append(r.ObscureKeys, "pass")
	}
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"host": "ftp.example.com", "user": "anonymous"}},
		{
			Options: map[string]string{
				"host": "files.example.com", "user": "backup", "explicit_tls": "true", "path": "/srv/backups",
				"port": "2121", "no_check_certificate": "false", "concurrency": "4",
				"disable_epsv": "true", "disable_mlsd": "true", "writing_mdtm": "true",
			},
			Secrets: map[string]string{"pass": "ftp-password"},
		},
		{
			Options: map[string]string{"host": "ftps.example.com", "user": "backup", "tls": "true", "port": "990"},
			Secrets: map[string]string{"pass": "ftps-password"},
		},
	}
}

// InvalidExamples implements backend.InvalidExampler: line breaks in what
// rclone writes into the control connection, both kinds of TLS, and bad
// numbers and paths.
func (Type) InvalidExamples() []backend.InvalidExample {
	cfg := func(opts map[string]string) backend.Config {
		c := backend.Config{
			Options: map[string]string{"host": "ftp.example.com", "user": "backup"},
			Secrets: map[string]string{"pass": "ftp-password"},
		}
		for k, v := range opts {
			c.Options[k] = v
		}
		return c
	}
	pass := func(p string) backend.Config {
		c := cfg(nil)
		c.Secrets["pass"] = p
		return c
	}
	return []backend.InvalidExample{
		{Name: "line feed in the host", Config: cfg(map[string]string{"host": "ftp.example.com\nSITE EXEC x"}), Field: "options.host"},
		{Name: "carriage return in the host", Config: cfg(map[string]string{"host": "ftp.example.com\rx"}), Field: "options.host"},
		{Name: "line break in the user", Config: cfg(map[string]string{"user": "joe\r\nMAIL FROM:<a@example.com>"}), Field: "options.user"},
		{Name: "line feed in the password", Config: pass("secret\nDELE x"), Field: "secrets.pass"},
		{Name: "carriage return in the password", Config: pass("secret\rDELE x"), Field: "secrets.pass"},
		{Name: "implicit and explicit TLS", Config: cfg(map[string]string{"tls": "true", "explicit_tls": "true"}), Field: "options.explicit_tls"},
		{Name: "port out of range", Config: cfg(map[string]string{"port": "65536"}), Field: "options.port"},
		{Name: "negative concurrency", Config: cfg(map[string]string{"concurrency": "-1"}), Field: "options.concurrency"},
		{Name: "dot-dot in the path", Config: cfg(map[string]string{"path": "/srv/../etc"}), Field: "options.path"},
	}
}
