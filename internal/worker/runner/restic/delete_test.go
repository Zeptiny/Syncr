package restic

import (
	"context"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const expectedResticID = "2d1912a057d761ffe3979dcdddfff925ef2ba21a67ea44b3e7b948a3cbaf4231"

// fakeBackend is the repository location a deletion test runs against: the
// names at the location, the files under keys/, and what restic answers.
type fakeBackend struct {
	t *testing.T
	// names are the direct children of the location; nil with missing set
	// means the location does not exist.
	names   []string
	missing bool
	keys    int
	// config is the reply of "cat config"; nil replies the golden config.
	config *reply
	locks  []string
	// fail makes "rclone delete" of that part fail.
	fail string
	// rmdirFails makes "rclone rmdir" of the location fail.
	rmdirFails bool
}

func fullRepository() []string {
	return []string{"config", "data", "index", "keys", "locks", "snapshots"}
}

func lsjsonLines(names []string) []string {
	lines := []string{"["}
	for i, n := range names {
		l := `{"Path":"` + n + `","Name":"` + n + `","Size":1,"ModTime":"","IsDir":true}`
		if i < len(names)-1 {
			l += ","
		}
		lines = append(lines, l)
	}
	return append(lines, "]")
}

var notFoundReply = reply{exit: 3, stderr: []string{
	"2026/01/02 03:04:05 ERROR : error listing: directory not found",
	"2026/01/02 03:04:05 NOTICE: Failed to lsjson with 2 errors: last error was: error in ListJSON: directory not found",
}}

// handle plays the backend: deletions change names, so later listings see
// them.
func (b *fakeBackend) handle(c call) reply {
	target := c.args[len(c.args)-1]
	base := target[strings.LastIndexAny(target, "/:")+1:]
	switch c.tool + " " + c.verb {
	case "restic cat":
		if b.config != nil {
			return *b.config
		}
		return reply{stdout: fixture(b.t, "cat_config.out")}
	case "restic unlock":
		return reply{}
	case "restic list":
		return reply{stdout: b.locks}
	case "rclone lsjson":
		if slices.Contains(c.args, "--files-only") { // keys/
			names := make([]string, b.keys)
			for i := range names {
				names[i] = "key" + strings.Repeat("0", i+1)
			}
			if !slices.Contains(b.names, "keys") {
				return notFoundReply
			}
			return reply{stdout: lsjsonLines(names)}
		}
		if b.missing {
			return notFoundReply
		}
		return reply{stdout: lsjsonLines(b.names)}
	case "rclone delete":
		if base == b.fail {
			return reply{exit: 1, stderr: []string{`{"level":"error","msg":"permission denied with key ` + testSecret + `"}`}}
		}
		return reply{stderr: fixture(b.t, "delete_rclone_stats.out")}
	case "rclone rmdir":
		if !slices.Contains(repositoryParts(), base) { // the location itself
			if b.rmdirFails {
				return reply{exit: 1, stderr: []string{"Failed to rmdir: directory not empty"}}
			}
			b.missing = true
			return reply{}
		}
		b.names = slices.DeleteFunc(b.names, func(n string) bool { return n == base })
		return reply{}
	case "rclone deletefile":
		b.names = slices.DeleteFunc(b.names, func(n string) bool { return n == base })
		return reply{}
	}
	b.t.Errorf("unexpected call %s %s %q", c.tool, c.verb, c.args)
	return reply{exit: 1}
}

func deleteSpec(removeDir bool) *syncrv1.JobSpec {
	return remoteSpec(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: remoteRepo(), ExpectedResticId: expectedResticID, RemoveDirectory: removeDir,
		}}
	})
}

// runDelete runs spec against b and returns the result, the calls and the
// error.
func runDelete(t *testing.T, b *fakeBackend, spec *syncrv1.JobSpec) (runner.Result, *scriptSupervisor, []runner.Progress, error) {
	t.Helper()
	b.t = t
	sup := &scriptSupervisor{handle: b.handle}
	te := newTestEnv(t, func(c *Config) { c.Supervisor = sup })
	var snaps []runner.Progress
	res, err := te.runners[kindDeleteData].Run(t.Context(), runnerJob(spec), func(p runner.Progress) { snaps = append(snaps, p) })
	return res, sup, snaps, err
}

// deleted returns the targets of the calls that delete something, in order,
// relative to the repository.
func deleted(sup *scriptSupervisor) []string {
	var out []string
	for _, c := range sup.calls {
		if c.tool == "rclone" && (c.verb == "delete" || c.verb == "deletefile" || c.verb == "rmdir") {
			out = append(out, c.verb+" "+strings.TrimPrefix(c.args[len(c.args)-1], "bk:bucket/repo one"))
		}
	}
	return out
}

