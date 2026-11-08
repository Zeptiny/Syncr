package jobkind_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// Fixed remote IDs of the tests.
func rid(n int) domain.RemoteID {
	id, err := domain.ParseRemoteID(fmt.Sprintf("00000000-0000-7000-8000-%012x", 3000+n))
	if err != nil {
		panic(err)
	}
	return id
}

func loc(remote domain.RemoteID, path string) *syncrv1.JobLocation {
	l := &syncrv1.JobLocation{Path: path}
	if !remote.IsZero() {
		l.RemoteId = remote.String()
	}
	return l
}

// def returns a transfer definition; mutate may adjust it.
func def(src, dst *syncrv1.JobLocation, mutate ...func(*syncrv1.RcloneTransferDefinition)) *syncrv1.JobDefinition {
	tr := &syncrv1.RcloneTransferDefinition{Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC, Source: src, Destination: dst}
	for _, m := range mutate {
		m(tr)
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: tr}}
}

func withOptions(o *syncrv1.RcloneTransferOptions) func(*syncrv1.RcloneTransferDefinition) {
	return func(tr *syncrv1.RcloneTransferDefinition) { tr.Options = o }
}

func withMode(m syncrv1.RcloneTransferMode) func(*syncrv1.RcloneTransferDefinition) {
	return func(tr *syncrv1.RcloneTransferDefinition) { tr.Mode = m }
}

func i32(v int32) *int32 { return &v }
func i64(v int64) *int64 { return &v }

// fields returns the field paths of a *domain.ValidationError, or nil.
func fields(err error) []string {
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		return nil
	}
	out := make([]string, len(verr.Fields))
	for i, f := range verr.Fields {
		out[i] = f.Field
	}
	return out
}

func TestTransferDescribe(t *testing.T) {
	t.Parallel()
	names := map[domain.RemoteID]string{rid(1): "Photos", rid(2): "B2 offsite"}
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want string
	}{
		{"remote to remote", def(loc(rid(1), "albums"), loc(rid(2), ""), withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY)),
			"Copy Photos → B2 offsite"},
		{"local to remote", def(loc(domain.RemoteID{}, "/srv/data"), loc(rid(2), "data")), "Sync /srv/data → B2 offsite"},
		{"remote to local", def(loc(rid(1), ""), loc(domain.RemoteID{}, `D:\Backup`), withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE)),
			`Move Photos → D:\Backup`},
		{"unnamed remote", def(loc(rid(3), "x"), loc(domain.RemoteID{}, "/dst")), "Sync Unknown remote → /dst"},
		{"invalid remote ID", def(&syncrv1.JobLocation{RemoteId: "nope"}, loc(domain.RemoteID{}, "")), "Sync Unknown remote → Worker filesystem"},
		{"no mode", def(loc(rid(1), ""), loc(rid(2), ""), withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_UNSPECIFIED)),
			"Transfer Photos → B2 offsite"},
	}
	for _, tt := range tests {
		if got := (jobkind.Transfer{}).Describe(tt.def, names); got != tt.want {
			t.Errorf("%s: Describe = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestTransferCheckAccepts(t *testing.T) {
	t.Parallel()
	a, b := rid(1), rid(2)
	tests := []struct {
		name  string
		def   *syncrv1.JobDefinition
		refs  []domain.RemoteID
		terms []string
	}{
		{"local to remote", def(loc(domain.RemoteID{}, "/srv/photos"), loc(a, "backup/photos")), []domain.RemoteID{a}, []string{"sync", "/srv/photos", "backup/photos"}},
		{"remote to local", def(loc(a, ""), loc(domain.RemoteID{}, "/restore")), []domain.RemoteID{a}, []string{"sync", "/restore"}},
		{"remote to remote", def(loc(b, "x"), loc(a, "y")), []domain.RemoteID{b, a}, []string{"sync", "x", "y"}},
		{"same remote, other path", def(loc(a, "x"), loc(a, "x/y")), []domain.RemoteID{a}, []string{"sync", "x", "x/y"}},
		{"local to local", def(loc(domain.RemoteID{}, "/a"), loc(domain.RemoteID{}, "/b")), nil, []string{"sync", "/a", "/b"}},
		{"windows paths", def(loc(domain.RemoteID{}, `C:\Users\me`), loc(domain.RemoteID{}, `\\nas\share\backup`)), nil, []string{"sync", `C:\Users\me`, `\\nas\share\backup`}},
		{"windows drive with slash", def(loc(domain.RemoteID{}, "d:/data"), loc(a, "")), []domain.RemoteID{a}, []string{"sync", "d:/data"}},
		{"absolute remote path", def(loc(a, "/home/me"), loc(domain.RemoteID{}, "/x")), []domain.RemoteID{a}, []string{"sync", "/home/me", "/x"}},
		{"copy", def(loc(a, "p"), loc(domain.RemoteID{}, "/x"), withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY)), []domain.RemoteID{a}, []string{"copy", "p", "/x"}},
		{"move with move options", def(loc(a, "p"), loc(domain.RemoteID{}, "/x"), withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE), withOptions(&syncrv1.RcloneTransferOptions{
			DeleteEmptySrcDirs: true, Transfers: i32(64), Checkers: i32(1), Retries: i32(100), LowLevelRetries: i32(0),
			BandwidthLimitBytesPerSecond: i64(0), MaxTransferBytes: i64(1), MaxDuration: durationpb.New(time.Hour),
			Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}, {Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "/photos/**"}},
		})), []domain.RemoteID{a}, []string{"move", "p", "/x"}},
		{"sync with track renames", def(loc(a, "p"), loc(domain.RemoteID{}, "/x"), withOptions(&syncrv1.RcloneTransferOptions{TrackRenames: true, Checksum: true})), []domain.RemoteID{a}, []string{"sync", "p", "/x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refs, err := jobkind.Transfer{}.Check(tt.def)
			if err != nil {
				t.Fatalf("Check = %v", err)
			}
			if !slices.Equal(refs.RemoteIDs, tt.refs) || !slices.Equal(refs.SearchTerms, tt.terms) {
				t.Errorf("refs = %+v, want %v %q", refs, tt.refs, tt.terms)
			}
		})
	}
}

