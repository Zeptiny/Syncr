//go:build integration

package system

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
)

const (
	postgresImage = "postgres:17-alpine"
	minioImage    = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"
	pgUser        = "syncr"
	pgPassword    = "syncr-e2e"
	// adminToken is the test controller's admin token (a test value).
	adminToken = "e2e-admin-token-0123456789-abcdefghij"
	// waitLong bounds every wait for an asynchronous outcome.
	waitLong = 90 * time.Second
)

// shared is the state of the package: the built binaries and one Postgres
// server whose databases the tests share out.
var shared struct {
	buildOnce sync.Once
	buildErr  error
	binDir    string

	pgOnce sync.Once
	pgErr  error
	pg     testcontainers.Container
	pgHost string
	pgPort string
	nextDB atomic.Int64
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.pg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := shared.pg.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "terminate postgres: %v\n", err)
		}
		cancel()
	}
	if shared.binDir != "" {
		_ = os.RemoveAll(shared.binDir)
	}
	os.Exit(code)
}

// requirements skips the test unless it can run here, and returns the
// pinned tools.
func requirements(t *testing.T) (rclone, restic string) {
	t.Helper()
	// Every test here runs jobs through RunJob, which takes a definition over
	// the caller's stored remotes since Milestone 3.
	t.Skip("m3: pending integration: bootstrap through SYNCR_SETUP_CODE, CompleteSetup and an API token, " +
		"create the remotes with CreateRemote and send JobDefinitions to RunJob")
	if runtime.GOOS != "linux" {
		t.Skip("skipping: the binary end-to-end tests run on Linux only (Postgres and MinIO need Linux containers, and the drills use POSIX signals)")
	}
	rclone, restic = os.Getenv("SYNCR_TEST_RCLONE"), os.Getenv("SYNCR_TEST_RESTIC")
	if rclone == "" || restic == "" {
		t.Skip("skipping: set SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC to the pinned rclone and restic")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	return rclone, restic
}

// binaries builds syncr-controller and syncr-worker once per test run.
func binaries(t *testing.T) (controllerBin, workerBin string) {
	t.Helper()
	shared.buildOnce.Do(func() {
		out, err := exec.CommandContext(context.Background(), "go", "env", "GOMOD").Output()
		if err != nil {
			shared.buildErr = fmt.Errorf("go env GOMOD: %w", err)
			return
		}
		root := filepath.Dir(strings.TrimSpace(string(out)))
		dir, err := os.MkdirTemp("", "syncr-e2e-bin-")
		if err != nil {
			shared.buildErr = err
			return
		}
		shared.binDir = dir
		for _, name := range []string{"syncr-controller", "syncr-worker"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, name), "./cmd/"+name)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			b, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				shared.buildErr = fmt.Errorf("go build %s: %w\n%s", name, err, b)
				return
			}
		}
	})
	if shared.buildErr != nil {
		t.Fatal(shared.buildErr)
	}
	return filepath.Join(shared.binDir, "syncr-controller"), filepath.Join(shared.binDir, "syncr-worker")
}

// database creates an empty database on the shared Postgres server and
// returns its URL.
func database(t *testing.T) string {
	t.Helper()
	shared.pgOnce.Do(func() { shared.pgErr = startPostgres() })
	if shared.pgErr != nil {
		t.Fatalf("postgres: %v", shared.pgErr)
	}
	name := fmt.Sprintf("syncr_e2e_%d", shared.nextDB.Add(1))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, pgURL("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	return pgURL(name)
}

func pgURL(db string) string {
	u := url.URL{
		Scheme: "postgres", User: url.UserPassword(pgUser, pgPassword),
		Host: net.JoinHostPort(shared.pgHost, shared.pgPort), Path: "/" + db, RawQuery: "sslmode=disable",
	}
	return u.String()
}

func startPostgres() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"POSTGRES_USER": pgUser, "POSTGRES_PASSWORD": pgPassword, "POSTGRES_DB": "postgres"},
			Tmpfs:        map[string]string{"/var/lib/postgresql/data": "rw"},
			Cmd:          []string{"postgres", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	shared.pg = ctr
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	if shared.pgHost, err = ctr.Host(ctx); err != nil {
		return err
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return err
	}
	shared.pgPort = port.Port()
	return nil
}

// ---------------------------------------------------------------------------
// Processes
// ---------------------------------------------------------------------------

// syncBuffer collects a process's output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// process is a running binary. Its output is logged when the test fails.
type process struct {
	t    *testing.T
	name string
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
	err  error
}

