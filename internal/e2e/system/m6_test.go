//go:build integration

package system

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// timeLimitWait bounds the wait for a job its one-minute time limit stops
// (the limit, the grace before the kill, and the report).
const timeLimitWait = 3 * time.Minute

// throttledRestic writes a wrapper that runs restic with --limit-upload
// kib (KiB/s) and returns its path. The worker takes it as its configured
// restic: it reports restic's own version, so it passes the pin check.
func throttledRestic(t *testing.T, restic string, kib int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restic")
	script := fmt.Sprintf("#!/bin/sh\nexec %s --limit-upload %d \"$@\"\n", strconv.Quote(restic), kib)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitTerminal polls the job until it ended, for up to d.
func (c *controller) waitTerminal(jobID string, d time.Duration) *syncrv1.Job {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for {
		resp, err := c.jobs.GetJob(c.t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: jobID}))
		if err == nil && terminal(resp.Msg.GetJob().GetStatus()) {
			return resp.Msg.GetJob()
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("job %s did not end within %v (last %v, %v)", jobID, d, resp, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// withLimit sets def's time limit.
func withLimit(def *syncrv1.JobDefinition, limit time.Duration) *syncrv1.JobDefinition {
	def.MaxRuntime = durationpb.New(limit)
	return def
}

// TestSystemTimeLimit: jobs with a one-minute time limit whose data moves
// too slowly are stopped by the worker and end failed (time_limit), with
// no process left behind. One is an rclone copy whose source is throttled
// (--bwlimit); the other is a restic backup (uploads throttled) holding
// its repository's concurrency key, and the check queued behind it on the
// same repository starts once the backup was stopped, and succeeds.
func TestSystemTimeLimit(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, throttledRestic(t, restic, 32), false)
	t.Cleanup(func() { killMatching(t, w.dir) })
	w.start()
	info := c.waitOnline(w.id)
	if !slices.Contains(info.GetInfo().GetFeatures(), "job.time_limit") {
		t.Fatalf("the worker does not report job.time_limit: %v", info.GetInfo().GetFeatures())
	}

	// A local repository, then 4 MiB to copy at 16 KiB/s and to back up
	// at 32 KiB/s: minutes each, against a limit of one.
	rc := syncrv1connect.NewResticServiceClient(c.client(), c.base)
	disk := c.createRemote(&syncrv1.RemoteInput{Name: "Disk", BackendType: "local", Options: map[string]string{"path": w.root}})
	created, err := rc.CreateRepository(t.Context(), connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: &syncrv1.RepositoryInput{
		Name: "Vault", RemoteId: disk, Path: "repo", Password: resticPassword, Mode: syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT, WorkerId: w.id,
	}}))
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo := created.Msg.GetRepository().GetId()
	src := filepath.Join(w.root, "time-limit-src")
	writeRandom(t, filepath.Join(src, "big.bin"), 4<<20)

	copyID := c.runJob(w.id, withLimit(localCopy(src, filepath.Join(w.root, "dst"), 16<<10), time.Minute))
	backupID := c.runJob(w.id, withLimit(backupJob(repo, []string{src}, nil, nil), time.Minute))
	checkID := c.runJob(w.id, &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{RepositoryId: repo}}})
	queued := c.waitJob(checkID, "queued behind the backup", func(j *syncrv1.Job) bool { return j.GetWaitingForJobId() != "" })
	if queued.GetWaitingForJobId() != backupID || queued.GetTimeLimit() != nil {
		t.Errorf("the check waits for %q (time limit %v), want the backup %s and no limit", queued.GetWaitingForJobId(), queued.GetTimeLimit(), backupID)
	}

	for what, id := range map[string]string{"copy": copyID, "backup": backupID} {
		j := c.waitTerminal(id, timeLimitWait)
		wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_FAILED)
		if j.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT ||
			j.GetError().GetMessage() != "stopped after 1m0s: the job's time limit was reached" || j.GetTimeLimit().AsDuration() != time.Minute {
			t.Errorf("%s: error %v, time limit %v", what, j.GetError(), j.GetTimeLimit())
		}
		if ran := j.GetFinishedAt().AsTime().Sub(j.GetStartedAt().AsTime()); ran < time.Minute || ran > time.Minute+30*time.Second {
			t.Errorf("%s ran %v, want its one-minute limit", what, ran)
		}
	}
	// Every process of the stopped jobs is gone.
	waitNoneMatching(t, src, 15*time.Second)

	check := c.waitTerminal(checkID, waitLong)
	wantStatus(t, check, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	backup := c.waitTerminal(backupID, time.Second)
	if check.GetStartedAt().AsTime().Before(backup.GetFinishedAt().AsTime()) {
		t.Errorf("the check started at %v, before the backup ended at %v", check.GetStartedAt().AsTime(), backup.GetFinishedAt().AsTime())
	}
	if strings.Contains(w.proc.out.String(), resticPassword) || strings.Contains(c.proc.out.String(), resticPassword) {
		t.Error("the repository password appears in a log")
	}
}

