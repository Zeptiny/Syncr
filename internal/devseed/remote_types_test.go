package devseed_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSeedRemoteTypes: one demo remote of each type of 2.3, the copy from
// NAS share (no checksums), the canned probes on each, and attic-nas
// without their features.
func TestSeedRemoteTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	want := map[string]struct {
		typ     string
		secrets []string
		entries []string // the root's listing
	}{
		"NAS share":     {"smb", []string{"pass"}, []string{"Documents"}},
		"Old FTP":       {"ftp", []string{"pass"}, []string{"2019", "2020", "2021", "readme.txt"}},
		"Azure archive": {"azureblob", []string{"key"}, []string{"office", "scans"}},
		"GCS backups":   {"gcs", []string{"service_account_credentials"}, []string{"restic"}},
	}
	resolved, err := e.deps.Remotes.Resolve(ctx, s.Admin.UserID, []domain.RemoteID{
		s.Remotes["NAS share"], s.Remotes["Old FTP"], s.Remotes["Azure archive"], s.Remotes["GCS backups"],
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[domain.RemoteID]jobkind.Remote{}
	for id, r := range resolved {
		kinds[id] = jobkind.Remote{ID: id, Rclone: r.Rclone, NoChecksum: r.Capabilities.NoChecksum}
	}
	for name, w := range want {
		id := s.Remotes[name]
		r, err := e.deps.Remotes.Get(ctx, s.Admin, id)
		if err != nil || r.Type != w.typ || !slices.Equal(r.SecretKeys, w.secrets) {
			t.Errorf("%s = %+v (%v)", name, r, err)
			continue
		}
		defs, l, err := jobkind.Locate(id, "", kinds)
		if err != nil {
			t.Fatal(err)
		}
		spec := &syncrv1.ProbeSpec{Remotes: defs, Op: &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{Location: l, MaxEntries: 100}}}
		res, err := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "p", Spec: spec})
		var names []string
		for _, en := range res.GetListing().GetEntries() {
			names = append(names, en.GetName())
		}
		if err != nil || res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || !slices.Equal(names, w.entries) {
			t.Errorf("%s: listing = %v (%v, %v), want %v", name, names, res.GetStatus(), err, w.entries)
		}
		about := &syncrv1.ProbeSpec{Remotes: defs, Op: &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{Location: l}}}
		res, err = e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "a", Spec: about})
		if u := res.GetUsage(); err != nil || (w.typ == "smb") == u.GetUnsupported() {
			t.Errorf("%s: usage = %v (%v)", name, u, err)
		}
	}
	// The demo GCS key is compacted onto one line for the worker.
	if k := resolved[s.Remotes["GCS backups"]].Rclone.Secrets["service_account_credentials"]; strings.ContainsAny(k, "\n") || !strings.Contains(k, "DEMO-ONLY") {
		t.Errorf("GCS key = %q", k)
	}

	// The copy from NAS share compares no checksums; one that did would be
	// refused, as NAS share keeps none.
	share := s.Remotes["NAS share"]
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{RemoteID: share, Limit: jobs.MaxPageSize})
	if err != nil || len(page.Jobs) != 1 {
		t.Fatalf("NAS share's jobs = %d (%v), want 1", len(page.Jobs), err)
	}
	def, err := jobkind.Unmarshal(page.Jobs[0].Definition)
	if err != nil || page.Jobs[0].Status != domain.JobSucceeded || def.GetRcloneTransfer().GetSource().GetRemoteId() != share.String() ||
		def.GetRcloneTransfer().GetOptions().GetChecksum() {
		t.Errorf("NAS share's job = %+v (%v)", page.Jobs[0], err)
	}
	def.GetRcloneTransfer().Options.Checksum = true
	_, refs, err := e.deps.Kinds.Check(def)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := e.deps.Remotes.Infos(ctx, s.Admin.UserID, refs.RemoteIDs)
	if err != nil || !infos[share].Capabilities.NoChecksum {
		t.Fatalf("Infos(NAS share) = %+v (%v)", infos[share], err)
	}
	ki := map[domain.RemoteID]jobkind.RemoteInfo{}
	for id, info := range infos {
		ki[id] = jobkind.NewRemoteInfo(info.Wrapped.RemoteID, info.Wrapped.Path, info.Capabilities)
	}
	var verr *domain.ValidationError
	if _, _, err := e.deps.Kinds.CheckWith(def, ki); !errors.As(err, &verr) || verr.Fields[0].Message != jobkind.NoChecksumMessage {
		t.Errorf("CheckWith(checksum from NAS share) = %v", err)
	}

	// Through the remotes service and the job kinds, as RunJob and the
	// scheduler check: a union with NAS share (or Old FTP) as a member
	// keeps no checksums either; a union of MinIO and B2 is as before.
	union := func(name string, members ...string) domain.RemoteID {
		in := remotes.Input{Name: name, Type: "union", Options: map[string]string{}}
		for _, m := range members {
			in.Members = append(in.Members, remotes.Member{RemoteID: s.Remotes[m], Mode: backend.UnionReadWrite})
		}
		r, err := e.deps.Remotes.Create(ctx, s.Admin, in)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	for _, tt := range []struct {
		union   domain.RemoteID
		refused bool
	}{
		{union("Share and B2", "NAS share", "B2 offsite"), true},
		{union("FTP and MinIO", "MinIO archive", "Old FTP"), true},
		{union("MinIO and B2", "MinIO archive", "B2 offsite"), false},
	} {
		for _, d := range []*syncrv1.JobDefinition{
			{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
				Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
				Source:      &syncrv1.JobLocation{RemoteId: tt.union.String(), Path: "a"},
				Destination: &syncrv1.JobLocation{Path: "/srv/a"}, Options: &syncrv1.RcloneTransferOptions{Checksum: true},
			}}},
			{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{
				Path1: &syncrv1.JobLocation{Path: "/srv/a"}, Path2: &syncrv1.JobLocation{RemoteId: tt.union.String(), Path: "a"},
				Options: &syncrv1.RcloneBisyncOptions{Compare: []syncrv1.BisyncCompare{syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM}},
			}}},
		} {
			infos, err := e.deps.Remotes.Infos(ctx, s.Admin.UserID, []domain.RemoteID{tt.union})
			if err != nil {
				t.Fatal(err)
			}
			ki := map[domain.RemoteID]jobkind.RemoteInfo{}
			for id, info := range infos {
				ki[id] = jobkind.NewRemoteInfo(info.Wrapped.RemoteID, info.Wrapped.Path, info.Capabilities)
			}
			_, _, err = e.deps.Kinds.CheckWith(d, ki)
			if refused := errors.As(err, &verr) && verr.Fields[0].Message == jobkind.NoChecksumMessage; refused != tt.refused || (!refused && err != nil) {
				t.Errorf("CheckWith(%s on union %s) = %v, want refused %v", jobkind.KindOf(d), tt.union, err, tt.refused)
			}
		}
	}

	// attic-nas, an old build, cannot use them; the current workers can.
	feats := []string{link.FeatureRcloneSMB, link.FeatureRcloneFTP, link.FeatureRcloneAzureBlob, link.FeatureRcloneGCS}
	for name, want := range map[string]bool{"nas-01": true, "backup-host": true, "office-pc": true, "attic-nas": false} {
		w, err := e.deps.Workers.Get(ctx, s.Workers[name])
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range feats {
			if w.Info.HasFeatures(f) != want {
				t.Errorf("%s reports %s: %v, want %v", name, f, !want, want)
			}
		}
	}

	for _, secret := range []string{"syncr-demo-smb-password", "syncr-demo-ftp-password", "DEMO-ONLY-NOT-A-REAL-KEY"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Errorf("the log contains a demo secret")
		}
	}
}