func TestDeleteDataDeletesThePartsInOrder(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{names: append(fullRepository(), "README.txt"), keys: 1}
	res, sup, snaps, err := runDelete(t, b, deleteSpec(true))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"delete /locks", "rmdir /locks", "delete /snapshots", "rmdir /snapshots", "delete /index", "rmdir /index",
		"delete /data", "rmdir /data", "delete /keys", "rmdir /keys", "deletefile /config",
	}
	if got := deleted(sup); !slices.Equal(got, want) {
		t.Errorf("deletions =\n  %q\nwant\n  %q", got, want)
	}
	// Verification and the lock check come before anything is deleted.
	if got := sup.verbs()[:3]; !slices.Equal(got, []string{"restic cat", "restic unlock", "restic list"}) {
		t.Errorf("first calls = %v", got)
	}
	out := res.Output.GetResticDeleteData()
	// Each part's fixture reports 5 deleted objects, plus the config.
	if out.GetDeletedObjects() != 26 || out.GetDirectoryRemoved() || out.GetAlreadyGone() ||
		!slices.Equal(out.GetRemovedParts(), []string{"locks", "snapshots", "index", "data", "keys", "config"}) {
		t.Errorf("output = %v", out)
	}
	if res.Stats.Deletes != 26 || res.ExitCode != 0 {
		t.Errorf("result = %+v", res)
	}
	if len(snaps) == 0 || !slices.IsSortedFunc(snaps, func(a, b runner.Progress) int { return int(a.Deletes - b.Deletes) }) {
		t.Errorf("progress = %+v, want growing Deletes", snaps)
	}
	if slices.Contains(b.names, "config") || !slices.Contains(b.names, "README.txt") {
		t.Errorf("left at the location: %v", b.names)
	}
}

func TestDeleteDataRcloneInvocation(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{names: fullRepository(), keys: 1}
	_, sup, _, err := runDelete(t, b, deleteSpec(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range sup.calls {
		if c.tool != "rclone" {
			continue
		}
		// No rclone.conf, hard deletes, and the target after "--".
		if !slices.Equal(c.args[:3], []string{"--config=", "--b2-hard-delete", "--drive-use-trash=false"}) || c.args[len(c.args)-2] != "--" {
			t.Errorf("rclone args = %q", c.args)
		}
		if c.env["RCLONE_CONFIG_BK_SECRET_ACCESS_KEY"] != testSecret || c.env["RCLONE_CONFIG_BK_TYPE"] != "s3" {
			t.Errorf("rclone env lacks the remote: %v", keys(c.env))
		}
		if _, ok := c.env["RESTIC_PASSWORD"]; ok {
			t.Error("rclone was given the repository password")
		}
		if c.env["TMPDIR"] != c.dir {
			t.Errorf("TMPDIR = %q, dir = %q", c.env["TMPDIR"], c.dir)
		}
	}
	del := sup.calls[slices.IndexFunc(sup.calls, func(c call) bool { return c.verb == "delete" })]
	want := []string{"--config=", "--b2-hard-delete", "--drive-use-trash=false", "delete", "--rmdirs", "--use-json-log", "--stats=1s", "--stats-log-level=NOTICE", "--", "bk:bucket/repo one/locks"}
	if !slices.Equal(del.args, want) {
		t.Errorf("delete args = %q\nwant %q", del.args, want)
	}
}

func TestDeleteDataRemovesAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		names     []string
		removeDir bool
		rmdirFail bool
		wantRmdir bool
		want      bool
	}{
		{"own folder", fullRepository(), true, false, true, true},
		{"other files remain", append(fullRepository(), "notes.txt"), true, false, false, false},
		{"not asked to", fullRepository(), false, false, false, false},
		{"rmdir fails", fullRepository(), true, true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{names: tc.names, keys: 1, rmdirFails: tc.rmdirFail}
			res, sup, _, err := runDelete(t, b, deleteSpec(tc.removeDir))
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Output.GetResticDeleteData().GetDirectoryRemoved(); got != tc.want {
				t.Errorf("directory_removed = %v, want %v", got, tc.want)
			}
			if got := slices.Contains(deleted(sup), "rmdir "); got != tc.wantRmdir {
				t.Errorf("rmdir of the location ran = %v, want %v (%q)", got, tc.wantRmdir, deleted(sup))
			}
		})
	}
}

