package notify_test

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/notify"
)

func TestRedactorForms(t *testing.T) {
	const (
		secretURL = "https://user:p%40ss-word@n8n.lan:5678/webhook/Sup3r-Secret_path/x1y2z3w4?token=qwerty-12345&mode=a"
		signing   = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
		bearer    = "Bearer tok-abcdef-123456"
	)
	r := notify.NewRedactor(descriptor.Values{Secrets: map[string]string{"url": secretURL, "signing_secret": signing, "authorization": bearer}})
	leaks := []string{
		"Sup3r-Secret_path", "sup3r-secret_path", "SUP3R-SECRET_PATH", // lower-cased by the receiver
		"/webhook/Sup3r-Secret_path/x1y2z3w4", "webhook/Sup3r-Secret_path/x1y2z3w4",
		`\/webhook\/Sup3r-Secret_path\/x1y2z3w4`, // PHP's json_encode
		"x1y2z3w4", "qwerty-12345", "token=qwerty-12345",
		"p%40ss-word", "p@ss-word", "user:p%40ss-word",
		signing, "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", bearer, "tok-abcdef-123456",
		url.QueryEscape(signing), base64.StdEncoding.EncodeToString([]byte("tok-abcdef-123456")),
		secretURL,
	}
	for _, leak := range leaks {
		for _, tmpl := range []string{"%s", `The requested webhook "POST %s" is not registered`, `{"error":"%s"}`} {
			in := strings.Replace(tmpl, "%s", leak, 1)
			if out := r.Redact(in); strings.Contains(strings.ToLower(out), strings.ToLower(leak)) {
				t.Errorf("Redact(%q) = %q still contains %q", in, out, leak)
			}
		}
	}
	// Words of the API and the host stay readable.
	in := "n8n.lan answered 404 for the webhook"
	if out := r.Redact(in); out != in {
		t.Errorf("Redact(%q) = %q", in, out)
	}
}

func TestRedactorJSONAndHTMLEscapes(t *testing.T) {
	r := notify.NewRedactor(descriptor.Values{Secrets: map[string]string{"url": `https://h.lan/hook/a"b<c>&d-secret`}})
	for _, in := range []string{`a\"b<c>&d-secret`, `a\"b<c>&d-secret`, `a&#34;b&lt;c&gt;&amp;d-secret`} {
		if out := r.Redact(in); strings.Contains(out, "secret") {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
}

func TestRedactorExtraAndNil(t *testing.T) {
	r := notify.NewRedactor(descriptor.Values{}, "syncr-mailer", "")
	if got := r.Redact("535 user syncr-mailer rejected"); strings.Contains(got, "syncr-mailer") {
		t.Errorf("extra value not redacted: %q", got)
	}
	var nilR *notify.Redactor
	if got := nilR.Redact("x"); got != "x" {
		t.Errorf("nil Redactor changed the text: %q", got)
	}
	if got := notify.NewRedactor(descriptor.Values{}).Redact("x"); got != "x" {
		t.Errorf("empty Redactor changed the text: %q", got)
	}
	// Short secrets are not searched for.
	if got := notify.NewRedactor(descriptor.Values{Secrets: map[string]string{"p": "abc"}}).Redact("abc"); got != "abc" {
		t.Errorf("a 3-character secret was redacted: %q", got)
	}
}
