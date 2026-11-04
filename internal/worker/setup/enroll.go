package setup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
	"github.com/Zeptiny/Syncr/internal/worker/config"
)

// EnrollRequest is what the controller needs to register a worker.
type EnrollRequest struct {
	Token redact.Secret
	// Name is the worker's display name.
	Name string
	OS   string
	Arch string
	Mode config.LinkMode
	// URL and Fingerprint are for passive workers: where the controller
	// reaches the worker and the SHA-256 fingerprint it pins.
	URL         string
	Fingerprint string
}

// Credentials are what the controller returns once.
type Credentials struct {
	WorkerID string
	Secret   redact.Secret
}

// Enroller exchanges an enrollment token for worker credentials.
type Enroller interface {
	Enroll(ctx context.Context, req EnrollRequest) (Credentials, error)
}

// EnrollerFactory builds an Enroller for a controller. caFile is an optional
// PEM file added to the trusted roots.
type EnrollerFactory func(controllerURL, caFile string) (Enroller, error)

// enrollTimeout bounds the whole enrollment call.
const enrollTimeout = 30 * time.Second

// NewConnectEnroller calls EnrollmentService.Enroll over Connect. The TLS
// configuration trusts the system roots plus caFile; there is deliberately no
// way to skip verification.
func NewConnectEnroller(controllerURL, caFile string) (Enroller, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pool, err := LoadCAPool(caFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = pool
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: 15 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	}
	client := &http.Client{Transport: transport, Timeout: enrollTimeout}
	return &connectEnroller{
		url:    strings.TrimRight(controllerURL, "/"),
		client: syncrv1connect.NewEnrollmentServiceClient(client, strings.TrimRight(controllerURL, "/")),
	}, nil
}

// LoadCAPool returns the system roots plus the certificates in a PEM file.
func LoadCAPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile) //nolint:gosec // the path is the operator's --ca-file
	if err != nil {
		return nil, fmt.Errorf("read the CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the CA file %s contains no PEM certificate (expected the controller's CA or its self-signed certificate)", caFile)
	}
	return pool, nil
}

type connectEnroller struct {
	url    string
	client syncrv1connect.EnrollmentServiceClient
}

func (c *connectEnroller) Enroll(ctx context.Context, req EnrollRequest) (Credentials, error) {
	msg := &syncrv1.EnrollRequest{
		EnrollmentToken: req.Token.Reveal(),
		Name:            req.Name,
		Os:              req.OS,
		Arch:            req.Arch,
		Mode:            syncrv1.WorkerMode_WORKER_MODE_ACTIVE,
		Url:             req.URL,
		CertFingerprint: req.Fingerprint,
	}
	if req.Mode == config.LinkPassive {
		msg.Mode = syncrv1.WorkerMode_WORKER_MODE_PASSIVE
	}
	resp, err := c.client.Enroll(ctx, connect.NewRequest(msg))
	if err != nil {
		return Credentials{}, classifyEnrollError(c.url, err)
	}
	if resp.Msg.GetWorkerId() == "" || resp.Msg.GetWorkerSecret() == "" {
		return Credentials{}, fmt.Errorf("the server at %s answered without worker credentials; it does not look like a Syncr controller", c.url)
	}
	return Credentials{WorkerID: resp.Msg.GetWorkerId(), Secret: redact.Secret(resp.Msg.GetWorkerSecret())}, nil
}

// EnrollError is a failed enrollment with advice on what to do.
type EnrollError struct {
	Kind EnrollErrorKind
	// Message explains the failure and the fix.
	Message string
	Err     error
}

// EnrollErrorKind classifies an EnrollError, for tests and callers.
type EnrollErrorKind int

// The kinds of enrollment failure.
const (
	EnrollOther EnrollErrorKind = iota
	// EnrollUnreachable: no connection could be made.
	EnrollUnreachable
	// EnrollTLS: the controller's certificate was not accepted.
	EnrollTLS
	// EnrollTokenRejected: the token is wrong, expired or used.
	EnrollTokenRejected
	// EnrollNotController: the URL answers, but not with the Syncr API.
	EnrollNotController
	// EnrollInvalid: the controller rejected the request's contents.
	EnrollInvalid
)

func (e *EnrollError) Error() string { return e.Message }
func (e *EnrollError) Unwrap() error { return e.Err }