func TestDeleteDataNeverRemovesTheRemoteRoot(t *testing.T) {
	t.Parallel()
	spec := deleteSpec(true)
	spec.GetResticDeleteData().Repository.Location.Path = ""
	b := &fakeBackend{names: fullRepository(), keys: 1}
	res, sup, _, err := runDelete(t, b, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output.GetResticDeleteData().GetDirectoryRemoved() {
		t.Error("directory_removed for the remote's root")
	}
	for _, c := range sup.calls {
		if c.tool == "rclone" && c.verb == "rmdir" && c.args[len(c.args)-1] == "bk:" {
			t.Error("rmdir of the remote's root")
		}
		if c.verb == "deletefile" && c.args[len(c.args)-1] != "bk:config" {
			t.Errorf("deletefile target = %q", c.args[len(c.args)-1])
		}
	}
}

func TestDeleteDataVerification(t *testing.T) {
	t.Parallel()
	other := strings.ReplaceAll(strings.Join(fixture(t, "cat_config.out"), "\n"), expectedResticID, strings.Repeat("ab", 32))
	wrongPassword := exitError(12, "Fatal: wrong password or no key found")
	noRepository := exitError(10, "Fatal: repository does not exist")
	tests := []struct {
		name string
		b    *fakeBackend
		// want is the failure code; UNSPECIFIED means success.
		want        syncrv1.JobErrorCode
		wantDeleted []string
		check       func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, err error)
	}{
		{
			name: "another repository", b: &fakeBackend{names: fullRepository(), keys: 1, config: &reply{stdout: strings.Split(other, "\n")}},
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_MISMATCH,
			check: func(t *testing.T, _ *syncrv1.ResticDeleteDataOutput, err error) {
				if !strings.Contains(err.Error(), "nothing was deleted") {
					t.Errorf("message = %q", err)
				}
			},
		},
		{
			name: "wrong password with keys", b: &fakeBackend{names: fullRepository(), keys: 2, config: &wrongPassword},
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
		},
		{
			name: "resume: only the config is left to delete", b: &fakeBackend{names: []string{"config", "notes.txt"}, config: &wrongPassword},
			wantDeleted: []string{"deletefile /config"},
			check: func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, _ error) {
				if !slices.Equal(out.GetRemovedParts(), []string{"config"}) || out.GetDeletedObjects() != 1 {
					t.Errorf("output = %v", out)
				}
			},
		},
		{
			name: "resume: an empty keys folder", b: &fakeBackend{names: []string{"config", "keys"}, keys: 0, config: &wrongPassword},
			wantDeleted: []string{"deletefile /config"},
		},
		{
			name: "wrong password without a config", b: &fakeBackend{names: []string{"data"}, config: &wrongPassword},
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
		},
		{
			name: "already gone: an empty location", b: &fakeBackend{names: []string{"notes.txt"}, config: &noRepository},
			check: func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, _ error) {
				if !out.GetAlreadyGone() || out.GetDeletedObjects() != 0 {
					t.Errorf("output = %v", out)
				}
			},
		},
		{
			name: "already gone: no location", b: &fakeBackend{missing: true, config: &noRepository},
			check: func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, _ error) {
				if !out.GetAlreadyGone() {
					t.Errorf("output = %v", out)
				}
			},
		},
		{
			name: "config missing but data remains", b: &fakeBackend{names: []string{"data", "index"}, config: &noRepository},
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND,
			check: func(t *testing.T, _ *syncrv1.ResticDeleteDataOutput, err error) {
				if !strings.Contains(err.Error(), "index, data") {
					t.Errorf("message = %q, want the remaining parts", err)
				}
			},
		},
		{
			name: "locked by another client", b: &fakeBackend{names: fullRepository(), keys: 1, locks: fixture(t, "list_locks.out")},
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED,
		},
		{
			name: "restic cannot run", b: &fakeBackend{names: fullRepository(), keys: 1, config: &reply{exit: 1, stderr: []string{"boom"}}},
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, sup, _, err := runDelete(t, tc.b, deleteSpec(false))
			if tc.want == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				mustFail(t, err, tc.want)
				if res.Output != nil {
					t.Errorf("a failed run has output %v", res.Output)
				}
			}
			if got := deleted(sup); !slices.Equal(got, tc.wantDeleted) {
				t.Errorf("deletions = %q, want %q", got, tc.wantDeleted)
			}
			if tc.check != nil {
				tc.check(t, res.Output.GetResticDeleteData(), err)
			}
		})
	}
}

