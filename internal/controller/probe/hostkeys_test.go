package probe_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// wireKey returns a public key in the SSH wire format: the strings of
// fields, each with its length (RFC 4251 section 5).
func wireKey(fields ...string) []byte {
	var b []byte
	for _, f := range fields {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

// hostKey is the SftpHostKey of blob, whose type is algo, with its
// fingerprint as ssh-keygen -l prints it.
func hostKey(algo string, blob []byte) *syncrv1.SftpHostKey {
	sum := sha256.Sum256(blob)
	return &syncrv1.SftpHostKey{
		Algorithm: algo, Key: base64.StdEncoding.EncodeToString(blob),
		FingerprintSha256: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
	}
}

// The keys of the test server: an ed25519 key and an ECDSA P-256 key (the
// key bytes are made up; only the format matters here).
var (
	edKey    = hostKey("ssh-ed25519", wireKey("ssh-ed25519", strings.Repeat("e", 32)))
	ecdsaKey = hostKey("ecdsa-sha2-nistp256", wireKey("ecdsa-sha2-nistp256", "nistp256", "\x04"+strings.Repeat("p", 64)))
)

// hostKeys answers sftp_host_keys probes with keys.
func hostKeys(keys ...*syncrv1.SftpHostKey) func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		return &syncrv1.ProbeResult{
			ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK,
			Output: &syncrv1.ProbeResult_SftpHostKeys{SftpHostKeys: &syncrv1.SftpHostKeys{Keys: keys}},
		}, nil
	}
}

// TestGetSftpHostKeys: the probe goes to a picked or chosen worker with the
// address normalized (port 0 is 22, IPv6 brackets dropped) and the listing
// timeout; the keys come back with their fingerprints and the worker.
func TestGetSftpHostKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = hostKeys(edKey, ecdsaKey)
	busy := e.workers.add("busy", domain.WorkerOnline, current())
	idle := e.workers.add("idle", domain.WorkerOnline, current())
	e.jobs.unfinished[busy] = 3
	ctx := context.Background()

	for _, actor := range []domain.Actor{alice, bob} {
		resp, err := e.svc.GetSftpHostKeys(ctx, actor, "sftp.example.com", 0, domain.WorkerID{})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetKeys()) != 2 || resp.GetKeys()[0].GetAlgorithm() != "ssh-ed25519" || resp.GetKeys()[0].GetKey() != edKey.GetKey() ||
			resp.GetKeys()[0].GetFingerprintSha256() != edKey.GetFingerprintSha256() || resp.GetKeys()[1].GetAlgorithm() != "ecdsa-sha2-nistp256" {
			t.Errorf("keys = %v", resp.GetKeys())
		}
		if resp.GetWorker().GetId() != idle.String() || resp.GetWorker().GetName() != "idle" {
			t.Errorf("worker = %v, want the least busy one", resp.GetWorker())
		}
		sent := e.transport.last(t)
		if op := sent.probe.GetSpec().GetSftpHostKeys(); sent.worker != idle || op.GetHost() != "sftp.example.com" || op.GetPort() != 22 ||
			sent.probe.GetTimeout().AsDuration() != 20*time.Second || len(sent.probe.GetSpec().GetRemotes()) != 0 {
			t.Errorf("sent %v to %v", sent.probe, sent.worker)
		}
	}
	// A chosen worker, an IPv6 address in brackets and another port.
	if _, err := e.svc.GetSftpHostKeys(ctx, alice, " [fd00::7] ", 2222, busy); err != nil {
		t.Fatal(err)
	}
	if sent := e.transport.last(t); sent.worker != busy || sent.probe.GetSpec().GetSftpHostKeys().GetHost() != "fd00::7" ||
		sent.probe.GetSpec().GetSftpHostKeys().GetPort() != 2222 {
		t.Errorf("sent %v to %v", sent.probe.GetSpec(), sent.worker)
	}
	// The worker's fingerprints are not needed: they are computed here.
	bare := &syncrv1.SftpHostKey{Algorithm: edKey.GetAlgorithm(), Key: edKey.GetKey()}
	e.transport.answer = hostKeys(bare, bare)
	resp, err := e.svc.GetSftpHostKeys(ctx, alice, "sftp.example.com", 22, domain.WorkerID{})
	if err != nil || len(resp.GetKeys()) != 1 || resp.GetKeys()[0].GetFingerprintSha256() != edKey.GetFingerprintSha256() {
		t.Errorf("without fingerprints, twice = (%v, %v), want one key with its fingerprint", resp.GetKeys(), err)
	}
	if !strings.HasPrefix(edKey.GetFingerprintSha256(), "SHA256:") || strings.HasSuffix(edKey.GetFingerprintSha256(), "=") {
		t.Errorf("fingerprint format %q", edKey.GetFingerprintSha256())
	}
}

