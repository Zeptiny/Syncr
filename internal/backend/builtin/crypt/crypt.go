// Package crypt is the backend descriptor of encrypted remotes (rclone
// backend "crypt"): the worker encrypts file contents and names and stores
// them in a folder of another of the owner's remotes.
//
// That folder is not a configuration value: it is the remote's wrapped
// remote (Capabilities.Wrapper, the "remote" field of kind
// backend.KindWrapped), which the remotes use case manages and the job
// builder turns into RemoteDefinition.wrapped. The worker builds rclone's
// "remote" option from it and always sets "strict_names", so neither is
// ever an option here. The password, the salt and the name settings are
// fixed after creation (Field.Fixed): another value would make the stored
// files unreadable. Not offered: no_data_encryption, pass_bad_blocks,
// show_mapping and server_side_across_configs.
package crypt

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the crypt backend type.
type Type struct{}

var (
	_ backend.Exampler        = Type{}
	_ backend.InvalidExampler = Type{}
)

// PasswordHelp is the help of the password field: the warning the form
// shows above it.
const PasswordHelp = "Syncr never shows this password again. Without it, nobody can read the files in this remote, " +
	"not Syncr and not you. Keep a copy somewhere safe, such as a password manager."

// maxSuffixRunes bounds the characters of a suffix after its ".".
const maxSuffixRunes = 31

// suffixNone is the suffix value that adds no suffix.
const suffixNone = "none"

// ID implements backend.Type.
func (Type) ID() string { return "crypt" }

// Label implements backend.Type.
func (Type) Label() string { return "Encrypted (crypt)" }

// Description implements backend.Type.
func (Type) Description() string {
	return "Encrypts files on the worker and stores them in a folder of another of your remotes."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "remote", Label: "Store in", Kind: backend.KindWrapped, Required: true,
			Help: "One of your remotes and a folder on it. Only encrypted data is written there."},
		{Name: "password", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true, Required: true, Fixed: true,
			Help: PasswordHelp},
		{Name: "password2", Label: "Salt (second password)", Kind: backend.KindString, Secret: true, Obscure: true, Fixed: true,
			Help: "Optional salt. Also needed to read the files; keep it with the password."},
		{Name: "filename_encryption", Label: "File name encryption", Kind: backend.KindEnum, Default: "standard", Fixed: true,
			Choices: []backend.Choice{
				{Value: "standard", Label: "Standard: encrypt the names"},
				{Value: "obfuscate", Label: "Obfuscate: hide the names, keep their length"},
				{Value: "off", Label: "Off: names stay readable"},
			}},
		{Name: "directory_name_encryption", Label: "Encrypt folder names", Kind: backend.KindBool, Default: "true", Fixed: true,
			Help: "Only with file-name encryption."},
		{Name: "filename_encoding", Label: "Name encoding", Kind: backend.KindEnum, Default: "base32", Advanced: true, Fixed: true,
			Choices: []backend.Choice{
				{Value: "base32", Label: "base32"}, {Value: "base64", Label: "base64"}, {Value: "base32768", Label: "base32768"},
			},
			Help: "base64 needs a case-sensitive remote; base32768 suits remotes that limit name length in characters (OneDrive, Dropbox)."},
		{Name: "suffix", Label: "Suffix", Kind: backend.KindString, Default: ".bin", Advanced: true, Fixed: true,
			Help: "Added to file names when names are not encrypted; none for no suffix."},
	}
}

// Capabilities implements backend.Type: a wrapper whose remotes keep no
// checksums (rclone's crypt has no hashes).
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{Wrapper: true, NoChecksum: true}
}

// Validate implements backend.Type: the field checks and a suffix that is
// "none" or "." followed by 1 to 31 characters without "/", "\", ":",
// white space or control characters. rclone 1.75.2 takes any suffix: one
// without a leading "." gets one (and an error in the log), and a "/"
// makes every file a folder on the wrapped remote. The wrapped remote is
// checked by the remotes use case.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if s := cfg.Options["suffix"]; s != "" && !validSuffix(s) {
		v.Add("options.suffix", fmt.Sprintf(`must be %q or "." followed by 1 to %d characters without "/", "\", ":", spaces or control characters`,
			suffixNone, maxSuffixRunes))
	}
	return v.Err()
}

// validSuffix reports whether s is a suffix Validate accepts.
func validSuffix(s string) bool {
	if s == suffixNone {
		return true
	}
	rest, ok := strings.CutPrefix(s, ".")
	if !ok || !utf8.ValidString(rest) {
		return false
	}
	if n := utf8.RuneCountInString(rest); n < 1 || n > maxSuffixRunes {
		return false
	}
	return !strings.ContainsFunc(rest, func(r rune) bool {
		return r == '/' || r == '\\' || r == ':' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// ToRclone implements backend.Type: the mapped fields with an empty Root
// (paths are relative to the encrypted remote itself). It never emits
// "remote" or "strict_names": the worker sets both.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "crypt"
	return r, nil
}

// Examples implements backend.Exampler. They never set the wrapped field,
// which is not a configuration value.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{}, Secrets: map[string]string{"password": "correct horse battery staple"}},
		{
			Options: map[string]string{
				"filename_encryption": "off", "directory_name_encryption": "false", "filename_encoding": "base64", "suffix": ".enc",
			},
			Secrets: map[string]string{"password": "pw", "password2": "salt"},
		},
		{
			Options: map[string]string{"filename_encryption": "obfuscate", "filename_encoding": "base32768", "suffix": "none"},
			Secrets: map[string]string{"password": "pw"},
		},
	}
}

// InvalidExamples implements backend.InvalidExampler: suffixes Validate
// refuses.
func (Type) InvalidExamples() []backend.InvalidExample {
	cfg := func(suffix string) backend.Config {
		return backend.Config{Options: map[string]string{"filename_encryption": "off", "suffix": suffix}, Secrets: map[string]string{"password": "pw"}}
	}
	const field = "options.suffix"
	return []backend.InvalidExample{
		{Name: "no dot", Config: cfg("enc"), Field: field},
		{Name: "only a dot", Config: cfg("."), Field: field},
		{Name: "a slash", Config: cfg(".a/b"), Field: field},
		{Name: "a backslash", Config: cfg(`.a\b`), Field: field},
		{Name: "a colon", Config: cfg(".a:b"), Field: field},
		{Name: "a space", Config: cfg(".a b"), Field: field},
		{Name: "a tab", Config: cfg(".a\tb"), Field: field},
		{Name: "too long", Config: cfg("." + strings.Repeat("x", maxSuffixRunes+1)), Field: field},
		{Name: "none in capitals", Config: cfg("NONE"), Field: field},
	}
}
