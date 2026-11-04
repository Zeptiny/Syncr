package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var fingerprintFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)

func parse(t *testing.T, p Pair) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(p.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CertPEM is not a certificate: %q", p.CertPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestGenerate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p, err := Generate(Options{
		Hostname: "Worker1.Example.com",
		DNSNames: []string{"alias.example.com", "worker1.example.com"},
		IPs:      []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::1"), net.ParseIP("192.0.2.10")},
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(t, p)

	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key = %T, want ECDSA P-256", cert.PublicKey)
	}
	if cert.Subject.CommonName != "Worker1.Example.com" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if want := []string{"worker1.example.com", "alias.example.com"}; !slices.Equal(cert.DNSNames, want) {
		t.Errorf("DNS SANs = %q, want %q (lower-cased, deduplicated)", cert.DNSNames, want)
	}
	var ips []string
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	if want := []string{"192.0.2.10", "2001:db8::1"}; !slices.Equal(ips, want) {
		t.Errorf("IP SANs = %q, want %q", ips, want)
	}
	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Errorf("EKU = %v, want serverAuth only", cert.ExtKeyUsage)
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature || cert.IsCA {
		t.Errorf("key usage = %v, IsCA = %v", cert.KeyUsage, cert.IsCA)
	}
	if !cert.NotBefore.Before(now) {
		t.Errorf("NotBefore = %v, want it backdated before %v", cert.NotBefore, now)
	}
	if got := cert.NotAfter.Sub(now); got < 3649*24*time.Hour || got > 3651*24*time.Hour {
		t.Errorf("validity = %v, want about 10 years", got)
	}
	if !p.NotAfter.Equal(cert.NotAfter) {
		t.Errorf("Pair.NotAfter = %v, cert = %v", p.NotAfter, cert.NotAfter)
	}

	// The fingerprint is the SHA-256 of the DER encoding, in lower-case hex.
	sum := sha256.Sum256(cert.Raw)
	if want := hex.EncodeToString(sum[:]); p.Fingerprint != want {
		t.Errorf("Fingerprint = %s, want %s", p.Fingerprint, want)
	}
	if !fingerprintFormat.MatchString(p.Fingerprint) {
		t.Errorf("Fingerprint %q is not 64 lower-case hex digits", p.Fingerprint)
	}

	// Self-signed: the certificate verifies as its own root for each SAN.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	for _, name := range []string{"worker1.example.com", "alias.example.com", "192.0.2.10", "2001:db8::1"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, CurrentTime: now.Add(time.Hour), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
}

func TestGenerateDefaultsAndIPHostname(t *testing.T) {
	t.Parallel()
	p, err := Generate(Options{})
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(t, p)
	if len(cert.DNSNames)+len(cert.IPAddresses) == 0 {
		t.Error("default certificate has no subject alternative names")
	}

	p, err = Generate(Options{Hostname: "10.1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	cert = parse(t, p)
	if len(cert.DNSNames) != 0 || len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.1.2.3" {
		t.Errorf("IP host name: DNS %q IPs %v, want a single IP SAN", cert.DNSNames, cert.IPAddresses)
	}
}

func TestGenerateIsUnique(t *testing.T) {
	t.Parallel()
	a, err := Generate(Options{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(Options{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint == b.Fingerprint || string(a.KeyPEM) == string(b.KeyPEM) {
		t.Error("two generated certificates are identical")
	}
}

func TestWriteLoadRoundTripAndPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "worker.crt"), filepath.Join(dir, "worker.key")
	p, err := Generate(Options{Hostname: "w.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(certPath, keyPath, p, false); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{keyPath: 0o600, certPath: 0o644} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s mode = %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
			}
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("directory has %d entries, want 2 (no temporary files)", len(entries))
	}

	loaded, err := Load(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Leaf == nil || loaded.Leaf.Subject.CommonName != "w.example.com" {
		t.Errorf("Leaf = %v", loaded.Leaf)
	}
	if got := Fingerprint(loaded.Certificate[0]); got != p.Fingerprint {
		t.Errorf("loaded fingerprint %s != generated %s", got, p.Fingerprint)
	}
	if got, err := FingerprintFile(certPath); err != nil || got != p.Fingerprint {
		t.Errorf("FingerprintFile = %q, %v; want %s", got, err, p.Fingerprint)
	}
	if got, err := LoadFingerprint(certPath, keyPath); err != nil || got != p.Fingerprint {
		t.Errorf("LoadFingerprint = %q, %v; want %s", got, err, p.Fingerprint)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := Generate(Options{Hostname: "a"})
	b, _ := Generate(Options{Hostname: "b"})
	aCert, aKey := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	bKey := filepath.Join(dir, "b.key")
	if _, err := Write(aCert, aKey, a, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bKey, b.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(aCert, bKey); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("mismatched key: err = %v, want ErrKeyMismatch", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.crt"), aKey); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing certificate: err = %v, want os.ErrNotExist", err)
	}
	if _, err := FingerprintFile(aKey); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("key file as certificate: err = %v", err)
	}
}

func TestServesAndFingerprintMatchesWhatClientsSee(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key")
	p, err := Generate(Options{Hostname: "localhost", IPs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(certPath, keyPath, p, false); err != nil {
		t.Fatal(err)
	}
	cert, err := Load(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*tls.Conn).Handshake() //nolint:errcheck // test server
		_ = c.Close()
	}()

	var seen string
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			seen = Fingerprint(raw[0])
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if seen != p.Fingerprint {
		t.Errorf("client saw %s, setup would print %s", seen, p.Fingerprint)
	}
}

func TestSplitHosts(t *testing.T) {
	t.Parallel()
	dns, ips, err := SplitHosts([]string{"a.example.com", " 10.0.0.1 ", "", "::1", "host_1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dns, []string{"a.example.com", "host_1"}) || len(ips) != 2 {
		t.Errorf("dns = %q, ips = %v", dns, ips)
	}
	for _, bad := range []string{"https://x.example.com", "a b", "-x.example.com", "x..y"} {
		if _, _, err := SplitHosts([]string{bad}); err == nil {
			t.Errorf("SplitHosts(%q) accepted it", bad)
		}
	}
}
