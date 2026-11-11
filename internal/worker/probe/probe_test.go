package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// fakeTools implements Restic, Rclone and HostKeys. Each call records its
// op and bound, waits for delay (or the context) and answers from the
// fields.
type fakeTools struct {
	delay time.Duration
	err   error
	// entries and snapshots size the answers.
	entries, snapshots int
	entryName          string

	// mu guards calls and bound: concurrent probes share one fake.
	mu    sync.Mutex
	calls []string
	// bound is the max_entries or max_snapshots the last call was given.
	bound int
}

func (f *fakeTools) wait(ctx context.Context, op string, bound int) error {
	f.mu.Lock()
	f.calls = append(f.calls, op)
	f.bound = bound
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(f.delay):
		}
	}
	return f.err
}

func (f *fakeTools) listing(path string) *syncrv1.ProbeListing {
	l := &syncrv1.ProbeListing{Path: path}
	name := f.entryName
	if name == "" {
		name = "file"
	}
	for i := range f.entries {
		l.Entries = append(l.Entries, &syncrv1.ProbeEntry{Name: fmt.Sprintf("%s-%05d", name, i), Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE})
	}
	return l
}

func (f *fakeTools) Snapshots(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticSnapshots) (*syncrv1.ResticSnapshotsOutput, error) {
	if err := f.wait(ctx, "restic_snapshots", int(op.GetMaxSnapshots())); err != nil {
		return nil, err
	}
	out := &syncrv1.ResticSnapshotsOutput{}
	for i := range f.snapshots {
		out.Snapshots = append(out.Snapshots, &syncrv1.ResticSnapshot{
			Id: fmt.Sprintf("%064d", i), Time: timestamppb.New(time.Unix(int64(i), 0)), Paths: []string{strings.Repeat("p", 100)},
		})
	}
	return out, nil
}

func (f *fakeTools) Ls(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticLs) (*syncrv1.ProbeListing, error) {
	if err := f.wait(ctx, "restic_ls", int(op.GetMaxEntries())); err != nil {
		return nil, err
	}
	return f.listing(op.GetPath()), nil
}

func (f *fakeTools) Init(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticInit) (*syncrv1.ResticRepositoryConfig, error) {
	if err := f.wait(ctx, "restic_init", 0); err != nil {
		return nil, err
	}
	return &syncrv1.ResticRepositoryConfig{Id: strings.Repeat("ab", 32), Version: 2}, nil
}

func (f *fakeTools) Config(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticConfig) (*syncrv1.ResticRepositoryConfig, error) {
	if err := f.wait(ctx, "restic_config", 0); err != nil {
		return nil, err
	}
	return &syncrv1.ResticRepositoryConfig{Id: strings.Repeat("cd", 32), Version: 2}, nil
}

func (f *fakeTools) List(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, op *syncrv1.ProbeListRemote) (*syncrv1.ProbeListing, error) {
	if err := f.wait(ctx, "list_remote", int(op.GetMaxEntries())); err != nil {
		return nil, err
	}
	return f.listing(op.GetLocation().GetPath()), nil
}

func (f *fakeTools) About(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeAbout) (*syncrv1.ProbeUsage, error) {
	if err := f.wait(ctx, "about", 0); err != nil {
		return nil, err
	}
	return &syncrv1.ProbeUsage{Total: proto.Int64(100), Free: proto.Int64(40)}, nil
}

func (f *fakeTools) HostKeys(ctx context.Context, host string, port int) ([]*syncrv1.SftpHostKey, error) {
	if err := f.wait(ctx, "sftp_host_keys", port); err != nil {
		return nil, err
	}
	return []*syncrv1.SftpHostKey{
		{Algorithm: "ssh-ed25519", Key: "AAAA" + host, FingerprintSha256: "SHA256:x"},
		{Algorithm: "ecdsa-sha2-nistp256", Key: "AAAB" + host, FingerprintSha256: "SHA256:y"},
	}, nil
}

// fakeLocal implements Local.
type fakeLocal struct {
	listing localfs.Listing
	err     error
	limit   int
	dirs    bool
}

