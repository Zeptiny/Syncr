// Package redact keeps secrets out of logs and other human-readable output.
//
// It offers three complementary mechanisms:
//
//   - Secret, a string type that renders as [redacted] wherever it is
//     formatted, logged or encoded, so a secret stays safe even when logged
//     under an innocent key;
//   - ReplaceAttr, a slog.HandlerOptions.ReplaceAttr function that redacts
//     attributes whose key (or enclosing group) looks secret, as a safety net
//     for plain strings;
//   - Scrub, which removes known secret values from free text (a tool's
//     error message that may echo a password).
//
// Neither mechanism can see inside opaque values: a struct or map logged with
// slog.Any is encoded by the handler as a whole. Never log values that carry
// secrets in plain string fields (e.g. job specs).
//
// This package is a leaf: it imports only the standard library.
package redact

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"
)

// Placeholder replaces redacted values.
const Placeholder = "[redacted]"

// Secret is a string that must never be displayed. Every formatting path
// (fmt verbs, slog, JSON, text encoding) renders it as Placeholder; use Reveal
// to obtain the value where it is actually needed.
//
// Decoding is unaffected: YAML, JSON and environment loaders fill a Secret like
// any string.
//
// fmt cannot call methods on unexported struct fields, so keep Secret fields
// exported (or never print the enclosing struct).
type Secret string

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s == "" }

// String implements fmt.Stringer.
func (Secret) String() string { return Placeholder }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return Placeholder }

// Format implements fmt.Formatter so every verb (including %d, %x and %q)
// prints the placeholder.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Placeholder) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(Placeholder) }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(Placeholder), nil }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Placeholder + `"`), nil }

// ReplaceAttr redacts attributes whose key, or the name of any enclosing
// group, looks secret according to IsSecretKey. Its signature matches
// slog.HandlerOptions.ReplaceAttr.
func ReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if IsSecretKey(a.Key) {
		return slog.String(a.Key, Placeholder)
	}
	for _, g := range groups {
		if IsSecretKey(g) {
			return slog.String(a.Key, Placeholder)
		}
	}
	return a
}

// IsSecretKey reports whether a key looks like it names a secret.
//
// The key is split into words at non-alphanumeric characters and camelCase
// boundaries ("clientSecret", "AWS_SECRET_ACCESS_KEY", "x-api-key"). It is
// secret if any word equals a short secret word (pass, pwd, key, pem, ...) or
// contains a longer one (password, secret, token, credential, ...). The match
// is deliberately broad: over-redaction is a cheaper failure than a leak, so
// log non-secret identifiers under keys without these words (e.g.
// "concurrency_group" rather than "concurrency_key").
func IsSecretKey(key string) bool {
	for _, word := range splitWords(key) {
		if isExactSecretWord(word) {
			return true
		}
		for _, fragment := range secretFragments() {
			if strings.Contains(word, fragment) {
				return true
			}
		}
	}
	return false
}

// isExactSecretWord matches short words that are too ambiguous as substrings
// ("pass" in "passive", "key" in "keyboard").
func isExactSecretWord(word string) bool {
	switch word {
	case "pass", "pwd", "pin", "key", "keys", "pem", "sas", "dsn", "jwt", "otp",
		"auth", "authorization", "bearer", "cookie", "cookies", "signature":
		return true
	}
	return false
}

// secretFragments are matched as substrings of each word, which also catches
// concatenations such as "apikey" or "accesstoken".
func secretFragments() []string {
	return []string{"password", "passwd", "passphrase", "secret", "token", "credential", "apikey", "privatekey", "accesskey"}
}

// splitWords splits a key into lower-case words at non-alphanumeric
// characters and camelCase boundaries ("APIKey" -> "api", "key").
func splitWords(key string) []string {
	var words []string
	runes := []rune(key)
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			words = append(words, strings.ToLower(string(runes[start:end])))
		}
		start = -1
	}
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
			continue
		}
		prev := runes[i-1]
		lowerToUpper := unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev))
		acronymEnd := unicode.IsUpper(r) && unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if lowerToUpper || acronymEnd {
			flush(i)
			start = i
		}
	}
	flush(len(runes))
	return words
}