func TestTransferCheckRejects(t *testing.T) {
	t.Parallel()
	a := rid(1)
	local := func(p string) *syncrv1.JobLocation { return loc(domain.RemoteID{}, p) }
	ok := local("/dst")
	const f = "definition.rclone_transfer"
	opts := func(o *syncrv1.RcloneTransferOptions) *syncrv1.JobDefinition {
		return def(loc(a, "src"), ok, withOptions(o))
	}
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want []string
	}{
		{"no transfer", &syncrv1.JobDefinition{}, []string{f}},
		{"nil transfer", &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{}}, []string{f}},
		{"unspecified mode", def(loc(a, ""), ok, withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_UNSPECIFIED)), []string{f + ".mode"}},
		{"unknown mode", def(loc(a, ""), ok, withMode(99)), []string{f + ".mode"}},
		{"no source", def(nil, ok), []string{f + ".source"}},
		{"no destination", def(loc(a, ""), nil), []string{f + ".destination"}},
		{"neither", def(nil, nil), []string{f + ".source", f + ".destination"}},
		{"bad remote id", def(&syncrv1.JobLocation{RemoteId: "photos", Path: "x"}, ok), []string{f + ".source.remote_id"}},
		{"zero remote id", def(&syncrv1.JobLocation{RemoteId: "00000000-0000-0000-0000-000000000000"}, ok), []string{f + ".source.remote_id"}},
		{"dot-dot on a remote", def(loc(a, "a/../b"), ok), []string{f + ".source.path"}},
		{"backslash dot-dot on a remote", def(loc(a, `a\..\b`), ok), []string{f + ".source.path"}},
		{"leading dot-dot on a remote", def(loc(a, "../etc"), ok), []string{f + ".source.path"}},
		{"NUL on a remote", def(loc(a, "a\x00b"), ok), []string{f + ".source.path"}},
		{"line break on a remote", def(loc(a, "a\nb"), ok), []string{f + ".source.path"}},
		{"too long on a remote", def(loc(a, strings.Repeat("a", backend.MaxPathLen+1)), ok), []string{f + ".source.path"}},
		{"empty local", def(loc(a, ""), local("")), []string{f + ".destination.path"}},
		{"relative local", def(loc(a, ""), local("data/x")), []string{f + ".destination.path"}},
		{"drive-relative local", def(loc(a, ""), local("C:data")), []string{f + ".destination.path"}},
		{"dot-dot local", def(loc(a, ""), local("/data/../etc")), []string{f + ".destination.path"}},
		{"windows dot-dot local", def(loc(a, ""), local(`C:\data\..\Windows`)), []string{f + ".destination.path"}},
		{"incomplete UNC", def(loc(a, ""), local(`\\server`)), []string{f + ".destination.path"}},
		{"NUL local", def(loc(a, ""), local("/data\x00")), []string{f + ".destination.path"}},
		{"same remote location", def(loc(a, "x/y"), loc(a, "x//y/")), []string{f + ".destination"}},
		{"same local location", def(local("/data"), local("/data/./")), []string{f + ".destination"}},
		{"transfers 0", opts(&syncrv1.RcloneTransferOptions{Transfers: i32(0)}), []string{f + ".options.transfers"}},
		{"transfers 65", opts(&syncrv1.RcloneTransferOptions{Transfers: i32(65)}), []string{f + ".options.transfers"}},
		{"checkers 257", opts(&syncrv1.RcloneTransferOptions{Checkers: i32(257)}), []string{f + ".options.checkers"}},
		{"retries 0", opts(&syncrv1.RcloneTransferOptions{Retries: i32(0)}), []string{f + ".options.retries"}},
		{"low level retries -1", opts(&syncrv1.RcloneTransferOptions{LowLevelRetries: i32(-1)}), []string{f + ".options.low_level_retries"}},
		{"negative bandwidth", opts(&syncrv1.RcloneTransferOptions{BandwidthLimitBytesPerSecond: i64(-1)}), []string{f + ".options.bandwidth_limit_bytes_per_second"}},
		{"zero max transfer", opts(&syncrv1.RcloneTransferOptions{MaxTransferBytes: i64(0)}), []string{f + ".options.max_transfer_bytes"}},
		{"zero max duration", opts(&syncrv1.RcloneTransferOptions{MaxDuration: durationpb.New(0)}), []string{f + ".options.max_duration"}},
		{"huge max duration", opts(&syncrv1.RcloneTransferOptions{MaxDuration: durationpb.New(367 * 24 * time.Hour)}), []string{f + ".options.max_duration"}},
		{"invalid max duration", opts(&syncrv1.RcloneTransferOptions{MaxDuration: &durationpb.Duration{Seconds: 1, Nanos: -1}}), []string{f + ".options.max_duration"}},
		{"checksum and size only", opts(&syncrv1.RcloneTransferOptions{Checksum: true, SizeOnly: true}), []string{f + ".options.size_only"}},
		{"delete empty src dirs on sync", opts(&syncrv1.RcloneTransferOptions{DeleteEmptySrcDirs: true}), []string{f + ".options.delete_empty_src_dirs"}},
		{"track renames on copy", def(loc(a, "s"), ok, withMode(syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY), withOptions(&syncrv1.RcloneTransferOptions{TrackRenames: true})), []string{f + ".options.track_renames"}},
		{"filter without action", opts(&syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Pattern: "*.tmp"}}}), []string{f + ".options.filters[0].action"}},
		{"blank filter", opts(&syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "x"}, {Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "  "}}}), []string{f + ".options.filters[1].pattern"}},
		{"filter with a line break", opts(&syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "a\nb"}}}), []string{f + ".options.filters[0].pattern"}},
		{"long filter", opts(&syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: strings.Repeat("x", 4097)}}}), []string{f + ".options.filters[0].pattern"}},
		{"too many filters", opts(&syncrv1.RcloneTransferOptions{Filters: slices.Repeat([]*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "x"}}, 1001)}), []string{f + ".options.filters"}},
		{"everything at once", def(&syncrv1.JobLocation{RemoteId: "x"}, local("rel"), withMode(0)), []string{f + ".mode", f + ".source.remote_id", f + ".destination.path"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := jobkind.Transfer{}.Check(tt.def)
			if !errors.Is(err, domain.ErrInvalidArgument) || !slices.Equal(fields(err), tt.want) {
				t.Errorf("Check = %v, want fields %q", err, tt.want)
			}
			if _, err := (jobkind.Transfer{}).Build(tt.def, nil); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("Build = %v, want the Check error", err)
			}
		})
	}
}

