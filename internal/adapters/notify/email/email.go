// Package email is the email notification channel: SMTP with the
// standard library's net/smtp, STARTTLS (the default, and then required)
// or implicit TLS, PLAIN or LOGIN authentication, and a multipart
// text/HTML message (notify.Text and notify.HTML).
//
// Each message carries Message-ID <delivery-id@syncr>, stable across
// retries, so a duplicate after an ambiguous failure can be recognized,
// plus Auto-Submitted: auto-generated and X-Syncr-Event.
//
// Replies: SMTP 5xx is permanent; 4xx, network and TLS errors are
// temporary; a server without STARTTLS when it is required, or without a
// supported AUTH mechanism, is permanent (the configuration must change).
package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/internal/sendutil"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

// Field names.
const (
	FieldHost     = "host"
	FieldPort     = "port"
	FieldSecurity = "security"
	FieldUsername = "username"
	FieldPassword = "password"
	FieldFrom     = "from"
	FieldTo       = "to"
	FieldHelo     = "helo_name"
)

// Security modes.
const (
	SecuritySTARTTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// Limits.
const (
	// MaxRecipients bounds the "to" list.
	MaxRecipients = 20
	// defaultTimeout bounds a send whose context has no deadline.
	defaultTimeout = time.Minute
)

// what names the target in reasons.
const what = "the SMTP server"

// hostRe matches a host name (an IP address is checked separately).
var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?$`)

// Channel is the email channel. It is safe for concurrent use.
type Channel struct {
	dial    netguard.DialFunc
	helo    string
	rootCAs *x509.CertPool
	now     func() time.Time
}

// Option configures New.
type Option func(*Channel)

// WithHelloName sets the default EHLO name (the controller's host name;
// default "localhost"). A contact's helo_name overrides it.
func WithHelloName(name string) Option {
	return func(c *Channel) {
		if name != "" && hostRe.MatchString(name) {
			c.helo = name
		}
	}
}

// WithRootCAs verifies servers against pool instead of the system roots
// (tests).
func WithRootCAs(pool *x509.CertPool) Option { return func(c *Channel) { c.rootCAs = pool } }

// WithClock sets the clock of the Date header (tests).
func WithClock(now func() time.Time) Option { return func(c *Channel) { c.now = now } }

// New returns the channel, connecting with dial (netguard's Dialer in
// production, so the relay's address is checked).
func New(dial netguard.DialFunc, opts ...Option) *Channel {
	c := &Channel{dial: dial, helo: "localhost", now: time.Now}
	for _, o := range opts {
		o(c)
	}
	return c
}

var (
	_ notify.Channel  = (*Channel)(nil)
	_ notify.Exampler = (*Channel)(nil)
)

// ID implements notify.Channel.
func (*Channel) ID() string { return "email" }

// Label implements notify.Channel.
func (*Channel) Label() string { return "Email" }

// Description implements notify.Channel.
func (*Channel) Description() string {
	return "Send email through an SMTP server (STARTTLS or TLS; PLAIN or LOGIN authentication)."
}

// Fields implements notify.Channel.
func (*Channel) Fields() []descriptor.Field {
	return []descriptor.Field{
		{Name: FieldHost, Label: "SMTP server", Kind: descriptor.KindString, Required: true, Placeholder: "smtp.example.com"},
		{Name: FieldPort, Label: "Port", Kind: descriptor.KindInt, Default: "587", Help: "587 for STARTTLS, 465 for TLS."},
		{Name: FieldSecurity, Label: "Security", Kind: descriptor.KindEnum, Default: SecuritySTARTTLS,
			Help: "STARTTLS fails if the server does not offer it; none sends in clear text and cannot be used with a password.",
			Choices: []descriptor.Choice{
				{Value: SecuritySTARTTLS, Label: "STARTTLS (required)"},
				{Value: SecurityTLS, Label: "TLS (implicit, port 465)"},
				{Value: SecurityNone, Label: "None (trusted local relay only)"},
			}},
		{Name: FieldUsername, Label: "Username", Kind: descriptor.KindString},
		{Name: FieldPassword, Label: "Password", Kind: descriptor.KindString, Secret: true},
		{Name: FieldFrom, Label: "From", Kind: descriptor.KindString, Required: true, Placeholder: "Syncr <syncr@example.com>"},
		{Name: FieldTo, Label: "To", Kind: descriptor.KindString, Required: true, Placeholder: "admin@example.com, ops@example.com",
			Help: "Up to 20 addresses, separated by commas."},
		{Name: FieldHelo, Label: "EHLO name", Kind: descriptor.KindString, Advanced: true,
			Help: "The host name Syncr introduces itself with; defaults to the controller's."},
	}
}

// Examples implements notify.Exampler.
func (*Channel) Examples() []descriptor.Values {
	return []descriptor.Values{
		{
			Options: map[string]string{
				FieldHost: "smtp.example.com", FieldPort: "587", FieldSecurity: SecuritySTARTTLS, FieldUsername: "syncr@example.com",
				FieldFrom: "Syncr <syncr@example.com>", FieldTo: "admin@example.com, Ops Team <ops@example.com>", FieldHelo: "syncr.example.com",
			},
			Secrets: map[string]string{FieldPassword: "example-smtp-password"}, //nolint:gosec // an example for the contract suite, not a credential
		},
		{
			Options: map[string]string{FieldHost: "smtp.office365.example", FieldPort: "465", FieldSecurity: SecurityTLS,
				FieldUsername: "alerts", FieldFrom: "alerts@example.org", FieldTo: "me@example.org"},
			Secrets: map[string]string{FieldPassword: "another-smtp-password"}, //nolint:gosec // an example for the contract suite, not a credential
		},
		{Options: map[string]string{FieldHost: "192.168.1.5", FieldPort: "25", FieldSecurity: SecurityNone,
			FieldFrom: "nas@home.lan", FieldTo: "me@home.lan"}},
	}
}

// config is a parsed configuration.
type config struct {
	host, security, helo string
	port                 int
	username, password   string
	from                 *mail.Address
	to                   []*mail.Address
}

// parse checks cfg (after descriptor.CheckFields) and adds problems to v.
func (c *Channel) parse(cfg descriptor.Values, v *domain.Validation) config {
	o := cfg.Options
	out := config{
		host: strings.TrimSpace(o[FieldHost]), security: o[FieldSecurity], helo: o[FieldHelo],
		username: o[FieldUsername], password: cfg.Secrets[FieldPassword], port: 587,
	}
	if out.security == "" {
		out.security = SecuritySTARTTLS
	}
	if out.host != "" && !validHost(out.host) {
		v.Add("options."+FieldHost, "must be a host name or IP address, without a port")
	}
	if p := o[FieldPort]; p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			v.Add("options."+FieldPort, "must be 1 to 65535")
		} else {
			out.port = n
		}
	}
	if out.helo != "" && !hostRe.MatchString(out.helo) {
		v.Add("options."+FieldHelo, "must be a host name")
	}
	switch {
	case out.password != "" && out.security == SecurityNone:
		v.Add("secrets."+FieldPassword, "cannot be sent without TLS; choose STARTTLS or TLS")
	case out.password != "" && out.username == "":
		v.Add("options."+FieldUsername, "is required with a password")
	case out.password == "" && out.username != "":
		v.Add("secrets."+FieldPassword, "is required with a username")
	}
	if f := o[FieldFrom]; f != "" {
		a, err := mail.ParseAddress(f)
		if err != nil || strings.ContainsAny(f, "\r\n") {
			v.Add("options."+FieldFrom, "must be one email address, e.g. Syncr <syncr@example.com>")
		} else {
			out.from = a
		}
	}
	if t := o[FieldTo]; t != "" {
		as, err := mail.ParseAddressList(t)
		switch {
		case err != nil || strings.ContainsAny(t, "\r\n"):
			v.Add("options."+FieldTo, "must be email addresses separated by commas")
		case len(as) > MaxRecipients:
			v.Add("options."+FieldTo, fmt.Sprintf("must have at most %d addresses", MaxRecipients))
		default:
			out.to = as
		}
	}
	return out
}

// validHost reports whether h is a host name or an IP literal (without
// brackets or a zone).
func validHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == ""
	}
	return len(h) <= 253 && hostRe.MatchString(h)
}

// Validate implements notify.Channel.
func (c *Channel) Validate(cfg descriptor.Values) error {
	var v domain.Validation
	for _, fe := range descriptor.CheckFields(c.Fields(), cfg) {
		v.Add(fe.Field, fe.Message)
	}
	c.parse(cfg, &v)
	return v.Err()
}

// Summary implements notify.Channel: "smtp.example.com → 2 recipients".
func (c *Channel) Summary(cfg descriptor.Values) string {
	var v domain.Validation
	p := c.parse(cfg, &v)
	host := p.host
	if host == "" || !validHost(host) {
		host = "SMTP server"
	}
	switch n := len(p.to); n {
	case 0:
		return host
	case 1:
		return host + " → " + p.to[0].Address
	default:
		return host + " → " + strconv.Itoa(n) + " recipients"
	}
}

// Send implements notify.Channel.
func (c *Channel) Send(ctx context.Context, cfg descriptor.Values, msg notify.Message) error {
	if err := ctx.Err(); err != nil {
		return sendutil.TransportError(err, what)
	}
	var v domain.Validation
	p := c.parse(cfg, &v)
	if v.Err() != nil || p.host == "" || p.from == nil || len(p.to) == 0 {
		return notify.Public(notify.Permanent(0, "the email configuration is not valid"))
	}
	if msg.Title == "" {
		msg.Title = notify.Title(msg)
	}
	data, err := BuildMessage(msg, p.from, p.to, c.now())
	if err != nil {
		return notify.Public(notify.Permanent(0, "the message cannot be encoded"))
	}
	if err := c.send(ctx, p, data); err != nil {
		return classify(err, redactor(cfg, p))
	}
	return nil
}

// errNoSTARTTLS and errNoAuth are configuration problems found on the
// server.
var (
	errNoSTARTTLS = errors.New("no STARTTLS")
	errNoAuth     = errors.New("no supported AUTH mechanism")
)

func (c *Channel) send(ctx context.Context, p config, data []byte) error {
	conn, err := c.dial(ctx, "tcp", net.JoinHostPort(p.host, strconv.Itoa(p.port)))
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = c.now().Add(defaultTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return err
	}
	tlsCfg := &tls.Config{ServerName: p.host, MinVersion: tls.VersionTLS12, RootCAs: c.rootCAs}
	if p.security == SecurityTLS {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return err
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, p.host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = cl.Close() }()
	helo := p.helo
	if helo == "" {
		helo = c.helo
	}
	if err := cl.Hello(helo); err != nil {
		return err
	}
	if p.security == SecuritySTARTTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errNoSTARTTLS
		}
		if err := cl.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if p.username != "" {
		_, mechs := cl.Extension("AUTH")
		var a smtp.Auth
		switch fields := strings.Fields(strings.ToUpper(mechs)); {
		case slices.Contains(fields, "PLAIN"):
			a = smtp.PlainAuth("", p.username, p.password, p.host)
		case slices.Contains(fields, "LOGIN"):
			a = &loginAuth{username: p.username, password: p.password}
		default:
			return errNoAuth
		}
		if err := cl.Auth(a); err != nil {
			return err
		}
	}
	if err := cl.Mail(p.from.Address); err != nil {
		return err
	}
	for _, to := range p.to {
		if err := cl.Rcpt(to.Address); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	// The message is accepted; a failing QUIT does not change that.
	_ = cl.Quit()
	return nil
}

// classify maps a send failure to a *notify.SendError. A server's reply is
// quoted only after r redacted the credentials from it.
func classify(err error, r *notify.Redactor) error {
	var tp *textproto.Error
	switch {
	case errors.Is(err, errNoSTARTTLS):
		return notify.Permanent(0, "the SMTP server does not offer STARTTLS; choose security TLS (usually port 465) or check the port")
	case errors.Is(err, errNoAuth):
		return notify.Permanent(0, "the SMTP server offers neither PLAIN nor LOGIN authentication")
	case errors.Is(err, errLoginChallenge):
		return notify.Permanent(0, "the SMTP server sent an unexpected LOGIN challenge")
	case errors.As(err, &tp):
		reason := fmt.Sprintf("%s answered %d", what, tp.Code)
		if s := sendutil.Snippet([]byte(tp.Msg), r); s != "" {
			reason += ": " + s
		}
		if tp.Code >= 500 {
			return notify.Permanent(tp.Code, reason)
		}
		return notify.Temporary(tp.Code, reason, 0)
	}
	var rh tls.RecordHeaderError
	if errors.As(err, &rh) {
		return notify.Temporary(0, "the SMTP server did not answer with TLS; check the port and security", 0)
	}
	return sendutil.TransportError(err, what)
}

// redactor returns the Redactor of a send: the secrets, the user name
// (servers name it in authentication failures) and the PLAIN credentials
// blob, which a server may echo base64-encoded.
func redactor(cfg descriptor.Values, p config) *notify.Redactor {
	extra := []string{p.username}
	if p.username != "" && p.password != "" {
		extra = append(extra, "\x00"+p.username+"\x00"+p.password)
	}
	return notify.NewRedactor(cfg, extra...)
}

// errLoginChallenge is an unexpected challenge of the LOGIN mechanism.
var errLoginChallenge = errors.New("unexpected LOGIN challenge")

// loginAuth is the LOGIN SASL mechanism (draft-murchison-sasl-login),
// which Office 365 and some relays require. Like smtp.PlainAuth it refuses
// to send credentials without TLS.
type loginAuth struct {
	username, password string
	step               int
}

// Start implements smtp.Auth.
func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("refusing to authenticate without TLS")
	}
	a.step = 0
	return "LOGIN", nil, nil
}

// Next implements smtp.Auth: "Username:" then "Password:" (by prompt, or
// in that order when the prompts are unusual).
func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.HasPrefix(prompt, "user"):
		return []byte(a.username), nil
	case strings.HasPrefix(prompt, "pass"):
		return []byte(a.password), nil
	case a.step == 1:
		return []byte(a.username), nil
	case a.step == 2:
		return []byte(a.password), nil
	}
	return nil, errLoginChallenge
}
