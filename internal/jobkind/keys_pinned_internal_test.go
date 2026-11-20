package jobkind

import (
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// pinnedRemotes are the fixed remotes of TestKeysOfExistingRemotesPinned:
// an S3 bucket, an SFTP server, a local folder and a union of the last two.
func pinnedRemotes() map[domain.RemoteID]Remote {
	s3, sftp, disk, u := testRemoteID(11), testRemoteID(12), testRemoteID(13), testRemoteID(14)
	return map[domain.RemoteID]Remote{
		s3: {ID: s3, Rclone: backend.Rclone{
			Backend: "s3", Root: "photos-bucket/base",
			Options: map[string]string{"provider": "Minio", "endpoint": "https://minio.example", "region": "us-east-1"},
			Secrets: map[string]string{"secret_access_key": "S3-SECRET"},
		}},
		sftp: {ID: sftp, Rclone: backend.Rclone{
			Backend: "sftp", Root: "/home/me",
			Options: map[string]string{"host": "nas.example", "user": "me", "port": "2222"},
			Secrets: map[string]string{"pass": "SFTP-SECRET"}, ObscureKeys: []string{"pass"},
		}},
		disk: {ID: disk, Rclone: backend.Rclone{Root: "/mnt/disk1", Options: map[string]string{}}},
		u: {ID: u, Rclone: backend.Rclone{Backend: "union", Options: map[string]string{"action_policy": "epall", "create_policy": "epmfs"}},
			Members: []Member{{RemoteID: sftp, Path: "pool", Mode: backend.UnionReadWrite}, {RemoteID: disk, Path: "p", Mode: backend.UnionNoCreate}}},
	}
}

// TestKeysOfExistingRemotesPinned pins the keys of existing remotes
// (which wrap nothing), computed on the code from before wrapper remotes
// existed: the two-way sync state key, the concurrency keys of its sides
// (from Check and from CheckWith with the remotes' infos) and the folder
// key and concurrency key of a backup of a remote. A change here would
// resync every pair and start every mirror anew.
func TestKeysOfExistingRemotesPinned(t *testing.T) {
	t.Parallel()
	rs := pinnedRemotes()
	s3, sftp, u := testRemoteID(11), testRemoteID(12), testRemoteID(14)
	reg, err := NewRegistry(Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	infos := map[domain.RemoteID]RemoteInfo{s3: {}, sftp: {}, u: {}, testRemoteID(13): {}}

	for _, tt := range []struct {
		name      string
		def       *syncrv1.JobDefinition
		stateKey  string
		locations []string
	}{
		{"a union and s3", bisyncDef(&syncrv1.JobLocation{RemoteId: u.String(), Path: "docs"}, &syncrv1.JobLocation{RemoteId: s3.String(), Path: "shared/docs"}),
			"3f935a35f665a4f2f5798f5cf41855e0275955f5ccba1b379719ced3cda8d451",
			[]string{"bisync:40a0c122971f41f66d23a12f068cd2bc294afa8d", "bisync:b152d51e8a2f5cc0791958a755b523f016ee8cfd"}},
		{"a worker path and sftp", bisyncDef(&syncrv1.JobLocation{Path: "/home/me/Shared"}, &syncrv1.JobLocation{RemoteId: sftp.String(), Path: "shared"}),
			"64491be359b7a00e4a0c25dcb99da3a3bd76e0947887730fb49355d662232234",
			[]string{"bisync:90fe916a670fafee2cf83f36481c5910d00fd619", "bisync:52ca1e5ba3ae734f4fda21d57e9d8b443b580d61"}},
	} {
		spec, err := Bisync{}.Build(tt.def, Resolved{Remotes: rs})
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := spec.GetRcloneBisync().GetStateKey(); got != tt.stateKey {
			t.Errorf("%s: state key %s, want %s", tt.name, got, tt.stateKey)
		}
		if got := (Bisync{}).ConcurrencyKeys(tt.def); !slices.Equal(got, tt.locations) {
			t.Errorf("%s: concurrency keys %q, want %q", tt.name, got, tt.locations)
		}
		want := slices.Sorted(slices.Values(tt.locations))
		for what, check := range map[string]func() (string, Refs, error){
			"Check":               func() (string, Refs, error) { return reg.Check(tt.def) },
			"CheckWith(infos)":    func() (string, Refs, error) { return reg.CheckWith(tt.def, infos) },
			"CheckWith(no infos)": func() (string, Refs, error) { return reg.CheckWith(tt.def, nil) },
		} {
			if _, refs, err := check(); err != nil || !slices.Equal(refs.ConcurrencyKeys, want) {
				t.Errorf("%s: %s keys = %q (%v), want %q", tt.name, what, refs.ConcurrencyKeys, err, want)
			}
		}
	}

	rsrc, err := newSpecRemotes([]domain.RemoteID{sftp, u}, rs)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[domain.RemoteID]string{sftp: "62cf79b7585b34cdd1e7b6614479273b", u: "9a3bb2625348000d4bd6ebf0b6cdd4e7"} {
		if key, err := rsrc.sourceKey(rs[id], "Photos/2026"); err != nil || key != want {
			t.Errorf("source key of %s = %s (%v), want %s", id, key, err, want)
		}
	}
	if got, want := remoteSourceLocationKey(location{remote: sftp, key: "Photos/2026", ok: true}),
		"remote-source:d3dbf606763c0006fa8d54fd4c503e5ffb4f0760"; got != want {
		t.Errorf("source location key = %s, want %s", got, want)
	}
}

func bisyncDef(p1, p2 *syncrv1.JobLocation) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{Path1: p1, Path2: p2}}}
}

// TestStateInputOfWrapper: a side on a wrapper remote is encoded as any
// remote, followed by "wrapped", the wrapped remote and the joined folder
// on it; secrets stay out.
func TestStateInputOfWrapper(t *testing.T) {
	t.Parallel()
	rs := pinnedRemotes()
	s3, vault := testRemoteID(11), testRemoteID(15)
	rs[vault] = Remote{ID: vault, Rclone: backend.Rclone{
		Backend: "crypt", Options: map[string]string{"filename_encryption": "standard"},
		Secrets: map[string]string{"password": "CRYPT-SECRET"}, ObscureKeys: []string{"password"},
	}, Wrapped: Wrap{RemoteID: s3, Path: "vault"}, NoChecksum: true}
	spec, err := newSpecRemotes([]domain.RemoteID{vault}, rs)
	if err != nil {
		t.Fatal(err)
	}
	in, err := spec.bisyncStateInput([2]bisyncSide{
		{loc: &syncrv1.Location{Path: "/home/me/Docs"}},
		{loc: &syncrv1.Location{Remote: "r1", Path: "docs"}, remote: vault},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "21:syncr-bisync-state-v1," +
		"5:local,13:/home/me/Docs," +
		"6:remote,36:" + vault.String() + ",5:crypt,1:1,19:filename_encryption,8:standard,1:0," +
		"7:wrapped," +
		"36:" + s3.String() + ",2:s3,1:3,8:endpoint,21:https://minio.example,8:provider,5:Minio,6:region,9:us-east-1,1:0," +
		"24:photos-bucket/base/vault," +
		"4:docs,"
	if string(in) != want {
		t.Errorf("input =\n%s\nwant\n%s", in, want)
	}
}
