package email

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/notify"
)

// MessageIDDomain is the right-hand side of the Message-ID header.
const MessageIDDomain = "syncr"

// MessageID returns the Message-ID of a delivery: "<id@syncr>", with the
// characters of id outside [A-Za-z0-9._-] dropped (a random ID when none
// is left).
func MessageID(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return -1
	}, id)
	if clean == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		clean = hex.EncodeToString(b)
	}
	return "<" + clean + "@" + MessageIDDomain + ">"
}

// BuildMessage returns the RFC 5322 message of msg: headers, then a
// multipart/alternative body with the notify.Text and notify.HTML
// renderings, both quoted-printable UTF-8. Lines end in CRLF.
func BuildMessage(msg notify.Message, from *mail.Address, to []*mail.Address, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	rcpts := make([]string, len(to))
	for i, a := range to {
		rcpts[i] = a.String()
	}
	header("To", strings.Join(rcpts, ",\r\n "))
	header("Subject", encodeSubject(msg.Title))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", MessageID(msg.ID))
	header("MIME-Version", "1.0")
	header("Auto-Submitted", "auto-generated")
	header("X-Auto-Response-Suppress", "All")
	if msg.Event != "" {
		header("X-Syncr-Event", string(msg.Event))
	}
	sum := sha256.Sum256([]byte(MessageID(msg.ID) + msg.Title))
	mw := multipart.NewWriter(&b)
	if err := mw.SetBoundary("syncr-" + hex.EncodeToString(sum[:12])); err != nil {
		return nil, fmt.Errorf("set boundary: %w", err)
	}
	header("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	for _, part := range []struct{ typ, body string }{
		{"text/plain; charset=utf-8", notify.Text(msg)},
		{"text/html; charset=utf-8", notify.HTML(msg)},
	} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, fmt.Errorf("create part: %w", err)
		}
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(strings.ReplaceAll(part.body, "\n", "\r\n"))); err != nil {
			return nil, fmt.Errorf("encode part: %w", err)
		}
		if err := qp.Close(); err != nil {
			return nil, fmt.Errorf("encode part: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close message: %w", err)
	}
	return b.Bytes(), nil
}

// encodeSubject encodes a one-line title as RFC 2047 words where needed,
// folding between encoded words so no line gets too long.
func encodeSubject(title string) string {
	enc := mime.QEncoding.Encode("utf-8", title)
	return strings.ReplaceAll(enc, "?= =?", "?=\r\n =?")
}
