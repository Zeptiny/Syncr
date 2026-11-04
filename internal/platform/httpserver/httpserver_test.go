package httpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestServeGracefulShutdown(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			_, _ = io.WriteString(w, "done")
		}),
	}
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	resp := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			resp <- result{err: err}
			return
		}
		defer func() { _ = r.Body.Close() }()
		b, err := io.ReadAll(r.Body)
		resp <- result{body: string(b), err: err}
	}()

	<-started
	cancel() // shutdown begins while the request is in flight
	time.Sleep(50 * time.Millisecond)
	close(release)

	if r := <-resp; r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body %q, err %v", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

func TestServeShutdownTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done() // never finishes on its own
		}),
	}
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln, 50*time.Millisecond) }()

	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			_ = r.Body.Close()
		}
	}()
	<-started
	cancel()

	if err := <-served; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve() = %v, want deadline exceeded", err)
	}
}

func TestServeListenerFailure(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	_ = ln.Close()
	err := Serve(t.Context(), &http.Server{ReadHeaderTimeout: time.Second}, ln, time.Second)
	if err == nil {
		t.Fatal("Serve() on a closed listener = nil, want error")
	}
}

// writeCert writes a self-signed certificate for 127.0.0.1 and returns the
// file paths and a pool trusting it.
func writeCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func TestServeTLS(t *testing.T) {
	t.Parallel()
	certFile, keyFile, pool := writeCert(t)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Protocols:         protocols,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.Proto)
		}),
	}
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- ServeTLS(ctx, srv, ln, certFile, keyFile, 5*time.Second) }()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+ln.Addr().String(), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "HTTP/2.0" {
		t.Errorf("served %q, want HTTP/2.0 over TLS", body)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("ServeTLS() = %v", err)
	}
}

func TestServeTLSBadCertificate(t *testing.T) {
	t.Parallel()
	ln := listen(t)
	missing := filepath.Join(t.TempDir(), "missing.pem")
	err := ServeTLS(t.Context(), &http.Server{ReadHeaderTimeout: time.Second}, ln, missing, missing, time.Second)
	if err == nil {
		t.Fatal("ServeTLS() with a missing certificate = nil, want error")
	}
	// The listener is closed: the port is free again.
	if _, err := ln.Accept(); err == nil {
		t.Error("listener still open")
	}
}
