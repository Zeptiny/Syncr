package gcs_test

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/gcs"
)

var _ backendtest.SentinelSecreter = gcs.Type{}

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, gcs.Type{})
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	want := backend.Capabilities{UnionMember: true, ResticRepository: true, ObjectStore: true}
	if got := (gcs.Type{}).Capabilities(); got != want {
		t.Errorf("Capabilities = %+v, want %+v", got, want)
	}
}

// Parts of the test key that no message may contain.
const (
	privateKey = "-----BEGIN PRIVATE KEY-----\nMIIEvQ-test-private-key-material\n-----END PRIVATE KEY-----\n"
	email      = "uploader@secret-project-4711.iam.gserviceaccount.com"
	keyID      = "a1b2c3d4e5f6a7b8c9d0"
)

// key is a service account key with fields changed by edit (a nil value
// deletes the field), on several lines as a downloaded key file.
func key(t *testing.T, edit map[string]any) string {
	t.Helper()
	m := map[string]any{
		"type": "service_account", "project_id": "secret-project-4711", "private_key_id": keyID,
		"private_key": privateKey, "client_email": email, "client_id": "1234567890",
		"auth_uri": "https://accounts.google.com/o/oauth2/auth", "token_uri": gcs.TokenURI,
	}
	for k, v := range edit {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func TestValidateKey(t *testing.T) {
	t.Parallel()
	valid := key(t, nil)
	for _, tt := range []struct {
		name    string
		key     string
		wantErr string // substring of the message; "" for valid
	}{
		{"a key file", valid, ""},
		{"no token_uri", key(t, map[string]any{"token_uri": nil}), ""},
		{"an empty token_uri", key(t, map[string]any{"token_uri": ""}), ""},
		{"with a byte order mark", "\xef\xbb\xbf" + valid, "is not a JSON key file"},
		{"cut short", valid[:len(valid)/2], "is not a JSON key file"},
		{"not an object", `"` + email + `"`, "is not a JSON key file"},
		{"external account", key(t, map[string]any{"type": "external_account"}), "must be a service account key"},
		{"user credentials", key(t, map[string]any{"type": "authorized_user"}), "must be a service account key"},
		{"no client_email", key(t, map[string]any{"client_email": nil}), "has no client_email"},
		{"empty private_key", key(t, map[string]any{"private_key": ""}), "has no private_key"},
		{"token_uri on localhost", key(t, map[string]any{"token_uri": "http://127.0.0.1:9/token"}), "token_uri must be " + gcs.TokenURI},
		{"token_uri in other case", key(t, map[string]any{"token_uri": strings.ToUpper(gcs.TokenURI)}), "token_uri must be"},
		{"token_uri not a string", key(t, map[string]any{"token_uri": 7}), "is not a JSON key file"},
		{"a second token_uri", strings.Replace(valid, "{", `{"TOKEN_URI": "http://evil.example/token",`, 1), "names a field twice"},
		// encoding/json matches names as strings.EqualFold does: the Kelvin
		// sign is a "k", the long s an "s".
		{"token_uri spelled with the Kelvin sign", key(t, map[string]any{"token_uri": nil, "to\u212aen_uri": "http://evil.example/token"}),
			"token_uri must be " + gcs.TokenURI},
		{"a second token_uri with the Kelvin sign", key(t, map[string]any{"to\u212aen_uri": "http://evil.example/token"}), "names a field twice"},
		{"a second universe_domain with a long s", key(t, map[string]any{"universe_domain": "googleapis.com", "univer\u017fe_domain": "evil.example"}),
			"names a field twice"},
		// Names encoding/json never takes for a field's are not duplicates.
		{"a name with a dotted capital I", key(t, map[string]any{"token_ur\u0130": "http://evil.example/token"}), ""},
		{"a name with a dash", key(t, map[string]any{"token-uri": "http://evil.example/token"}), ""},
	} {
		err := gcs.Type{}.Validate(backend.Config{Secrets: map[string]string{"service_account_credentials": tt.key}})
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: Validate = %v", tt.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "secrets.service_account_credentials") || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: Validate = %v, want %q", tt.name, err, tt.wantErr)
			continue
		}
		// Not even a part of the key: no 8-byte piece of its own values.
		for _, part := range []string{privateKey, email, keyID, "secret-project-4711", "127.0.0.1", "evil.example"} {
			for i := 0; i+8 <= len(part); i++ {
				if strings.Contains(err.Error(), part[i:i+8]) {
					t.Errorf("%s: the message contains %q, a part of the key: %v", tt.name, part[i:i+8], err)
					break
				}
			}
		}
	}
}

