package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// opSftpHostKeys is the probe operation that fetches an SSH server's host
// keys (Milestone 6).
const opSftpHostKeys = "sftp_host_keys"

// Limits of GetSftpHostKeys.
const (
	// maxHostLen is the longest host accepted: a DNS name has at most 253
	// characters, and an IPv6 address with a zone fits.
	maxHostLen = 253
	// defaultSSHPort is what port 0 means.
	defaultSSHPort = 22
	// MaxHostKeys is the most keys an answer may hold, as many as an sftp
	// remote's host_keys option takes; a server offers one per key type.
	MaxHostKeys = 16
	// maxAlgorithmLen bounds a key's algorithm name.
	maxAlgorithmLen = 64
)

// Field messages of GetSftpHostKeys.
const (
	hostMessage = "must be a host name or an IP address"
	portMessage = "must be between 1 and 65535 (0 means 22)"
)

// GetSftpHostKeys fetches the host keys of the SSH server at host:port
// (port 0 means 22) through workerID or, if zero, a worker the runner
// picks (any online worker that can run the probe, the least busy one),
// for an sftp remote's host_keys option. The worker only starts SSH
// handshakes and aborts them once the server presented its key: no
// credentials are sent and the remote's proxy options are not used. Any
// user may call it, within the per-user probe rate limit.
//
// The keys are checked before they are returned: each key's algorithm
// must be the one its wire format names, and the fingerprint is the
// SHA-256 of the key ("SHA256:" and the unpadded base64 of the digest, as
// ssh-keygen -l prints it), computed here rather than taken from the
// worker.
//
// Errors: a *domain.ValidationError on "host" or "port" for a malformed
// address (also when the worker refuses it); domain.ErrFailedPrecondition
// when the server cannot be reached, does not speak SSH, offers no key of
// a supported type or does not answer within the probe timeout (the
// worker's message, which names only the address), and when no worker can
// run the probe; domain.ErrUnavailable for a chosen worker that is offline,
// busy or does not answer; domain.ErrNotFound for a chosen worker that
// does not exist; domain.ErrRateLimited.
func (s *Service) GetSftpHostKeys(ctx context.Context, actor domain.Actor, host string, port int, workerID domain.WorkerID) (*syncrv1.GetSftpHostKeysResponse, error) {
	const what = "fetch the server's host keys"
	host, port, err := sshAddress(host, port)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := s.r.allow(actor.UserID); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	w, err := s.r.worker(ctx, selection{owner: actor.UserID, op: opSftpHostKeys, workerID: workerID})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_SftpHostKeys{SftpHostKeys: &syncrv1.ProbeSftpHostKeys{
		Host: host, Port: int32(port), //nolint:gosec // G115: checked to be at most 65535
	}}}
	res, err := s.r.exec(ctx, w, spec, s.r.cfg.Timeout, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, hostKeysFailure(err, net.JoinHostPort(host, strconv.Itoa(port))))
	}
	keys, err := checkHostKeys(res.GetSftpHostKeys().GetKeys())
	if err != nil {
		return nil, fmt.Errorf("%s: %w: worker %q answered with %w", what, domain.ErrFailedPrecondition, w.Name, err)
	}
	return &syncrv1.GetSftpHostKeysResponse{Keys: keys, Worker: workerRef(w)}, nil
}

// sshAddress validates an SSH server's address as the worker does: a host
// name or an IP address (surrounding spaces and the brackets of an IPv6
// address are dropped) and a port, 0 meaning 22. It returns the host and
// the port to send.
func sshAddress(host string, port int) (string, int, error) {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	var v domain.Validation
	switch {
	case host == "":
		v.Add("host", "is required")
	case !validHost(host):
		v.Add("host", hostMessage)
	}
	if port == 0 {
		port = defaultSSHPort
	}
	if port < 1 || port > 65535 {
		v.Add("port", portMessage)
	}
	if err := v.Err(); err != nil {
		return "", 0, err
	}
	return host, port, nil
}

