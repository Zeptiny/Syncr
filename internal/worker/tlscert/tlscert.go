// Package tlscert creates, stores and loads the self-signed certificate a
// passive worker serves.
//
// The controller does not validate the certificate against a CA: it pins the
// SHA-256 fingerprint of the leaf certificate's DER encoding, which an admin
// copies from the worker's setup output into the controller. Fingerprint
// computes exactly that value (64 lower-case hex digits).
//
// Allowed dependencies: the standard library and internal/worker/atomicfile.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/atomicfile"
)

// DefaultValidity is the lifetime of generated certificates: long, because
// rotating a pinned certificate means re-registering the worker.
const DefaultValidity = 10 * 365 * 24 * time.Hour

// backdate absorbs clock skew between the worker and anything that checks
// NotBefore.
const backdate = time.Hour

// Options configures Generate.
type Options struct {
	// Hostname is the certificate's common name and first DNS SAN. Empty
	// uses os.Hostname.
	Hostname string
	// DNSNames and IPs are added to the subject alternative names. Hostname
	// is always included.
	DNSNames []string
	IPs      []net.IP
	// Validity is the certificate lifetime; zero means DefaultValidity.
	Validity time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Rand is the entropy source; nil means crypto/rand.
	Rand io.Reader
}

// Pair is a generated certificate and its key.
type Pair struct {
	// CertPEM and KeyPEM are the PEM encodings to store ("CERTIFICATE" and
	// PKCS#8 "PRIVATE KEY").
	CertPEM []byte
	KeyPEM  []byte
	// CertDER is the DER encoding of the certificate.
	CertDER []byte
	// Fingerprint is Fingerprint(CertDER).
	Fingerprint string
	// NotAfter is when the certificate expires.
	NotAfter time.Time
}

// Generate creates a self-signed ECDSA P-256 certificate valid for serverAuth.
func Generate(opts Options) (Pair, error) {
	random := opts.Rand
	if random == nil {
		random = rand.Reader
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	validity := opts.Validity
	if validity == 0 {
		validity = DefaultValidity
	}
	host := opts.Hostname
	if host == "" {
		var err error
		if host, err = os.Hostname(); err != nil || host == "" {
			host = "syncr-worker"
		}
	}
	dns, ips := sans(host, opts.DNSNames, opts.IPs)

	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return Pair{}, fmt.Errorf("tlscert: generate key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 127)
	serial, err := rand.Int(random, serialLimit)
	if err != nil {
		return Pair{}, fmt.Errorf("tlscert: generate serial number: %w", err)
	}
	notBefore := now().Add(-backdate)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host, Organization: []string{"Syncr worker"}},
		NotBefore:             notBefore,
		NotAfter:              now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(random, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Pair{}, fmt.Errorf("tlscert: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Pair{}, fmt.Errorf("tlscert: encode key: %w", err)
	}
	return Pair{
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		CertDER:     der,
		Fingerprint: Fingerprint(der),
		NotAfter:    tmpl.NotAfter,
	}, nil
}

// sans merges the host name and the extra names into deduplicated DNS and IP
// lists. A host that is an IP address becomes an IP SAN.
func sans(host string, dnsNames []string, ips []net.IP) ([]string, []net.IP) {
	var dns []string
	var out []net.IP
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}
	addDNS := func(name string) {
		name = strings.ToLower(name)
		if name != "" && !seenDNS[name] {
			seenDNS[name] = true
			dns = append(dns, name)
		}
	}
	addIP := func(ip net.IP) {
		if ip != nil && !seenIP[ip.String()] {
			seenIP[ip.String()] = true
			out = append(out, ip)
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		addIP(ip)
	} else {
		addDNS(host)
	}
	for _, n := range dnsNames {
		addDNS(n)
	}
	for _, ip := range ips {
		addIP(ip)
	}
	return dns, out
}

// Fingerprint returns the SHA-256 digest of a certificate's DER encoding as
// 64 lower-case hex digits, the format the controller pins.
func Fingerprint(certDER []byte) string {
	sum := sha256.Sum256(certDER)
	return hex.EncodeToString(sum[:])
}

// FingerprintFile returns the fingerprint of the first certificate (the leaf)
// in a PEM file.
func FingerprintFile(certPath string) (string, error) {
	der, err := leafDER(certPath)
	if err != nil {
		return "", err
	}
	return Fingerprint(der), nil
}

func leafDER(certPath string) ([]byte, error) {
	data, err := os.ReadFile(certPath) //nolint:gosec // path from the worker configuration
	if err != nil {
		return nil, fmt.Errorf("tlscert: read certificate: %w", err)
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("tlscert: %s contains no PEM certificate", certPath)
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes, nil
		}
	}
}

// Write stores the pair: the key with mode 0600 (on Windows, an ACL for
// SYSTEM and Administrators, plus the current user if includeUser), the
// certificate as world-readable. Each file is replaced atomically. The key is
// written first, so a failure never leaves a certificate without its key.
// The returned error is non-nil only for failures to write; a Windows ACL
// that could not be set is reported in the result.
func Write(certPath, keyPath string, p Pair, includeUser bool) (atomicfile.Result, error) {
	res, err := atomicfile.Write(keyPath, p.KeyPEM, atomicfile.Options{Perm: 0o600, RestrictACL: true, IncludeCurrentUser: includeUser})
	if err != nil {
		return atomicfile.Result{}, fmt.Errorf("tlscert: write key: %w", err)
	}
	if _, err := atomicfile.Write(certPath, p.CertPEM, atomicfile.Options{Perm: 0o644}); err != nil {
		return atomicfile.Result{}, fmt.Errorf("tlscert: write certificate: %w", err)
	}
	return res, nil
}

// ErrKeyMismatch is returned (wrapped) by Load when the key does not belong
// to the certificate.
var ErrKeyMismatch = errors.New("tlscert: private key does not match the certificate")

// Load reads a certificate chain and its private key. The first certificate
// is the leaf, and is parsed into the result's Leaf field.
func Load(certPath, keyPath string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		if strings.Contains(err.Error(), "private key does not match") {
			return tls.Certificate{}, fmt.Errorf("%w (%s, %s)", ErrKeyMismatch, certPath, keyPath)
		}
		return tls.Certificate{}, fmt.Errorf("tlscert: load certificate and key: %w", err)
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, fmt.Errorf("tlscert: parse certificate: %w", err)
		}
	}
	return cert, nil
}

// LoadFingerprint loads the pair and returns the leaf fingerprint, which
// proves that both files are readable and belong together.
func LoadFingerprint(certPath, keyPath string) (string, error) {
	c, err := Load(certPath, keyPath)
	if err != nil {
		return "", err
	}
	return Fingerprint(c.Certificate[0]), nil
}

// SplitHosts splits user-supplied subject alternative names into DNS names
// and IP addresses.
func SplitHosts(values []string) (dns []string, ips []net.IP, err error) {
	for _, v := range values {
		v = strings.TrimSpace(v)
		switch {
		case v == "":
		case net.ParseIP(v) != nil:
			ips = append(ips, net.ParseIP(v))
		case validDNSName(v):
			dns = append(dns, v)
		default:
			return nil, nil, fmt.Errorf("tlscert: %q is neither an IP address nor a DNS name", v)
		}
	}
	return dns, ips, nil
}

func validDNSName(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !isHostChar(c) {
				return false
			}
		}
	}
	return true
}

func isHostChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}
