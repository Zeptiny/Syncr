package email_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/email"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

const (
	testUser     = "mailer@example.com"
	testPassword = "s3cret-smtp-password"
)

var fixedNow = time.Date(2026, 11, 4, 2, 31, 8, 0, time.UTC)

// testCA is a CA and a server certificate for 127.0.0.1 and localhost.
type testCA struct {
	pool *x509.CertPool
	cert tls.Certificate
}

func newCA(t *testing.T) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Syncr test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return testCA{pool: pool, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
}

// mailbox is the go-smtp backend: it records sessions and fails the
// commands it is told to.
type mailbox struct {
	mu      sync.Mutex
	mechs   []string
	rcptErr error
	dataErr error
	auths   []string // "MECH user password"
	// authMessage, if set, is the text of a failed authentication (servers
	// that quote the credentials they reject).
	authMessage func(user, pass string) string
	messages    []delivered
	tlsSeen     []bool
}

type delivered struct {
	from string
	to   []string
	data []byte
}

type session struct {
	box  *mailbox
	conn *smtp.Conn
	cur  delivered
}

func (b *mailbox) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{box: b, conn: c}, nil
}

func (s *session) AuthMechanisms() []string {
	s.box.mu.Lock()
	defer s.box.mu.Unlock()
	return s.box.mechs
}

func (s *session) check(mech, user, pass string) error {
	s.box.mu.Lock()
	defer s.box.mu.Unlock()
	s.box.auths = append(s.box.auths, mech+" "+user+" "+pass)
	if user != testUser || pass != testPassword {
		msg := "Authentication credentials invalid"
		if s.box.authMessage != nil {
			msg = s.box.authMessage(user, pass)
		}
		return &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: msg}
	}
	return nil
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(_, user, pass string) error { return s.check(mech, user, pass) }), nil
	case "LOGIN":
		return &loginServer{check: func(user, pass string) error { return s.check(mech, user, pass) }}, nil
	}
	return nil, smtp.ErrAuthUnsupported
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.cur.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.box.mu.Lock()
	err := s.box.rcptErr
	s.box.mu.Unlock()
	if err != nil {
		return err
	}
	s.cur.to = append(s.cur.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.box.mu.Lock()
	defer s.box.mu.Unlock()
	if s.box.dataErr != nil {
		return s.box.dataErr
	}
	s.cur.data = b
	s.box.messages = append(s.box.messages, s.cur)
	_, isTLS := s.conn.TLSConnectionState()
	s.box.tlsSeen = append(s.box.tlsSeen, isTLS)
	return nil
}

func (s *session) Reset()        { s.cur = delivered{} }
func (s *session) Logout() error { return nil }

// loginServer is the server side of the LOGIN mechanism (go-sasl has
// only the client).
type loginServer struct {
	check func(user, pass string) error
	step  int
	user  string
}

func (l *loginServer) Next(response []byte) ([]byte, bool, error) {
	l.step++
	switch l.step {
	case 1:
		if response != nil {
			l.user, l.step = string(response), 2
			return []byte("Password:"), false, nil
		}
		return []byte("Username:"), false, nil
	case 2:
		l.user = string(response)
		return []byte("Password:"), false, nil
	default:
		return nil, true, l.check(l.user, string(response))
	}
}

func (b *mailbox) received() []delivered {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]delivered(nil), b.messages...)
}

// startServer runs a go-smtp server on loopback: with STARTTLS when
// starttls, behind implicit TLS when implicitTLS.
func startServer(t *testing.T, ca testCA, box *mailbox, starttls, implicitTLS bool) int {
	t.Helper()
	srv := smtp.NewServer(box)
	srv.Domain = "localhost"
	srv.ReadTimeout, srv.WriteTimeout = 10*time.Second, 10*time.Second
	srv.AllowInsecureAuth = false
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{ca.cert}, MinVersion: tls.VersionTLS12}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case implicitTLS:
		ln = tls.NewListener(ln, tlsCfg)
	case starttls:
		srv.TLSConfig = tlsCfg
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", ln.Addr())
	}
	return addr.Port
}

