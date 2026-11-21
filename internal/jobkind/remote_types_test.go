package jobkind_test

import (
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestBackendFeatureRemoteTypes: the rclone backends of 2.3 need their link
// features; the older ones need none.
func TestBackendFeatureRemoteTypes(t *testing.T) {
	t.Parallel()
	for backendType, want := range map[string]string{
		"smb": link.FeatureRcloneSMB, "ftp": link.FeatureRcloneFTP, "azureblob": link.FeatureRcloneAzureBlob, "gcs": link.FeatureRcloneGCS,
		"crypt": link.FeatureRcloneCrypt,
		"s3":    "", "b2": "", "sftp": "", "webdav": "", "union": "", "": "",
	} {
		if got := jobkind.BackendFeature(backendType); got != want {
			t.Errorf("BackendFeature(%q) = %q, want %q", backendType, got, want)
		}
	}
	// A union with an SMB member, a crypt remote over FTP: every
	// definition counts.
	for _, tt := range []struct {
		defs []*syncrv1.RemoteDefinition
		want []string
	}{
		{[]*syncrv1.RemoteDefinition{{BackendType: "union"}, {BackendType: "smb"}, {BackendType: "s3"}}, []string{link.FeatureRcloneSMB}},
		{[]*syncrv1.RemoteDefinition{{BackendType: "crypt"}, {BackendType: "ftp"}}, []string{link.FeatureRcloneFTP, link.FeatureRcloneCrypt}}, // sorted
		{[]*syncrv1.RemoteDefinition{{BackendType: "gcs"}, {BackendType: "azureblob"}, {BackendType: "gcs"}},
			[]string{link.FeatureRcloneAzureBlob, link.FeatureRcloneGCS}},
	} {
		if got := jobkind.RemoteFeatures(tt.defs); !slices.Equal(got, tt.want) {
			t.Errorf("RemoteFeatures(%v) = %q, want %q", tt.defs, got, tt.want)
		}
	}
}

// remoteOf is a resolved remote of rclone backend b with the type's
// NoChecksum.
func remoteOf(id domain.RemoteID, b string, noChecksum bool) jobkind.Remote {
	return jobkind.Remote{ID: id, Rclone: backend.Rclone{
		Backend: b, Options: map[string]string{"host": b + ".example"}, Secrets: map[string]string{"pass": b + "-secret"},
	}, NoChecksum: noChecksum}
}

// TestNoChecksumThroughUnion: a union with a member that keeps no checksums
// (FTP) refuses a checksum comparison at Build, even when the union's own
// NoChecksum is not set (Build follows the members), and at CheckWith when
// its info says so (remotes.Service.Infos sets it). A union of S3 and B2
// builds exactly as before.
func TestNoChecksumThroughUnion(t *testing.T) {
	t.Parallel()
	reg, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	s3, b2, ftp, withFTP, plain := rid(1), rid(2), rid(3), rid(5), rid(6)
	S3, B2, FTP := s3Remote(s3, "bucket"), remoteOf(b2, "b2", false), remoteOf(ftp, "ftp", true)
	WithFTP := unionRemote(withFTP, jobkind.Member{RemoteID: s3, Mode: backend.UnionReadWrite}, jobkind.Member{RemoteID: ftp, Path: "pub", Mode: backend.UnionReadOnly})
	Plain := unionRemote(plain, jobkind.Member{RemoteID: s3, Mode: backend.UnionReadWrite}, jobkind.Member{RemoteID: b2, Mode: backend.UnionReadWrite})
	all := remoteMap(S3, B2, FTP, WithFTP, Plain)
	// As Infos gives them: the union with an FTP member keeps no checksums.
	infos := infosOf(S3, B2, FTP, Plain)
	infos[withFTP] = jobkind.NewRemoteInfo(domain.RemoteID{}, "", backend.Capabilities{Union: true, NoChecksum: true})

	none := domain.RemoteID{}
	checksum := withOptions(&syncrv1.RcloneTransferOptions{Checksum: true})
	compare := withBisyncOptions(&syncrv1.RcloneBisyncOptions{Compare: []syncrv1.BisyncCompare{
		syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE, syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM,
	}})
	for _, tt := range []struct {
		name  string
		def   *syncrv1.JobDefinition
		field string // "" when accepted
	}{
		{"transfer from the union with an FTP member", def(loc(withFTP, "a"), loc(none, "/b"), checksum), "definition.rclone_transfer.options.checksum"},
		{"transfer to the union with an FTP member", def(loc(none, "/b"), loc(withFTP, "a"), checksum), "definition.rclone_transfer.options.checksum"},
		{"two-way sync with the union with an FTP member", twoWay(local("/a"), loc(withFTP, "b"), compare), "definition.rclone_bisync.options.compare[1]"},
		{"transfer from FTP itself", def(loc(ftp, "a"), loc(none, "/b"), checksum), "definition.rclone_transfer.options.checksum"},
		{"transfer from the union of S3 and B2", def(loc(plain, "a"), loc(none, "/b"), checksum), ""},
		{"two-way sync with the union of S3 and B2", twoWay(local("/a"), loc(plain, "b"), compare), ""},
		{"the union with an FTP member without checksum", def(loc(withFTP, "a"), loc(none, "/b")), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
				if !errors.As(err, &verr) || !slices.Equal(fields(err), []string{tt.field}) || verr.Fields[0].Message != jobkind.NoChecksumMessage {
					t.Errorf("%s = %v, want %s with the generic message", what, err, tt.field)
				}
			}
		})
	}

	// The union of S3 and B2: the same spec as before the rule (members
	// that keep checksums change nothing).
	got, err := reg.Build(def(loc(plain, "a"), loc(none, "/b"), checksum), jobkind.Resolved{Remotes: all})
	if err != nil {
		t.Fatal(err)
	}
	want := &syncrv1.JobSpec{
		Remotes: []*syncrv1.RemoteDefinition{
			rdef("r1", Plain,
				&syncrv1.UnionUpstream{Location: sloc("r2", "bucket"), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE},
				&syncrv1.UnionUpstream{Location: sloc("r3", ""), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE}),
			rdef("r2", S3), rdef("r3", B2),
		},
		Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC, Source: sloc("r1", "a"), Destination: sloc("", "/b"),
			Options: &syncrv1.RcloneTransferOptions{Checksum: true},
		}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("Build(union of S3 and B2) =\n%v\nwant\n%v", got, want)
	}
}