// validHost reports whether host is a host name or an IP address: letters,
// digits and ".-_" (a name), plus ":" and a "%zone" for an IPv6 address,
// which must then parse.
func validHost(host string) bool {
	if len(host) > maxHostLen || strings.HasPrefix(host, "-") {
		return false
	}
	for _, r := range host {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_:%", r)
		if !ok {
			return false
		}
	}
	if strings.ContainsAny(host, ":%") {
		addr, _, _ := strings.Cut(host, "%")
		return net.ParseIP(addr) != nil && strings.Contains(addr, ":")
	}
	return true
}

// hostKeysFailure maps a failed host-key probe. The worker reports a
// server it cannot reach as TEMPORARY and one that is not SSH (or offers
// no supported key) as EXECUTION_FAILED, which failure already maps to
// domain.ErrFailedPrecondition with the worker's message; here a TIMEOUT
// is the server's too (the worker answered, the handshakes did not
// finish), and an INVALID_SPEC is the address the worker refused.
func hostKeysFailure(err error, addr string) error {
	var f *workerError
	if !errors.As(err, &f) {
		return err
	}
	switch f.probe.Code {
	case link.ErrorCodeName(syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT):
		return &workerError{probe: f.probe, mapped: fmt.Errorf("%w: the server at %s did not answer in time", domain.ErrFailedPrecondition, addr)}
	case jobs.CodeInvalidSpec:
		field, msg := "host", hostMessage
		if strings.Contains(f.probe.Message, "port") {
			field, msg = "port", portMessage
		}
		return &workerError{probe: f.probe, mapped: fieldError(field, msg)}
	}
	return err
}

// checkHostKeys checks a worker's answer (see GetSftpHostKeys) and returns
// the keys with the fingerprints computed here.
func checkHostKeys(in []*syncrv1.SftpHostKey) ([]*syncrv1.SftpHostKey, error) {
	switch {
	case len(in) == 0:
		return nil, errors.New("no host key")
	case len(in) > MaxHostKeys:
		return nil, fmt.Errorf("%d host keys, more than %d", len(in), MaxHostKeys)
	}
	out := make([]*syncrv1.SftpHostKey, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, k := range in {
		blob, err := base64.StdEncoding.DecodeString(k.GetKey())
		if err != nil {
			return nil, fmt.Errorf("host key %d: its key is not base64", i)
		}
		algo := k.GetAlgorithm()
		if !validAlgorithm(algo) || keyType(blob) != algo {
			return nil, fmt.Errorf("host key %d: its algorithm does not match the key", i)
		}
		sum := sha256.Sum256(blob)
		fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
		if got := k.GetFingerprintSha256(); got != "" && got != fp {
			return nil, fmt.Errorf("host key %d: its fingerprint does not match the key", i)
		}
		if seen[k.GetKey()] {
			continue
		}
		seen[k.GetKey()] = true
		out = append(out, &syncrv1.SftpHostKey{Algorithm: algo, Key: base64.StdEncoding.EncodeToString(blob), FingerprintSha256: fp})
	}
	return out, nil
}

// validAlgorithm reports whether a is a plausible key algorithm name, one
// that a known_hosts line or rclone's host_keys option can carry.
func validAlgorithm(a string) bool {
	if a == "" || len(a) > maxAlgorithmLen {
		return false
	}
	for _, r := range a {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-.@", r)
		if !ok {
			return false
		}
	}
	return true
}

// keyType returns the key type a public key in the SSH wire format names
// (its first string, RFC 4253 section 6.6); "" if blob is malformed.
func keyType(blob []byte) string {
	if len(blob) < 4 {
		return ""
	}
	n := int64(binary.BigEndian.Uint32(blob))
	if n == 0 || n > int64(len(blob)-4) {
		return ""
	}
	name := blob[4 : 4+n]
	if bytes.ContainsAny(name, " ,\n") {
		return ""
	}
	return string(name)
}
