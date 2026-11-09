package passive_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakeWorker is a WorkerLinkService that records the Authorization header
// it got and answers Pulls with padding events of a given size.
type fakeWorker struct {
	mu      sync.Mutex
	auth    []string
	padding int
}

func (f *fakeWorker) Command(_ context.Context, req *connect.Request[syncrv1.CommandRequest]) (*connect.Response[syncrv1.CommandResponse], error) {
	f.mu.Lock()
	f.auth = append(f.auth, req.Header().Get("Authorization"))
	f.mu.Unlock()
	return connect.NewResponse(&syncrv1.CommandResponse{Epoch: "ep"}), nil
}

func (f *fakeWorker) Pull(_ context.Context, req *connect.Request[syncrv1.PullRequest]) (*connect.Response[syncrv1.PullResponse], error) {
	f.mu.Lock()
	f.auth = append(f.auth, req.Header().Get("Authorization"))
	f.mu.Unlock()
	resp := &syncrv1.PullResponse{Epoch: "ep"}
	if f.padding > 0 {
		resp.Events = []*syncrv1.Event{{Seq: 1, Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{Hostname: strings.Repeat("h", f.padding)}}}}
	}
	return connect.NewResponse(resp), nil
}

// certificate is a key pair, optionally signed by a parent.
type certificate struct {
	der  []byte
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCert(t *testing.T, cn string, parent *certificate, isCA bool) *certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: isCA, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &certificate{der: der, cert: c, key: key}
}

func (c *certificate) fingerprint() string {
	sum := sha256.Sum256(c.der)
	return hex.EncodeToString(sum[:])
}

// serve starts a TLS server presenting chain (leaf first).
func serve(t *testing.T, w *fakeWorker, chain ...*certificate) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(syncrv1connect.NewWorkerLinkServiceHandler(w))
	srv := httptest.NewUnstartedServer(mux)
	tc := tls.Certificate{PrivateKey: chain[0].key}
	for _, c := range chain {
		tc.Certificate = append(tc.Certificate, c.der)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tc}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func target(url, fp string) workers.PassiveTarget {
	return workers.PassiveTarget{WorkerID: domain.NewWorkerID(), URL: url, CertFingerprint: fp, Secret: "s3cret-s3cret-s3cret"}
}

func TestClientPinsTheLeaf(t *testing.T) {
	t.Parallel()
	w := &fakeWorker{}
	// No host names at all: the pin replaces CA and host name checks.
	leaf := newCert(t, "syncr-worker", nil, false)
	srv := serve(t, w, leaf)

	upper := strings.ToUpper(leaf.fingerprint())
	colons := make([]string, 0, 32)
	for i := 0; i < len(upper); i += 2 {
		colons = append(colons, upper[i:i+2])
	}
	c, err := passive.NewClient(target(srv.URL+"/", strings.Join(colons, ":")), passive.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if resp, err := c.Pull(context.Background(), &syncrv1.PullRequest{}); err != nil || resp.GetEpoch() != "ep" {
		t.Fatalf("pull = %v, %v", resp, err)
	}
	if _, err := c.Command(context.Background(), &syncrv1.CommandRequest{}); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range w.auth {
		if a != "Bearer s3cret-s3cret-s3cret" {
			t.Errorf("authorization %q", a)
		}
	}
}

func TestClientRejectsOtherCertificates(t *testing.T) {
	t.Parallel()
	ca := newCert(t, "ca", nil, true)
	leaf := newCert(t, "127.0.0.1", ca, false)
	srv := serve(t, &fakeWorker{}, leaf, ca)
	for name, pin := range map[string]string{
		"another certificate": newCert(t, "other", nil, false).fingerprint(),
		// Pinning the CA does not make its leaves acceptable.
		"the issuing CA": ca.fingerprint(),
	} {
		c, err := passive.NewClient(target(srv.URL, pin), passive.ClientConfig{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Pull(context.Background(), &syncrv1.PullRequest{})
		if !errors.Is(err, passive.ErrFingerprintMismatch) || !strings.Contains(err.Error(), leaf.fingerprint()) {
			t.Errorf("%s: %v", name, err)
		}
		c.Close()
	}
}

func TestClientReadLimit(t *testing.T) {
	t.Parallel()
	leaf := newCert(t, "w", nil, false)
	srv := serve(t, &fakeWorker{padding: 64 << 10}, leaf)
	c, err := passive.NewClient(target(srv.URL, leaf.fingerprint()), passive.ClientConfig{ReadMaxBytes: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Pull(context.Background(), &syncrv1.PullRequest{}); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("oversized pull: %v", err)
	}
}

func TestClientTimeout(t *testing.T) {
	t.Parallel()
	leaf := newCert(t, "w", nil, false)
	block := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.der}, PrivateKey: leaf.key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	defer close(block)
	c, err := passive.NewClient(target(srv.URL, leaf.fingerprint()), passive.ClientConfig{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if _, err := c.Pull(context.Background(), &syncrv1.PullRequest{}); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("pull of a hanging worker: %v after %v", err, time.Since(start))
	}
}

func TestNewClientValidates(t *testing.T) {
	t.Parallel()
	fp := strings.Repeat("ab", 32)
	for name, tg := range map[string]workers.PassiveTarget{
		"http url":     target("http://w:7443", fp),
		"no host":      target("https://", fp),
		"short pin":    target("https://w:7443", "abcd"),
		"bad hex":      target("https://w:7443", strings.Repeat("zz", 32)),
		"empty secret": {URL: "https://w:7443", CertFingerprint: fp},
	} {
		if _, err := passive.NewClient(tg, passive.ClientConfig{}); !errors.Is(err, passive.ErrInvalidTarget) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Probe implements WorkerLinkService (m5: Track B tests Client.Probe).
func (*fakeWorker) Probe(context.Context, *connect.Request[syncrv1.ProbeRequest]) (*connect.Response[syncrv1.ProbeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