func (f *fakeLocal) List(_ context.Context, path string, limit int, dirsOnly bool) (localfs.Listing, error) {
	f.limit, f.dirs = limit, dirsOnly
	l := f.listing
	l.Path = path
	return l, f.err
}

func newExecutor(t *testing.T, tools *fakeTools, local Local, mutate ...func(*Config)) *Executor {
	t.Helper()
	cfg := Config{Local: local}
	if tools != nil {
		cfg.Restic, cfg.Rclone, cfg.HostKeys = tools, tools, tools
	}
	for _, m := range mutate {
		m(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func specOf(op string, limit int32) *syncrv1.ProbeSpec {
	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "r1", Path: "repo"}, Password: "pw"}
	loc := &syncrv1.Location{Remote: "r1", Path: "bucket"}
	s := &syncrv1.ProbeSpec{}
	switch op {
	case "list_local":
		s.Op = &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: "/srv", MaxEntries: limit, DirsOnly: true}}
	case "list_remote":
		s.Op = &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{Location: loc, MaxEntries: limit}}
	case "about":
		s.Op = &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{Location: loc}}
	case "restic_snapshots":
		s.Op = &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{Repository: repo, MaxSnapshots: limit}}
	case "restic_ls":
		s.Op = &syncrv1.ProbeSpec_ResticLs{ResticLs: &syncrv1.ProbeResticLs{Repository: repo, SnapshotId: "latest", Path: "/", MaxEntries: limit}}
	case "restic_init":
		s.Op = &syncrv1.ProbeSpec_ResticInit{ResticInit: &syncrv1.ProbeResticInit{Repository: repo}}
	case "restic_config":
		s.Op = &syncrv1.ProbeSpec_ResticConfig{ResticConfig: &syncrv1.ProbeResticConfig{Repository: repo}}
	case "sftp_host_keys":
		s.Op = &syncrv1.ProbeSpec_SftpHostKeys{SftpHostKeys: &syncrv1.ProbeSftpHostKeys{Host: "sftp.example.com", Port: 22}}
	}
	return s
}

func probeOf(op string, limit int32) *syncrv1.RunProbe {
	return &syncrv1.RunProbe{ProbeId: "p-" + op, Spec: specOf(op, limit)}
}

func mustFail(t *testing.T, res *syncrv1.ProbeResult, code syncrv1.JobErrorCode) {
	t.Helper()
	if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_FAILED || res.GetError().GetCode() != code || res.GetOutput() != nil {
		t.Fatalf("result = %v, want FAILED with %v and no output", res, code)
	}
}

func TestEveryOpIsDispatched(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{entries: 2, snapshots: 2}
	local := &fakeLocal{listing: localfs.Listing{Entries: []localfs.Entry{{Name: "a", Path: "/srv/a", Dir: true}}}}
	e := newExecutor(t, tools, local)
	if !slices.Equal(e.Ops(), link.ProbeOps()) {
		t.Errorf("Ops() = %v, want every op of the contract %v", e.Ops(), link.ProbeOps())
	}
	for _, op := range link.ProbeOps() {
		res := e.Probe(t.Context(), probeOf(op, 0))
		if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || res.GetProbeId() != "p-"+op || res.GetError() != nil || res.GetElapsed() == nil {
			t.Errorf("%s: result = %v", op, res)
			continue
		}
		var ok bool
		switch op {
		case "list_local":
			ok = res.GetListing().GetPath() == "/srv" && len(res.GetListing().GetEntries()) == 1
		case "list_remote":
			ok = res.GetListing().GetPath() == "bucket" && len(res.GetListing().GetEntries()) == 2
		case "restic_ls":
			ok = res.GetListing().GetPath() == "/" && len(res.GetListing().GetEntries()) == 2
		case "about":
			ok = res.GetUsage().GetTotal() == 100
		case "restic_snapshots":
			ok = len(res.GetSnapshots().GetSnapshots()) == 2
		case "restic_init":
			ok = strings.HasPrefix(res.GetRepositoryConfig().GetId(), "abab")
		case "restic_config":
			ok = strings.HasPrefix(res.GetRepositoryConfig().GetId(), "cdcd")
		case "sftp_host_keys":
			keys := res.GetSftpHostKeys().GetKeys()
			ok = len(keys) == 2 && keys[0].GetKey() == "AAAAsftp.example.com" && tools.bound == 22
		}
		if !ok {
			t.Errorf("%s: output = %v", op, res.GetOutput())
		}
	}
	if !local.dirs {
		t.Error("dirs_only was not passed to the local listing")
	}
}

