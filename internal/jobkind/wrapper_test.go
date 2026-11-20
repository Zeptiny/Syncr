package jobkind_test

import (
	"errors"
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

// cryptRemote is an encrypted remote that stores its data at path on the
// remote wrapped.
func cryptRemote(id, wrapped domain.RemoteID, path string) jobkind.Remote {
	return jobkind.Remote{ID: id, Rclone: backend.Rclone{
		Backend: "crypt", Options: map[string]string{"filename_encryption": "standard"},
		Secrets: map[string]string{"password": "crypt-secret"}, ObscureKeys: []string{"password"},
	}, Wrapped: jobkind.Wrap{RemoteID: wrapped, Path: path}, NoChecksum: true}
}

// cryptDef is the RemoteDefinition of a crypt remote under alias, storing
// its data at wrapped.
func cryptDef(alias string, r jobkind.Remote, wrapped *syncrv1.Location) *syncrv1.RemoteDefinition {
	d := rdef(alias, r)
	d.Wrapped = wrapped
	return d
}

func TestWrapperBuild(t *testing.T) {
	t.Parallel()
	s3, sftp, lcl, u, vault, other := rid(1), rid(2), rid(3), rid(5), rid(6), rid(7)
	S3, SFTP, LCL := s3Remote(s3, "bucket/base"), sftpRemote(sftp, "/home/me"), localRemote(lcl, "/srv/photos")
	U := unionRemote(u, jobkind.Member{RemoteID: s3, Path: "pool", Mode: backend.UnionReadWrite},
		jobkind.Member{RemoteID: lcl, Path: "disk1", Mode: backend.UnionReadOnly})
	VAULT, OTHER := cryptRemote(vault, s3, "vault"), cryptRemote(other, sftp, "/abs/secret")
	all := remoteMap(S3, SFTP, LCL, U, VAULT, OTHER)
	none := domain.RemoteID{}
	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		want *syncrv1.JobSpec
	}{
		{"local to crypt: the wrapped remote is aliased after it", def(loc(none, "/srv/docs"), loc(vault, "docs")),
			spec(sloc("", "/srv/docs"), sloc("r1", "docs"), nil,
				cryptDef("r1", VAULT, sloc("r2", "bucket/base/vault")), rdef("r2", S3))},
		{"crypt and its wrapped remote both used", def(loc(vault, ""), loc(s3, "plain")),
			spec(sloc("r1", ""), sloc("r2", "bucket/base/plain"), nil,
				cryptDef("r1", VAULT, sloc("r2", "bucket/base/vault")), rdef("r2", S3))},
		{"union members first, then wrapped remotes", def(loc(u, "x"), loc(other, "y")),
			spec(sloc("r1", "x"), sloc("r2", "y"), nil,
				rdef("r1", U,
					&syncrv1.UnionUpstream{Location: sloc("r3", "bucket/base/pool"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE},
					&syncrv1.UnionUpstream{Location: sloc("", "/srv/photos/disk1"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY}),
				cryptDef("r2", OTHER, sloc("r4", "/home/me/abs/secret")), rdef("r3", S3), rdef("r4", SFTP))},
		{"two crypt remotes", def(loc(vault, "a"), loc(other, "b")),
			spec(sloc("r1", "a"), sloc("r2", "b"), nil,
				cryptDef("r1", VAULT, sloc("r3", "bucket/base/vault")), cryptDef("r2", OTHER, sloc("r4", "/home/me/abs/secret")),
				rdef("r3", S3), rdef("r4", SFTP))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := jobkind.Transfer{}.Build(tt.def, jobkind.Resolved{Remotes: all})
			if err != nil {
				t.Fatalf("Build = %v", err)
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("Build =\n%v\nwant\n%v", got, tt.want)
			}
			if f := jobkind.RemoteFeatures(got.GetRemotes()); !slices.Equal(f, []string{link.FeatureRcloneCrypt}) {
				t.Errorf("RemoteFeatures = %q", f)
			}
		})
	}
	// Locate: the crypt remote, then the remote it stores its data in.
	defs, l, err := jobkind.Locate(vault, "docs/2026", all)
	if err != nil || !proto.Equal(l, sloc("r1", "docs/2026")) || len(defs) != 2 ||
		!proto.Equal(defs[0], cryptDef("r1", VAULT, sloc("r2", "bucket/base/vault"))) || !proto.Equal(defs[1], rdef("r2", S3)) {
		t.Errorf("Locate(crypt) = %v, %v, %v", defs, l, err)
	}
	// Nothing else needs a feature.
	plain, err := jobkind.Transfer{}.Build(def(loc(u, "x"), loc(sftp, "y")), jobkind.Resolved{Remotes: all})
	if err != nil || jobkind.RemoteFeatures(plain.GetRemotes()) != nil {
		t.Errorf("RemoteFeatures(no crypt) = %q (%v)", jobkind.RemoteFeatures(plain.GetRemotes()), err)
	}
}

func TestWrapperShapeRejected(t *testing.T) {
	t.Parallel()
	s3, lcl, u, vault, inner := rid(1), rid(3), rid(5), rid(6), rid(7)
	S3, LCL := s3Remote(s3, "bucket"), localRemote(lcl, "/srv")
	U := unionRemote(u, jobkind.Member{RemoteID: s3, Mode: backend.UnionReadWrite})
	tests := []struct {
		name    string
		remotes []jobkind.Remote
		want    error
	}{
		{"wraps a local remote", []jobkind.Remote{cryptRemote(vault, lcl, "x"), LCL}, domain.ErrInvalidArgument},
		{"wraps a union", []jobkind.Remote{cryptRemote(vault, u, "x"), U, S3}, domain.ErrInvalidArgument},
		{"wraps a wrapper", []jobkind.Remote{cryptRemote(vault, inner, "x"), cryptRemote(inner, s3, "y"), S3}, domain.ErrInvalidArgument},
		{"wraps itself", []jobkind.Remote{cryptRemote(vault, vault, "x")}, domain.ErrInvalidArgument},
		{"wrapped remote not resolved", []jobkind.Remote{cryptRemote(vault, s3, "x")}, domain.ErrNotFound},
		{"a dot-dot folder", []jobkind.Remote{cryptRemote(vault, s3, "a/../b"), S3}, domain.ErrInvalidArgument},
		{"a union that wraps", []jobkind.Remote{func() jobkind.Remote {
			r := cryptRemote(vault, s3, "x")
			r.Rclone.Backend, r.Members = "union", []jobkind.Member{{RemoteID: s3, Mode: backend.UnionReadWrite}}
			return r
		}(), S3}, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := jobkind.Transfer{}.Build(def(loc(vault, "a"), loc(domain.RemoteID{}, "/b")), jobkind.Resolved{Remotes: remoteMap(tt.remotes...)})
			if !errors.Is(err, tt.want) {
				t.Errorf("Build = %v, want %v", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "crypt-secret") {
				t.Errorf("the error contains a secret: %v", err)
			}
		})
	}
	// A union cannot contain a wrapper.
	withCrypt := unionRemote(u, jobkind.Member{RemoteID: vault, Mode: backend.UnionReadWrite})
	_, err := jobkind.Transfer{}.Build(def(loc(u, "a"), loc(domain.RemoteID{}, "/b")),
		jobkind.Resolved{Remotes: remoteMap(withCrypt, cryptRemote(vault, s3, "x"), S3)})
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Build(union of a crypt remote) = %v", err)
	}
}

