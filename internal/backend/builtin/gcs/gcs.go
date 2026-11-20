// Package gcs is the backend descriptor of Google Cloud Storage with a
// service account key (rclone backend "google cloud storage", type "gcs").
//
// The key is required: without one, rclone falls back to Google's
// Application Default Credentials, the worker host's own (the worker
// refuses such a remote too). rclone posts the signed sign-in request to
// the key's token_uri, whatever it names, so Validate accepts only
// Google's token endpoint there.
package gcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the gcs backend type.
type Type struct{}

var (
	_ backend.Exampler        = Type{}
	_ backend.InvalidExampler = Type{}
)

// TokenURI is the only token_uri a service account key may name: Google's
// OAuth 2.0 token endpoint, rclone's default when the key names none.
//
//nolint:gosec // G101: a public endpoint, not a credential
const TokenURI = "https://oauth2.googleapis.com/token"

// keyField is the field of the service account key.
const keyField = "service_account_credentials"

// ID implements backend.Type.
func (Type) ID() string { return "gcs" }

// Label implements backend.Type.
func (Type) Label() string { return "Google Cloud Storage" }

// Description implements backend.Type.
func (Type) Description() string {
	return "Google Cloud Storage buckets, with a service account key."
}

// Fields implements backend.Type.
//
//nolint:gosec // G101: field names and help, no credential
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: keyField, Label: "Service account key (JSON)", Kind: backend.KindText, Secret: true, Required: true,
			Placeholder: `{"type": "service_account", ...}`,
			Help: "The service account's JSON key file, pasted as it is. Create one in the Google Cloud console: " +
				"IAM & Admin, Service accounts, Keys, Add key."},
		{Name: "bucket", Label: "Bucket", Kind: backend.KindString, Rclone: backend.NotRclone,
			Help: "Bucket, optionally with a path prefix (bucket/prefix). Leave empty to name the bucket in each job's path."},
		{Name: "project_number", Label: "Project number", Kind: backend.KindString, Advanced: true,
			Help: "Needed to list or create buckets."},
		{Name: "location", Label: "Location", Kind: backend.KindEnum, Advanced: true, Choices: locations(),
			Help: "Where new buckets are created; empty for Google's default (US)."},
		{Name: "storage_class", Label: "Storage class", Kind: backend.KindEnum, Advanced: true,
			Help: "The class of new objects; empty for the bucket's default.",
			Choices: []backend.Choice{
				{Value: "MULTI_REGIONAL", Label: "Multi-regional"}, {Value: "REGIONAL", Label: "Regional"},
				{Value: "NEARLINE", Label: "Nearline"}, {Value: "COLDLINE", Label: "Coldline"},
				{Value: "ARCHIVE", Label: "Archive"}, {Value: "DURABLE_REDUCED_AVAILABILITY", Label: "Durable reduced availability"},
			}},
		{Name: "bucket_policy_only", Label: "Uniform bucket-level access", Kind: backend.KindBool, Advanced: true,
			Help: "Set it when the bucket uses uniform bucket-level access; object and bucket ACLs are then not sent."},
		{Name: "object_acl", Label: "Object ACL", Kind: backend.KindEnum, Advanced: true,
			Help: "The access control list of new objects; empty for private.",
			Choices: []backend.Choice{
				{Value: "authenticatedRead", Label: "Authenticated users can read"},
				{Value: "bucketOwnerFullControl", Label: "Bucket owner has full control"},
				{Value: "bucketOwnerRead", Label: "Bucket owner can read"},
				{Value: "private", Label: "Private"},
				{Value: "projectPrivate", Label: "Project members, by role"},
				{Value: "publicRead", Label: "Anyone can read"},
			}},
		{Name: "bucket_acl", Label: "Bucket ACL", Kind: backend.KindEnum, Advanced: true,
			Help: "The access control list of new buckets; empty for private.",
			Choices: []backend.Choice{
				{Value: "authenticatedRead", Label: "Authenticated users can read"},
				{Value: "private", Label: "Private"},
				{Value: "projectPrivate", Label: "Project members, by role"},
				{Value: "publicRead", Label: "Anyone can read"},
				{Value: "publicReadWrite", Label: "Anyone can read and write"},
			}},
		{Name: "endpoint", Label: "Endpoint", Kind: backend.KindString, Advanced: true,
			Help: "Another storage API endpoint, e.g. a private one. Leave empty normally."},
	}
}

