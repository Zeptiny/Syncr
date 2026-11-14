package jobkind_test

import (
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// twoWay returns a two-way sync definition; mutate may adjust it.
func twoWay(p1, p2 *syncrv1.JobLocation, mutate ...func(*syncrv1.RcloneBisyncDefinition)) *syncrv1.JobDefinition {
	b := &syncrv1.RcloneBisyncDefinition{Path1: p1, Path2: p2}
	for _, m := range mutate {
		m(b)
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: b}}
}

func withBisyncOptions(o *syncrv1.RcloneBisyncOptions) func(*syncrv1.RcloneBisyncDefinition) {
	return func(b *syncrv1.RcloneBisyncDefinition) { b.Options = o }
}

func local(p string) *syncrv1.JobLocation { return loc(domain.RemoteID{}, p) }

func TestBisyncCheckAccepts(t *testing.T) {
	t.Parallel()
	a, b := rid(1), rid(2)
	all := &syncrv1.RcloneBisyncOptions{
		Transfers: i32(64), Checkers: i32(1), BandwidthLimitBytesPerSecond: i64(0), DryRun: true,
		Compare: []syncrv1.BisyncCompare{syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM, syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE,
			syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME},
		ConflictResolve: syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_SMALLER,
		ConflictLoser:   syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_PATHNAME,
		ConflictSuffix:  strings.Repeat("a", 32) + ",B.1_-", MaxDeletePercent: i32(100), CheckAccess: true,
		ResyncMode: syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_SMALLER, CreateEmptySrcDirs: true, LowLevelRetries: i32(0),
		Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}},
	}
	tests := []struct {
		name  string
		def   *syncrv1.JobDefinition
		refs  []domain.RemoteID
		terms []string
	}{
		{"local and remote", twoWay(local("/home/me/Shared"), loc(a, "shared")), []domain.RemoteID{a}, []string{"bisync", "two-way", "/home/me/Shared", "shared"}},
		{"remote and local", twoWay(loc(a, ""), local(`C:\Users\me\Shared`)), []domain.RemoteID{a}, []string{"bisync", "two-way", `C:\Users\me\Shared`}},
		{"two remotes", twoWay(loc(b, "x"), loc(a, "x")), []domain.RemoteID{b, a}, []string{"bisync", "two-way", "x", "x"}},
		{"siblings on one remote", twoWay(loc(a, "shared"), loc(a, "shared-copy")), []domain.RemoteID{a}, []string{"bisync", "two-way", "shared", "shared-copy"}},
		{"local siblings", twoWay(local("/data/a"), local("/data/ab")), nil, []string{"bisync", "two-way", "/data/a", "/data/ab"}},
		{"a POSIX and a Windows path", twoWay(local("/data"), local(`C:\data`)), nil, []string{"bisync", "two-way", "/data", `C:\data`}},
		{"every option at a bound", twoWay(local("/a"), local("/b"), withBisyncOptions(all)), nil, []string{"bisync", "two-way", "/a", "/b"}},
		{"minimum delete limit", twoWay(local("/a"), local("/b"), withBisyncOptions(&syncrv1.RcloneBisyncOptions{MaxDeletePercent: i32(1)})), nil, []string{"bisync", "two-way", "/a", "/b"}},
		{"200 bytes of paths", twoWay(local("/"+strings.Repeat("a", 99)), loc(a, strings.Repeat("b", 100))), []domain.RemoteID{a}, nil},
		{"one-shot fields", twoWay(local("/a"), local("/b"), func(b *syncrv1.RcloneBisyncDefinition) { b.Resync, b.Force = true, true }), nil,
			[]string{"bisync", "two-way", "/a", "/b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refs, err := jobkind.Bisync{}.Check(tt.def)
			if err != nil {
				t.Fatalf("Check = %v", err)
			}
			if !slices.Equal(refs.RemoteIDs, tt.refs) || (tt.terms != nil && !slices.Equal(refs.SearchTerms, tt.terms)) {
				t.Errorf("refs = %+v, want %v %q", refs, tt.refs, tt.terms)
			}
		})
	}
}