func TestOpsFollowTheAvailableTools(t *testing.T) {
	t.Parallel()
	tools := &fakeTools{}
	hostKeys := &fakeTools{}
	// sftp_host_keys needs no tool: it is always served.
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"nothing", Config{}, []string{"sftp_host_keys"}},
		{"local only", Config{Local: &fakeLocal{}}, []string{"list_local", "sftp_host_keys"}},
		{"rclone only", Config{Rclone: tools}, []string{"list_remote", "about", "sftp_host_keys"}},
		{"restic only", Config{Restic: tools}, []string{"restic_snapshots", "restic_ls", "restic_init", "restic_config", "sftp_host_keys"}},
	}
	for _, tc := range tests {
		tc.cfg.HostKeys = hostKeys
		e, err := New(tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Ops(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: Ops() = %v, want %v", tc.name, got, tc.want)
		}
		for _, op := range link.ProbeOps() {
			if slices.Contains(tc.want, op) {
				continue
			}
			mustFail(t, e.Probe(t.Context(), probeOf(op, 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND)
		}
	}
	if len(tools.calls) != 0 {
		t.Errorf("unsupported ops reached a tool: %v", tools.calls)
	}
}

func TestProbeWithoutAnOperation(t *testing.T) {
	t.Parallel()
	e := newExecutor(t, &fakeTools{}, &fakeLocal{})
	mustFail(t, e.Probe(t.Context(), &syncrv1.RunProbe{ProbeId: "x", Spec: &syncrv1.ProbeSpec{}}), syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
	mustFail(t, e.Probe(t.Context(), &syncrv1.RunProbe{ProbeId: "x"}), syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
}

func TestListingBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		op        string
		requested int32
		want      int
	}{
		{"list_remote", 0, 100}, // the default (2000), capped by MaxEntries
		{"list_remote", 10, 10},
		{"list_remote", 1 << 20, 100},
		{"list_remote", -5, 100},
		{"restic_ls", 0, 100},
		{"restic_ls", 99, 99},
		{"restic_ls", 101, 100},
		{"restic_snapshots", 0, 50},
		{"restic_snapshots", 7, 7},
		{"restic_snapshots", 51, 50},
	}
	for _, tc := range tests {
		tools := &fakeTools{}
		e := newExecutor(t, tools, nil, func(c *Config) { c.MaxEntries, c.MaxSnapshots = 100, 50 })
		e.Probe(t.Context(), probeOf(tc.op, tc.requested))
		if tools.bound != tc.want {
			t.Errorf("%s with max %d: the tool was given %d, want %d", tc.op, tc.requested, tools.bound, tc.want)
		}
	}
	// Without a request bound the default applies when the cap allows it.
	tools := &fakeTools{}
	newExecutor(t, tools, nil).Probe(t.Context(), probeOf("list_remote", 0))
	if tools.bound != DefaultEntries {
		t.Errorf("default bound = %d, want %d", tools.bound, DefaultEntries)
	}
	local := &fakeLocal{}
	e := newExecutor(t, nil, local, func(c *Config) { c.MaxEntries = 100 })
	e.Probe(t.Context(), probeOf("list_local", 5000))
	if local.limit != 100 {
		t.Errorf("local listing limit = %d, want 100", local.limit)
	}
	// The request is not changed: the bound is applied to a copy.
	p := probeOf("restic_ls", 5000)
	e = newExecutor(t, &fakeTools{}, nil, func(c *Config) { c.MaxEntries = 100 })
	e.Probe(t.Context(), p)
	if p.GetSpec().GetResticLs().GetMaxEntries() != 5000 {
		t.Error("the executor changed the caller's request")
	}
}

func TestResultIsTrimmedToFit(t *testing.T) {
	t.Parallel()
	const limit = 16 << 10
	tools := &fakeTools{entries: 3000, snapshots: 500, entryName: strings.Repeat("n", 50)}
	e := newExecutor(t, tools, nil, func(c *Config) { c.MaxResultBytes = limit })

	res := e.Probe(t.Context(), probeOf("list_remote", 0))
	l := res.GetListing()
	if proto.Size(res) > limit || !l.GetTruncated() || len(l.GetEntries()) == 0 || len(l.GetEntries()) >= 3000 {
		t.Errorf("listing: %d bytes, %d entries, truncated=%v", proto.Size(res), len(l.GetEntries()), l.GetTruncated())
	}
	if !strings.HasSuffix(l.GetEntries()[0].GetName(), "-00000") {
		t.Errorf("the first entries were not kept: %q", l.GetEntries()[0].GetName())
	}

	res = e.Probe(t.Context(), probeOf("restic_snapshots", 0))
	s := res.GetSnapshots()
	if proto.Size(res) > limit || !s.GetTruncated() || len(s.GetSnapshots()) == 0 || len(s.GetSnapshots()) >= 500 {
		t.Errorf("snapshots: %d bytes, %d snapshots, truncated=%v", proto.Size(res), len(s.GetSnapshots()), s.GetTruncated())
	}
	// Snapshots come oldest first: the newest are kept.
	if last := s.GetSnapshots()[len(s.GetSnapshots())-1]; last.GetTime().GetSeconds() != 499 {
		t.Errorf("the newest snapshot was dropped: last is %v", last.GetTime().AsTime())
	}

	// A result that fits is left alone.
	tools.entries = 3
	res = e.Probe(t.Context(), probeOf("list_remote", 0))
	if len(res.GetListing().GetEntries()) != 3 || res.GetListing().GetTruncated() {
		t.Errorf("small listing = %v", res.GetListing())
	}
}

func TestErrorCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want syncrv1.JobErrorCode
	}{
		{"runner code", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, "wrong password"), syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD},
		{"wrapped runner code", fmt.Errorf("probe: %w", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS, "exists")), syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS},
		{"outside the roots", &localfs.PathError{Path: "/etc", Err: localfs.ErrNotAllowed}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"invalid path", &localfs.PathError{Path: "x", Err: localfs.ErrInvalidPath}, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC},
		{"missing", &fs.PathError{Op: "open", Path: "/srv/x", Err: fs.ErrNotExist}, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND},
		{"permission", &fs.PathError{Op: "open", Path: "/srv/x", Err: fs.ErrPermission}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"unresolvable", &localfs.PathError{Path: "/srv/x", Err: localfs.ErrUnresolvable}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"anything else", errors.New("boom"), syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL},
	}
	for _, tc := range tests {
		e := newExecutor(t, nil, &fakeLocal{err: tc.err})
		res := e.Probe(t.Context(), probeOf("list_local", 0))
		mustFail(t, res, tc.want)
		if res.GetError().GetMessage() == "" {
			t.Errorf("%s: no message", tc.name)
		}
	}
}

