package rclone

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// listingSupervisor answers rclone's lsjson with stdout, stderr and exit,
// and every other command with success. It records every spec.
type listingSupervisor struct {
	stdout, stderr []string
	exit           int

	mu    sync.Mutex
	specs []proc.Spec
}

func (s *listingSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	if spec.Args[0] != "lsjson" {
		return proc.Result{}, nil
	}
	for _, l := range s.stderr {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	for _, l := range s.stdout {
		spec.Stdout(proc.Line{Data: []byte(l)})
	}
	if s.exit != 0 {
		return proc.Result{ExitCode: s.exit}, &proc.ExitError{Path: spec.Path, ExitCode: s.exit}
	}
	return proc.Result{}, nil
}

// commands returns the first argument of each run, in order.
func (s *listingSupervisor) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.specs))
	for i, sp := range s.specs {
		out[i] = sp.Args[0]
	}
	return out
}

// last is the spec of the last run.
func (s *listingSupervisor) last() proc.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.specs[len(s.specs)-1]
}

// TestRunCryptSides: a transfer between two crypt remotes over one sftp
// remote reaches rclone as the crypt aliases; each crypt remote's "remote"
// is built from its wrapped location, strict_names is on, and the
// passwords are obscured and never on the command line. Each crypt
// remote's password is checked first, then the source's parent folder is
// listed, and lists it.
func TestRunCryptSides(t *testing.T) {
	t.Parallel()
	const pass, other = "crypt pa:ss w'rd", "second-crypt-pass"
	sup := &listingSupervisor{stdout: lsjson(item("2023", true, -1), item("2024", true, -1))}
	f := newFixture(t, &fakeSupervisor{})
	f.runner.cfg.Supervisor = sup
	job := transferJob(
		&syncrv1.Location{Remote: "old", Path: "photos/2024"},
		&syncrv1.Location{Remote: "vault", Path: "photos/2024"},
		sftpRemote("box", "sftp-pass"), cryptRemote("vault", "box", "/upload/vault", pass), cryptRemote("old", "box", "upload/old", other),
	)
	if _, err := f.runner.Run(t.Context(), job, nil); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got := sup.commands(); !slices.Equal(got, []string{"cat", "cat", "lsjson", "copy"}) {
		t.Fatalf("commands = %q, want the password checks, the listing of the source's parent, then the copy", got)
	}
	for i, want := range []string{"old:", "vault:"} {
		if sp := sup.specs[i]; sp.Args[len(sp.Args)-1] != want || !sp.StdoutChunks {
			t.Errorf("password check %d: args = %q (stdout in chunks: %v), want the root of %s", i, sp.Args, sp.StdoutChunks, want)
		}
	}
	if args := sup.specs[2].Args; args[len(args)-1] != "old:photos/" {
		t.Errorf("listing args = %q, want the parent of the source", args)
	}
	spec := sup.last()
	if n := len(spec.Args); n < 3 || !slices.Equal(spec.Args[n-3:], []string{"--", "old:photos/2024", "vault:photos/2024"}) {
		t.Errorf("args = %q", spec.Args)
	}
	env := spec.Env
	for k, want := range map[string]string{
		"RCLONE_CONFIG_VAULT_TYPE": "crypt", "RCLONE_CONFIG_VAULT_REMOTE": "box:/upload/vault", "RCLONE_CONFIG_VAULT_STRICT_NAMES": "true",
		"RCLONE_CONFIG_OLD_TYPE": "crypt", "RCLONE_CONFIG_OLD_REMOTE": "box:upload/old", "RCLONE_CONFIG_OLD_STRICT_NAMES": "true",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{"RCLONE_CONFIG_VAULT_PASSWORD", "RCLONE_CONFIG_OLD_PASSWORD"} {
		if p := env[k]; p == "" || p == pass || p == other {
			t.Errorf("%s not obscured: %q", k, p)
		}
	}
	for _, sp := range sup.specs {
		for _, arg := range sp.Args {
			if strings.Contains(arg, pass) || strings.Contains(arg, other) || strings.Contains(arg, env["RCLONE_CONFIG_VAULT_PASSWORD"]) {
				t.Errorf("secret on the command line: %q", arg)
			}
		}
	}
}

// TestRunCryptSourceCheck: a source below a crypt remote's root must be
// listed by its parent folder, or the transfer fails before it starts:
// NOT_FOUND when the folder is missing (with the wrong password its name
// encrypts to another one, which a bucket lists as empty), the listing's
// own failure (undecryptable names, scrubbed) otherwise. A source at the
// root, or on another remote, is not checked. (The password check runs
// first, for the crypt remote; here it finds nothing to decrypt.)
func TestRunCryptSourceCheck(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-PASS"
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", secret), cryptRemote("vault", "box", "upload/vault", secret)}
	for _, tt := range []struct {
		name     string
		source   *syncrv1.Location
		sup      *listingSupervisor
		commands []string
		code     syncrv1.JobErrorCode // unspecified: the transfer runs
		message  string
	}{
		{"folder in its parent", &syncrv1.Location{Remote: "vault", Path: "a/photos/"},
			&listingSupervisor{stdout: lsjson(item("other", true, -1), item("photos", true, -1))}, []string{"cat", "lsjson", "sync"}, 0, ""},
		{"file in its parent", &syncrv1.Location{Remote: "vault", Path: "notes.txt"},
			&listingSupervisor{stdout: lsjson(item("notes.txt", false, 5))}, []string{"cat", "lsjson", "sync"}, 0, ""},
		{"root", &syncrv1.Location{Remote: "vault", Path: "/"}, &listingSupervisor{}, []string{"cat", "sync"}, 0, ""},
		{"another remote", &syncrv1.Location{Remote: "box", Path: "upload/x"}, &listingSupervisor{}, []string{"sync"}, 0, ""},
		{"missing folder", &syncrv1.Location{Remote: "vault", Path: "a/photos"},
			&listingSupervisor{stdout: lsjson(item("photos2", true, -1), item("photo", false, 1))}, []string{"cat", "lsjson"},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, `"a/photos" is not in its parent folder on the crypt remote "vault"`},
		{"empty parent", &syncrv1.Location{Remote: "vault", Path: "photos"},
			&listingSupervisor{stdout: lsjson()}, []string{"cat", "lsjson"},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the remote's password is wrong"},
		{"undecryptable names", &syncrv1.Location{Remote: "vault", Path: "photos"},
			&listingSupervisor{exit: 1, stderr: []string{
				"2026/11/20 03:27:20 ERROR : error listing: there were 2 undecryptable name errors. first error: r2ufo1mensmf7iupqctcvvjsfk: undecryptable dir name detected: bad PKCS#7 padding - too long " + secret,
			}}, []string{"cat", "lsjson"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
		{"parent not found", &syncrv1.Location{Remote: "vault", Path: "a/photos"},
			&listingSupervisor{exit: 3, stderr: []string{"2026/11/20 03:27:21 ERROR : error listing: directory not found"}},
			[]string{"cat", "lsjson"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, &fakeSupervisor{})
			f.runner.cfg.Supervisor = tt.sup
			job := transferJob(tt.source, &syncrv1.Location{Path: f.data}, remotes...)
			job.Spec.GetRcloneTransfer().Mode = modeSync
			res, err := f.runner.Run(t.Context(), job, nil)
			if got := tt.sup.commands(); !slices.Equal(got, tt.commands) {
				t.Errorf("commands = %q, want %q", got, tt.commands)
			}
			switch {
			case tt.code == 0 && err != nil:
				t.Errorf("Run() = %v, want the transfer to run", err)
			case tt.code != 0 && (runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.message)):
				t.Errorf("Run() = %v (code %v), want code %v and %q", err, runner.CodeOf(err), tt.code, tt.message)
			case tt.code != 0 && res.ExitCode != -1 && res.ExitCode != 0:
				t.Errorf("exit code = %d", res.ExitCode)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the secret: %v", err)
			}
			if left := f.scratchEntries(t); len(left) != 0 {
				t.Errorf("scratch dir not cleaned up: %q", left)
			}
		})
	}
}

// The password check's output, recorded with rclone 1.75.2 (rclonetool's
// testdata has the whole runs): a file whose first block does not decrypt,
// and the notice of a name that does not (the check runs with
// strict_names off, so rclone skips it and goes on).
const (
	badBlockLine = `{"level":"error","msg":"Failed to send to output: failed to authenticate decrypted block - bad password?","object":"gYfkfj/S.iRn","objectType":"*crypt.Object","source":"operations/operations.go:1365"}`
	skippedName  = `{"level":"notice","msg":"Skipping undecryptable file name: bad PKCS#7 padding - too long","object":"6g41vk8scebcsit9k76am1j720/ebufkhlofpt2if49ob9mqv4ohc","objectType":"string","source":"crypt/crypt.go:358"}`
	foreignName  = `{"level":"notice","msg":"Skipping undecryptable file name: illegal base32 data at input byte 6","object":"un3t32b3ne803ib9jr3d3g2eu0/README.md","objectType":"string","source":"crypt/crypt.go:358"}`
)

// catRun is how cryptSupervisor answers the password check of one crypt
// remote: data on stdout (in chunks), lines on stderr, an exit code.
type catRun struct {
	stdout string
	stderr []string
	exit   int
}

// cryptSupervisor answers "rclone cat" (the password check) per remote
// root as cats says (no entry: nothing to decrypt) and everything else
// with success. It records every spec, and whether a check ended because
// the runner stopped it.
type cryptSupervisor struct {
	cats map[string]catRun

	mu      sync.Mutex
	specs   []proc.Spec
	stopped []string
}

func (s *cryptSupervisor) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	if spec.Args[0] != "cat" {
		return proc.Result{}, nil
	}
	target := spec.Args[len(spec.Args)-1]
	run := s.cats[target]
	if run.stdout != "" {
		spec.Stdout(proc.Line{Data: []byte(run.stdout)})
	}
	for _, l := range run.stderr {
		if ctx.Err() != nil {
			break
		}
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	if ctx.Err() != nil {
		s.mu.Lock()
		s.stopped = append(s.stopped, target)
		s.mu.Unlock()
		return proc.Result{ExitCode: -1}, &proc.CanceledError{Path: spec.Path, ExitCode: -1, Cause: context.Cause(ctx)}
	}
	if run.exit != 0 {
		return proc.Result{ExitCode: run.exit}, &proc.ExitError{Path: spec.Path, ExitCode: run.exit}
	}
	return proc.Result{}, nil
}

func (s *cryptSupervisor) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.specs))
	for i, sp := range s.specs {
		out[i] = sp.Args[0] + " " + sp.Args[len(sp.Args)-1]
	}
	return out
}