// classifyEnrollError turns a transport or Connect error into advice.
func classifyEnrollError(controller string, err error) error {
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		certInvalid      x509.CertificateInvalidError
		recordHeader     tls.RecordHeaderError
		dnsErr           *net.DNSError
		netErr           net.Error
		connectErr       *connect.Error
	)
	wrap := func(kind EnrollErrorKind, format string, args ...any) error {
		return &EnrollError{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
	}
	switch {
	case errors.As(err, &unknownAuthority):
		return wrap(EnrollTLS, "cannot verify the TLS certificate of %s: it is signed by an authority this machine does not trust.\n"+
			"If the controller uses a private CA or a self-signed certificate, pass the CA (or the certificate itself) as a PEM file:\n"+
			"  --ca-file /path/to/ca.pem\n"+
			"Certificate verification cannot be turned off.", controller)
	case errors.As(err, &hostname):
		return wrap(EnrollTLS, "the TLS certificate of %s does not match the host name in the URL (%v).\n"+
			"Use the exact name the certificate is issued for in --controller, or add the right name to the controller's certificate.", controller, hostname.Error())
	case errors.As(err, &certInvalid):
		return wrap(EnrollTLS, "the TLS certificate of %s is not valid (%v).\n"+
			"If it expired, renew it on the controller. If this machine's clock is wrong, fix the clock. "+
			"For a private CA, pass it with --ca-file /path/to/ca.pem.", controller, certInvalid.Error())
	case errors.As(err, &recordHeader):
		return wrap(EnrollTLS, "%s did not answer with TLS (is it an http:// port?). Use the controller's https:// address in --controller.", controller)
	case errors.As(err, &dnsErr):
		return wrap(EnrollUnreachable, "cannot resolve the host name of %s (%v).\nCheck the spelling of --controller and this machine's DNS.", controller, dnsErr.Err)
	case errors.As(err, &netErr):
		// A transport failure; errors the server sent are connect errors
		// and never net errors.
		if netErr.Timeout() {
			return wrap(EnrollUnreachable, "the controller at %s did not answer in time.\nCheck the URL, the network and any firewall or proxy in between.", controller)
		}
		return wrap(EnrollUnreachable, "cannot reach the controller at %s: %v\nCheck that the URL and port are right, that the controller is running, and that no firewall blocks this machine.", controller, err)
	case errors.As(err, &connectErr):
		return classifyConnect(controller, connectErr, err)
	case errors.Is(err, context.DeadlineExceeded):
		return wrap(EnrollUnreachable, "the controller at %s did not answer in time.\nCheck the URL, the network and any firewall or proxy in between.", controller)
	}
	return wrap(EnrollOther, "enrollment with %s failed: %v", controller, err)
}

func classifyConnect(controller string, ce *connect.Error, err error) error {
	wrap := func(kind EnrollErrorKind, format string, args ...any) error {
		return &EnrollError{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
	}
	switch ce.Code() {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return wrap(EnrollTokenRejected, "the controller rejected the enrollment token: it is wrong, expired or already used.\n"+
			"Create a new token in the controller (Workers, Add worker) and run the command again. Tokens are single use.")
	case connect.CodeInvalidArgument:
		return wrap(EnrollInvalid, "the controller rejected the request: %s", ce.Message())
	case connect.CodeUnimplemented, connect.CodeNotFound:
		return wrap(EnrollNotController, "%s does not answer the Syncr enrollment API (%s).\n"+
			"Check that --controller is the controller's base URL, without a path such as /api, "+
			"and that a reverse proxy in front of it forwards /syncr.v1.EnrollmentService/.", controller, ce.Code())
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded:
		return wrap(EnrollUnreachable, "the controller at %s is not available: %s\nCheck the URL and the network, then try again.", controller, ce.Message())
	case connect.CodeUnknown:
		// A reverse proxy error page or a non-Connect server: Connect maps
		// unexpected HTTP statuses to unknown or internal.
		return wrap(EnrollNotController, "%s did not answer like a Syncr controller: %s\nCheck --controller, and the reverse proxy in front of the controller if there is one.", controller, ce.Message())
	default:
		return wrap(EnrollOther, "enrollment with %s failed: %s: %s", controller, ce.Code(), ce.Message())
	}
}
