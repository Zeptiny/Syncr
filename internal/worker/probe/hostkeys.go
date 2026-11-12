package probe

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// HostKeys fetches the public host keys of an SSH server for the
// sftp_host_keys probe. *SSHScanner implements it.
type HostKeys interface {
	// HostKeys returns one entry per key type the server at host:port offers
	// (port 0 means 22), never an empty list without an error.
	HostKeys(ctx context.Context, host string, port int) ([]*syncrv1.SftpHostKey, error)
}

// DefaultSSHDialTimeout bounds each connection attempt of an SSHScanner,
// the TCP connection and the start of the SSH handshake together; the
// probe's own timeout bounds them all.
const DefaultSSHDialTimeout = 10 * time.Second

// maxHostLen is the longest host name accepted (a DNS name has at most 253
// characters; an IPv6 address with a zone fits too).
const maxHostLen = 253

// SSHScanner fetches host keys by starting SSH handshakes, one per host key
// algorithm, and aborting each as soon as the server presented its key:
// nothing is authenticated, no credentials are sent and nothing is changed
// on the server. A server offers one key per type, so asking for each
// algorithm in turn finds them all. RSA is asked for once (rsa-sha2-512,
// then rsa-sha2-256, then the old ssh-rsa), because the three name the same
// key. The connection is direct: a remote's socks_proxy or http_proxy is not
// used.
//
// The zero value is ready to use.
type SSHScanner struct {
	// DialTimeout bounds each connection attempt; zero means
	// DefaultSSHDialTimeout.
	DialTimeout time.Duration
	// Dial opens a TCP connection; nil means a net.Dialer with DialTimeout.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// hostKeyAlgorithms are the algorithms asked for, in order: ed25519, the
// ECDSA curves, then RSA (see SSHScanner). DSA is not asked for: neither
// current OpenSSH nor rclone accepts it.
func hostKeyAlgorithms() []string {
	return []string{
		ssh.KeyAlgoED25519,
		ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA,
	}
}

// errKeySeen aborts a handshake once the server's host key is recorded.
var errKeySeen = errors.New("host key recorded")

// HostKeys implements HostKeys. A server that cannot be reached fails with
// TEMPORARY; one that does not complete an SSH handshake, or offers no key
// of a supported type, with EXECUTION_FAILED; an invalid host or port with
// INVALID_SPEC. Messages name the address, never anything else.
func (s *SSHScanner) HostKeys(ctx context.Context, host string, port int) ([]*syncrv1.SftpHostKey, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if err := checkHost(host); err != nil {
		return nil, err
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid port %d", port)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var keys []*syncrv1.SftpHostKey
	seen := make(map[string]bool)
	for _, algo := range hostKeyAlgorithms() {
		if strings.HasPrefix(algo, "rsa-") || algo == ssh.KeyAlgoRSA {
			if seen[ssh.KeyAlgoRSA] {
				continue
			}
		}
		key, err := s.fetch(ctx, addr, algo)
		if err != nil {
			if neg, ok := errors.AsType[*ssh.AlgorithmNegotiationError](err); ok && neg.What == "host key" {
				continue // the server has no key of this type
			}
			return nil, s.failure(ctx, addr, err)
		}
		if seen[key.Type()] {
			continue
		}
		seen[key.Type()] = true
		keys = append(keys, &syncrv1.SftpHostKey{
			Algorithm:         key.Type(),
			Key:               base64.StdEncoding.EncodeToString(key.Marshal()),
			FingerprintSha256: ssh.FingerprintSHA256(key),
		})
	}
	if len(keys) == 0 {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
			"%s offers no host key of a supported type (ed25519, ECDSA or RSA)", addr)
	}
	return keys, nil
}

// fetch runs one handshake that offers only algo and returns the host key
// the server presented.
func (s *SSHScanner) fetch(ctx context.Context, addr, algo string) (ssh.PublicKey, error) {
	timeout := s.DialTimeout
	if timeout <= 0 {
		timeout = DefaultSSHDialTimeout
	}
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := s.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	conn, err := dial(attempt, "tcp", addr)
	if err != nil {
		return nil, &dialError{err: err}
	}
	defer func() { _ = conn.Close() }()
	// The handshake has the same deadline as the attempt, and stops at once
	// when the probe is cancelled.
	if d, ok := attempt.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	stop := context.AfterFunc(attempt, func() { _ = conn.Close() })
	defer stop()

	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{
		HostKeyAlgorithms: []string{algo},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errKeySeen
		},
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err == nil {
		// Unreachable: the callback never accepts a key. Close the session
		// rather than leave it open should that change.
		go ssh.DiscardRequests(reqs)
		go func() {
			for ch := range chans {
				_ = ch.Reject(ssh.Prohibited, "")
			}
		}()
		_ = c.Close()
	}
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("the server presented no host key")
	}
	return nil, err
}

// dialError marks a failure to open the TCP connection.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// failure maps an attempt's error to the probe's error.
func (s *SSHScanner) failure(ctx context.Context, addr string, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if _, ok := errors.AsType[*dialError](err); ok {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, "cannot connect to %s: %v", addr, err)
	}
	if neg, ok := errors.AsType[*ssh.AlgorithmNegotiationError](err); ok {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
			"%s: the server's SSH %s algorithms are not supported (it offers %s)", addr, neg.What, strings.Join(neg.RequestedAlgorithms, ", "))
	}
	return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "%s did not complete an SSH handshake: %v", addr, err)
}

// checkHost accepts a host name or an IP address (an IPv6 address without
// brackets, possibly with a zone).
func checkHost(host string) error {
	if host == "" || len(host) > maxHostLen {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid host: it must be a host name or an IP address")
	}
	for _, r := range host {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_:%", r)
		if !ok {
			return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid host %q: it must be a host name or an IP address", host)
		}
	}
	if strings.Contains(host, ":") && net.ParseIP(strings.SplitN(host, "%", 2)[0]) == nil {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid host %q: it must be a host name or an IP address", host)
	}
	return nil
}