// TestGetSftpHostKeysValidates: a malformed host or port is a field error
// before the rate limit and before any worker is asked.
func TestGetSftpHostKeysValidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = hostKeys(edKey)
	e.workers.add("nas", domain.WorkerOnline, current())
	ctx := context.Background()
	for _, tt := range []struct {
		host  string
		port  int
		field string
	}{
		{"", 22, "host"},
		{"   ", 22, "host"},
		{"bad host", 22, "host"},
		{"host;rm", 22, "host"},
		{"-oProxyCommand=x", 22, "host"},
		{"user@host", 22, "host"},
		{"1:2", 22, "host"},
		{"fe80::1%", 22, ""}, // an empty zone parses: the worker decides
		{"%eth0", 22, "host"},
		{strings.Repeat("a", 254), 22, "host"},
		{"sftp.example.com", -1, "port"},
		{"sftp.example.com", 65536, "port"},
	} {
		before := e.transport.count()
		_, err := e.svc.GetSftpHostKeys(ctx, alice, tt.host, tt.port, domain.WorkerID{})
		if tt.field == "" {
			continue
		}
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || verr.Fields[0].Field != tt.field {
			t.Errorf("GetSftpHostKeys(%q, %d) = %v, want a field error on %s", tt.host, tt.port, err, tt.field)
		}
		if e.transport.count() != before {
			t.Errorf("GetSftpHostKeys(%q, %d) asked a worker", tt.host, tt.port)
		}
	}
	if len(e.limiter.keys) != 1 { // the empty zone
		t.Errorf("the rate limit was consulted %d times for invalid requests", len(e.limiter.keys))
	}
	for _, ok := range []struct {
		host string
		port int
	}{{"sftp.example.com", 1}, {"sftp.example.com", 65535}, {"10.0.0.5", 22}, {"::1", 22}, {"fe80::1%eth0", 22}, {"my_host-1.lan", 22}, {strings.Repeat("a", 253), 22}} {
		if _, err := e.svc.GetSftpHostKeys(ctx, alice, ok.host, ok.port, domain.WorkerID{}); err != nil {
			t.Errorf("GetSftpHostKeys(%q, %d): %v", ok.host, ok.port, err)
		}
	}
	e.limiter.close()
	before := e.transport.count()
	if _, err := e.svc.GetSftpHostKeys(ctx, alice, "sftp.example.com", 22, domain.WorkerID{}); !errors.Is(err, domain.ErrRateLimited) || e.transport.count() != before {
		t.Errorf("rate limited: %v", err)
	}
	if e.limiter.keys[len(e.limiter.keys)-1] != alice.UserID.String() {
		t.Error("the rate limit is not per user")
	}
}