// baseEnv is the environment of the processes: the test's, without any
// SYNCR_* variable, plus extra.
func baseEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SYNCR_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func startProcess(t *testing.T, name, bin string, env []string, args ...string) *process {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Env = env
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &process{t: t, name: name, cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.kill()
		if t.Failed() {
			t.Logf("---- %s output ----\n%s", name, tail(out.String(), 16<<10))
		}
	})
	return p
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// kill ends the process at once (SIGKILL), like a crash or power loss.
func (p *process) kill() {
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

// stop asks the process to stop (SIGTERM) and waits for it.
func (p *process) stop(timeout time.Duration) error {
	if p.exited() {
		return p.err
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-p.done:
		return p.err
	case <-time.After(timeout):
		p.kill()
		return fmt.Errorf("%s did not stop within %s", p.name, timeout)
	}
}

// killMatching kills every other process whose command line contains
// marker: the tool processes a killed worker leaves behind (they run in
// their own process group, and nothing stops them when the worker dies).
func killMatching(t *testing.T, marker string) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Logf("cannot list processes: %v", err)
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, []byte(marker)) {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// ---------------------------------------------------------------------------
// Controller
// ---------------------------------------------------------------------------

// controller is a syncr-controller process on its own database, with
// clients of its admin API.
type controller struct {
	t     *testing.T
	bin   string
	env   []string
	base  string
	proc  *process
	admin syncrv1connect.WorkerAdminServiceClient
	jobs  syncrv1connect.JobServiceClient
}

type bearer struct{ next http.RoundTripper }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+adminToken)
	return b.next.RoundTrip(r)
}

func newController(t *testing.T, bin string) *controller {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", freePort(t))
	c := &controller{
		t: t, bin: bin, base: "http://" + addr,
		env: baseEnv(
			"SYNCR_HTTP_ADDR="+addr,
			"SYNCR_DATABASE_URL="+database(t),
			"SYNCR_SECRETS_KEY="+base64.StdEncoding.EncodeToString(key),
			"SYNCR_ADMIN_TOKEN="+adminToken,
			"SYNCR_LOG_LEVEL=debug",
		),
	}
	client := &http.Client{Transport: bearer{http.DefaultTransport}}
	c.admin = syncrv1connect.NewWorkerAdminServiceClient(client, c.base)
	c.jobs = syncrv1connect.NewJobServiceClient(client, c.base)
	c.start()
	return c
}