// TestRunCryptPasswordCheck: before a transfer, the password of each crypt
// remote among its sides is checked once, by reading the first byte of a
// file from the remote's root. A block that decrypts, or nothing to
// decrypt, lets the transfer run; a file or a name that does not decrypt
// (whatever the mode, the path or the side) fails it with
// EXECUTION_FAILED before rclone transfers anything; a check that cannot
// run is classified as rclone's failures are.
func TestRunCryptPasswordCheck(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-PASS"
	wrong := catRun{stderr: []string{badBlockLine, badBlockLine, badBlockLine + secret, badBlockLine}, exit: 1}
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", secret), cryptRemote("vault", "box", "upload/vault", secret)}
	vault := func(path string) *syncrv1.Location { return &syncrv1.Location{Remote: "vault", Path: path} }
	for _, tt := range []struct {
		name     string
		mode     syncrv1.RcloneTransferMode
		src, dst *syncrv1.Location // nil: the local folder
		cats     map[string]catRun
		stopped  []string // checks the runner stopped
		commands []string
		code     syncrv1.JobErrorCode // unspecified: the transfer runs
		message  string
	}{
		{"readable root", modeSync, vault(""), nil, map[string]catRun{"vault:": {stdout: "\xd5"}}, []string{"vault:"},
			[]string{"cat vault:", "sync <local>"}, 0, ""},
		{"nothing to decrypt", modeSync, vault(""), nil, nil, nil, []string{"cat vault:", "sync <local>"}, 0, ""},
		{"folder not created yet", modeCopy, nil, vault("new"),
			map[string]catRun{"vault:": {stderr: []string{`{"level":"notice","msg":"Failed to cat: directory not found"}`}, exit: 3}}, nil,
			[]string{"cat vault:", "copy vault:new"}, 0, ""},
		{"wrong password, root source", modeSync, vault(""), nil, map[string]catRun{"vault:": wrong}, []string{"vault:"},
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "source: the encrypted remote cannot decrypt the files stored in its folder, so its password is wrong"},
		{"wrong password, folder source", modeMove, vault("photos/2024"), nil, map[string]catRun{"vault:": wrong}, []string{"vault:"},
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "bad password?"},
		{"wrong password, destination", modeSync, nil, vault(""), map[string]catRun{"vault:": wrong}, []string{"vault:"},
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "destination: the encrypted remote cannot decrypt"},
		{"undecryptable names", modeCopy, vault("photos"), nil, map[string]catRun{"vault:": {stderr: []string{skippedName, skippedName}}}, nil,
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "ebufkhlofpt2if49ob9mqv4ohc: Skipping undecryptable file name"},
		{"three undecryptable names stop the check", modeCopy, vault("photos"), nil,
			map[string]catRun{"vault:": {stderr: []string{skippedName, skippedName, skippedName, skippedName}}}, []string{"vault:"},
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "password is wrong"},
		{"a foreign file, then the remote's own data", modeSync, vault(""), nil,
			map[string]catRun{"vault:": {stderr: []string{foreignName}, stdout: "\xd5"}}, []string{"vault:"},
			[]string{"cat vault:", "sync <local>"}, 0, ""},
		{"exit 3 without a missing folder", modeSync, vault(""), nil,
			map[string]catRun{"vault:": {stderr: []string{`{"level":"notice","msg":"Failed to cat: something new"}`}, exit: 3}}, nil,
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "cannot check the encrypted remote's password"},
		{"one remote on both sides, checked once", modeSync, vault(""), vault("b"),
			map[string]catRun{"vault:": {stdout: "x"}}, []string{"vault:"},
			[]string{"cat vault:", "sync vault:b"}, 0, ""},
		{"check fails", modeSync, vault(""), nil,
			map[string]catRun{"vault:": {stderr: []string{`{"level":"error","msg":"error listing: connection reset by peer","object":""}`}, exit: 5}}, nil,
			[]string{"cat vault:"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, "source: cannot check the encrypted remote's password: rclone cat"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sup := &cryptSupervisor{cats: tt.cats}
			f := newFixture(t, &fakeSupervisor{})
			f.runner.cfg.Supervisor = sup
			src, dst := tt.src, tt.dst
			if src == nil {
				src = &syncrv1.Location{Path: f.data}
			}
			if dst == nil {
				dst = &syncrv1.Location{Path: f.data}
			}
			job := transferJob(src, dst, remotes...)
			job.Spec.GetRcloneTransfer().Mode = tt.mode
			_, err := f.runner.Run(t.Context(), job, nil)
			got := sup.commands()
			for i, c := range got {
				// The local folder is an absolute path of this OS (a
				// drive letter on Windows); remote paths are not.
				if verb, path, ok := strings.Cut(c, " "); ok && filepath.IsAbs(path) {
					got[i] = verb + " <local>"
				}
			}
			if !slices.Equal(got, tt.commands) {
				t.Errorf("commands = %q, want %q", got, tt.commands)
			}
			if !slices.Equal(sup.stopped, tt.stopped) {
				t.Errorf("checks stopped by the runner = %q, want %q", sup.stopped, tt.stopped)
			}
			switch {
			case tt.code == 0 && err != nil:
				t.Errorf("Run() = %v, want the transfer to run", err)
			case tt.code != 0 && (runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.message)):
				t.Errorf("Run() = %v (code %v), want code %v and %q", err, runner.CodeOf(err), tt.code, tt.message)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the secret: %v", err)
			}
			strict := rcloneenv.EnvName("vault", "strict_names")
			for _, sp := range sup.specs {
				if sp.Args[0] == "cat" && (!sp.StdoutChunks || !slices.Contains(sp.Args, "--min-size=1B") || !slices.Contains(sp.Args, "--count=1")) {
					t.Errorf("check args = %q, chunks %v", sp.Args, sp.StdoutChunks)
				}
				// Only the check turns strict_names off, replacing the
				// worker's variable (rclonetool.CryptCheckEnv).
				want := "true"
				if sp.Args[0] == "cat" {
					want = "false"
				}
				if got := sp.Env[strict]; got != want {
					t.Errorf("%s: %s = %q, want %q", sp.Args[0], strict, got, want)
				}
			}
			if n := len(sup.specs); n > 1 && len(sup.specs[0].Env) != len(sup.specs[n-1].Env) {
				t.Errorf("the check's environment has %d variables, the transfer's %d", len(sup.specs[0].Env), len(sup.specs[n-1].Env))
			}
		})
	}
}