func TestBisyncCheckRejects(t *testing.T) {
	t.Parallel()
	a, b := rid(1), rid(2)
	const f = "definition.rclone_bisync"
	opts := func(o *syncrv1.RcloneBisyncOptions) *syncrv1.JobDefinition {
		return twoWay(local("/a"), loc(a, "b"), withBisyncOptions(o))
	}
	compare := func(c ...syncrv1.BisyncCompare) *syncrv1.JobDefinition {
		return opts(&syncrv1.RcloneBisyncOptions{Compare: c})
	}
	suffix := func(s string) *syncrv1.JobDefinition { return opts(&syncrv1.RcloneBisyncOptions{ConflictSuffix: s}) }
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want []string
	}{
		{"no two-way sync", &syncrv1.JobDefinition{}, []string{f}},
		{"nil two-way sync", &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{}}, []string{f}},
		{"no sides", twoWay(nil, nil), []string{f + ".path1", f + ".path2"}},
		{"bad remote id", twoWay(&syncrv1.JobLocation{RemoteId: "r2", Path: "x"}, local("/a")), []string{f + ".path1.remote_id"}},
		{"relative local", twoWay(loc(a, "x"), local("data")), []string{f + ".path2.path"}},
		{"dot-dot on a remote", twoWay(loc(a, "x/../y"), local("/a")), []string{f + ".path1.path"}},
		{"dot-dot local", twoWay(loc(a, "x"), local(`C:\a\..\b`)), []string{f + ".path2.path"}},

		{"same remote path", twoWay(loc(a, "shared"), loc(a, "shared/")), []string{f + ".path2"}},
		{"same remote path, leading slash", twoWay(loc(a, "/shared"), loc(a, "shared")), []string{f + ".path2"}},
		{"remote path inside the other", twoWay(loc(a, "shared"), loc(a, "shared/sub")), []string{f + ".path2"}},
		{"remote path around the other", twoWay(loc(a, "shared/sub/x"), loc(a, "shared")), []string{f + ".path2"}},
		{"remote root and a path", twoWay(loc(a, "x"), loc(a, "")), []string{f + ".path2"}},
		{"same local path", twoWay(local("/data/x"), local("/data/./x/")), []string{f + ".path2"}},
		{"local path inside the other", twoWay(local("/data"), local("/data/x")), []string{f + ".path2"}},
		{"local root", twoWay(local("/srv/x"), local("/")), []string{f + ".path2"}},
		{"windows paths differ in case only", twoWay(local(`C:\Data`), local(`c:/data/Sub`)), []string{f + ".path2"}},
		{"windows drive root", twoWay(local(`D:\`), local(`d:\x`)), []string{f + ".path2"}},
		{"UNC share and a folder in it", twoWay(local(`\\nas\share\x`), local(`\\NAS\Share`)), []string{f + ".path2"}},

		{"paths too long", twoWay(local("/"+strings.Repeat("a", 100)), loc(a, strings.Repeat("b", 100))), []string{f + ".path1.path"}},

		{"transfers 0", opts(&syncrv1.RcloneBisyncOptions{Transfers: i32(0)}), []string{f + ".options.transfers"}},
		{"transfers 65", opts(&syncrv1.RcloneBisyncOptions{Transfers: i32(65)}), []string{f + ".options.transfers"}},
		{"checkers 257", opts(&syncrv1.RcloneBisyncOptions{Checkers: i32(257)}), []string{f + ".options.checkers"}},
		{"low level retries 1001", opts(&syncrv1.RcloneBisyncOptions{LowLevelRetries: i32(1001)}), []string{f + ".options.low_level_retries"}},
		{"negative bandwidth", opts(&syncrv1.RcloneBisyncOptions{BandwidthLimitBytesPerSecond: i64(-1)}), []string{f + ".options.bandwidth_limit_bytes_per_second"}},
		{"delete limit 0", opts(&syncrv1.RcloneBisyncOptions{MaxDeletePercent: i32(0)}), []string{f + ".options.max_delete_percent"}},
		{"delete limit 101", opts(&syncrv1.RcloneBisyncOptions{MaxDeletePercent: i32(101)}), []string{f + ".options.max_delete_percent"}},
		{"compare unspecified", compare(syncrv1.BisyncCompare_BISYNC_COMPARE_UNSPECIFIED), []string{f + ".options.compare[0]"}},
		{"compare unknown", compare(syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE, 9), []string{f + ".options.compare[1]"}},
		{"compare twice", compare(syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME, syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME),
			[]string{f + ".options.compare[1]"}},
		{"unknown conflict resolution", opts(&syncrv1.RcloneBisyncOptions{ConflictResolve: 8}), []string{f + ".options.conflict_resolve"}},
		// rclone's "delete" loser has no value; 3 is not one.
		{"delete loser", opts(&syncrv1.RcloneBisyncOptions{ConflictLoser: 3}), []string{f + ".options.conflict_loser"}},
		{"unknown resync mode", opts(&syncrv1.RcloneBisyncOptions{ResyncMode: 7}), []string{f + ".options.resync_mode"}},
		{"suffix too long", suffix(strings.Repeat("a", 33)), []string{f + ".options.conflict_suffix"}},
		{"three suffixes", suffix("a,b,c"), []string{f + ".options.conflict_suffix"}},
		{"empty second suffix", suffix("a,"), []string{f + ".options.conflict_suffix"}},
		{"suffix with a slash", suffix("a/b"), []string{f + ".options.conflict_suffix"}},
		{"suffix with a space", suffix("my copy"), []string{f + ".options.conflict_suffix"}},
		{"suffix with a flag", suffix("--force"), nil},
		{"filter without action", opts(&syncrv1.RcloneBisyncOptions{Filters: []*syncrv1.FilterRule{{Pattern: "*.tmp"}}}), []string{f + ".options.filters[0].action"}},
		{"filter with a line break", opts(&syncrv1.RcloneBisyncOptions{Filters: []*syncrv1.FilterRule{
			{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "a"}, {Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "a\nb"},
		}}), []string{f + ".options.filters[1].pattern"}},
		{"too many filters", opts(&syncrv1.RcloneBisyncOptions{Filters: slices.Repeat([]*syncrv1.FilterRule{
			{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "x"}}, 1001)}), []string{f + ".options.filters"}},
		{"everything at once", twoWay(loc(b, "x"), loc(b, "x/y"), withBisyncOptions(&syncrv1.RcloneBisyncOptions{Checkers: i32(0), ConflictLoser: 9})),
			[]string{f + ".path2", f + ".options.checkers", f + ".options.conflict_loser"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := jobkind.Bisync{}.Check(tt.def)
			if tt.want == nil {
				// A suffix that starts with "-" is still only letters,
				// digits, ".", "_" and "-": the worker passes it as
				// --conflict-suffix=<value>, never as a flag of its own.
				if err != nil {
					t.Errorf("Check = %v, want it accepted", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidArgument) || !slices.Equal(fields(err), tt.want) {
				t.Errorf("Check = %v, want fields %q", err, tt.want)
			}
			if _, err := (jobkind.Bisync{}).Build(tt.def, jobkind.Resolved{}); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Build = %v, want the Check error", err)
			}
		})
	}
}

// bisyncSpec is the spec of a two-way sync between p1 and p2.
func bisyncSpec(p1, p2 *syncrv1.Location, opts *syncrv1.RcloneBisyncOptions, defs ...*syncrv1.RemoteDefinition) *syncrv1.JobSpec {
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisync{
		Path1: p1, Path2: p2, Options: opts,
	}}}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestBisyncBuild(t *testing.T) {
	t.Parallel()
	s3, sftp, lcl, u := rid(1), rid(2), rid(3), rid(5)
	S3, SFTP, LCL := s3Remote(s3, "bucket/base"), sftpRemote(sftp, "/home/me"), localRemote(lcl, "/srv/photos")
	U := unionRemote(u,
		jobkind.Member{RemoteID: s3, Path: "pool", Mode: backend.UnionReadWrite},
		jobkind.Member{RemoteID: lcl, Path: "disk1", Mode: backend.UnionReadOnly},
	)
	all := remoteMap(S3, SFTP, LCL, U)
	opts := &syncrv1.RcloneBisyncOptions{DryRun: true, MaxDeletePercent: i32(25), ConflictSuffix: "mine,theirs",
		Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}}}
	none := domain.RemoteID{}
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want *syncrv1.JobSpec
	}{
		{"local path and s3", twoWay(loc(none, "/home/me/Shared/"), loc(s3, "/shared"), withBisyncOptions(opts)),
			bisyncSpec(sloc("", "/home/me/Shared"), sloc("r1", "bucket/base/shared"), opts, rdef("r1", S3))},
		{"sftp and a windows path", twoWay(loc(sftp, "docs"), loc(none, `D:\Docs`)),
			bisyncSpec(sloc("r1", "/home/me/docs"), sloc("", `D:\Docs`), nil, rdef("r1", SFTP))},
		{"local remote and s3", twoWay(loc(lcl, "2026"), loc(s3, "")),
			bisyncSpec(sloc("", "/srv/photos/2026"), sloc("r1", "bucket/base"), nil, rdef("r1", S3))},
		{"two paths on one remote", twoWay(loc(s3, "a"), loc(s3, "b")),
			bisyncSpec(sloc("r1", "bucket/base/a"), sloc("r1", "bucket/base/b"), nil, rdef("r1", S3))},
		{"union, then its members", twoWay(loc(u, "albums"), loc(sftp, "copy")),
			bisyncSpec(sloc("r1", "albums"), sloc("r2", "/home/me/copy"), nil,
				rdef("r1", U,
					&syncrv1.UnionUpstream{Location: sloc("r3", "bucket/base/pool"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE},
					&syncrv1.UnionUpstream{Location: sloc("", "/srv/photos/disk1"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
				),
				rdef("r2", SFTP), rdef("r3", S3))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := jobkind.Bisync{}.Build(tt.def, jobkind.Resolved{Remotes: all})
			if err != nil {
				t.Fatalf("Build = %v", err)
			}
			key := got.GetRcloneBisync().GetStateKey()
			if !hex64.MatchString(key) {
				t.Errorf("state key %q is not 64 lower-case hex digits", key)
			}
			cmp := proto.CloneOf(got)
			cmp.GetRcloneBisync().StateKey = ""
			if !proto.Equal(cmp, tt.want) {
				t.Errorf("Build =\n%v\nwant\n%v", cmp, tt.want)
			}
			again, err := jobkind.Bisync{}.Build(tt.def, jobkind.Resolved{Remotes: all})
			if err != nil || !proto.Equal(again, got) {
				t.Errorf("Build is not deterministic: %v", err)
			}
		})
	}

	// The one-shot fields reach the spec; they do not change the key.
	plain := twoWay(local("/a"), loc(s3, "b"))
	once := twoWay(local("/a"), loc(s3, "b"), func(b *syncrv1.RcloneBisyncDefinition) { b.Resync, b.Force = true, true })
	p, err1 := jobkind.Bisync{}.Build(plain, jobkind.Resolved{Remotes: all})
	o, err2 := jobkind.Bisync{}.Build(once, jobkind.Resolved{Remotes: all})
	if err1 != nil || err2 != nil || !o.GetRcloneBisync().GetResync() || !o.GetRcloneBisync().GetForce() ||
		p.GetRcloneBisync().GetResync() || o.GetRcloneBisync().GetStateKey() != p.GetRcloneBisync().GetStateKey() {
		t.Errorf("one-shot spec = %v (%v, %v)", o, err1, err2)
	}

	// The registry adds the hooks, as for every kind.
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	hooked := twoWay(local("/a"), local("/b"))
	hooked.Hooks = &syncrv1.JobHooks{Before: []string{"mount-usb"}}
	spec, err := r.Build(hooked, jobkind.Resolved{})
	if err != nil || !slices.Equal(spec.GetHooks().GetBefore(), []string{"mount-usb"}) || spec.GetRcloneBisync().GetPath2().GetPath() != "/b" {
		t.Errorf("Registry.Build = %v (%v)", spec, err)
	}
}

func TestBisyncBuildDoesNotAlias(t *testing.T) {
	t.Parallel()
	id := rid(1)
	r := sftpRemote(id, "")
	opts := &syncrv1.RcloneBisyncOptions{Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "x"}}}
	got, err := jobkind.Bisync{}.Build(twoWay(loc(id, "a"), local("/b"), withBisyncOptions(opts)), jobkind.Resolved{Remotes: remoteMap(r)})
	if err != nil {
		t.Fatal(err)
	}
	got.Remotes[0].Secrets["pass"] = "mutated"
	got.Remotes[0].Options["host"] = "mutated"
	got.GetRcloneBisync().Options.Filters[0].Pattern = "mutated"
	if r.Rclone.Secrets["pass"] != "sftp-secret" || r.Rclone.Options["host"] != "nas.example" || opts.Filters[0].Pattern != "x" {
		t.Error("the spec aliases the resolved remote or the definition")
	}
}

// TestBisyncBuildPathLength: the full rclone paths, with the remotes'
// roots and the alias, are at most 200 bytes together, counted as the
// worker counts them.
func TestBisyncBuildPathLength(t *testing.T) {
	t.Parallel()
	s3, lcl := rid(1), rid(3)
	// "r1:" + "bucket/base/" is 15 bytes in front of the path on s3.
	S3, LCL := s3Remote(s3, "bucket/base"), localRemote(lcl, "/srv/photos")
	all := remoteMap(S3, LCL)
	local100 := "/" + strings.Repeat("a", 99)
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		ok   bool
	}{
		{"exactly 200", twoWay(local(local100), loc(s3, strings.Repeat("b", 85))), true},
		{"201 with the root", twoWay(local(local100), loc(s3, strings.Repeat("b", 86))), false},
		// "/srv/photos/" is 12 bytes in front of the path on the local
		// remote.
		{"local remote, exactly 200", twoWay(loc(lcl, strings.Repeat("c", 88)), local(local100)), true},
		{"local remote, 201", twoWay(loc(lcl, strings.Repeat("c", 89)), local(local100)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := (jobkind.Bisync{}).Check(tt.def); err != nil {
				t.Fatalf("Check = %v, want the early check to pass", err)
			}
			spec, err := jobkind.Bisync{}.Build(tt.def, jobkind.Resolved{Remotes: all})
			if tt.ok {
				if err != nil {
					t.Fatalf("Build = %v", err)
				}
				b := spec.GetRcloneBisync()
				n := 0
				for _, l := range []*syncrv1.Location{b.GetPath1(), b.GetPath2()} {
					n += len(l.GetPath())
					if l.GetRemote() != "" {
						n += len(l.GetRemote()) + 1
					}
				}
				if n != 200 {
					t.Errorf("the rclone paths are %d bytes, want 200", n)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidArgument) || !slices.Equal(fields(err), []string{"definition.rclone_bisync.path1.path"}) ||
				!strings.Contains(err.Error(), "the two paths together are too long for a two-way sync (rclone keeps its state in files named after them)") {
				t.Errorf("Build = %v, want the path length error", err)
			}
		})
	}
}

func TestBisyncBuildRejects(t *testing.T) {
	t.Parallel()
	a, lcl, u := rid(1), rid(3), rid(5)
	tests := []struct {
		name    string
		def     *syncrv1.JobDefinition
		remotes map[domain.RemoteID]jobkind.Remote
		want    error
		field   string
	}{
		{"unresolved remote", twoWay(loc(a, "x"), local("/y")), nil, domain.ErrNotFound, ""},
		{"unresolved member", twoWay(loc(u, "x"), local("/y")),
			remoteMap(unionRemote(u, jobkind.Member{RemoteID: a, Mode: backend.UnionReadWrite})), domain.ErrNotFound, ""},
		{"root with dot-dot", twoWay(local("/y"), loc(a, "x")), remoteMap(s3Remote(a, "bucket/../other")), domain.ErrInvalidArgument, "definition.rclone_bisync.path2.path"},
		// Only the remote's type tells that these are the same folder.
		{"local remote and its own folder", twoWay(loc(lcl, ""), local("/srv/photos/2026")), remoteMap(localRemote(lcl, "/srv/photos")),
			domain.ErrInvalidArgument, "definition.rclone_bisync.path2"},
		{"local remote around a worker path", twoWay(local("/srv"), loc(lcl, "x")), remoteMap(localRemote(lcl, "/srv/photos")),
			domain.ErrInvalidArgument, "definition.rclone_bisync.path2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := (jobkind.Bisync{}).Check(tt.def); err != nil {
				t.Fatalf("Check = %v", err)
			}
			_, err := jobkind.Bisync{}.Build(tt.def, jobkind.Resolved{Remotes: tt.remotes})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Build = %v, want %v", err, tt.want)
			}
			if got := fields(err); tt.field != "" && !slices.Equal(got, []string{tt.field}) {
				t.Errorf("fields = %q, want %q", got, tt.field)
			}
			for _, secret := range []string{"s3-secret", "AKIA", "minio.example"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q contains a value of the remote", err)
				}
			}
		})
	}
}

// stateKey builds def over remotes and returns its state key.
func stateKey(t *testing.T, def *syncrv1.JobDefinition, remotes ...jobkind.Remote) string {
	t.Helper()
	spec, err := jobkind.Bisync{}.Build(def, jobkind.Resolved{Remotes: remoteMap(remotes...)})
	if err != nil {
		t.Fatalf("Build = %v", err)
	}
	return spec.GetRcloneBisync().GetStateKey()
}

// TestBisyncStateKey: the same pair has the same key; a rotated secret
// keeps it; any other change of a side, or swapping the sides, gives a new
// one.
func TestBisyncStateKey(t *testing.T) {
	t.Parallel()
	s3, sftp, d1, d2, u := rid(1), rid(2), rid(3), rid(4), rid(5)
	r2 := func(mutate ...func(*jobkind.Remote)) jobkind.Remote {
		r := s3Remote(s3, "shared-bucket")
		r.Rclone.Options = map[string]string{"provider": "Cloudflare", "endpoint": "https://acct.r2.example", "access_key_id": "AKID"}
		r.Rclone.Secrets = map[string]string{"secret_access_key": "first-secret"}
		for _, m := range mutate {
			m(&r)
		}
		return r
	}
	union := func(mutate ...func(*jobkind.Remote)) []jobkind.Remote {
		ur := unionRemote(u,
			jobkind.Member{RemoteID: d1, Path: "pool", Mode: backend.UnionReadWrite},
			jobkind.Member{RemoteID: sftp, Path: "pool", Mode: backend.UnionReadWrite})
		for _, m := range mutate {
			m(&ur)
		}
		return []jobkind.Remote{ur, localRemote(d1, "/mnt/disk1"), sftpRemote(sftp, "/home/me"), localRemote(d2, "/mnt/disk2")}
	}
	pair := twoWay(local("/home/me/Shared"), loc(s3, "shared"))
	base := stateKey(t, pair, r2())
	unionPair := twoWay(loc(u, "docs"), loc(s3, "shared"))
	unionBase := stateKey(t, unionPair, append(union(), r2())...)

	same := map[string]string{
		"again": stateKey(t, pair, r2()),
		"rotated secret": stateKey(t, pair, r2(func(r *jobkind.Remote) {
			r.Rclone.Secrets = map[string]string{"secret_access_key": "second-secret"}
		})),
		"another secret added": stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.Secrets["session_token"] = "token" })),
		"obscured keys":        stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.ObscureKeys = []string{"secret_access_key"} })),
		"options in another map order": stateKey(t, pair, r2(func(r *jobkind.Remote) {
			r.Rclone.Options = maps.Clone(r.Rclone.Options)
		})),
		"the one-shot fields": stateKey(t, twoWay(local("/home/me/Shared"), loc(s3, "shared"), func(b *syncrv1.RcloneBisyncDefinition) {
			b.Resync, b.Force = true, true
		}), r2()),
		"the options": stateKey(t, twoWay(local("/home/me/Shared"), loc(s3, "shared"),
			withBisyncOptions(&syncrv1.RcloneBisyncOptions{MaxDeletePercent: i32(10), Filters: []*syncrv1.FilterRule{
				{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}}})), r2()),
		"the same path written otherwise": stateKey(t, twoWay(local("/home/me/./Shared/"), loc(s3, "/shared//")), r2()),
	}
	for name, got := range same {
		if got != base {
			t.Errorf("%s: key %s, want the same key %s", name, got, base)
		}
	}
	if got := stateKey(t, unionPair, append(union(func(r *jobkind.Remote) { r.Rclone.Secrets = map[string]string{"x": "y"} }), r2())...); got != unionBase {
		t.Errorf("a union's secret changed the key")
	}

	differ := map[string]string{
		"endpoint":                  stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.Options["endpoint"] = "https://other.r2.example" })),
		"option added":              stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.Options["region"] = "auto" })),
		"access key id (an option)": stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.Options["access_key_id"] = "AKID2" })),
		"bucket (the root)":         stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.Root = "other-bucket" })),
		"backend type":              stateKey(t, pair, r2(func(r *jobkind.Remote) { r.Rclone.Backend = "b2" })),
		"remote path":               stateKey(t, twoWay(local("/home/me/Shared"), loc(s3, "shared2")), r2()),
		"local path":                stateKey(t, twoWay(local("/home/me/shared"), loc(s3, "shared")), r2()),
		"another remote":            stateKey(t, twoWay(local("/home/me/Shared"), loc(rid(9), "shared")), r2(func(r *jobkind.Remote) { r.ID = rid(9) })),
		"sides swapped":             stateKey(t, twoWay(loc(s3, "shared"), local("/home/me/Shared")), r2()),
		// A local remote is its folder: the same folder as a worker path
		// is the same side, and moving the remote's root moves it.
		"local remote's root": stateKey(t, twoWay(loc(d1, "Shared"), loc(s3, "shared")), r2(), localRemote(d1, "/home/you")),
	}
	differ["option value moved to another key"] = stateKey(t, pair, r2(func(r *jobkind.Remote) {
		r.Rclone.Options = map[string]string{"provider": "Cloudflare", "endpoint": "https://acct.r2.example", "access_key_idAKID": ""}
	}))
	seen := map[string]string{base: "base"}
	for name, got := range differ {
		if got == base {
			t.Errorf("%s: the key did not change", name)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s have the same key", name, other)
		}
		seen[got] = name
	}
	if got := stateKey(t, twoWay(loc(d1, "Shared"), loc(s3, "shared")), r2(), localRemote(d1, "/home/me")); got != base {
		t.Errorf("a local remote's folder has key %s, the same worker path %s", got, base)
	}

	unionDiffer := map[string][]jobkind.Remote{
		"member path": append(union(func(r *jobkind.Remote) { r.Members[1].Path = "pool2" }), r2()),
		"member mode": append(union(func(r *jobkind.Remote) { r.Members[0].Mode = backend.UnionNoCreate }), r2()),
		"member order": append(union(func(r *jobkind.Remote) {
			r.Members[0], r.Members[1] = r.Members[1], r.Members[0]
		}), r2()),
		"member replaced": append(union(func(r *jobkind.Remote) { r.Members[0].RemoteID = d2 }), r2()),
		"member's option": func() []jobkind.Remote {
			rs := append(union(), r2())
			rs[2].Rclone.Options = map[string]string{"host": "other.example", "user": "me"}
			return rs
		}(),
		"union option": append(union(func(r *jobkind.Remote) { r.Rclone.Options = map[string]string{"action_policy": "all"} }), r2()),
	}
	for name, rs := range unionDiffer {
		if got := stateKey(t, unionPair, rs...); got == unionBase {
			t.Errorf("union %s: the key did not change", name)
		}
	}
	if got := stateKey(t, unionPair, append(union(), r2())...); got != unionBase {
		t.Error("the union's key is not stable")
	}
	// A member's secret is no part of it either.
	rs := append(union(), r2())
	rs[2].Rclone.Secrets = map[string]string{"pass": "rotated"}
	if got := stateKey(t, unionPair, rs...); got != unionBase {
		t.Error("a member's secret changed the key")
	}
}

var bisyncKeyRe = regexp.MustCompile(`^bisync:[0-9a-f]{40}$`)

// TestBisyncConcurrencyKeys: one key per side, of ADR 0013's form, shared
// by every pair that uses the location, whatever the other side.
func TestBisyncConcurrencyKeys(t *testing.T) {
	t.Parallel()
	r2, other := rid(1), rid(2)
	keys := func(def *syncrv1.JobDefinition) []string {
		t.Helper()
		got := jobkind.Bisync{}.ConcurrencyKeys(def)
		if len(got) != 2 {
			t.Fatalf("keys = %q, want two", got)
		}
		for _, k := range got {
			if !bisyncKeyRe.MatchString(k) || len(k) > 200 || !strings.HasPrefix(k, jobkind.BisyncKeyPrefix) {
				t.Errorf("key %q does not match ADR 0013's pattern", k)
			}
		}
		return got
	}
	desktop := keys(twoWay(local("/home/ada/Shared"), loc(r2, "shared")))
	laptop := keys(twoWay(local(`C:\Users\ada\Shared`), loc(r2, "/shared/")))
	if desktop[1] != laptop[1] {
		t.Errorf("the desktop's and the laptop's R2 keys differ: %s, %s", desktop[1], laptop[1])
	}
	if desktop[0] == laptop[0] || desktop[0] == desktop[1] {
		t.Errorf("keys = %q and %q, want only the R2 key shared", desktop, laptop)
	}
	// The same location as Folder B of another pair.
	if k := keys(twoWay(loc(r2, "shared"), local("/srv/other"))); k[0] != desktop[1] {
		t.Errorf("R2 as path1 = %s, want %s", k[0], desktop[1])
	}
	// A local key ignores the other side, and Windows paths ignore case.
	if k := keys(twoWay(local(`c:\users\ADA\shared`), loc(other, "x"))); k[0] != laptop[0] {
		t.Errorf("windows key = %s, want %s", k[0], laptop[0])
	}
	if k := keys(twoWay(local("/home/ada/shared"), loc(r2, "x"))); k[0] == desktop[0] {
		t.Error("POSIX paths that differ in case share a key")
	}
	distinct := []string{
		keys(twoWay(local("/srv/a"), loc(r2, "shared")))[0],
		keys(twoWay(local("/srv/b"), loc(other, "shared")))[1], // another remote, the same path
		keys(twoWay(local("/srv/c"), loc(r2, "shared/sub")))[1],
		keys(twoWay(local("/shared"), loc(r2, "x")))[0], // a local path spelled like the remote one
		desktop[1],
	}
	if len(slices.Compact(slices.Sorted(slices.Values(distinct)))) != len(distinct) {
		t.Errorf("keys of different locations collide: %q", distinct)
	}

	// The registry stores them sorted, and none for an invalid definition.
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	_, refs, err := r.Check(twoWay(local("/home/ada/Shared"), loc(r2, "shared")))
	if err != nil || !slices.IsSorted(refs.ConcurrencyKeys) || len(refs.ConcurrencyKeys) != 2 ||
		!slices.Contains(refs.ConcurrencyKeys, desktop[0]) || !slices.Contains(refs.ConcurrencyKeys, desktop[1]) {
		t.Errorf("Registry.Check keys = %q (%v)", refs.ConcurrencyKeys, err)
	}
	if got := (jobkind.Bisync{}).ConcurrencyKeys(twoWay(local("rel"), loc(r2, "x"))); got != nil {
		t.Errorf("keys of an invalid definition = %q", got)
	}
}

// TestBisyncRegistry: the kind's answers through the registry.
func TestBisyncRegistry(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := r.Get("rclone_bisync"); !ok || k.Name() != "rclone_bisync" {
		t.Fatalf("Get(rclone_bisync) = (%v, %v)", k, ok)
	}
	if got := r.Label("rclone_bisync"); got != "Two-way sync" {
		t.Errorf("Label = %q", got)
	}
	if got := r.RequiredFeatures("rclone_bisync"); !slices.Equal(got, []string{link.FeatureRcloneBisync}) {
		t.Errorf("RequiredFeatures = %q", got)
	}
	if ok, why := r.Schedulable("rclone_bisync"); !ok || why != "" {
		t.Errorf("Schedulable = (%v, %q)", ok, why)
	}
	if ok, why := r.UserStartable("rclone_bisync"); !ok || why != "" {
		t.Errorf("UserStartable = (%v, %q)", ok, why)
	}
	if ok, why := r.LabelTargetAllowed(twoWay(local("/a"), local("/b"))); ok || why != "a two-way sync keeps its state on one worker; choose a worker" {
		t.Errorf("LabelTargetAllowed = (%v, %q)", ok, why)
	}
	if got := r.RequiredOS(twoWay(local("/a"), local("/b"))); got != (jobkind.OSRequirement{}) {
		t.Errorf("RequiredOS = %+v", got)
	}

	oneShot := func(resync, force bool) *syncrv1.JobDefinition {
		return twoWay(local("/a"), local("/b"), func(b *syncrv1.RcloneBisyncDefinition) { b.Resync, b.Force = resync, force })
	}
	if err := r.CheckSchedulable(oneShot(false, false)); err != nil {
		t.Errorf("CheckSchedulable(plain) = %v", err)
	}
	for _, tt := range []struct {
		resync, force bool
		want          []string
	}{
		{true, false, []string{"definition.rclone_bisync.resync"}},
		{false, true, []string{"definition.rclone_bisync.force"}},
		{true, true, []string{"definition.rclone_bisync.resync", "definition.rclone_bisync.force"}},
	} {
		def := oneShot(tt.resync, tt.force)
		err := r.CheckSchedulable(def)
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || !slices.Equal(fields(err), tt.want) || verr.Fields[0].Message != "can only be set for a single run" {
			t.Errorf("CheckSchedulable(resync %v, force %v) = %v, want %q", tt.resync, tt.force, err, tt.want)
		}
		// RunJob's check accepts them.
		if kind, _, err := r.Check(def); err != nil || kind != "rclone_bisync" {
			t.Errorf("Check(resync %v, force %v) = (%q, %v)", tt.resync, tt.force, kind, err)
		}
	}
}

func TestBisyncDescribe(t *testing.T) {
	t.Parallel()
	names := map[domain.RemoteID]string{rid(1): "R2 offsite", rid(2): "NAS"}
	once := func(resync, force bool) func(*syncrv1.RcloneBisyncDefinition) {
		return func(b *syncrv1.RcloneBisyncDefinition) { b.Resync, b.Force = resync, force }
	}
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want string
	}{
		{"local and remote", twoWay(local("/home/you/Shared"), loc(rid(1), "shared")), "Two-way sync /home/you/Shared ⇄ R2 offsite"},
		{"two remotes", twoWay(loc(rid(2), "x"), loc(rid(1), "")), "Two-way sync NAS ⇄ R2 offsite"},
		{"resync", twoWay(local(`C:\Shared`), loc(rid(1), ""), once(true, false)), `Two-way sync C:\Shared ⇄ R2 offsite · Resync`},
		{"force", twoWay(local("/a"), loc(rid(1), ""), once(false, true)), "Two-way sync /a ⇄ R2 offsite · Delete limit ignored"},
		{"both", twoWay(local("/a"), local("/b"), once(true, true)), "Two-way sync /a ⇄ /b · Resync · Delete limit ignored"},
		{"unnamed remote", twoWay(loc(rid(3), "x"), local("")), "Two-way sync Unknown remote ⇄ Worker filesystem"},
		{"nothing", twoWay(nil, nil), "Two-way sync Worker filesystem ⇄ Worker filesystem"},
	}
	for _, tt := range tests {
		if got := (jobkind.Bisync{}).Describe(tt.def, jobkind.Names{Remotes: names}); got != tt.want {
			t.Errorf("%s: Describe = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestBisyncSummarize(t *testing.T) {
	t.Parallel()
	out := func(o *syncrv1.RcloneBisyncOutput) *syncrv1.JobOutput {
		return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_RcloneBisync{RcloneBisync: o}}
	}
	tests := []struct {
		name string
		out  *syncrv1.JobOutput
		want []jobkind.Detail
	}{
		{"none", nil, nil},
		{"another kind's", &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticCheck{ResticCheck: &syncrv1.ResticCheckOutput{}}}, nil},
		{"normal run", out(&syncrv1.RcloneBisyncOutput{}), []jobkind.Detail{{Label: "Conflicts", Value: "None"}}},
		{"conflicts", out(&syncrv1.RcloneBisyncOutput{Conflicts: 1234, ConflictPaths: []string{"a"}}), []jobkind.Detail{{Label: "Conflicts", Value: "1,234"}}},
		{"first run", out(&syncrv1.RcloneBisyncOutput{ResyncReason: syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN}),
			[]jobkind.Detail{{Label: "Resync", Value: "First run"}}},
		{"requested dry run", out(&syncrv1.RcloneBisyncOutput{ResyncReason: syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED, DryRun: true}),
			[]jobkind.Detail{{Label: "Resync", Value: "Requested"}, {Label: "Dry run", Value: "Nothing was changed"}}},
		{"dry run with conflicts", out(&syncrv1.RcloneBisyncOutput{Conflicts: 2, DryRun: true}),
			[]jobkind.Detail{{Label: "Conflicts", Value: "2"}, {Label: "Dry run", Value: "Nothing was changed"}}},
	}
	for _, tt := range tests {
		if got := (jobkind.Bisync{}).Summarize(nil, tt.out); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Summarize = %+v, want %+v", tt.name, got, tt.want)
		}
	}
	// Through the registry, from the stored bytes.
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(out(&syncrv1.RcloneBisyncOutput{Conflicts: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Summarize("rclone_bisync", nil, raw); !slices.Equal(got, []jobkind.Detail{{Label: "Conflicts", Value: "2"}}) {
		t.Errorf("Registry.Summarize = %+v", got)
	}
}
