package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestSecretNeverFormats(t *testing.T) {
	t.Parallel()
	const value = "hunter2"
	s := Secret(value)

	type wrapper struct{ Token Secret }
	outputs := map[string]string{
		"%s":         fmt.Sprintf("%s", s),
		"%v":         fmt.Sprintf("%v", s),
		"%+v":        fmt.Sprintf("%+v", s),
		"%#v":        fmt.Sprintf("%#v", s),
		"%q":         fmt.Sprintf("%q", s),
		"%x":         fmt.Sprintf("%x", s),
		"%d":         fmt.Sprintf("%d", s),
		"struct %v":  fmt.Sprintf("%v", wrapper{Token: s}),
		"struct %+v": fmt.Sprintf("%+v", wrapper{Token: s}),
		"String":     s.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, value) || !strings.Contains(out, Placeholder) {
			t.Errorf("%s: got %q", name, out)
		}
	}

	b, err := json.Marshal(wrapper{Token: s})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"Token":"[redacted]"}`; got != want {
		t.Errorf("json = %s, want %s", got, want)
	}

	if got := s.Reveal(); got != value {
		t.Errorf("Reveal() = %q, want %q", got, value)
	}
}

func TestSecretDecodesNormally(t *testing.T) {
	t.Parallel()
	var v struct{ Token Secret }
	if err := json.Unmarshal([]byte(`{"Token":"hunter2"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Token.Reveal() != "hunter2" {
		t.Errorf("decoded %q", v.Token.Reveal())
	}
}

func TestIsSecretKey(t *testing.T) {
	t.Parallel()
	secret := []string{
		"password", "Password", "db_password", "dbPassword", "SYNCR_DB_PASSWORD",
		"pass", "key_file_pass", "passwd", "passphrase",
		"secret", "client_secret", "clientSecret", "AWS_SECRET_ACCESS_KEY", "secret_access_key",
		"token", "accessToken", "access_token", "refresh-token", "worker.token", "csrftoken",
		"key", "api_key", "apiKey", "APIKey", "x-api-key", "apikey", "sse_customer_key", "privateKey", "private_key",
		"authorization", "Authorization", "auth", "cookie", "Set-Cookie", "dsn", "database_dsn",
		"credentials", "credential_file", "sas_url", "key_pem", "pem",
	}
	notSecret := []string{
		"", "msg", "level", "time", "job_id", "jobID", "worker", "passive", "mode", "path", "bytes",
		"keyboard", "monkey", "author", "pinned", "url", "host", "user", "username", "kind", "alias",
	}
	for _, k := range secret {
		if !IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = false, want true", k)
		}
	}
	for _, k := range notSecret {
		if IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = true, want false", k)
		}
	}
}

func TestSplitWords(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"APIKey":                {"api", "key"},
		"clientSecret":          {"client", "secret"},
		"AWS_SECRET_ACCESS_KEY": {"aws", "secret", "access", "key"},
		"x-api-key":             {"x", "api", "key"},
		"s3Bucket":              {"s3", "bucket"},
		"job.id":                {"job", "id"},
		"__":                    nil,
	}
	for in, want := range tests {
		if got := splitWords(in); !slices.Equal(got, want) {
			t.Errorf("splitWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReplaceAttrInHandler(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: ReplaceAttr}))

	logger.With("api_key", "with-attr-leak").Info("hello",
		"password", "plain-leak",
		"token", Secret("typed-leak"),
		"innocent", Secret("typed-innocent-leak"),
		slog.Group("credentials", slog.String("user", "group-leak")),
		slog.Group("remote", slog.String("secret_access_key", "nested-leak"), slog.String("bucket", "visible-bucket")),
		"job_id", "visible-job",
	)

	out := buf.String()
	if strings.Contains(out, "leak") {
		t.Errorf("log output leaks a secret: %s", out)
	}
	for _, want := range []string{"visible-bucket", "visible-job", `"msg":"hello"`, Placeholder} {
		if !strings.Contains(out, want) {
			t.Errorf("log output %s does not contain %q", out, want)
		}
	}
}