// Resolved remotes of the build tests.
func s3Remote(id domain.RemoteID, root string) jobkind.Remote {
	return jobkind.Remote{ID: id, Rclone: backend.Rclone{
		Backend: "s3", Options: map[string]string{"provider": "Minio", "endpoint": "https://minio.example"},
		Secrets: map[string]string{"access_key_id": "AKIA", "secret_access_key": "s3-secret"}, Root: root,
	}}
}

func sftpRemote(id domain.RemoteID, root string) jobkind.Remote {
	return jobkind.Remote{ID: id, Rclone: backend.Rclone{
		Backend: "sftp", Options: map[string]string{"host": "nas.example", "user": "me"},
		Secrets: map[string]string{"pass": "sftp-secret"}, ObscureKeys: []string{"pass"}, Root: root,
	}}
}

func localRemote(id domain.RemoteID, root string) jobkind.Remote {
	return jobkind.Remote{ID: id, Rclone: backend.Rclone{Options: map[string]string{}, Secrets: map[string]string{}, Root: root}}
}

func unionRemote(id domain.RemoteID, members ...jobkind.Member) jobkind.Remote {
	return jobkind.Remote{ID: id, Rclone: backend.Rclone{Backend: "union", Options: map[string]string{"action_policy": "epall"}, Secrets: map[string]string{}}, Members: members}
}