// TestGetSftpHostKeysWorkers: the worker must be able to run the probe.
func TestGetSftpHostKeysWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = hostKeys(edKey)
	ctx := context.Background()
	older := current()
	older.ProbeOps = slices.DeleteFunc(slices.Clone(link.ProbeOps()), func(op string) bool { return op == "sftp_host_keys" })
	old := e.workers.add("old", domain.WorkerOnline, older)
	asleep := e.workers.add("asleep", domain.WorkerOffline, current())

	if _, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, domain.WorkerID{}); !errors.Is(err, probe.ErrNoWorker) {
		t.Errorf("no capable online worker: %v", err)
	}
	if _, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, old); !errors.Is(err, domain.ErrFailedPrecondition) ||
		!strings.Contains(err.Error(), "upgrade it") {
		t.Errorf("an outdated chosen worker: %v", err)
	}
	if _, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, asleep); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("an offline chosen worker: %v", err)
	}
	if _, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, domain.NewWorkerID()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("an unknown chosen worker: %v", err)
	}
	if e.transport.count() != 0 {
		t.Errorf("%d probes were sent", e.transport.count())
	}
	e.workers.add("new", domain.WorkerOnline, workers.Info{Features: []string{link.FeatureProbe}, ProbeOps: []string{"sftp_host_keys"}})
	if _, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, domain.WorkerID{}); err != nil {
		t.Errorf("with a capable worker: %v", err)
	}
}

// TestGetSftpHostKeysFailures: what the worker reports maps to the errors
// the API promises: an unreachable server, one that is not SSH and one
// that does not answer in time are failed preconditions with the worker's
// message, an address the worker refuses is a field error; a malformed
// answer is refused.
func TestGetSftpHostKeysFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.workers.add("nas", domain.WorkerOnline, current())
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		code  syncrv1.JobErrorCode
		msg   string
		is    error
		field string
		text  string
	}{
		{"unreachable", syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, "cannot connect to h:22: connection refused", domain.ErrFailedPrecondition, "", "cannot connect to h:22"},
		{"not SSH", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "h:22 did not complete an SSH handshake: EOF", domain.ErrFailedPrecondition, "", "did not complete an SSH handshake"},
		{"no supported key", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "h:22 offers no host key of a supported type", domain.ErrFailedPrecondition, "", "no host key"},
		{"too slow", syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT, "the probe timed out", domain.ErrFailedPrecondition, "", "the server at h:22 did not answer in time"},
		{"a host the worker refuses", syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, `invalid host "h": it must be a host name`, nil, "host", ""},
		{"a port the worker refuses", syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid port 0", nil, "port", ""},
		{"busy", syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "too many probes", domain.ErrUnavailable, "", "busy"},
	} {
		e.transport.answer = failed(tt.code, tt.msg)
		_, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, domain.WorkerID{})
		if tt.is != nil && (!errors.Is(err, tt.is) || !strings.Contains(err.Error(), tt.text)) {
			t.Errorf("%s: err = %v, want %v with %q", tt.name, err, tt.is, tt.text)
		}
		if errors.Is(tt.is, domain.ErrFailedPrecondition) && errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("%s: err = %v is also unavailable", tt.name, err)
		}
		var verr *domain.ValidationError
		if tt.field != "" && (!errors.As(err, &verr) || verr.Fields[0].Field != tt.field) {
			t.Errorf("%s: err = %v, want a field error on %s", tt.name, err, tt.field)
		}
	}

	tooMany := make([]*syncrv1.SftpHostKey, probe.MaxHostKeys+1)
	for i := range tooMany {
		tooMany[i] = hostKey("ssh-ed25519", wireKey("ssh-ed25519", strings.Repeat(string(rune('a'+i)), 32)))
	}
	for name, keys := range map[string][]*syncrv1.SftpHostKey{
		"no keys":               nil,
		"too many":              tooMany,
		"not base64":            {{Algorithm: "ssh-ed25519", Key: "not base64!"}},
		"another algorithm":     {{Algorithm: "ssh-rsa", Key: edKey.GetKey()}},
		"a truncated key":       {{Algorithm: "ssh-ed25519", Key: base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 40, 's'})}},
		"a wrong fingerprint":   {{Algorithm: edKey.GetAlgorithm(), Key: edKey.GetKey(), FingerprintSha256: ecdsaKey.GetFingerprintSha256()}},
		"an algorithm with ' '": {hostKey("ssh ed", wireKey("ssh ed", "x"))},
	} {
		e.transport.answer = hostKeys(keys...)
		if _, err := e.svc.GetSftpHostKeys(ctx, alice, "h", 22, domain.WorkerID{}); !errors.Is(err, domain.ErrFailedPrecondition) {
			t.Errorf("%s: err = %v, want a failed precondition", name, err)
		}
	}
}