func newChannel(ca testCA) *email.Channel {
	return email.New(netguard.Policy{Mode: netguard.ModeAny}.Dialer(5*time.Second),
		email.WithRootCAs(ca.pool), email.WithClock(func() time.Time { return fixedNow }), email.WithHelloName("syncr.test"))
}

func conf(port int, security string, auth bool) descriptor.Values {
	v := descriptor.Values{Options: map[string]string{
		email.FieldHost: "127.0.0.1", email.FieldPort: strconv.Itoa(port), email.FieldSecurity: security,
		email.FieldFrom: "Syncr Alerts <syncr@example.com>", email.FieldTo: "admin@example.com, Öps <ops@example.com>",
	}}
	if auth {
		v.Options[email.FieldUsername] = testUser
		v.Secrets = map[string]string{email.FieldPassword: testPassword}
	}
	return v
}

func send(t *testing.T, ch *email.Channel, cfg descriptor.Values, msg notify.Message) error {
	t.Helper()
	if err := ch.Validate(cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ch.Send(ctx, cfg, msg)
}

// checkMessage parses a received message as a mail client would.
func checkMessage(t *testing.T, d delivered, msg notify.Message) {
	t.Helper()
	if d.from != "syncr@example.com" || strings.Join(d.to, ",") != "admin@example.com,ops@example.com" {
		t.Errorf("envelope = %s → %v", d.from, d.to)
	}
	m, err := mail.ReadMessage(bytes.NewReader(d.data))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject != msg.Title {
		t.Errorf("Subject = %q (%v), want %q", subject, err, msg.Title)
	}
	if got := m.Header.Get("Message-ID"); got != "<"+msg.ID+"@syncr>" {
		t.Errorf("Message-ID = %q", got)
	}
	if m.Header.Get("Auto-Submitted") != "auto-generated" || m.Header.Get("X-Syncr-Event") != string(msg.Event) {
		t.Errorf("headers = %v", m.Header)
	}
	if date, err := m.Header.Date(); err != nil || !date.Equal(fixedNow) {
		t.Errorf("Date = %v (%v)", date, err)
	}
	if to, err := m.Header.AddressList("To"); err != nil || len(to) != 2 || to[1].Name != "Öps" {
		t.Errorf("To = %v (%v)", to, err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q (%v)", m.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	parts := map[string]string{}
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts[pt] = string(b)
	}
	text, html := parts["text/plain"], parts["text/html"]
	if want := strings.ReplaceAll(notify.Text(msg), "\n", "\r\n"); text != want {
		t.Errorf("text part:\n%q\nwant\n%q", text, want)
	}
	if !strings.Contains(html, "<h2") || !strings.Contains(html, "nas-01") {
		t.Errorf("html part = %q", html)
	}
}

func TestSTARTTLSWithPlainAndLogin(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	for _, mech := range []string{sasl.Plain, "LOGIN"} {
		box := &mailbox{mechs: []string{mech}}
		port := startServer(t, ca, box, true, false)
		msg := notifytest.SampleMessage()
		if err := send(t, newChannel(ca), conf(port, email.SecuritySTARTTLS, true), msg); err != nil {
			t.Fatalf("%s: %v", mech, err)
		}
		got := box.received()
		if len(got) != 1 || len(box.auths) != 1 || box.auths[0] != mech+" "+testUser+" "+testPassword || !box.tlsSeen[0] {
			t.Fatalf("%s: messages %d, auths %v, tls %v", mech, len(got), box.auths, box.tlsSeen)
		}
		checkMessage(t, got[0], msg)
	}
}

func TestImplicitTLS(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	box := &mailbox{mechs: []string{sasl.Plain}}
	port := startServer(t, ca, box, false, true)
	msg := notifytest.SampleMessage()
	msg.Title = "Ünïcödé " + strings.Repeat("long subject ", 20)
	if err := send(t, newChannel(ca), conf(port, email.SecurityTLS, true), msg); err != nil {
		t.Fatal(err)
	}
	got := box.received()
	if len(got) != 1 {
		t.Fatalf("%d messages", len(got))
	}
	checkMessage(t, got[0], msg)
	for _, line := range strings.Split(string(got[0].data), "\r\n") {
		if len(line) > 998 {
			t.Errorf("a line of %d bytes", len(line))
		}
	}
}

func TestPlainRelayWithoutAuth(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	box := &mailbox{}
	port := startServer(t, ca, box, false, false)
	if err := send(t, newChannel(ca), conf(port, email.SecurityNone, false), notifytest.SampleMessage()); err != nil {
		t.Fatal(err)
	}
	if got := box.received(); len(got) != 1 || box.tlsSeen[0] {
		t.Errorf("messages %d, tls %v", len(got), box.tlsSeen)
	}
}

func TestFailures(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	tests := []struct {
		name      string
		box       *mailbox
		starttls  bool
		security  string
		auth      bool
		password  string
		status    int
		permanent bool
		reason    string
	}{
		{"no STARTTLS", &mailbox{mechs: []string{sasl.Plain}}, false, email.SecuritySTARTTLS, true, "", 0, true, "STARTTLS"},
		{"5xx recipient", &mailbox{rcptErr: &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user"}}, true, email.SecuritySTARTTLS, false, "", 550, true, "No such user"},
		{"4xx data", &mailbox{dataErr: &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Try again later"}}, true, email.SecuritySTARTTLS, false, "", 451, false, "Try again later"},
		{"wrong password", &mailbox{mechs: []string{sasl.Plain}}, true, email.SecuritySTARTTLS, true, "wrong-password-xyz", 535, true, "535"},
		{"no mechanism", &mailbox{mechs: []string{"CRAM-MD5"}}, true, email.SecuritySTARTTLS, true, "", 0, true, "PLAIN nor LOGIN"},
	}
	for _, tt := range tests {
		port := startServer(t, ca, tt.box, tt.starttls, false)
		cfg := conf(port, tt.security, tt.auth)
		if tt.password != "" {
			cfg.Secrets[email.FieldPassword] = tt.password
		}
		err := send(t, newChannel(ca), cfg, notifytest.SampleMessage())
		var serr *notify.SendError
		if !errors.As(err, &serr) {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if serr.Status != tt.status || serr.Permanent != tt.permanent || !strings.Contains(serr.Reason, tt.reason) {
			t.Errorf("%s: %+v", tt.name, serr)
		}
		if strings.Contains(err.Error(), testPassword) || (tt.password != "" && strings.Contains(err.Error(), tt.password)) {
			t.Errorf("%s: the error contains the password", tt.name)
		}
		if n := len(tt.box.received()); n != 0 {
			t.Errorf("%s: %d messages delivered", tt.name, n)
		}
	}
}

// TestServerRepliesQuotingCredentialsAreRedacted: some servers repeat the
// user name, the password or the PLAIN blob in the 535 reply.
func TestServerRepliesQuotingCredentialsAreRedacted(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	const wrong = "wrong-Pa55word-xyz"
	blob := base64.StdEncoding.EncodeToString([]byte("\x00" + testUser + "\x00" + wrong))
	box := &mailbox{mechs: []string{sasl.Plain}, authMessage: func(user, pass string) string {
		return "5.7.8 Error: authentication failed for " + user + " password " + pass + " (" + blob + ") " + strings.ToUpper(user)
	}}
	port := startServer(t, ca, box, true, false)
	cfg := conf(port, email.SecuritySTARTTLS, true)
	cfg.Secrets[email.FieldPassword] = wrong
	err := send(t, newChannel(ca), cfg, notifytest.SampleMessage())
	var serr *notify.SendError
	if !errors.As(err, &serr) || serr.Status != 535 {
		t.Fatalf("Send = %v, want a 535 SendError", err)
	}
	for _, secret := range []string{testUser, strings.ToUpper(testUser), wrong, blob} {
		if strings.Contains(serr.Reason, secret) {
			t.Errorf("reason %q contains %q", serr.Reason, secret)
		}
	}
}

func TestTLSIsVerified(t *testing.T) {
	t.Parallel()
	ca, other := newCA(t), newCA(t)
	box := &mailbox{}
	port := startServer(t, ca, box, true, false)
	err := send(t, newChannel(other), conf(port, email.SecuritySTARTTLS, false), notifytest.SampleMessage())
	var serr *notify.SendError
	if !errors.As(err, &serr) || serr.Permanent || !strings.Contains(serr.Reason, "certificate") {
		t.Errorf("untrusted certificate = %v", err)
	}
	// Implicit TLS against a plain server.
	plain := startServer(t, ca, &mailbox{}, false, false)
	err = send(t, newChannel(ca), conf(plain, email.SecurityTLS, false), notifytest.SampleMessage())
	if !errors.As(err, &serr) || serr.Permanent {
		t.Errorf("TLS to a plain server = %v", err)
	}
	if len(box.received()) != 0 {
		t.Error("a message was delivered")
	}
}

func TestNetworkPolicy(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	box := &mailbox{}
	port := startServer(t, ca, box, true, false)
	ch := email.New(netguard.Policy{}.Dialer(5*time.Second), email.WithRootCAs(ca.pool))
	err := send(t, ch, conf(port, email.SecuritySTARTTLS, false), notifytest.SampleMessage())
	var serr *notify.SendError
	if !errors.As(err, &serr) || !serr.Permanent || !strings.Contains(serr.Reason, "network policy") {
		t.Errorf("send to loopback = %v", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	ch := email.New(netguard.Policy{}.Dialer(time.Second))
	base := func() descriptor.Values { return conf(587, email.SecuritySTARTTLS, true) }
	tests := []struct {
		name  string
		edit  func(*descriptor.Values)
		field string
	}{
		{"password without TLS", func(v *descriptor.Values) { v.Options[email.FieldSecurity] = email.SecurityNone }, "secrets.password"},
		{"password without user", func(v *descriptor.Values) { delete(v.Options, email.FieldUsername) }, "options.username"},
		{"user without password", func(v *descriptor.Values) { v.Secrets = nil }, "secrets.password"},
		{"host with port", func(v *descriptor.Values) { v.Options[email.FieldHost] = "smtp.example.com:25" }, "options.host"},
		{"host with space", func(v *descriptor.Values) { v.Options[email.FieldHost] = "smtp example" }, "options.host"},
		{"port", func(v *descriptor.Values) { v.Options[email.FieldPort] = "70000" }, "options.port"},
		{"from", func(v *descriptor.Values) { v.Options[email.FieldFrom] = "a@b.c, d@e.f" }, "options.from"},
		{"to", func(v *descriptor.Values) { v.Options[email.FieldTo] = "not an address" }, "options.to"},
		{"header injection", func(v *descriptor.Values) { v.Options[email.FieldTo] = "a@b.c\r\nBcc: evil@x.y" }, "options.to"},
		{"too many", func(v *descriptor.Values) {
			v.Options[email.FieldTo] = strings.TrimSuffix(strings.Repeat("a@b.c,", 21), ",")
		}, "options.to"},
		{"helo", func(v *descriptor.Values) { v.Options[email.FieldHelo] = "bad name" }, "options.helo_name"},
	}
	for _, tt := range tests {
		v := base()
		tt.edit(&v)
		err := ch.Validate(v)
		var verr *domain.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		found := false
		for _, f := range verr.Fields {
			found = found || f.Field == tt.field
		}
		if !found {
			t.Errorf("%s: %+v, want %s", tt.name, verr.Fields, tt.field)
		}
	}
	v := base()
	v.Options[email.FieldHost] = "smtp.example.com"
	if got := ch.Summary(v); got != "smtp.example.com → 2 recipients" {
		t.Errorf("Summary = %q", got)
	}
	if got := email.MessageID("0195f0a4-3b7c<>\r\n"); got != "<0195f0a4-3b7c@syncr>" {
		t.Errorf("MessageID = %q", got)
	}
}

func TestContract(t *testing.T) {
	t.Parallel()
	ca := newCA(t)
	box := &mailbox{mechs: []string{sasl.Plain}, rcptErr: &smtp.SMTPError{Code: 550, Message: "rejected"}}
	port := startServer(t, ca, box, true, false)
	failing := conf(port, email.SecuritySTARTTLS, true)
	failing.Options[email.FieldUsername] = testUser
	failing.Secrets[email.FieldPassword] = testPassword
	notifytest.Run(t, newChannel(ca), notifytest.WithFailingTarget(failing))
}