// startSFTP starts an SFTP server (atmoz/sftp) with user and password and
// returns it with its host and port.
func startSFTP(t *testing.T, user, password string) (testcontainers.Container, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	ctr, err := testcontainers.Run(ctx, "atmoz/sftp:alpine",
		testcontainers.WithCmd(user+":"+password+":1001:1001:upload"),
		testcontainers.WithExposedPorts("22/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("22/tcp").WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := ctr.MappedPort(ctx, "22/tcp")
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(mapped.Port())
	if err != nil {
		t.Fatal(err)
	}
	return ctr, host, port
}

// serverKeys returns the SSH server's host keys from its files, as
// "<algorithm> <key> <fingerprint>".
func serverKeys(t *testing.T, ctr testcontainers.Container) []string {
	t.Helper()
	var keys []string
	for _, name := range []string{"ssh_host_ed25519_key.pub", "ssh_host_rsa_key.pub"} {
		r, err := ctr.CopyFileFromContainer(t.Context(), "/etc/ssh/"+name)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		keys = append(keys, k.Type()+" "+base64.StdEncoding.EncodeToString(k.Marshal())+" "+ssh.FingerprintSHA256(k))
	}
	slices.Sort(keys)
	return keys
}

// TestSystemSftpHostKeys: the server's host keys are fetched through the
// API (a worker connects to the server), saved with an SFTP remote, and a
// transfer to the server works; with another key pinned instead, the
// transfer fails because rclone refuses the server.
func TestSystemSftpHostKeys(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	const user, password = "syncr", "e2e-sftp-password"
	ctr, host, port := startSFTP(t, user, password)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	c.waitOnline(w.id)
	ctx := t.Context()

	probes := syncrv1connect.NewProbeServiceClient(c.client(), c.base)
	fetched, err := probes.GetSftpHostKeys(ctx, connect.NewRequest(&syncrv1.GetSftpHostKeysRequest{Host: host, Port: int32(port), WorkerId: w.id}))
	if err != nil {
		t.Fatalf("GetSftpHostKeys: %v", err)
	}
	var got, pins []string
	for _, k := range fetched.Msg.GetKeys() {
		got = append(got, k.GetAlgorithm()+" "+k.GetKey()+" "+k.GetFingerprintSha256())
		pins = append(pins, k.GetAlgorithm()+" "+k.GetKey())
	}
	slices.Sort(got)
	if want := serverKeys(t, ctr); !slices.Equal(got, want) {
		t.Fatalf("fetched host keys\n%q\nwant the server's\n%q", got, want)
	}
	if fetched.Msg.GetWorker().GetId() != w.id {
		t.Errorf("fetched through %v, want worker %s", fetched.Msg.GetWorker(), w.id)
	}

	// shell_type none: the chrooted server has no shell for hash commands.
	options := map[string]string{"host": host, "port": strconv.Itoa(port), "user": user, "shell_type": "none", "host_keys": strings.Join(pins, "\n")}
	remote := c.createRemote(&syncrv1.RemoteInput{Name: "SSH server", BackendType: "sftp", Options: options, Secrets: map[string]string{"pass": password}})
	src := filepath.Join(w.root, "src")
	writeRandom(t, filepath.Join(src, "a.bin"), 64<<10)
	ok, _ := c.watch(c.runJob(w.id, copyDef(local(src), onRemote(remote, "upload/pinned"), 0)))
	wantStatus(t, ok, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	if ok.GetStats().GetFiles() != 1 {
		t.Errorf("stats = %v", ok.GetStats())
	}

	// Another key pinned: the server is refused.
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := ssh.NewPublicKey(other.Public())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := c.remotes.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: remote}))
	if err != nil {
		t.Fatal(err)
	}
	options["host_keys"] = otherKey.Type() + " " + base64.StdEncoding.EncodeToString(otherKey.Marshal())
	if _, err := c.remotes.UpdateRemote(ctx, connect.NewRequest(&syncrv1.UpdateRemoteRequest{
		RemoteId: remote, Version: stored.Msg.GetRemote().GetVersion(),
		Input: &syncrv1.RemoteInput{Name: "SSH server", BackendType: "sftp", Options: options},
	})); err != nil {
		t.Fatalf("UpdateRemote: %v", err)
	}
	refused, _ := c.watch(c.runJob(w.id, copyDef(local(src), onRemote(remote, "upload/other"), 0)))
	wantStatus(t, refused, syncrv1.JobStatus_JOB_STATUS_FAILED)
	if msg := refused.GetError().GetMessage(); !strings.Contains(msg, "host key") {
		t.Errorf("the refused transfer's error = %v, want it to name the host key", refused.GetError())
	}
	if strings.Contains(w.proc.out.String(), password) || strings.Contains(c.proc.out.String(), password) {
		t.Error("the SFTP password appears in a log")
	}
}