// infosOf returns the remote infos of resolved remotes, as the remotes
// service's Infos gives them.
func infosOf(rs ...jobkind.Remote) map[domain.RemoteID]jobkind.RemoteInfo {
	out := map[domain.RemoteID]jobkind.RemoteInfo{}
	for _, r := range rs {
		out[r.ID] = jobkind.RemoteInfo{Wraps: r.Wrapped, NoChecksum: r.NoChecksum}
	}
	return out
}

func TestNoChecksumRefused(t *testing.T) {
	t.Parallel()
	reg, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	s3, vault := rid(1), rid(6)
	S3, VAULT := s3Remote(s3, "bucket"), cryptRemote(vault, s3, "vault")
	all := remoteMap(S3, VAULT)
	infos := infosOf(S3, VAULT)
	none := domain.RemoteID{}
	checksum := withOptions(&syncrv1.RcloneTransferOptions{Checksum: true})
	compare := func(c ...syncrv1.BisyncCompare) func(*syncrv1.RcloneBisyncDefinition) {
		return withBisyncOptions(&syncrv1.RcloneBisyncOptions{Compare: c})
	}
	size, modtime, sum := syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE, syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME, syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM
	tests := []struct {
		name  string
		def   *syncrv1.JobDefinition
		field string // "" when accepted
	}{
		{"checksum from crypt", def(loc(vault, "a"), loc(none, "/b"), checksum), "definition.rclone_transfer.options.checksum"},
		{"checksum to crypt", def(loc(none, "/b"), loc(vault, "a"), checksum), "definition.rclone_transfer.options.checksum"},
		{"checksum between plain remotes", def(loc(s3, "a"), loc(none, "/b"), checksum), ""},
		{"crypt without checksum", def(loc(vault, "a"), loc(s3, "b"), withOptions(&syncrv1.RcloneTransferOptions{SizeOnly: true})), ""},
		{"two-way sync comparing checksums", twoWay(local("/a"), loc(vault, "b"), compare(size, sum)), "definition.rclone_bisync.options.compare[1]"},
		{"two-way sync on path1", twoWay(loc(vault, "b"), local("/a"), compare(sum)), "definition.rclone_bisync.options.compare[0]"},
		{"two-way sync comparing sizes and times", twoWay(local("/a"), loc(vault, "b"), compare(size, modtime)), ""},
		{"two-way sync of plain remotes", twoWay(local("/a"), loc(s3, "b"), compare(sum)), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Without infos (Check), nothing is known of the remotes.
			if _, _, err := reg.Check(tt.def); err != nil {
				t.Fatalf("Check = %v", err)
			}
			_, _, err := reg.CheckWith(tt.def, infos)
			_, buildErr := reg.Build(tt.def, jobkind.Resolved{Remotes: all})
			if tt.field == "" {
				if err != nil || buildErr != nil {
					t.Errorf("CheckWith = %v, Build = %v; want both accepted", err, buildErr)
				}
				return
			}
			for what, err := range map[string]error{"CheckWith": err, "Build": buildErr} {
				var verr *domain.ValidationError
				if !errors.As(err, &verr) || !slices.Equal(fields(err), []string{tt.field}) ||
					verr.Fields[0].Message != "is not available: a remote that keeps no checksums" {
					t.Errorf("%s = %v, want %s with the generic message", what, err, tt.field)
				}
			}
		})
	}
}

