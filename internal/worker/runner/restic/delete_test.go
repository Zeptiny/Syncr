package restic

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

const expectedResticID = "2d1912a057d761ffe3979dcdddfff925ef2ba21a67ea44b3e7b948a3cbaf4231"

// hexName returns a file name of 64 lower-case hex digits (restic's
// names), distinct per n.
func hexName(n int) string { return fmt.Sprintf("%064x", n) }

// fullRepository returns the paths of a repository as restic lays it out,
// relative to its location (folders end with "/"): one key, no lock, two
// snapshots, one index file, and three packs in two of data/'s
// sub-folders plus an empty one. Deleting it deletes 8 objects.
func fullRepository() []string {
	return []string{
		"config", "keys/" + hexName(1), "locks/", "snapshots/" + hexName(2), "snapshots/" + hexName(3), "index/" + hexName(4),
		"data/00/" + hexName(5), "data/00/" + hexName(6), "data/ab/" + hexName(7), "data/ff/",
	}
}

// fakeBackend is the repository location a deletion test runs against, as
// rclone sees it, and what restic answers. Deletions change it, so later
// listings see them.
type fakeBackend struct {
	t *testing.T
	// loc is the rclone path of the location ("bk:bucket/repo one" when
	// empty).
	loc string
	// entries are the paths below the location (true for folders); missing
	// means the location itself does not exist.
	entries map[string]bool
	missing bool
	// config is the reply of "cat config"; nil replies the golden config.
	config *reply
	locks  []string
	// fail makes "rclone delete" of that part fail.
	fail string
	// rmdirFails makes "rclone rmdir" of the location fail.
	rmdirFails bool
}

// newBackend returns a location holding paths (and their folders).
func newBackend(paths ...string) *fakeBackend {
	b := &fakeBackend{entries: map[string]bool{}}
	b.add(paths...)
	return b
}

func (b *fakeBackend) add(paths ...string) {
	for _, p := range paths {
		dir := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		b.entries[p] = dir
		for parent := filepath.ToSlash(filepath.Dir(p)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			b.entries[parent] = true
		}
	}
}

