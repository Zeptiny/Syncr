// Package smb is the backend descriptor of SMB shares: Windows file
// sharing, Samba and most NAS devices (rclone backend "smb").
//
// The user is always sent: left empty, rclone would sign in with the
// worker's own account name (the worker refuses such a remote too). rclone
// keeps no checksums on SMB, so the type is NoChecksum.
package smb

import (
	"strconv"
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the smb backend type.
type Type struct{}

var (
	_ backend.Exampler        = Type{}
	_ backend.InvalidExampler = Type{}
)

// ID implements backend.Type.
func (Type) ID() string { return "smb" }

// Label implements backend.Type.
func (Type) Label() string { return "SMB (Windows share)" }

// Description implements backend.Type.
func (Type) Description() string {
	return "A shared folder on a Windows computer, a Samba server or a NAS, over SMB."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "host", Label: "Host", Kind: backend.KindString, Required: true, Placeholder: "nas.local",
			Help: "The server's name or IP address."},
		{Name: "user", Label: "User", Kind: backend.KindString, Required: true,
			Help: `"guest" for a share open to guests.`},
		{Name: "pass", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true},
		{Name: "share", Label: "Share", Kind: backend.KindString, Rclone: backend.NotRclone, Placeholder: "backups",
			Help: "Share, optionally with a folder (share/folder). Leave empty to name the share in each job's path."},
		{Name: "domain", Label: "Domain", Kind: backend.KindString, Default: "WORKGROUP", Advanced: true,
			Help: "The Windows domain or workgroup of the user."},
		{Name: "port", Label: "Port", Kind: backend.KindInt, Default: "445", Advanced: true},
		{Name: "spn", Label: "Service principal name", Kind: backend.KindString, Advanced: true, Placeholder: "cifs/nas.example.com",
			Help: "Only for servers that require one. Leave empty if unsure."},
		{Name: "case_insensitive", Label: "Case-insensitive names", Kind: backend.KindBool, Advanced: true,
			Help: "On unless set: Windows shares do not tell names apart by case. Turn it off for a server that does."},
	}
}

// Capabilities implements backend.Type: rclone keeps no checksums on SMB.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true, NoChecksum: true}
}

// Validate implements backend.Type: the field checks (which also refuse
// line breaks in the host, user and password), a port in range and a
// well-formed share path.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if port := cfg.Options["port"]; port != "" && !portInRange(port) {
		v.Add("options.port", "must be between 1 and 65535")
	}
	if _, err := backend.JoinRemote(cfg.Options["share"], ""); err != nil {
		v.Add("options.share", err.Error())
	}
	return v.Err()
}

// portInRange reports whether p is a port number; a non-number is reported
// by CheckFields.
func portInRange(p string) bool {
	n, err := strconv.Atoi(p)
	return err != nil || (n >= 1 && n <= 65535)
}

// ToRclone implements backend.Type; the share (and folder) becomes the
// Root, without leading or trailing slashes.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "smb"
	root, _ := backend.JoinRemote(cfg.Options["share"], "")
	r.Root = strings.Trim(root, "/")
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"host": "nas.local", "user": "guest", "share": "public"}},
		{
			Options: map[string]string{
				"host": "fileserver.example.com", "user": "backup", "share": "/backups/syncr/", "domain": "EXAMPLE",
				"port": "4450", "spn": "cifs/fileserver.example.com", "case_insensitive": "false",
			},
			Secrets: map[string]string{"pass": "smb-password"},
		},
	}
}

// InvalidExamples implements backend.InvalidExampler.
func (Type) InvalidExamples() []backend.InvalidExample {
	cfg := func(opts map[string]string) backend.Config {
		o := map[string]string{"host": "nas.local", "user": "backup"}
		for k, v := range opts {
			o[k] = v
		}
		return backend.Config{Options: o, Secrets: map[string]string{"pass": "smb-password"}}
	}
	return []backend.InvalidExample{
		{Name: "port out of range", Config: cfg(map[string]string{"port": "70000"}), Field: "options.port"},
		{Name: "port zero", Config: cfg(map[string]string{"port": "0"}), Field: "options.port"},
		{Name: "dot-dot in the share", Config: cfg(map[string]string{"share": "backups/../admin$"}), Field: "options.share"},
		{Name: "line break in the host", Config: cfg(map[string]string{"host": "nas.local\r\nevil"}), Field: "options.host"},
		{Name: "line break in the user", Config: cfg(map[string]string{"user": "backup\nevil"}), Field: "options.user"},
		{Name: "line break in the password", Config: backend.Config{
			Options: map[string]string{"host": "nas.local", "user": "backup"}, Secrets: map[string]string{"pass": "smb\npassword"},
		}, Field: "secrets.pass"},
	}
}