func TestBisyncNestedThroughWrapper(t *testing.T) {
	t.Parallel()
	reg, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	s3, sftp, vault, twin, abs := rid(1), rid(2), rid(6), rid(7), rid(8)
	S3, SFTP := s3Remote(s3, "bucket"), sftpRemote(sftp, "")
	VAULT, TWIN, ABS := cryptRemote(vault, s3, "vault"), cryptRemote(twin, s3, "vault/"), cryptRemote(abs, sftp, "/srv/secret")
	all := remoteMap(S3, SFTP, VAULT, TWIN, ABS)
	infos := infosOf(S3, SFTP, VAULT, TWIN, ABS)
	tests := []struct {
		name   string
		def    *syncrv1.JobDefinition
		nested bool
	}{
		{"crypt root and its folder", twoWay(loc(vault, ""), loc(s3, "vault")), true},
		{"crypt root and a folder inside its folder", twoWay(loc(vault, ""), loc(s3, "vault/x")), true},
		{"a crypt folder and the same plain path", twoWay(loc(vault, "a"), loc(s3, "/vault/a/")), true},
		{"the wrapped remote's root and a crypt folder", twoWay(loc(s3, ""), loc(vault, "a")), true},
		{"a folder around the crypt folder", twoWay(loc(s3, "vault/a/b"), loc(vault, "a")), true},
		{"two crypt remotes over one folder", twoWay(loc(vault, "a"), loc(twin, "a/b")), true},
		{"an absolute wrapped folder", twoWay(loc(abs, "x"), loc(sftp, "/srv/secret/x/y")), true},
		{"beside the crypt folder", twoWay(loc(vault, ""), loc(s3, "vault2")), false},
		{"two crypt folders side by side", twoWay(loc(vault, "a"), loc(twin, "b")), false},
		{"another remote", twoWay(loc(vault, ""), loc(sftp, "vault")), false},
		{"a worker path", twoWay(local("/srv/vault"), loc(vault, "")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Check alone cannot know: the sides are on different remotes.
			if _, _, err := reg.Check(tt.def); err != nil {
				t.Fatalf("Check = %v", err)
			}
			_, _, err := reg.CheckWith(tt.def, infos)
			_, buildErr := reg.Build(tt.def, jobkind.Resolved{Remotes: all})
			for what, err := range map[string]error{"CheckWith": err, "Build": buildErr} {
				if !tt.nested {
					if err != nil {
						t.Errorf("%s = %v, want accepted", what, err)
					}
					continue
				}
				var verr *domain.ValidationError
				if !errors.As(err, &verr) || !slices.Equal(fields(err), []string{"definition.rclone_bisync.path2"}) ||
					verr.Fields[0].Message != "must differ from path1, and neither may be inside the other" {
					t.Errorf("%s = %v, want the nested sides refused on path2", what, err)
				}
			}
		})
	}
}

