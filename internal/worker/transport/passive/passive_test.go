package passive

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

const secret = "0123456789abcdef-secret"

// writeCert writes a self-signed certificate for 127.0.0.1 (generated like
// setup does) and returns the file paths and the certificate.
func writeCert(t *testing.T) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	pair, err := tlscert.Generate(tlscert.Options{Hostname: "worker", IPs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if _, err := tlscert.Write(certFile, keyFile, pair, true); err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(pair.CertDER)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

// gateRunner blocks every job until it is cancelled.
type gateRunner struct{}

func (gateRunner) Kind() string { return "rclone_transfer" }

func (gateRunner) Run(ctx context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	emit(runner.Progress{Bytes: 1})
	<-ctx.Done()
	return runner.Result{ExitCode: -1}, context.Cause(ctx)
}

type fixture struct {
	agent  *agent.Agent
	server *Server
	client syncrv1connect.WorkerLinkServiceClient
	served chan error
}

func newFixture(t *testing.T, readMax int) *fixture {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register(gateRunner{}); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Runners: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })
	certFile, keyFile, cert := writeCert(t)
	srv, err := New(linkcore.New(a, a.Outbox(), nil), Options{Addr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, Secret: secret, ReadMaxBytes: readMax})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.Raw)
	if srv.Fingerprint() != hex.EncodeToString(sum[:]) {
		t.Errorf("fingerprint %s", srv.Fingerprint())
	}
	ln, err := srv.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{agent: a, server: srv, served: make(chan error, 1)}
	go func() { f.served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	f.client = syncrv1connect.NewWorkerLinkServiceClient(hc, "https://"+ln.Addr().String())
	return f
}

func authed[T any](msg *T, s string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	if s != "" {
		r.Header().Set("Authorization", link.PassiveAuthorization(s))
	}
	return r
}

func startCmd(id string) *syncrv1.Command {
	return &syncrv1.Command{Id: "start:" + id, Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{
		JobId: id, Spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}},
	}}}
}

func TestCommandAndPull(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 0)
	ctx := context.Background()
	resp, err := f.client.Command(ctx, authed(&syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("j")}}, secret))
	if err != nil || resp.Msg.GetEpoch() != f.agent.Epoch() || len(resp.Msg.GetAcks()) != 1 {
		t.Fatalf("command = %v, %v", resp, err)
	}
	pull, err := f.client.Pull(ctx, authed(&syncrv1.PullRequest{}, secret))
	if err != nil || pull.Msg.GetEvents()[0].GetHello() == nil || len(pull.Msg.GetState().GetJobs()) != 1 {
		t.Fatalf("pull = %v, %v", pull, err)
	}
	// Ack everything.
	last := pull.Msg.GetEvents()[len(pull.Msg.GetEvents())-1].GetSeq()
	if _, err := f.client.Pull(ctx, authed(&syncrv1.PullRequest{Epoch: pull.Msg.GetEpoch(), AfterSeq: last}, secret)); err != nil {
		t.Fatal(err)
	}
	if st := f.agent.Outbox().Stats(); st.Acked != last {
		t.Errorf("acked %d, want %d", st.Acked, last)
	}
}

func TestBadCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 0)
	for name, s := range map[string]string{"none": "", "wrong": secret + "x", "prefix": secret[:10]} {
		_, err := f.client.Command(context.Background(), authed(&syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("j")}}, s))
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Message() != "invalid controller credentials" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if st := f.agent.State(); len(st.GetJobs()) != 0 {
		t.Errorf("a rejected command reached the agent: %v", st)
	}
}

func TestReadLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 32<<10)
	big := &syncrv1.CommandRequest{Commands: []*syncrv1.Command{{Id: strings.Repeat("x", 64<<10)}}}
	if _, err := f.client.Command(context.Background(), authed(big, secret)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("oversized command: %v", err)
	}
}

// TestShutdownServesUntilDrained: after the agent shut down, Shutdown keeps
// serving Pulls until the controller acked the CANCELLED result.
func TestShutdownServesUntilDrained(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 0)
	ctx := context.Background()
	if _, err := f.client.Command(ctx, authed(&syncrv1.CommandRequest{Commands: []*syncrv1.Command{startCmd("j")}}, secret)); err != nil {
		t.Fatal(err)
	}
	if err := f.agent.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		stopped <- f.server.Shutdown(sctx)
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-stopped:
		t.Fatalf("stopped before the events were collected: %v", err)
	default:
	}
	pull, err := f.client.Pull(ctx, authed(&syncrv1.PullRequest{}, secret))
	if err != nil {
		t.Fatal(err)
	}
	evs := pull.Msg.GetEvents()
	if r := evs[len(evs)-1].GetJobResult(); r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN {
		t.Fatalf("last event = %v", evs[len(evs)-1])
	}
	if _, err := f.client.Pull(ctx, authed(&syncrv1.PullRequest{Epoch: pull.Msg.GetEpoch(), AfterSeq: evs[len(evs)-1].GetSeq()}, secret)); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if err := <-f.served; err != nil {
		t.Errorf("Serve = %v", err)
	}
}

func TestShutdownDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, 0) // the Hello is never collected
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := f.server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("Shutdown = %v after %v", err, time.Since(start))
	}
	if err := <-f.served; err != nil {
		t.Errorf("Serve = %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	certFile, keyFile, _ := writeCert(t)
	core := linkcore.New(nil, nil, nil)
	for name, o := range map[string]Options{
		"no addr":      {CertFile: certFile, KeyFile: keyFile, Secret: secret},
		"short secret": {Addr: ":0", CertFile: certFile, KeyFile: keyFile, Secret: "short"},
		"no cert":      {Addr: ":0", CertFile: filepath.Join(t.TempDir(), "missing.pem"), KeyFile: keyFile, Secret: secret},
		"key as cert":  {Addr: ":0", CertFile: keyFile, KeyFile: keyFile, Secret: secret},
		"negative":     {Addr: ":0", CertFile: certFile, KeyFile: keyFile, Secret: secret, ReadTimeout: -1},
	} {
		if _, err := New(core, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(nil, Options{Addr: ":0", CertFile: certFile, KeyFile: keyFile, Secret: secret}); err == nil {
		t.Error("nil core accepted")
	}
}
