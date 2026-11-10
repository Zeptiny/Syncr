package jobkind_test

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

func TestLocate(t *testing.T) {
	t.Parallel()
	s3, sftp, lcl, win, u := rid(1), rid(2), rid(3), rid(4), rid(5)
	S3, SFTP, LCL, WIN := s3Remote(s3, "bucket/base"), sftpRemote(sftp, "/home/me"), localRemote(lcl, "/srv/backups"), localRemote(win, `D:\Backups`)
	U := unionRemote(u,
		jobkind.Member{RemoteID: s3, Path: "pool", Mode: backend.UnionReadWrite},
		jobkind.Member{RemoteID: lcl, Path: "disk1", Mode: backend.UnionReadOnly},
	)
	all := remoteMap(S3, SFTP, LCL, WIN, U)
	tests := []struct {
		name string
		id   domain.RemoteID
		path string
		defs []*syncrv1.RemoteDefinition
		loc  *syncrv1.Location
	}{
		{name: "s3 below its root", id: s3, path: "restic/photos", defs: []*syncrv1.RemoteDefinition{rdef("r1", S3)}, loc: sloc("r1", "bucket/base/restic/photos")},
		{name: "s3 root", id: s3, defs: []*syncrv1.RemoteDefinition{rdef("r1", S3)}, loc: sloc("r1", "bucket/base")},
		{name: "sftp, cleaned", id: sftp, path: "/repo//x/", defs: []*syncrv1.RemoteDefinition{rdef("r1", SFTP)}, loc: sloc("r1", "/home/me/repo/x")},
		{name: "local remote", id: lcl, path: "photos", loc: sloc("", "/srv/backups/photos")},
		{name: "windows local remote", id: win, path: "photos/2026", loc: sloc("", `D:\Backups\photos\2026`)},
		{name: "union and its members", id: u, path: "repo", defs: []*syncrv1.RemoteDefinition{
			rdef("r1", U,
				&syncrv1.UnionUpstream{Location: sloc("r2", "bucket/base/pool"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE},
				&syncrv1.UnionUpstream{Location: sloc("", "/srv/backups/disk1"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
			),
			rdef("r2", S3),
		}, loc: sloc("r1", "repo")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defs, loc, err := jobkind.Locate(tt.id, tt.path, all)
			if err != nil {
				t.Fatalf("Locate = %v", err)
			}
			if !proto.Equal(loc, tt.loc) {
				t.Errorf("location = %v, want %v", loc, tt.loc)
			}
			if len(defs) != len(tt.defs) {
				t.Fatalf("definitions = %v, want %v", defs, tt.defs)
			}
			for i := range defs {
				if !proto.Equal(defs[i], tt.defs[i]) {
					t.Errorf("definition %d = %v, want %v", i, defs[i], tt.defs[i])
				}
			}
		})
	}
}

func TestLocateRejects(t *testing.T) {
	t.Parallel()
	s3, lcl, u := rid(1), rid(3), rid(5)
	tests := []struct {
		name    string
		id      domain.RemoteID
		path    string
		remotes map[domain.RemoteID]jobkind.Remote
		want    error
		isPath  bool
	}{
		{name: "no remote", want: domain.ErrInvalidArgument},
		{name: "unresolved", id: s3, remotes: nil, want: domain.ErrNotFound},
		{name: "unresolved member", id: u, remotes: remoteMap(unionRemote(u, jobkind.Member{RemoteID: s3, Mode: backend.UnionReadWrite})), want: domain.ErrNotFound},
		{name: "dot-dot on a remote", id: s3, path: "a/../b", remotes: remoteMap(s3Remote(s3, "")), want: domain.ErrInvalidArgument, isPath: true},
		{name: "dot-dot on a local remote", id: lcl, path: `a\..\b`, remotes: remoteMap(localRemote(lcl, `C:\x`)), want: domain.ErrInvalidArgument, isPath: true},
		{name: "relative local root", id: lcl, path: "a", remotes: remoteMap(localRemote(lcl, "srv")), want: domain.ErrInvalidArgument, isPath: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := jobkind.Locate(tt.id, tt.path, tt.remotes)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Locate = %v, want %v", err, tt.want)
			}
			if pe := (*backend.PathError)(nil); errors.As(err, &pe) != tt.isPath {
				t.Errorf("Locate = %v; a *backend.PathError: %v, want %v", err, !tt.isPath, tt.isPath)
			}
			for _, secret := range []string{"s3-secret", "AKIA", "minio.example"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q contains a value of the remote", err)
				}
			}
		})
	}
}

func TestLocateRepository(t *testing.T) {
	t.Parallel()
	s3 := rid(1)
	S3 := s3Remote(s3, "bucket")
	repo := jobkind.Repository{ID: domain.NewResticRepoID(), RemoteID: s3, Path: "restic/photos", Password: "repo-secret", ResticID: strings.Repeat("ab", 32)}
	defs, rr, err := jobkind.LocateRepository(repo, remoteMap(S3))
	if err != nil {
		t.Fatal(err)
	}
	want := &syncrv1.ResticRepository{Location: sloc("r1", "bucket/restic/photos"), Password: "repo-secret"}
	if !proto.Equal(rr, want) || len(defs) != 1 || !proto.Equal(defs[0], rdef("r1", S3)) {
		t.Errorf("LocateRepository = (%v, %v)", defs, rr)
	}
	_, _, err = jobkind.LocateRepository(repo, nil)
	if !errors.Is(err, domain.ErrNotFound) || strings.Contains(err.Error(), "repo-secret") {
		t.Errorf("LocateRepository(unresolved) = %v", err)
	}
}

func TestResticRepoKey(t *testing.T) {
	t.Parallel()
	id, err := domain.ParseResticRepoID("0195f0a4-3b7c-7d1e-9f00-123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	if got := jobkind.ResticRepoKey(id); got != "restic-repo:0195f0a4-3b7c-7d1e-9f00-123456789abc" ||
		!strings.HasPrefix(got, jobkind.ResticRepoKeyPrefix) {
		t.Errorf("ResticRepoKey = %q", got)
	}
}

func TestBuiltinLabels(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Label("rclone_transfer"); got != "Transfer" {
		t.Errorf("Label(rclone_transfer) = %q", got)
	}
	if ok, why := r.Schedulable("rclone_transfer"); !ok || why != "" {
		t.Errorf("Schedulable(rclone_transfer) = (%v, %q)", ok, why)
	}
	if ok, why := r.UserStartable("rclone_transfer"); !ok || why != "" {
		t.Errorf("UserStartable(rclone_transfer) = (%v, %q)", ok, why)
	}
	if got := r.RequiredFeatures("rclone_transfer"); got != nil {
		t.Errorf("RequiredFeatures(rclone_transfer) = %q", got)
	}
}