func TestBisyncKeysThroughWrapper(t *testing.T) {
	t.Parallel()
	reg, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	s3, sftp, vault := rid(1), rid(2), rid(6)
	infos := infosOf(s3Remote(s3, "bucket"), sftpRemote(sftp, ""), cryptRemote(vault, s3, "vault"))
	keysWith := func(def *syncrv1.JobDefinition, infos map[domain.RemoteID]jobkind.RemoteInfo) []string {
		t.Helper()
		_, refs, err := reg.CheckWith(def, infos)
		if err != nil {
			t.Fatalf("CheckWith = %v", err)
		}
		return refs.ConcurrencyKeys
	}
	// The crypt side "docs" holds the key of the plain folder vault/docs
	// on s3; the other side's key is unchanged.
	cryptPair := twoWay(local("/home/me/Docs"), loc(vault, "docs"))
	plainPair := twoWay(local("/home/me/Docs"), loc(s3, "vault/docs"))
	if got, want := keysWith(cryptPair, infos), keysWith(plainPair, infos); !slices.Equal(got, want) {
		t.Errorf("crypt side keys %q, want those of the folder its data is in, %q", got, want)
	}
	if got := (jobkind.Bisync{}).ConcurrencyKeysWith(cryptPair, infos); got[0] != (jobkind.Bisync{}).ConcurrencyKeys(cryptPair)[0] {
		t.Errorf("the worker path's key changed: %q", got)
	}
	// Without infos the crypt side is keyed as itself.
	if got, want := keysWith(cryptPair, nil), keysWith(plainPair, nil); slices.Equal(got, want) {
		t.Errorf("without infos the crypt side has the wrapped folder's key: %q", got)
	}
	// Remotes that wrap nothing keep their keys with infos.
	other := twoWay(loc(sftp, "x"), loc(s3, "y"))
	if got, want := keysWith(other, infos), keysWith(other, nil); !slices.Equal(got, want) {
		t.Errorf("keys of plain remotes changed with infos: %q, want %q", got, want)
	}
}

// TestWrapperStateKey: a crypt side's state key and a backup of a crypt
// folder's key follow the folder the remote stores its data in.
func TestWrapperStateKey(t *testing.T) {
	t.Parallel()
	s3, sftp, vault := rid(1), rid(2), rid(6)
	S3, SFTP := s3Remote(s3, "bucket"), sftpRemote(sftp, "")
	pair := twoWay(local("/home/me/Docs"), loc(vault, "docs"))
	base := stateKey(t, pair, S3, SFTP, cryptRemote(vault, s3, "vault"))
	rotated := cryptRemote(vault, s3, "vault")
	rotated.Rclone.Secrets = map[string]string{"password": "another"}
	if got := stateKey(t, pair, S3, SFTP, rotated); got != base {
		t.Error("the crypt password changed the state key")
	}
	moved := map[string]string{
		"another folder": stateKey(t, pair, S3, SFTP, cryptRemote(vault, s3, "vault2")),
		"another remote": stateKey(t, pair, S3, SFTP, cryptRemote(vault, sftp, "vault")),
		"the wrapped remote's root": func() string {
			s := s3Remote(s3, "other-bucket")
			return stateKey(t, pair, s, SFTP, cryptRemote(vault, s3, "vault"))
		}(),
		"a crypt option": func() string {
			r := cryptRemote(vault, s3, "vault")
			r.Rclone.Options = map[string]string{"filename_encryption": "off"}
			return stateKey(t, pair, S3, SFTP, r)
		}(),
	}
	for name, got := range moved {
		if got == base {
			t.Errorf("%s: the state key did not change", name)
		}
	}
}