func TestDeleteDataSkipsMissingParts(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{names: []string{"config", "keys", "data"}, keys: 1}
	res, sup, _, err := runDelete(t, b, deleteSpec(false))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deleted(sup), []string{"delete /data", "rmdir /data", "delete /keys", "rmdir /keys", "deletefile /config"}; !slices.Equal(got, want) {
		t.Errorf("deletions = %q, want %q", got, want)
	}
	if got := res.Output.GetResticDeleteData().GetRemovedParts(); !slices.Equal(got, []string{"data", "keys", "config"}) {
		t.Errorf("removed_parts = %v", got)
	}
}

func TestDeleteDataFailurePartWayNamesWhatIsGone(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{names: fullRepository(), keys: 1, fail: "data"}
	res, sup, _, err := runDelete(t, b, deleteSpec(true))
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	msg := err.Error()
	if !strings.Contains(msg, "deleted locks, snapshots, index; failed while deleting data") {
		t.Errorf("message = %q", msg)
	}
	mustNotLeak(t, msg)
	// keys and the config are kept, so a retry can still verify.
	if got := deleted(sup); slices.Contains(got, "delete /keys") || slices.Contains(got, "deletefile /config") {
		t.Errorf("deletions went on after the failure: %q", got)
	}
	if res.Stats.Deletes != 15 {
		t.Errorf("Deletes = %d, want the 15 objects of the three parts", res.Stats.Deletes)
	}
}

func TestDeleteDataLocalRepository(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{names: fullRepository(), keys: 1}
	b.t = t
	sup := &scriptSupervisor{handle: b.handle}
	te := newTestEnv(t, func(c *Config) { c.Supervisor = sup })
	repo := te.local("repos", "one")
	spec := specWith(nil, func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: localRepo(repo), ExpectedResticId: expectedResticID, RemoveDirectory: true,
		}}
	})
	res, err := te.runners[kindDeleteData].Run(t.Context(), runnerJob(spec), func(runner.Progress) {})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Output.GetResticDeleteData().GetDirectoryRemoved() {
		t.Error("the emptied directory was not removed")
	}
	for _, c := range sup.calls {
		if c.tool != "rclone" {
			continue
		}
		if target := c.args[len(c.args)-1]; !strings.HasPrefix(target, repo) {
			t.Errorf("%s target %q is outside the repository", c.verb, target)
		}
		for k := range c.env {
			if strings.HasPrefix(k, "RCLONE_CONFIG_") {
				t.Errorf("a local repository needs no remote: %s", k)
			}
		}
	}

	// A repository outside local_roots is refused before anything runs.
	sup.calls = nil
	spec.GetResticDeleteData().Repository = localRepo("/elsewhere/repo")
	_, err = te.runners[kindDeleteData].Run(t.Context(), runnerJob(spec), func(runner.Progress) {})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
	if len(sup.calls) != 0 {
		t.Errorf("calls for a refused path: %v", sup.verbs())
	}
}

func TestDeleteDataRejectsBadSpecs(t *testing.T) {
	t.Parallel()
	bad := deleteSpec(false)
	bad.GetResticDeleteData().ExpectedResticId = "abc"
	for name, spec := range map[string]*syncrv1.JobSpec{
		"no operation":        remoteSpec(func(*syncrv1.JobSpec) {}),
		"malformed restic id": bad,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, sup, _, err := runDelete(t, &fakeBackend{names: fullRepository()}, spec)
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
			if len(sup.calls) != 0 {
				t.Errorf("calls for an invalid spec: %v", sup.verbs())
			}
		})
	}
}

func TestDeleteDataCancellation(t *testing.T) {
	t.Parallel()
	b := &fakeBackend{names: fullRepository(), keys: 1}
	b.t = t
	sup := &scriptSupervisor{}
	sup.handle = func(c call) reply {
		if c.tool == "rclone" && c.verb == "delete" && strings.HasSuffix(c.args[len(c.args)-1], "/index") {
			return reply{err: &proc.CanceledError{Cause: context.Canceled}}
		}
		return b.handle(c)
	}
	te := newTestEnv(t, func(c *Config) { c.Supervisor = sup })
	res, err := te.runners[kindDeleteData].Run(t.Context(), runnerJob(deleteSpec(true)), func(runner.Progress) {})
	if !runner.Canceled(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if res.Stats.Deletes != 10 {
		t.Errorf("Deletes = %d, want the objects deleted before the cancellation", res.Stats.Deletes)
	}
	if got := deleted(sup); got[len(got)-1] != "delete /index" {
		t.Errorf("deletions went on after the cancellation: %q", got)
	}
}