// TestRunCryptPasswordCheckCanceled: a job canceled during the check stops
// there, canceled, without running the transfer.
func TestRunCryptPasswordCheckCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	sup := &cryptSupervisor{cats: map[string]catRun{"vault:": {stderr: []string{badBlockLine}}}}
	f := newFixture(t, &fakeSupervisor{})
	f.runner.cfg.Supervisor = canceling{sup, cancel}
	job := transferJob(&syncrv1.Location{Remote: "vault"}, &syncrv1.Location{Path: f.data},
		sftpRemote("box", "x"), cryptRemote("vault", "box", "upload/vault", "y"))
	_, err := f.runner.Run(ctx, job, nil)
	if !runner.Canceled(err) {
		t.Errorf("Run() = %v, want canceled", err)
	}
	if got := sup.commands(); !slices.Equal(got, []string{"cat vault:"}) {
		t.Errorf("commands = %q", got)
	}
}

// TestRunCryptPasswordCheckCanceledAfter: a job canceled after its check
// ended (here, with nothing to decrypt) is canceled before the transfer
// starts, and says so.
func TestRunCryptPasswordCheckCanceledAfter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	sup := &cryptSupervisor{}
	f := newFixture(t, &fakeSupervisor{})
	f.runner.cfg.Supervisor = cancelingAfter{sup, cancel}
	job := transferJob(&syncrv1.Location{Remote: "vault"}, &syncrv1.Location{Path: f.data},
		sftpRemote("box", "x"), cryptRemote("vault", "box", "upload/vault", "y"))
	_, err := f.runner.Run(ctx, job, nil)
	if !runner.Canceled(err) || !strings.Contains(err.Error(), "canceled after the check of the encrypted remote's password") {
		t.Errorf("Run() = %v, want canceled after the check", err)
	}
	if got := sup.commands(); !slices.Equal(got, []string{"cat vault:"}) {
		t.Errorf("commands = %q", got)
	}
}