func remoteMap(rs ...jobkind.Remote) map[domain.RemoteID]jobkind.Remote {
	m := make(map[domain.RemoteID]jobkind.Remote, len(rs))
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

func rdef(alias string, r jobkind.Remote, ups ...*syncrv1.UnionUpstream) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias: alias, BackendType: r.Rclone.Backend, Options: r.Rclone.Options, Secrets: r.Rclone.Secrets,
		ObscureKeys: r.Rclone.ObscureKeys, UnionUpstreams: ups,
	}
}

func sloc(remote, path string) *syncrv1.Location {
	return &syncrv1.Location{Remote: remote, Path: path}
}

func spec(src, dst *syncrv1.Location, opts *syncrv1.RcloneTransferOptions, defs ...*syncrv1.RemoteDefinition) *syncrv1.JobSpec {
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC, Source: src, Destination: dst, Options: opts,
	}}}
}

func TestTransferBuild(t *testing.T) {
	t.Parallel()
	s3, sftp, lcl, win, u := rid(1), rid(2), rid(3), rid(4), rid(5)
	S3, SFTP, LCL, WIN := s3Remote(s3, "bucket/base"), sftpRemote(sftp, "/home/me"), localRemote(lcl, "/srv/photos"), localRemote(win, `D:\Photos`)
	// The union's members: the s3 remote, a local remote and the sftp
	// remote, which the job also uses directly.
	U := unionRemote(u,
		jobkind.Member{RemoteID: s3, Path: "pool", Mode: backend.UnionReadWrite},
		jobkind.Member{RemoteID: lcl, Path: "disk1", Mode: backend.UnionReadOnly},
		jobkind.Member{RemoteID: sftp, Mode: backend.UnionNoCreate},
	)
	all := remoteMap(S3, SFTP, LCL, WIN, U)
	opts := &syncrv1.RcloneTransferOptions{DryRun: true, Transfers: i32(8), Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}}}
	none := domain.RemoteID{}
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want *syncrv1.JobSpec
	}{
		{"local path to s3", def(loc(none, "/data/src/"), loc(s3, "/photos/2026")),
			spec(sloc("", "/data/src"), sloc("r1", "bucket/base/photos/2026"), nil, rdef("r1", S3))},
		{"s3 root to local path", def(loc(s3, ""), loc(none, `C:\Restore`)),
			spec(sloc("r1", "bucket/base"), sloc("", `C:\Restore`), nil, rdef("r1", S3))},
		{"local remote to sftp", def(loc(lcl, "2026/06"), loc(sftp, "backup")),
			spec(sloc("", "/srv/photos/2026/06"), sloc("r1", "/home/me/backup"), nil, rdef("r1", SFTP))},
		{"windows local remote", def(loc(win, "2026/06"), loc(none, "/x")),
			spec(sloc("", `D:\Photos\2026\06`), sloc("", "/x"), nil)},
		{"two remotes in reference order", def(loc(sftp, "a"), loc(s3, "b"), withOptions(opts)),
			spec(sloc("r1", "/home/me/a"), sloc("r2", "bucket/base/b"), opts, rdef("r1", SFTP), rdef("r2", S3))},
		{"one remote twice", def(loc(s3, "a"), loc(s3, "b")),
			spec(sloc("r1", "bucket/base/a"), sloc("r1", "bucket/base/b"), nil, rdef("r1", S3))},
		{"union, then its members", def(loc(u, "albums"), loc(sftp, "copy")),
			spec(sloc("r1", "albums"), sloc("r2", "/home/me/copy"), nil,
				rdef("r1", U,
					&syncrv1.UnionUpstream{Location: sloc("r3", "bucket/base/pool"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE},
					&syncrv1.UnionUpstream{Location: sloc("", "/srv/photos/disk1"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
					&syncrv1.UnionUpstream{Location: sloc("r2", "/home/me"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE},
				),
				rdef("r2", SFTP), rdef("r3", S3))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := jobkind.Transfer{}.Build(tt.def, all)
			if err != nil {
				t.Fatalf("Build = %v", err)
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("Build =\n%v\nwant\n%v", got, tt.want)
			}
			again, err := jobkind.Transfer{}.Build(tt.def, all)
			if err != nil || !proto.Equal(again, got) {
				t.Errorf("Build is not deterministic: %v", err)
			}
		})
	}
}

func TestTransferBuildDoesNotAlias(t *testing.T) {
	t.Parallel()
	s3 := rid(1)
	r := sftpRemote(s3, "")
	opts := &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "x"}}}
	d := def(loc(s3, "a"), loc(domain.RemoteID{}, "/b"), withOptions(opts))
	got, err := jobkind.Transfer{}.Build(d, remoteMap(r))
	if err != nil {
		t.Fatal(err)
	}
	got.Remotes[0].Secrets["pass"] = "mutated"
	got.Remotes[0].Options["host"] = "mutated"
	got.Remotes[0].ObscureKeys[0] = "mutated"
	got.GetRcloneTransfer().Options.Filters[0].Pattern = "mutated"
	if r.Rclone.Secrets["pass"] != "sftp-secret" || r.Rclone.Options["host"] != "nas.example" || r.Rclone.ObscureKeys[0] != "pass" || opts.Filters[0].Pattern != "x" {
		t.Error("the spec aliases the resolved remote or the definition")
	}
}