func TestToRclone(t *testing.T) {
	t.Parallel()
	k := key(t, nil)
	r, err := gcs.Type{}.ToRclone(backend.Config{
		Options: map[string]string{"bucket": "/b/prefix/", "location": "eu", "bucket_policy_only": "true"},
		Secrets: map[string]string{"service_account_credentials": k},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Secrets["service_account_credentials"]
	if r.Backend != "gcs" || r.Root != "b/prefix" || strings.ContainsAny(got, "\r\n") || len(r.ObscureKeys) != 0 ||
		r.Options["location"] != "eu" || r.Options["bucket_policy_only"] != "true" || len(r.Options) != 2 {
		t.Errorf("ToRclone = %+v", r)
	}
	var a, b map[string]any
	if json.Unmarshal([]byte(k), &a) != nil || json.Unmarshal([]byte(got), &b) != nil || !jsonEqual(a, b) {
		t.Errorf("the compacted key differs: %q", got)
	}
	// Deterministic whatever the layout of the pasted key.
	r2, err := gcs.Type{}.ToRclone(backend.Config{Secrets: map[string]string{"service_account_credentials": strings.ReplaceAll(k, "\n", "\r\n")}})
	if err != nil || r2.Secrets["service_account_credentials"] != got {
		t.Errorf("a key pasted with Windows line breaks maps to %q (%v), want %q", r2.Secrets["service_account_credentials"], err, got)
	}
	if _, err := (gcs.Type{}).ToRclone(backend.Config{Secrets: map[string]string{"service_account_credentials": "not-json-" + keyID}}); err == nil ||
		strings.Contains(err.Error(), keyID) {
		t.Errorf("ToRclone(not JSON) = %v", err)
	}
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestSentinelSecret(t *testing.T) {
	t.Parallel()
	v, ok := gcs.Type{}.SentinelSecret("service_account_credentials", "SENTINEL-1")
	if !ok || !strings.Contains(v, "SENTINEL-1") {
		t.Fatalf("SentinelSecret = %q, %v", v, ok)
	}
	if err := (gcs.Type{}).Validate(backend.Config{Secrets: map[string]string{"service_account_credentials": v}}); err != nil {
		t.Errorf("Validate(sentinel key) = %v", err)
	}
	if _, ok := (gcs.Type{}).SentinelSecret("other", "x"); ok {
		t.Error("SentinelSecret answers for another field")
	}
}

// TestUniformAccessByDefault: without a choice, the remote asks rclone for
// uniform bucket-level access (bucket_policy_only), so rclone sends no ACL
// that a bucket with uniform access would refuse; turned off, it is sent
// as "false" with the ACLs.
func TestUniformAccessByDefault(t *testing.T) {
	t.Parallel()
	k := key(t, nil)
	for _, tt := range []struct {
		opts map[string]string
		want map[string]string
	}{
		{map[string]string{"bucket": "b"}, map[string]string{"bucket_policy_only": "true"}},
		{map[string]string{"bucket": "b", "bucket_policy_only": "true"}, map[string]string{"bucket_policy_only": "true"}},
		{map[string]string{"bucket": "b", "bucket_policy_only": "false", "object_acl": "publicRead"},
			map[string]string{"bucket_policy_only": "false", "object_acl": "publicRead"}},
	} {
		r, err := gcs.Type{}.ToRclone(backend.Config{Options: tt.opts, Secrets: map[string]string{"service_account_credentials": k}})
		if err != nil || !maps.Equal(r.Options, tt.want) {
			t.Errorf("ToRclone(%v) options = %v (%v), want %v", tt.opts, r.Options, err, tt.want)
		}
	}
	for _, f := range (gcs.Type{}).Fields() {
		if f.Name == "bucket_policy_only" && f.Default != "true" {
			t.Errorf("bucket_policy_only defaults to %q, want true", f.Default)
		}
	}
}