// cancelingAfter cancels the job's context after each run.
type cancelingAfter struct {
	Supervisor
	cancel context.CancelFunc
}

func (c cancelingAfter) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	defer c.cancel()
	return c.Supervisor.Run(ctx, spec)
}

// canceling cancels the job's context before each run.
type canceling struct {
	Supervisor
	cancel context.CancelFunc
}

func (c canceling) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	c.cancel()
	return c.Supervisor.Run(ctx, spec)
}

// TestBisyncCryptPasswordCheck: a two-way sync checks the password of each
// crypt side before it opens the pair's state, and a wrong one fails it
// (EXECUTION_FAILED, naming the side) before rclone bisync runs.
func TestBisyncCryptPasswordCheck(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-PASS"
	f := newBisyncFixture(t, &fakeSupervisor{})
	sup := &cryptSupervisor{cats: map[string]catRun{"vault:": {stderr: []string{badBlockLine, badBlockLine, badBlockLine}, exit: 1}}}
	f.runner.r.cfg.Supervisor = sup
	job := f.job(func(op *syncrv1.RcloneBisync) { op.Path2 = &syncrv1.Location{Remote: "vault", Path: "pair"} })
	job.Spec.Remotes = []*syncrv1.RemoteDefinition{sftpRemote("box", secret), cryptRemote("vault", "box", "upload/vault", secret)}
	_, err := f.runner.Run(t.Context(), job, nil)
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || !strings.Contains(err.Error(), "path2: the encrypted remote cannot decrypt") {
		t.Errorf("Run() = %v (code %v)", err, runner.CodeOf(err))
	}
	if got := sup.commands(); !slices.Equal(got, []string{"cat vault:"}) {
		t.Errorf("commands = %q, want only the check", got)
	}
	if _, err := os.Stat(f.pairDir()); !os.IsNotExist(err) {
		t.Errorf("the pair's state was opened: %v", err)
	}

	// The right password: the first run resyncs (the local side has a
	// file, so it may).
	writeTestFile(t, filepath.Join(f.data, "a", "f1.txt"), "x")
	sup.cats = map[string]catRun{"vault:": {stdout: "x"}}
	if _, err := f.runner.Run(t.Context(), job, nil); err != nil {
		t.Errorf("Run() with the right password = %v", err)
	}
	if got := sup.commands(); len(got) < 3 || got[1] != "cat vault:" || !strings.HasPrefix(got[2], "bisync ") {
		t.Errorf("commands = %q, want the check, then rclone bisync", got)
	}
}

