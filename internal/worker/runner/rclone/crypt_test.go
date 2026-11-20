package rclone

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
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
// passwords are obscured and never on the command line. The source's
// parent folder is listed first, and lists it.
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
	if got := sup.commands(); !slices.Equal(got, []string{"lsjson", "copy"}) {
		t.Fatalf("commands = %q, want the listing of the source's parent, then the copy", got)
	}
	if args := sup.specs[0].Args; args[len(args)-1] != "old:photos/" {
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
// root, or on another remote, is not checked.
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
			&listingSupervisor{stdout: lsjson(item("other", true, -1), item("photos", true, -1))}, []string{"lsjson", "sync"}, 0, ""},
		{"file in its parent", &syncrv1.Location{Remote: "vault", Path: "notes.txt"},
			&listingSupervisor{stdout: lsjson(item("notes.txt", false, 5))}, []string{"lsjson", "sync"}, 0, ""},
		{"root", &syncrv1.Location{Remote: "vault", Path: "/"}, &listingSupervisor{}, []string{"sync"}, 0, ""},
		{"another remote", &syncrv1.Location{Remote: "box", Path: "upload/x"}, &listingSupervisor{}, []string{"sync"}, 0, ""},
		{"missing folder", &syncrv1.Location{Remote: "vault", Path: "a/photos"},
			&listingSupervisor{stdout: lsjson(item("photos2", true, -1), item("photo", false, 1))}, []string{"lsjson"},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, `"a/photos" is not in its parent folder on the crypt remote "vault"`},
		{"empty parent", &syncrv1.Location{Remote: "vault", Path: "photos"},
			&listingSupervisor{stdout: lsjson()}, []string{"lsjson"},
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the remote's password is wrong"},
		{"undecryptable names", &syncrv1.Location{Remote: "vault", Path: "photos"},
			&listingSupervisor{exit: 1, stderr: []string{
				"2026/11/20 03:27:20 ERROR : error listing: there were 2 undecryptable name errors. first error: r2ufo1mensmf7iupqctcvvjsfk: undecryptable dir name detected: bad PKCS#7 padding - too long " + secret,
			}}, []string{"lsjson"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
		{"parent not found", &syncrv1.Location{Remote: "vault", Path: "a/photos"},
			&listingSupervisor{exit: 3, stderr: []string{"2026/11/20 03:27:21 ERROR : error listing: directory not found"}},
			[]string{"lsjson"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found"},
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