// locations are rclone 1.75.2's example locations (the empty default
// left out: an empty field is the default).
func locations() []backend.Choice {
	return []backend.Choice{
		{Value: "asia", Label: "Asia (multi-region)"}, {Value: "eu", Label: "Europe (multi-region)"},
		{Value: "us", Label: "United States (multi-region)"},
		{Value: "asia-east1", Label: "Taiwan (asia-east1)"}, {Value: "asia-east2", Label: "Hong Kong (asia-east2)"},
		{Value: "asia-northeast1", Label: "Tokyo (asia-northeast1)"}, {Value: "asia-northeast2", Label: "Osaka (asia-northeast2)"},
		{Value: "asia-northeast3", Label: "Seoul (asia-northeast3)"}, {Value: "asia-south1", Label: "Mumbai (asia-south1)"},
		{Value: "asia-south2", Label: "Delhi (asia-south2)"}, {Value: "asia-southeast1", Label: "Singapore (asia-southeast1)"},
		{Value: "asia-southeast2", Label: "Jakarta (asia-southeast2)"},
		{Value: "australia-southeast1", Label: "Sydney (australia-southeast1)"},
		{Value: "australia-southeast2", Label: "Melbourne (australia-southeast2)"},
		{Value: "europe-north1", Label: "Finland (europe-north1)"}, {Value: "europe-west1", Label: "Belgium (europe-west1)"},
		{Value: "europe-west2", Label: "London (europe-west2)"}, {Value: "europe-west3", Label: "Frankfurt (europe-west3)"},
		{Value: "europe-west4", Label: "Netherlands (europe-west4)"}, {Value: "europe-west6", Label: "Zürich (europe-west6)"},
		{Value: "europe-central2", Label: "Warsaw (europe-central2)"},
		{Value: "us-central1", Label: "Iowa (us-central1)"}, {Value: "us-east1", Label: "South Carolina (us-east1)"},
		{Value: "us-east4", Label: "Northern Virginia (us-east4)"}, {Value: "us-east5", Label: "Ohio (us-east5)"},
		{Value: "us-west1", Label: "Oregon (us-west1)"}, {Value: "us-west2", Label: "California (us-west2)"},
		{Value: "us-west3", Label: "Salt Lake City (us-west3)"}, {Value: "us-west4", Label: "Las Vegas (us-west4)"},
		{Value: "northamerica-northeast1", Label: "Montréal (northamerica-northeast1)"},
		{Value: "northamerica-northeast2", Label: "Toronto (northamerica-northeast2)"},
		{Value: "southamerica-east1", Label: "São Paulo (southamerica-east1)"},
		{Value: "southamerica-west1", Label: "Santiago (southamerica-west1)"},
		{Value: "asia1", Label: "Tokyo and Osaka (dual-region asia1)"},
		{Value: "eur4", Label: "Finland and Netherlands (dual-region eur4)"},
		{Value: "nam4", Label: "Iowa and South Carolina (dual-region nam4)"},
	}
}

// Capabilities implements backend.Type: Google Cloud Storage keeps MD5
// checksums.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true, ObjectStore: true}
}

// Validate implements backend.Type: the field checks, a service account
// key that rclone can use and that sends its sign-in only to Google (see
// checkKey), and a well-formed bucket path. No message contains any part of
// the key.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if key := cfg.Secrets[keyField]; key != "" {
		if msg := checkKey(key); msg != "" {
			v.Add("secrets."+keyField, msg)
		}
	}
	if _, err := backend.JoinRemote(cfg.Options["bucket"], ""); err != nil {
		v.Add("options.bucket", err.Error())
	}
	return v.Err()
}

// serviceAccountKey holds the fields of a key that Validate checks, decoded
// with encoding/json as rclone decodes them (field names match regardless
// of case).
type serviceAccountKey struct {
	Type        string  `json:"type"`
	ClientEmail string  `json:"client_email"`
	PrivateKey  string  `json:"private_key"`
	TokenURI    *string `json:"token_uri"`
}

// checkKey returns why key is not a usable service account key, or "": it
// must be one JSON object without repeated names (compared regardless of
// case, as encoding/json matches them, so that no second "token_uri" can
// hide behind the first), of type "service_account", with a client_email
// and a private_key, and with a token_uri that is absent or TokenURI. The
// message never contains any part of the key.
func checkKey(key string) string {
	const notJSON = "is not a JSON key file: paste the whole file as it is"
	names, ok := objectNames(key)
	if !ok {
		return notJSON
	}
	seen := map[string]bool{}
	for _, n := range names {
		l := strings.ToLower(n)
		if seen[l] {
			return "names a field twice"
		}
		seen[l] = true
	}
	var k serviceAccountKey
	if err := json.Unmarshal([]byte(key), &k); err != nil {
		return notJSON
	}
	switch {
	case k.Type != "service_account":
		return `must be a service account key ("type": "service_account"); other credentials are not supported`
	case k.ClientEmail == "":
		return "has no client_email"
	case k.PrivateKey == "":
		return "has no private_key"
	case k.TokenURI != nil && *k.TokenURI != "" && *k.TokenURI != TokenURI:
		return "token_uri must be " + TokenURI
	}
	return ""
}