// TestProbeListCryptPasswordCheck: listing a folder of a crypt remote (the
// file browser, and "Test remote", which lists its root) checks the
// password first: a wrong one fails the probe in every name mode instead
// of showing garbled names or an empty folder.
func TestProbeListCryptPasswordCheck(t *testing.T) {
	t.Parallel()
	remotes := []*syncrv1.RemoteDefinition{sftpRemote("box", "x"), cryptRemote("vault", "box", "upload/vault", "y")}
	for _, tt := range []struct {
		name     string
		loc      *syncrv1.Location
		cat      catRun
		commands []string
		code     syncrv1.JobErrorCode
	}{
		{"right password", &syncrv1.Location{Remote: "vault", Path: "photos"}, catRun{stdout: "x"}, []string{"cat vault:", "lsjson vault:photos"}, 0},
		{"wrong password, root", &syncrv1.Location{Remote: "vault"}, catRun{stderr: []string{badBlockLine}, exit: 1}, []string{"cat vault:"},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"wrong password, folder", &syncrv1.Location{Remote: "vault", Path: "photos"}, catRun{stderr: []string{skippedName}}, []string{"cat vault:"},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"not a crypt remote", &syncrv1.Location{Remote: "box", Path: "upload"}, catRun{}, []string{"lsjson box:upload"}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sup := &cryptSupervisor{cats: map[string]catRun{"vault:": tt.cat}}
			f := newFixture(t, &fakeSupervisor{})
			f.runner.cfg.Supervisor = sup
			p := &Prober{r: f.runner}
			_, err := p.List(t.Context(), "0195f0a4-3b7c-7d1e-9f00-0000000000f1", remotes, &syncrv1.ProbeListRemote{Location: tt.loc})
			if got := sup.commands(); !slices.Equal(got, tt.commands) {
				t.Errorf("commands = %q, want %q", got, tt.commands)
			}
			switch {
			case tt.code == 0 && err != nil:
				t.Errorf("List() = %v", err)
			case tt.code != 0 && (runner.CodeOf(err) != tt.code || !strings.HasPrefix(err.Error(), "the encrypted remote cannot decrypt")):
				t.Errorf("List() = %v (code %v), want code %v", err, runner.CodeOf(err), tt.code)
			}
		})
	}
}