// startSecond starts another controller on c's database (same settings, its
// own address). It does not wait for it to answer.
func (c *controller) startSecond(t *testing.T) (*process, string) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", freePort(t))
	env := slices.DeleteFunc(slices.Clone(c.env), func(kv string) bool { return strings.HasPrefix(kv, "SYNCR_HTTP_ADDR=") })
	return startProcess(t, "syncr-controller (second)", c.bin, append(env, "SYNCR_HTTP_ADDR="+addr)), "http://" + addr
}

// healthy reports whether a controller answers /healthz at base.
func healthy(t *testing.T, base string) bool {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitFor polls cond until it holds, for up to d.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// databaseURL returns the controller's database URL.
func (c *controller) databaseURL() string {
	for _, kv := range c.env {
		if v, ok := strings.CutPrefix(kv, "SYNCR_DATABASE_URL="); ok {
			return v
		}
	}
	return ""
}

// controllerLockHolder returns the backend PID holding the single-controller
// guard of the database at url (0: nobody). The guard is the session-level
// advisory lock on 0x53594e435243 (postgres.controllerLockID).
func controllerLockHolder(t *testing.T, url string) int32 {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var pid int32
	err = conn.QueryRow(t.Context(), `SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1 AND classid = $1 AND objid = $2
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		int64(0x5359), int64(0x4e435243)).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// TestSystemSingleController: one controller serves a database. A second
// one waits for the guard without listening, says why, and takes over when
// the first stops. When the guard's session of the serving controller is
// killed while another controller waits, that one takes over and the first
// stops on its own with a non-zero exit status.
func TestSystemSingleController(t *testing.T) {
	t.Parallel()
	requirements(t)
	cbin, _ := binaries(t)
	first := newController(t, cbin)
	url := first.databaseURL()
	if pid := controllerLockHolder(t, url); pid == 0 {
		t.Fatal("the serving controller holds no guard")
	}

	second, secondBase := first.startSecond(t)
	waitFor(t, "the second controller to say it waits", 30*time.Second, func() bool {
		return strings.Contains(second.out.String(), "another controller is running against this database")
	})
	if healthy(t, secondBase) || second.exited() {
		t.Fatalf("the second controller serves or exited while the first runs:\n%s", tail(second.out.String(), 4<<10))
	}

	// The first stops: the second takes over without a restart.
	if err := first.proc.stop(30 * time.Second); err != nil {
		t.Fatalf("stop the first controller: %v", err)
	}
	waitFor(t, "the second controller to serve", 60*time.Second, func() bool { return healthy(t, secondBase) })

	// A third waits; the second's guard session is killed on the server.
	// The third gets the guard, and the second notices at its next check
	// and stops.
	third, thirdBase := first.startSecond(t)
	waitFor(t, "the third controller to wait", 30*time.Second, func() bool {
		return strings.Contains(third.out.String(), "another controller is running against this database")
	})
	holder := controllerLockHolder(t, url)
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var killed bool
	if err := conn.QueryRow(t.Context(), "SELECT pg_terminate_backend($1, 10000)", holder).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the guard's session %d: %v, %v", holder, killed, err)
	}
	waitFor(t, "the third controller to serve", 60*time.Second, func() bool { return healthy(t, thirdBase) })
	waitFor(t, "the second controller to stop", 60*time.Second, second.exited)
	if second.err == nil || !strings.Contains(second.out.String(), "another controller took over the database") {
		t.Errorf("the second controller exited with %v:\n%s", second.err, tail(second.out.String(), 4<<10))
	}
	if !strings.Contains(second.out.String(), "shutdown: store closed") {
		t.Errorf("the second controller did not shut down in order:\n%s", tail(second.out.String(), 4<<10))
	}
	if err := third.stop(30 * time.Second); err != nil {
		t.Errorf("stop the third controller: %v", err)
	}
}