func TestMessageIsClipped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 2000) + "\xff"
	e := newExecutor(t, nil, &fakeLocal{err: errors.New(long)})
	msg := e.Probe(t.Context(), probeOf("list_local", 0)).GetError().GetMessage()
	if len(msg) > MaxMessageBytes || !strings.HasSuffix(msg, "...") || !strings.HasPrefix(msg, "éé") || strings.ContainsRune(msg, '�') {
		t.Errorf("message of %d bytes: %.40q...", len(msg), msg)
	}
}

func TestTimeoutCancelAndBusy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		tools := &fakeTools{delay: time.Hour}
		e := newExecutor(t, tools, nil, func(c *Config) { c.MaxConcurrent = 2 })
		start := time.Now()

		// The default timeout.
		mustFail(t, e.Probe(t.Context(), probeOf("about", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
		if got := time.Since(start); got != DefaultTimeout {
			t.Errorf("timed out after %v, want %v", got, DefaultTimeout)
		}
		// The request's timeout, and the cap on it.
		for _, tc := range []struct{ ask, want time.Duration }{{5 * time.Second, 5 * time.Second}, {time.Hour, link.MaxProbeTimeout}} {
			start = time.Now()
			p := probeOf("about", 0)
			p.Timeout = durationpb.New(tc.ask)
			res := e.Probe(t.Context(), p)
			mustFail(t, res, syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
			if got := time.Since(start); got != tc.want || res.GetElapsed().AsDuration() != tc.want {
				t.Errorf("asked %v: timed out after %v (elapsed %v), want %v", tc.ask, got, res.GetElapsed().AsDuration(), tc.want)
			}
		}

		// Cancellation by the caller is not a failure.
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(time.Second)
			cancel()
		}()
		res := e.Probe(ctx, probeOf("about", 0))
		if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED || res.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED {
			t.Errorf("cancelled result = %v", res)
		}

		// With every slot taken, one more is refused at once.
		done := make(chan *syncrv1.ProbeResult, 2)
		for range 2 {
			go func() { done <- e.Probe(t.Context(), probeOf("about", 0)) }()
		}
		synctest.Wait()
		start = time.Now()
		mustFail(t, e.Probe(t.Context(), probeOf("about", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY)
		if time.Since(start) != 0 {
			t.Error("a refused probe waited")
		}
		<-done
		<-done
		// The slots are free again.
		tools.delay = 0
		if res := e.Probe(t.Context(), probeOf("about", 0)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("after the slots freed: %v", res)
		}
	})
}

// panicLocal panics in List.
type panicLocal struct{}

func (panicLocal) List(context.Context, string, int, bool) (localfs.Listing, error) {
	panic("boom with a secret")
}

func TestPanicBecomesAnInternalError(t *testing.T) {
	t.Parallel()
	e := newExecutor(t, nil, panicLocal{})
	res := e.Probe(t.Context(), probeOf("list_local", 0))
	mustFail(t, res, syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	if strings.Contains(res.GetError().GetMessage(), "secret") {
		t.Errorf("the panic value leaked: %q", res.GetError().GetMessage())
	}
	// The slot was released.
	e = newExecutor(t, nil, panicLocal{}, func(c *Config) { c.MaxConcurrent = 1 })
	e.Probe(t.Context(), probeOf("list_local", 0))
	mustFail(t, e.Probe(t.Context(), probeOf("list_local", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
}

func TestLocalListingConversion(t *testing.T) {
	t.Parallel()
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	local := &fakeLocal{listing: localfs.Listing{Truncated: true, Entries: []localfs.Entry{
		{Name: "dir", Path: "/srv/dir", Dir: true, Mode: fs.ModeDir | 0o755, ModTime: mod},
		{Name: "link-to-dir", Path: "/srv/link-to-dir", Dir: true, Symlink: true, Mode: fs.ModeSymlink | 0o777},
		{Name: "link", Path: "/srv/link", Symlink: true, Mode: fs.ModeSymlink | 0o777},
		{Name: "file", Path: "/srv/file", Size: 42, Mode: 0o644, ModTime: mod},
		{Name: "sock", Path: "/srv/sock", Mode: fs.ModeSocket | 0o600},
		{Name: "bad\xffname", Path: "/srv/bad\xffname", Mode: 0o600},
	}}}
	e := newExecutor(t, nil, local)
	l := e.Probe(t.Context(), probeOf("list_local", 0)).GetListing()
	if !l.GetTruncated() || len(l.GetEntries()) != 6 {
		t.Fatalf("listing = %v", l)
	}
	want := []syncrv1.ProbeEntryType{
		syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR,
		syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK, syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE,
		syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER, syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE,
	}
	for i, e := range l.GetEntries() {
		if e.GetType() != want[i] {
			t.Errorf("%s: type = %v, want %v", e.GetName(), e.GetType(), want[i])
		}
	}
	file := l.GetEntries()[3]
	if file.GetSize() != 42 || file.GetMode() != "-rw-r--r--" || !file.GetModTime().AsTime().Equal(mod) || file.GetPath() != "/srv/file" {
		t.Errorf("file = %v", file)
	}
	if l.GetEntries()[2].GetModTime() != nil {
		t.Error("an unknown modification time was sent")
	}
	if name := l.GetEntries()[5].GetName(); name != "bad�name" {
		t.Errorf("invalid UTF-8 name = %q", name)
	}
	// The result can be encoded.
	if _, err := proto.Marshal(l); err != nil {
		t.Errorf("marshal: %v", err)
	}
}

func TestNewRejectsNegativeLimits(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{{MaxConcurrent: -1}, {MaxEntries: -1}, {MaxSnapshots: -1}, {MaxResultBytes: -1}, {DefaultTimeout: -1}, {MaxTimeout: -1}, {MaxTimeout: link.MaxProbeTimeout + 1}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

// stuckLocal is a local listing blocked in a system call: it ignores its
// context and returns only when release is closed.
type stuckLocal struct {
	release chan struct{}

	mu      sync.Mutex
	running int
}

func (l *stuckLocal) List(context.Context, string, int, bool) (localfs.Listing, error) {
	l.mu.Lock()
	l.running++
	l.mu.Unlock()
	<-l.release
	l.mu.Lock()
	l.running--
	l.mu.Unlock()
	return localfs.Listing{}, nil
}

func (l *stuckLocal) inFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// TestStuckLocalListing: a local listing that cannot be interrupted does
// not hold its probe past the deadline or a cancel, and keeps its slot
// until it returns, so stuck listings never exceed the slots.
func TestStuckLocalListing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		local := &stuckLocal{release: make(chan struct{})}
		e := newExecutor(t, nil, local, func(c *Config) { c.MaxConcurrent = 2 })
		start := time.Now()
		p := probeOf("list_local", 0)
		p.Timeout = durationpb.New(5 * time.Second)
		mustFail(t, e.Probe(t.Context(), p), syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
		if got := time.Since(start); got != 5*time.Second {
			t.Errorf("answered after %v, want the timeout", got)
		}
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)
		if res := e.Probe(ctx, p); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED {
			t.Errorf("cancelled = %v", res)
		}
		// Both slots are held by the stuck listings: more are refused at
		// once and start no listing.
		for range 3 {
			mustFail(t, e.Probe(t.Context(), p), syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY)
		}
		if n := local.inFlight(); n != 2 {
			t.Errorf("%d listings in flight, want 2 (the slots)", n)
		}
		// Once the calls return, the slots are free again.
		close(local.release)
		synctest.Wait()
		if n := local.inFlight(); n != 0 {
			t.Fatalf("%d listings still in flight", n)
		}
		if res := e.Probe(t.Context(), p); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("after the listings returned: %v", res)
		}
	})
}

// TestLocalListingHonorsItsContext: the listing gets the probe's context
// with its deadline.
func TestLocalListingHonorsItsContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		deadlines := make(chan time.Duration, 2)
		local := localFunc(func(ctx context.Context) (localfs.Listing, error) {
			d, _ := ctx.Deadline()
			deadlines <- time.Until(d)
			<-ctx.Done()
			return localfs.Listing{}, ctx.Err()
		})
		e := newExecutor(t, nil, local, func(c *Config) { c.MaxConcurrent = 1 })
		p := probeOf("list_local", 0)
		p.Timeout = durationpb.New(7 * time.Second)
		mustFail(t, e.Probe(t.Context(), p), syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
		if d := <-deadlines; d != 7*time.Second {
			t.Errorf("listing deadline = %v, want the probe's timeout", d)
		}
		// The listing returned with its context: the slot is free at once.
		synctest.Wait()
		mustFail(t, e.Probe(t.Context(), p), syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
	})
}

// localFunc is a Local made of a function of the listing's context.
type localFunc func(ctx context.Context) (localfs.Listing, error)

func (f localFunc) List(ctx context.Context, _ string, _ int, _ bool) (localfs.Listing, error) {
	return f(ctx)
}