// paths returns what the location holds, sorted (folders end with "/").
func (b *fakeBackend) paths() []string {
	var out []string
	for p, dir := range b.entries {
		if dir {
			p += "/"
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// has reports whether the location holds p.
func (b *fakeBackend) has(p string) bool {
	_, ok := b.entries[strings.TrimSuffix(p, "/")]
	return ok
}

// children returns the paths below dir ("" is the location) relative to
// it, at most depth levels deep (0: any depth).
func (b *fakeBackend) children(dir string, depth int) []string {
	var out []string
	for p := range b.entries {
		rel := p
		if dir != "" {
			var ok bool
			if rel, ok = strings.CutPrefix(p, dir+"/"); !ok {
				continue
			}
		}
		if depth > 0 && strings.Count(rel, "/") >= depth {
			continue
		}
		out = append(out, rel)
	}
	slices.Sort(out)
	return out
}

// exists reports whether folder dir exists ("" is the location).
func (b *fakeBackend) exists(dir string) bool {
	if dir == "" {
		return !b.missing
	}
	return !b.missing && b.entries[dir]
}

var notFoundReply = reply{exit: 3, stderr: []string{
	"2026/01/02 03:04:05 ERROR : error listing: directory not found",
	"2026/01/02 03:04:05 NOTICE: Failed to lsjson with 2 errors: last error was: error in ListJSON: directory not found",
}}

// ruleRegexp translates an rclone filter rule the runner uses ("/" segments
// that are literal, "*", or a {{regexp}}) into a Go regular expression of
// the whole relative path.
func ruleRegexp(t *testing.T, rule string) *regexp.Regexp {
	t.Helper()
	var segs []string
	for seg := range strings.SplitSeq(strings.TrimPrefix(rule, "/"), "/") {
		switch {
		case strings.HasPrefix(seg, "{{") && strings.HasSuffix(seg, "}}"):
			segs = append(segs, "(?:"+seg[2:len(seg)-2]+")")
		case seg == "*":
			segs = append(segs, "[^/]*")
		default:
			segs = append(segs, regexp.QuoteMeta(seg))
		}
	}
	return regexp.MustCompile("^" + strings.Join(segs, "/") + "$")
}

// flags are the parsed flags of an rclone call.
type flags struct {
	recursive, filesOnly, leaveRoot bool
	depth                           int
	includes                        []string
}

func parseFlags(t *testing.T, args []string) flags {
	t.Helper()
	var f flags
	for _, a := range args {
		switch {
		case a == "-R":
			f.recursive = true
		case a == "--files-only":
			f.filesOnly = true
		case a == "--leave-root":
			f.leaveRoot = true
		case strings.HasPrefix(a, "--max-depth="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--max-depth="))
			if err != nil {
				t.Fatal(err)
			}
			f.depth = n
		case strings.HasPrefix(a, "--include="):
			f.includes = append(f.includes, strings.TrimPrefix(a, "--include="))
		}
	}
	if !f.recursive && f.depth == 0 {
		f.depth = 1
	}
	return f
}

func (f flags) included(t *testing.T, rel string) bool {
	if len(f.includes) == 0 {
		return true
	}
	for _, rule := range f.includes {
		if ruleRegexp(t, rule).MatchString(rel) {
			return true
		}
	}
	return false
}

// handle plays the backend.
func (b *fakeBackend) handle(c call) reply {
	target := c.args[len(c.args)-1]
	loc := b.loc
	if loc == "" {
		loc = "bk:bucket/repo one"
	}
	dir := strings.Trim(filepath.ToSlash(strings.TrimPrefix(target, loc)), "/")
	f := parseFlags(b.t, c.args)
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
		if !b.exists(dir) {
			return notFoundReply
		}
		var lines []string
		for _, rel := range b.children(dir, f.depth) {
			full := strings.TrimPrefix(dir+"/"+rel, "/")
			if f.filesOnly && b.entries[full] || !b.entries[full] && !f.included(b.t, rel) {
				continue
			}
			lines = append(lines, fmt.Sprintf(`{"Path":%q,"Name":%q,"Size":1,"ModTime":"","IsDir":%v}`, rel, filepath.Base(rel), b.entries[full]))
		}
		for i := range len(lines) - 1 {
			lines[i] += ","
		}
		return reply{stdout: append(append([]string{"["}, lines...), "]")}
	case "rclone delete":
		if strings.HasSuffix(dir, b.fail) && b.fail != "" {
			return reply{exit: 1, stderr: []string{`{"level":"error","msg":"permission denied with key ` + testSecret + `"}`}}
		}
		if !b.exists(dir) {
			return reply{exit: 3, stderr: []string{"2026/01/02 03:04:05 ERROR : error listing: directory not found"}}
		}
		n := 0
		for _, rel := range b.children(dir, f.depth) {
			if full := dir + "/" + rel; !b.entries[full] && f.included(b.t, rel) {
				delete(b.entries, full)
				n++
			}
		}
		stats := strings.Replace(fixture(b.t, "delete_rclone_stats.out")[0], `"deletes":5`, fmt.Sprintf(`"deletes":%d`, n), 1)
		return reply{stderr: []string{stats}}
	case "rclone rmdirs":
		// What the runner asks for: data/'s sub-folders named by 2 hex
		// digits that hold no file (one holding only a folder looks empty
		// to rclone, and the backend refuses to remove it).
		if !f.leaveRoot || !slices.Equal(f.includes, []string{packDirContentRule}) || f.depth != 2 {
			b.t.Errorf("unexpected rmdirs %q", c.args)
		}
		if !b.exists(dir) {
			return reply{exit: 3, stderr: []string{"Failed to rmdirs: directory not found"}}
		}
		failed := false
		for _, sub := range b.children(dir, 1) {
			full := dir + "/" + sub
			if !b.entries[full] || !isPackDir(sub) {
				continue
			}
			switch inside := b.children(full, 0); {
			case len(inside) == 0:
				delete(b.entries, full)
			case !slices.ContainsFunc(inside, func(rel string) bool { return !b.entries[full+"/"+rel] }):
				failed = true // only folders inside: rmdir refuses
			}
		}
		if failed {
			return reply{exit: 1, stderr: []string{"Failed to rmdir: directory not empty"}}
		}
		return reply{}
	case "rclone rmdir":
		if dir == "" && b.rmdirFails || len(b.children(dir, 0)) > 0 {
			return reply{exit: 1, stderr: []string{"Failed to rmdir: directory not empty"}}
		}
		if dir == "" {
			b.missing = true
		} else {
			delete(b.entries, dir)
		}
		return reply{}
	case "rclone deletefile":
		delete(b.entries, dir)
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

// resumed marks spec as the retry of an attempt that may have deleted
// files.
func resumed(spec *syncrv1.JobSpec) *syncrv1.JobSpec {
	spec.GetResticDeleteData().Resume = true
	return spec
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
		if c.tool == "rclone" && (c.verb == "delete" || c.verb == "deletefile" || c.verb == "rmdir" || c.verb == "rmdirs") {
			out = append(out, c.verb+" "+strings.TrimPrefix(c.args[len(c.args)-1], "bk:bucket/repo one"))
		}
	}
	return out
}

func TestDeleteDataDeletesThePartsInOrder(t *testing.T) {
	t.Parallel()
	b := newBackend(append(fullRepository(), "README.txt")...)
	res, sup, snaps, err := runDelete(t, b, deleteSpec(true))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"delete /locks", "rmdir /locks", "delete /snapshots", "rmdir /snapshots", "delete /index", "rmdir /index",
		"delete /data", "rmdirs /data", "rmdir /data", "delete /keys", "rmdir /keys", "deletefile /config",
	}
	if got := deleted(sup); !slices.Equal(got, want) {
		t.Errorf("deletions =\n  %q\nwant\n  %q", got, want)
	}
	// Verification and the lock check come before anything is deleted.
	if got := sup.verbs()[:3]; !slices.Equal(got, []string{"restic cat", "restic unlock", "restic list"}) {
		t.Errorf("first calls = %v", got)
	}
	out := res.Output.GetResticDeleteData()
	if out.GetDeletedObjects() != 8 || out.GetDirectoryRemoved() || out.GetAlreadyGone() ||
		!slices.Equal(out.GetRemovedParts(), []string{"locks", "snapshots", "index", "data", "keys", "config"}) {
		t.Errorf("output = %v", out)
	}
	if res.Stats.Deletes != 8 || res.ExitCode != 0 || len(res.Warnings) != 0 {
		t.Errorf("result = %+v", res)
	}
	if len(snaps) == 0 || !slices.IsSortedFunc(snaps, func(a, b runner.Progress) int { return int(a.Deletes - b.Deletes) }) {
		t.Errorf("progress = %+v, want growing Deletes", snaps)
	}
	if got := b.paths(); !slices.Equal(got, []string{"README.txt"}) {
		t.Errorf("left at the location: %v", got)
	}
}

func TestDeleteDataRcloneInvocation(t *testing.T) {
	t.Parallel()
	b := newBackend(fullRepository()...)
	_, sup, _, err := runDelete(t, b, deleteSpec(false))
	if err != nil {
		t.Fatal(err)
	}
	var deletes [][]string
	for _, c := range sup.calls {
		if c.tool != "rclone" {
			continue
		}
		// No rclone.conf, hard deletes, and the target after "--".
		if !slices.Equal(c.args[:3], []string{"--config=", "--b2-hard-delete", "--drive-use-trash=false"}) || c.args[len(c.args)-2] != "--" {
			t.Errorf("rclone args = %q", c.args)
		}
		// Nothing sweeps a folder's empty sub-folders.
		if slices.Contains(c.args, "--rmdirs") || c.verb == "purge" {
			t.Errorf("rclone args = %q", c.args)
		}
		if c.env["RCLONE_CONFIG_BK_SECRET_ACCESS_KEY"] != testSecret || c.env["RCLONE_CONFIG_BK_TYPE"] != "s3" {
			t.Errorf("rclone env lacks the remote: %v", keys(c.env))
		}
		if _, ok := c.env["RESTIC_PASSWORD"]; ok {
			t.Error("rclone was given the repository password")
		}
		// The job's directory is the temp directory; the working
		// directory is the scratch root, which no process can pin.
		if filepath.Dir(c.env["TMPDIR"]) != c.dir || filepath.Base(c.env["TMPDIR"]) != "restic-job-1" {
			t.Errorf("TMPDIR = %q, dir = %q", c.env["TMPDIR"], c.dir)
		}
		if c.verb == "delete" {
			deletes = append(deletes, c.args[3:])
		}
	}
	// Only files named like restic's: by 64 hex digits, in data/ inside
	// sub-folders named by 2 hex digits.
	tail := []string{"--use-json-log", "--stats=1s", "--stats-log-level=NOTICE", "--"}
	want := [][]string{
		append(append([]string{"delete", "--include=/{{[0-9a-f]{64}}}", "--max-depth=1"}, tail...), "bk:bucket/repo one/locks"),
		append(append([]string{"delete", "--include=/{{[0-9a-f]{64}}}", "--max-depth=1"}, tail...), "bk:bucket/repo one/snapshots"),
		append(append([]string{"delete", "--include=/{{[0-9a-f]{64}}}", "--max-depth=1"}, tail...), "bk:bucket/repo one/index"),
		append(append([]string{"delete", "--include=/{{[0-9a-f]{2}}}/{{[0-9a-f]{64}}}", "--max-depth=2"}, tail...), "bk:bucket/repo one/data"),
		append(append([]string{"delete", "--include=/{{[0-9a-f]{64}}}", "--max-depth=1"}, tail...), "bk:bucket/repo one/keys"),
	}
	if !slices.EqualFunc(deletes, want, slices.Equal) {
		t.Errorf("delete args =\n  %q\nwant\n  %q", deletes, want)
	}
}

// Only restic's files go: other files in restic's folders stay, and so does
// another repository stored inside one of them, and the warning names what
// was kept.
func TestDeleteDataKeepsForeignEntries(t *testing.T) {
	t.Parallel()
	foreign := []string{
		"README.txt",
		"keys/notes.txt", "keys/" + hexName(9) + ".bak", "keys/old/",
		"snapshots/" + strings.ToUpper(hexName(10)),
		"data/" + hexName(11), "data/zz/" + hexName(12), "data/ab/readme", "data/cd/sub/",
		// A repository nested in data/.
		"data/nested/config", "data/nested/keys/" + hexName(13), "data/nested/data/00/" + hexName(14), "data/nested/data/01/",
	}
	b := newBackend(append(fullRepository(), foreign...)...)
	res, _, _, err := runDelete(t, b, deleteSpec(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range foreign {
		if !b.has(p) {
			t.Errorf("%s was deleted", p)
		}
	}
	for _, p := range fullRepository() {
		if b.has(p) && !strings.HasSuffix(p, "/") {
			t.Errorf("restic's %s survived", p)
		}
	}
	// The emptied folders are removed; the ones still holding something stay.
	for p, want := range map[string]bool{
		"locks": false, "index": false, "data/00": false, "data/ff": false, "data/ab": true, "data/cd": true, "data": true, "keys": true,
		"snapshots": true,
	} {
		if b.has(p) != want {
			t.Errorf("folder %s exists = %v, want %v", p, b.has(p), want)
		}
	}
	out := res.Output.GetResticDeleteData()
	if out.GetDirectoryRemoved() || out.GetDeletedObjects() != 8 {
		t.Errorf("output = %v", out)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %q, want one naming what was kept", res.Warnings)
	}
	w := res.Warnings[0]
	for _, name := range []string{"kept 9 entries", "data/ab/readme", "data/cd/sub/", "data/nested/", "data/zz/", "and 1 more"} {
		if !strings.Contains(w, name) {
			t.Errorf("warning %q does not say %q", w, name)
		}
	}
	if strings.Contains(w, "README.txt") || strings.Contains(w, "data/ab/,") || strings.Contains(w, "nested/config") {
		t.Errorf("warning %q names an entry outside restic's folders, a folder of restic's, or one inside a kept folder", w)
	}
}

func TestDeleteDataRemovesAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		paths     []string
		removeDir bool
		rmdirFail bool
		wantRmdir bool
		want      bool
	}{
		{"own folder", fullRepository(), true, false, true, true},
		{"other files remain", append(fullRepository(), "notes.txt"), true, false, false, false},
		{"other files remain in a part", append(fullRepository(), "keys/notes.txt"), true, false, false, false},
		{"not asked to", fullRepository(), false, false, false, false},
		{"rmdir fails", fullRepository(), true, true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBackend(tc.paths...)
			b.rmdirFails = tc.rmdirFail
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
	b := newBackend(fullRepository()...)
	b.loc = "bk:"
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
	if got := b.paths(); len(got) != 0 {
		t.Errorf("left at the root: %v", got)
	}
}

func TestDeleteDataVerification(t *testing.T) {
	t.Parallel()
	other := strings.ReplaceAll(strings.Join(fixture(t, "cat_config.out"), "\n"), expectedResticID, strings.Repeat("ab", 32))
	wrongPassword := exitError(12, "Fatal: wrong password or no key found")
	noRepository := exitError(10, "Fatal: repository does not exist")
	with := func(b *fakeBackend, config *reply) *fakeBackend {
		b.config = config
		return b
	}
	missing := with(newBackend(), &noRepository)
	missing.missing = true
	tests := []struct {
		name   string
		b      *fakeBackend
		resume bool
		// want is the failure code; UNSPECIFIED means success.
		want        syncrv1.JobErrorCode
		wantDeleted []string
		check       func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, err error)
	}{
		{
			name: "another repository", b: with(newBackend(fullRepository()...), &reply{stdout: strings.Split(other, "\n")}),
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_MISMATCH,
			check: func(t *testing.T, _ *syncrv1.ResticDeleteDataOutput, err error) {
				if !strings.Contains(err.Error(), "nothing was deleted") {
					t.Errorf("message = %q", err)
				}
			},
		},
		{
			name: "wrong password with keys", b: with(newBackend(fullRepository()...), &wrongPassword), resume: true,
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
		},
		{
			name: "resume: only the config is left to delete", b: with(newBackend("config", "notes.txt"), &wrongPassword), resume: true,
			wantDeleted: []string{"deletefile /config"},
			check: func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, _ error) {
				if !slices.Equal(out.GetRemovedParts(), []string{"config"}) || out.GetDeletedObjects() != 1 {
					t.Errorf("output = %v", out)
				}
			},
		},
		{
			name: "resume: keys/ holds no key, only something else", b: with(newBackend("config", "keys/notes.txt"), &wrongPassword), resume: true,
			wantDeleted: []string{"deletefile /config"},
		},
		{
			// A first attempt cannot tell this from a config it may not
			// delete: no key, so nothing verifies it.
			name: "a config without keys on a first attempt", b: with(newBackend("config", "keys/"), &wrongPassword),
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
		},
		{
			name: "wrong password without a config", b: with(newBackend("data/00/"+hexName(1)), &wrongPassword), resume: true,
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
		},
		{
			name: "resume: already gone", b: with(newBackend("notes.txt", "data/00/", "keys/notes.txt"), &noRepository), resume: true,
			check: func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, _ error) {
				if !out.GetAlreadyGone() || out.GetDeletedObjects() != 0 {
					t.Errorf("output = %v", out)
				}
			},
		},
		{
			name: "resume: already gone, no location", b: missing, resume: true,
			check: func(t *testing.T, out *syncrv1.ResticDeleteDataOutput, _ error) {
				if !out.GetAlreadyGone() {
					t.Errorf("output = %v", out)
				}
			},
		},
		{
			// The wrong worker, or remote settings that changed: the data
			// is elsewhere, and the repository must not be forgotten.
			name: "nothing found on a first attempt", b: with(newBackend("notes.txt"), &noRepository),
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND,
			check: func(t *testing.T, _ *syncrv1.ResticDeleteDataOutput, err error) {
				for _, s := range []string{"no restic repository was found at the location on this worker", "nothing was deleted", "check the worker and the remote's settings", "without deleting its data"} {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("message = %q, want it to say %q", err, s)
					}
				}
			},
		},
		{
			name: "config missing but data remains", b: with(newBackend("data/00/"+hexName(1), "index/"+hexName(2), "keys/notes.txt"), &noRepository), resume: true,
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND,
			check: func(t *testing.T, _ *syncrv1.ResticDeleteDataOutput, err error) {
				if !strings.Contains(err.Error(), "(index, data)") {
					t.Errorf("message = %q, want the parts holding restic's files", err)
				}
			},
		},
		{
			name: "locked by another client", b: func() *fakeBackend {
				b := newBackend(fullRepository()...)
				b.locks = fixture(t, "list_locks.out")
				return b
			}(),
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED,
		},
		{
			name: "restic cannot run", b: with(newBackend(fullRepository()...), &reply{exit: 1, stderr: []string{"boom"}}),
			want: syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := tc.b.paths()
			spec := deleteSpec(false)
			if tc.resume {
				spec = resumed(spec)
			}
			res, sup, _, err := runDelete(t, tc.b, spec)
			if tc.want == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				mustFail(t, err, tc.want)
				if res.Output != nil {
					t.Errorf("a failed run has output %v", res.Output)
				}
				if got := tc.b.paths(); !slices.Equal(got, before) {
					t.Errorf("a failed verification changed the location: %v, was %v", got, before)
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
	b := newBackend("config", "keys/"+hexName(1), "data/00/"+hexName(2))
	res, sup, _, err := runDelete(t, b, deleteSpec(false))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deleted(sup), []string{"delete /data", "rmdirs /data", "rmdir /data", "delete /keys", "rmdir /keys", "deletefile /config"}; !slices.Equal(got, want) {
		t.Errorf("deletions = %q, want %q", got, want)
	}
	if got := res.Output.GetResticDeleteData().GetRemovedParts(); !slices.Equal(got, []string{"data", "keys", "config"}) {
		t.Errorf("removed_parts = %v", got)
	}
}

func TestDeleteDataFailurePartWayNamesWhatIsGone(t *testing.T) {
	t.Parallel()
	b := newBackend(fullRepository()...)
	b.fail = "data"
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
	if res.Stats.Deletes != 3 {
		t.Errorf("Deletes = %d, want the 3 objects of the three parts", res.Stats.Deletes)
	}
}

func TestDeleteDataLocalRepository(t *testing.T) {
	t.Parallel()
	b := newBackend(fullRepository()...)
	b.t = t
	sup := &scriptSupervisor{handle: b.handle}
	te := newTestEnv(t, func(c *Config) { c.Supervisor = sup })
	repo := te.local("repos", "one")
	b.loc = repo
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
			_, sup, _, err := runDelete(t, newBackend(fullRepository()...), spec)
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
			if len(sup.calls) != 0 {
				t.Errorf("calls for an invalid spec: %v", sup.verbs())
			}
		})
	}
}

func TestDeleteDataCancellation(t *testing.T) {
	t.Parallel()
	b := newBackend(fullRepository()...)
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
	if res.Stats.Deletes != 2 {
		t.Errorf("Deletes = %d, want the objects deleted before the cancellation", res.Stats.Deletes)
	}
	if got := deleted(sup); got[len(got)-1] != "delete /index" {
		t.Errorf("deletions went on after the cancellation: %q", got)
	}
}

func TestForeignEntries(t *testing.T) {
	t.Parallel()
	l := listing{exists: true, entries: []entry{
		{"00", true}, {"zz", true}, {"ab", true}, {"ab/readme", false}, {"zz/x", false}, {hexName(1), false}, {"cd", false},
	}}
	if got, want := foreign("data", l), []string{"data/" + hexName(1), "data/ab/readme", "data/cd", "data/zz/"}; !slices.Equal(got, want) {
		t.Errorf("foreign(data) = %q, want %q", got, want)
	}
	// Outside data/ a folder named by two hex digits is not restic's.
	flat := listing{exists: true, entries: []entry{{"ab", true}, {"notes.txt", false}}}
	if got, want := foreign("keys", flat), []string{"keys/ab/", "keys/notes.txt"}; !slices.Equal(got, want) {
		t.Errorf("foreign(keys) = %q, want %q", got, want)
	}
	for name, want := range map[string]bool{"00": true, "ff": true, "a0": true, "AB": false, "0": false, "abc": false, "g0": false} {
		if got := isPackDir(name); got != want {
			t.Errorf("isPackDir(%q) = %v, want %v", name, got, want)
		}
	}
}