// TestRemoteSourceThroughWrapper: a backup of a folder of a crypt remote
// is refused when the folder its data is stored in is, holds or lies
// inside the repository's folder, and gets a key that follows the wrapped
// folder.
func TestRemoteSourceThroughWrapper(t *testing.T) {
	t.Parallel()
	s3, src := rid(1), rid(5)
	S3 := s3Remote(s3, "bucket")
	build := func(wrapped, srcPath, repoPath string) (*syncrv1.JobSpec, error) {
		d := srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path = srcPath })
		return jobkind.ResticBackup{}.Build(d, srcResolved(t, s3, repoPath, S3, cryptRemote(src, s3, wrapped)))
	}
	for _, tt := range []struct {
		name                       string
		wrapped, srcPath, repoPath string
		want                       string
	}{
		{"the repository's folder", "restic", "repo", "restic/repo", jobkind.RemoteSourceIsRepository},
		{"around the repository", "restic", "", "restic/repo", jobkind.RemoteSourceHoldsRepository},
		{"inside the repository", "restic/repo", "data", "restic/repo", jobkind.RemoteSourceInsideRepository},
		{"beside it", "vault", "photos", "restic/repo", ""},
	} {
		spec, err := build(tt.wrapped, tt.srcPath, tt.repoPath)
		if tt.want == "" {
			if err != nil {
				t.Errorf("%s: Build = %v", tt.name, err)
				continue
			}
			if d := spec.GetRemotes(); len(d) != 2 || d[1].GetWrapped().GetRemote() != "r1" || d[1].GetWrapped().GetPath() != "bucket/vault" {
				t.Errorf("%s: remotes = %v", tt.name, d)
			}
			continue
		}
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || !slices.Equal(fields(err), []string{"definition.restic_backup.remote_source.path"}) || verr.Fields[0].Message != tt.want {
			t.Errorf("%s: Build = %v, want %q", tt.name, err, tt.want)
		}
	}
	a, err1 := build("vault", "photos", "restic/repo")
	b, err2 := build("vault2", "photos", "restic/repo")
	if err1 != nil || err2 != nil || a.GetResticBackup().GetRemoteSource().GetSourceKey() == b.GetResticBackup().GetRemoteSource().GetSourceKey() {
		t.Errorf("moving the crypt remote's folder kept the source key (%v, %v)", err1, err2)
	}
}

func TestRemoteFeatures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		defs []*syncrv1.RemoteDefinition
		want []string
	}{
		{nil, nil},
		{[]*syncrv1.RemoteDefinition{{BackendType: "s3"}, {BackendType: "union"}}, nil},
		{[]*syncrv1.RemoteDefinition{{BackendType: "crypt"}, {BackendType: "s3"}, {BackendType: "crypt"}}, []string{link.FeatureRcloneCrypt}},
	} {
		if got := jobkind.RemoteFeatures(tt.defs); !slices.Equal(got, tt.want) {
			t.Errorf("RemoteFeatures(%v) = %q, want %q", tt.defs, got, tt.want)
		}
	}
}

// TestCheckWithOtherKinds: kinds without remote checks are only Checked.
func TestCheckWithOtherKinds(t *testing.T) {
	t.Parallel()
	reg, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	backup := srcBackupDef()
	k1, r1, err1 := reg.Check(backup)
	k2, r2, err2 := reg.CheckWith(backup, map[domain.RemoteID]jobkind.RemoteInfo{rid(5): {NoChecksum: true, Wraps: jobkind.Wrap{RemoteID: rid(1)}}})
	if err1 != nil || err2 != nil || k1 != k2 || !slices.Equal(r1.ConcurrencyKeys, r2.ConcurrencyKeys) || !slices.Equal(r1.RemoteIDs, r2.RemoteIDs) {
		t.Errorf("CheckWith(backup) = %q %+v %v, Check = %q %+v %v", k2, r2, err2, k1, r1, err1)
	}
	if _, _, err := reg.CheckWith(&syncrv1.JobDefinition{}, nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("CheckWith(no operation) = %v", err)
	}
}

// TestNewRemoteInfo: the one mapping from a remote's wrapped location and
// capabilities to what checks need.
func TestNewRemoteInfo(t *testing.T) {
	t.Parallel()
	id := domain.NewRemoteID()
	got := jobkind.NewRemoteInfo(id, "vault", backend.Capabilities{Wrapper: true, NoChecksum: true})
	if want := (jobkind.RemoteInfo{Wraps: jobkind.Wrap{RemoteID: id, Path: "vault"}, NoChecksum: true}); got != want {
		t.Errorf("NewRemoteInfo = %+v, want %+v", got, want)
	}
	if got := jobkind.NewRemoteInfo(domain.RemoteID{}, "", backend.Capabilities{}); !got.Wraps.IsZero() || got.NoChecksum {
		t.Errorf("NewRemoteInfo of a plain remote = %+v", got)
	}
}