func TestTransferBuildRejects(t *testing.T) {
	t.Parallel()
	a, b, u, inner := rid(1), rid(2), rid(5), rid(6)
	none := domain.RemoteID{}
	member := func(id domain.RemoteID, mode backend.UnionMode) jobkind.Member {
		return jobkind.Member{RemoteID: id, Mode: mode}
	}
	misnamed := s3Remote(b, "")
	tests := []struct {
		name    string
		def     *syncrv1.JobDefinition
		remotes map[domain.RemoteID]jobkind.Remote
		want    error
		field   string
	}{
		{"unresolved remote", def(loc(a, "x"), loc(none, "/y")), nil, domain.ErrNotFound, ""},
		{"unresolved member", def(loc(u, "x"), loc(none, "/y")),
			remoteMap(unionRemote(u, member(a, backend.UnionReadWrite), member(b, backend.UnionReadWrite)), s3Remote(a, "")), domain.ErrNotFound, ""},
		{"resolved under another id", def(loc(a, "x"), loc(none, "/y")), map[domain.RemoteID]jobkind.Remote{a: misnamed}, domain.ErrInvalidArgument, ""},
		{"nested union", def(loc(u, "x"), loc(none, "/y")),
			remoteMap(unionRemote(u, member(inner, backend.UnionReadWrite), member(a, backend.UnionReadWrite)), unionRemote(inner, member(a, backend.UnionReadWrite)), s3Remote(a, "")), domain.ErrInvalidArgument, ""},
		{"union without members", def(loc(u, "x"), loc(none, "/y")), remoteMap(unionRemote(u)), domain.ErrInvalidArgument, ""},
		{"members of a non-union", def(loc(a, "x"), loc(none, "/y")),
			remoteMap(jobkind.Remote{ID: a, Rclone: backend.Rclone{Backend: "s3"}, Members: []jobkind.Member{member(b, backend.UnionReadWrite)}}), domain.ErrInvalidArgument, ""},
		{"unknown member mode", def(loc(u, "x"), loc(none, "/y")),
			remoteMap(unionRemote(u, member(a, "write_only"), member(b, backend.UnionReadWrite)), s3Remote(a, ""), s3Remote(b, "")), domain.ErrInvalidArgument, ""},
		{"local remote with a relative root", def(loc(a, "x"), loc(none, "/y")), remoteMap(localRemote(a, "srv")), domain.ErrInvalidArgument, "definition.rclone_transfer.source.path"},
		{"root with dot-dot", def(loc(none, "/y"), loc(a, "x")), remoteMap(s3Remote(a, "bucket/../other")), domain.ErrInvalidArgument, "definition.rclone_transfer.destination.path"},
		{"member path with dot-dot", def(loc(u, "x"), loc(none, "/y")),
			remoteMap(unionRemote(u, jobkind.Member{RemoteID: a, Path: "../up", Mode: backend.UnionReadWrite}, member(b, backend.UnionReadWrite)), s3Remote(a, ""), s3Remote(b, "")), domain.ErrInvalidArgument, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := jobkind.Transfer{}.Build(tt.def, tt.remotes)
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