// objectNames returns the member names of the JSON object s, which must be
// exactly one valid JSON object; ok is false otherwise.
func objectNames(s string) (names []string, ok bool) {
	if !json.Valid([]byte(s)) {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, isName := t.(string)
		if !isName {
			return nil, false
		}
		names = append(names, name)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, false
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return names, true
}

// ToRclone implements backend.Type: Backend "gcs" (rclone's prefix for
// "google cloud storage"), the bucket as the Root, and the key compacted
// onto one line (json.Compact), so the same key always maps to the same
// value. A key that is not JSON (never stored: Validate refuses it) is an
// error that names no part of it.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "gcs"
	r.Root = strings.Trim(cfg.Options["bucket"], "/")
	if key, ok := r.Secrets[keyField]; ok {
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(key)); err != nil {
			return backend.Rclone{}, errors.New("gcs: the service account key is not JSON")
		}
		r.Secrets[keyField] = buf.String()
	}
	return r, nil
}

// exampleKey is a service account key for the examples and tests, with
// extra (another private_key_id) when not empty. It is made up: the
// private key is no key, and no such account exists.
func exampleKey(extra string, indent bool) string {
	id := "0000000000000000000000000000000000000000"
	if extra != "" {
		id = extra
	}
	fields := []string{
		`"type": "service_account"`,
		`"project_id": "example-project"`,
		`"private_key_id": "` + id + `"`,
		`"private_key": "-----BEGIN PRIVATE KEY-----\nNOT-A-REAL-KEY\n-----END PRIVATE KEY-----\n"`,
		`"client_email": "backup@example-project.iam.gserviceaccount.com"`,
		`"client_id": "000000000000000000000"`,
		`"auth_uri": "https://accounts.google.com/o/oauth2/auth"`,
		`"token_uri": "` + TokenURI + `"`,
	}
	if indent {
		return "{\n  " + strings.Join(fields, ",\n  ") + "\n}\n"
	}
	return "{" + strings.ReplaceAll(strings.Join(fields, ","), `": "`, `":"`) + "}"
}

// SentinelSecret implements backendtest.SentinelSecreter: the contract
// suite's sentinel inside a valid, compact key (as its private_key_id), so
// that Validate accepts it and ToRclone passes it on unchanged.
func (Type) SentinelSecret(name, sentinel string) (string, bool) {
	if name != keyField {
		return "", false
	}
	return exampleKey(sentinel, false), true
}

// Examples implements backend.Exampler: a key pasted as a file (on several
// lines) and one already on one line, with every other field set.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"bucket": "example-backups"}, Secrets: map[string]string{keyField: exampleKey("", true)}},
		{
			Options: map[string]string{
				"bucket": "example-archive/syncr", "project_number": "123456789012", "location": "europe-west3",
				"storage_class": "NEARLINE", "bucket_policy_only": "true", "object_acl": "private", "bucket_acl": "private",
				"endpoint": "https://storage.googleapis.com",
			},
			Secrets: map[string]string{keyField: exampleKey("", false)},
		},
	}
}

// InvalidExamples implements backend.InvalidExampler: keys that are not
// JSON, not a service account's, incomplete, or that send their sign-in
// elsewhere.
func (Type) InvalidExamples() []backend.InvalidExample {
	cfg := func(key string) backend.Config {
		return backend.Config{Options: map[string]string{"bucket": "b"}, Secrets: map[string]string{keyField: key}}
	}
	good := exampleKey("", false)
	const field = "secrets." + keyField
	without := func(name string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(good), &m)
		delete(m, name)
		b, _ := json.Marshal(m)
		return string(b)
	}
	with := func(name, value string) string {
		return strings.TrimSuffix(good, "}") + `,"` + name + `":"` + value + `"}`
	}
	replaced := func(old, repl string) string { return strings.Replace(good, old, repl, 1) }
	return []backend.InvalidExample{
		{Name: "not JSON", Config: cfg("private-key-material-not-json"), Field: field},
		{Name: "truncated", Config: cfg(good[:len(good)-1]), Field: field},
		{Name: "a JSON array", Config: cfg("[" + good + "]"), Field: field},
		{Name: "two objects", Config: cfg(good + good), Field: field},
		{Name: "another type", Config: cfg(replaced(`"service_account"`, `"external_account"`)), Field: field},
		{Name: "an OAuth client", Config: cfg(replaced(`"service_account"`, `"authorized_user"`)), Field: field},
		{Name: "no type", Config: cfg(without("type")), Field: field},
		{Name: "no client_email", Config: cfg(without("client_email")), Field: field},
		{Name: "no private_key", Config: cfg(without("private_key")), Field: field},
		{Name: "token_uri elsewhere", Config: cfg(replaced(TokenURI, "http://127.0.0.1:8080/token")), Field: field},
		{Name: "token_uri over http", Config: cfg(replaced(TokenURI, "http://oauth2.googleapis.com/token")), Field: field},
		{Name: "token_uri with a suffix", Config: cfg(replaced(TokenURI, TokenURI+"?x=1")), Field: field},
		{Name: "token_uri not a string", Config: cfg(replaced(`"`+TokenURI+`"`, "42")), Field: field},
		{Name: "a second token_uri", Config: cfg(with("token_uri", "https://token.example.com/")), Field: field},
		{Name: "a second token_uri in other case", Config: cfg(with("Token_URI", "https://token.example.com/")), Field: field},
	}
}