// start starts the controller process and waits until it answers.
func (c *controller) start() {
	c.t.Helper()
	c.proc = startProcess(c.t, "syncr-controller", c.bin, c.env)
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(c.t.Context(), http.MethodGet, c.base+"/healthz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if c.proc.exited() || time.Now().After(deadline) {
			c.t.Fatalf("controller did not come up (exited: %v, err %v)", c.proc.exited(), c.proc.err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// token creates an enrollment token through the admin API.
func (c *controller) token() string {
	c.t.Helper()
	resp, err := c.admin.CreateEnrollmentToken(c.t.Context(), connect.NewRequest(&syncrv1.CreateEnrollmentTokenRequest{}))
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.Msg.GetToken()
}

// waitOnline waits until the worker is online and has reported its Hello.
func (c *controller) waitOnline(id string) *syncrv1.Worker {
	c.t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		resp, err := c.admin.GetWorker(c.t.Context(), connect.NewRequest(&syncrv1.GetWorkerRequest{WorkerId: id}))
		if err == nil {
			w := resp.Msg.GetWorker()
			if w.GetStatus() == syncrv1.WorkerStatus_WORKER_STATUS_ONLINE && w.GetInfo().GetWorkerVersion() != "" {
				return w
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("worker %s did not come online (last: %v, err %v)", id, resp, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runJob queues a job on the worker.
//
// m3: pending integration: RunJob no longer takes a JobSpec; the
// integration pass turns the specs of these tests into remotes
// (CreateRemote) and JobDefinitions. Until then the tests are skipped (see
// requirements).
func (c *controller) runJob(workerID string, _ *syncrv1.JobSpec) string {
	c.t.Helper()
	resp, err := c.jobs.RunJob(c.t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: workerID}))
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.Msg.GetJob().GetId()
}

func terminal(s syncrv1.JobStatus) bool {
	switch s {
	case syncrv1.JobStatus_JOB_STATUS_SUCCEEDED, syncrv1.JobStatus_JOB_STATUS_FAILED,
		syncrv1.JobStatus_JOB_STATUS_CANCELLED, syncrv1.JobStatus_JOB_STATUS_INTERRUPTED:
		return true
	}
	return false
}

// watch follows the job with WatchJob until it is terminal and returns it;
// a stream that ends early (watch limit, controller restart) is resumed.
// It also returns the statuses it saw, in order.
func (c *controller) watch(jobID string) (*syncrv1.Job, []syncrv1.JobStatus) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), waitLong)
	defer cancel()
	var seen []syncrv1.JobStatus
	for {
		stream, err := c.jobs.WatchJob(ctx, connect.NewRequest(&syncrv1.WatchJobRequest{JobId: jobID}))
		if err == nil {
			for stream.Receive() {
				j := stream.Msg().GetJob()
				if j == nil {
					continue
				}
				if len(seen) == 0 || seen[len(seen)-1] != j.GetStatus() {
					seen = append(seen, j.GetStatus())
				}
				if terminal(j.GetStatus()) {
					_ = stream.Close()
					return j, seen
				}
			}
			err = stream.Err()
			_ = stream.Close()
		}
		if ctx.Err() != nil {
			c.t.Fatalf("job %s: no terminal status within %s (saw %v, last error %v)", jobID, waitLong, seen, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitJob polls the job until ok (tolerating a controller that is down).
func (c *controller) waitJob(jobID, what string, ok func(*syncrv1.Job) bool) *syncrv1.Job {
	c.t.Helper()
	deadline := time.Now().Add(waitLong)
	var last *syncrv1.Job
	for {
		resp, err := c.jobs.GetJob(c.t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: jobID}))
		if err == nil {
			last = resp.Msg.GetJob()
			if ok(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("job %s: timed out waiting for %s (last %v, err %v)", jobID, what, last, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// worker is a machine with syncr-worker set up against a controller.
type worker struct {
	t      *testing.T
	bin    string
	dir    string
	root   string // the local root jobs use
	config string
	env    []string
	id     string
	proc   *process
}

// setupWorker runs `syncr-worker setup --no-service` against c, enrolling
// with a token from the admin API (passive: listening on a free port).
func setupWorker(t *testing.T, c *controller, bin, rclone, restic string, passive bool) *worker {
	t.Helper()
	dir := t.TempDir()
	w := &worker{
		t: t, bin: bin, dir: dir, root: filepath.Join(dir, "data"), config: filepath.Join(dir, "worker.yaml"),
		env: baseEnv("SYNCR_RCLONE_PATH="+rclone, "SYNCR_RESTIC_PATH="+restic),
	}
	if err := os.MkdirAll(w.root, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"setup", "--no-service", "--controller", c.base, "--token", c.token(),
		"--data-dir", filepath.Join(dir, "state"), "--local-root", w.root, "--config", w.config,
		"--name", t.Name(),
	}
	if passive {
		port := freePort(t)
		args = append(args, "--passive", "--listen", "127.0.0.1:"+port, "--advertise-url", "https://127.0.0.1:"+port)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	t.Logf("$ syncr-worker %s\n%s", redactToken(args), out)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(string(out), "Setup complete") {
		t.Fatal("setup did not complete")
	}
	cfg := workerconfig.Default()
	if err := platformconfig.Load(w.config, nil, &cfg); err != nil {
		t.Fatal(err)
	}
	w.id = cfg.Link.WorkerID
	if w.id == "" {
		t.Fatal("setup wrote no worker ID")
	}
	return w
}

func redactToken(args []string) string {
	out := append([]string(nil), args...)
	for i := range out {
		if i > 0 && out[i-1] == "--token" {
			out[i] = "<token>"
		}
	}
	return strings.Join(out, " ")
}

// start runs `syncr-worker run --config`.
func (w *worker) start() {
	w.t.Helper()
	w.proc = startProcess(w.t, "syncr-worker", w.bin, w.env, "run", "--config", w.config)
}

// ---------------------------------------------------------------------------
// Jobs and files
// ---------------------------------------------------------------------------

// copySpec is a local-to-local rclone copy; bwlimit > 0 slows it down.
func copySpec(src, dst string, bwlimit int64) *syncrv1.JobSpec {
	tr := &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: src},
		Destination: &syncrv1.Location{Path: dst},
	}
	if bwlimit > 0 {
		tr.Options = &syncrv1.RcloneTransferOptions{BandwidthLimitBytesPerSecond: &bwlimit}
	}
	return &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: tr}}
}

// writeRandom writes size random bytes to path.
func writeRandom(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sameFiles fails unless every file under src exists under dst with the same
// content.
func sameFiles(t *testing.T, src, dst string) {
	t.Helper()
	n := 0
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		a, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			return fmt.Errorf("copy of %s: %w", rel, err)
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("copy of %s differs", rel)
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no files compared")
	}
}

// wantStatus fails unless j has status want.
func wantStatus(t *testing.T, j *syncrv1.Job, want syncrv1.JobStatus) {
	t.Helper()
	if j.GetStatus() != want {
		t.Fatalf("job %s: status %v, want %v (error %v)", j.GetId(), j.GetStatus(), want, j.GetError())
	}
}
