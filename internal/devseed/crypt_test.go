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
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSeedCryptRemote: the encrypted remote Vault stores its data in the
// folder "vault" of B2 offsite (which it keeps from being deleted), has
// its transfer jobs, and the canned probes list it decrypted, the B2
// folder encrypted, and its usage as B2's.
func TestSeedCryptRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)
	vaultID, b2ID := s.Remotes["Vault"], s.Remotes["B2 offsite"]

	vault, err := e.deps.Remotes.Get(ctx, s.Admin, vaultID)
	if err != nil || vault.Type != "crypt" || vault.Wrapped != (remotes.Wrapped{RemoteID: b2ID, Path: "vault"}) ||
		!slices.Equal(vault.SecretKeys, []string{"password", "password2"}) {
		t.Fatalf("Vault = %+v (%v)", vault, err)
	}
	using, err := e.deps.Remotes.RemotesUsing(ctx, s.Admin, b2ID)
	if err != nil || !slices.ContainsFunc(using, func(r remotes.Remote) bool { return r.ID == vaultID }) {
		t.Errorf("RemotesUsing(B2 offsite) = %v (%v)", using, err)
	}
	if err := e.deps.Remotes.Delete(ctx, s.Admin, b2ID); !errors.Is(err, domain.ErrFailedPrecondition) ||
		!strings.Contains(err.Error(), `encrypted remote "Vault"`) {
		t.Errorf("Delete(B2 offsite) = %v, want refused naming Vault", err)
	}

	// Its jobs: transfers into and out of it, none comparing checksums.
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{RemoteID: vaultID, Limit: jobs.MaxPageSize})
	if err != nil || len(page.Jobs) != 3 {
		t.Fatalf("Vault's jobs = %d (%v), want 3", len(page.Jobs), err)
	}
	for _, j := range page.Jobs {
		def, err := jobkind.Unmarshal(j.Definition)
		if err != nil || j.Kind != "rclone_transfer" || j.Status != domain.JobSucceeded || def.GetRcloneTransfer().GetOptions().GetChecksum() {
			t.Errorf("job %s = %s %s (%v)", j.ID, j.Kind, j.Status, err)
		}
	}

	// The canned probes, with the specs the probe runner builds.
	resolved, err := e.deps.Remotes.Resolve(ctx, s.Admin.UserID, []domain.RemoteID{vaultID})
	if err != nil || len(resolved) != 2 {
		t.Fatalf("Resolve(Vault) = %d remotes (%v)", len(resolved), err)
	}
	kinds := map[domain.RemoteID]jobkind.Remote{}
	for id, r := range resolved {
		kinds[id] = jobkind.Remote{ID: id, Rclone: r.Rclone, Wrapped: jobkind.Wrap{RemoteID: r.Wrapped.RemoteID, Path: r.Wrapped.Path},
			NoChecksum: r.Capabilities.NoChecksum}
	}
	probe := func(id domain.RemoteID, path string, op func(*syncrv1.Location) *syncrv1.ProbeSpec) *syncrv1.ProbeResult {
		t.Helper()
		defs, l, err := jobkind.Locate(id, path, kinds)
		if err != nil {
			t.Fatal(err)
		}
		spec := op(l)
		spec.Remotes = defs
		res, err := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "p", Spec: spec})
		if err != nil || res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Fatalf("probe of %s at %q = %v (%v)", id, path, res, err)
		}
		return res
	}
	list := func(l *syncrv1.Location) *syncrv1.ProbeSpec {
		return &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{Location: l, MaxEntries: 100}}}
	}
	names := func(res *syncrv1.ProbeResult) []string {
		var out []string
		for _, en := range res.GetListing().GetEntries() {
			out = append(out, en.GetName())
		}
		return out
	}
	if got := names(probe(vaultID, "", list)); !slices.Equal(got, []string{"documents"}) {
		t.Errorf("Vault root = %v", got)
	}
	if got := names(probe(vaultID, "documents/taxes", list)); !slices.Equal(got, []string{"2024", "2025"}) {
		t.Errorf("Vault documents/taxes = %v", got)
	}
	if got := names(probe(b2ID, "vault", list)); len(got) != 1 || got[0] != "8v2nh9hl0ruqd9u1g7m5tlhd8k" {
		t.Errorf("B2 vault = %v, want one encrypted folder", got)
	}
	about := func(l *syncrv1.Location) *syncrv1.ProbeSpec {
		return &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{Location: l}}}
	}
	if v, b := probe(vaultID, "", about).GetUsage(), probe(b2ID, "", about).GetUsage(); v.GetUsed() == 0 || v.GetUsed() != b.GetUsed() {
		t.Errorf("Vault usage = %v, want B2's %v", v, b)
	}
	if strings.Contains(e.logs.String(), "syncr-demo-vault-password") {
		t.Error("the log contains Vault's password")
	}
}

// TestSeedCryptFeature: the current demo workers accept encrypted remotes;
// attic-nas, an old build, does not, so a job on Vault there would fail
// with worker_outdated and probes skip it.
func TestSeedCryptFeature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)
	for name, want := range map[string]bool{"nas-01": true, "backup-host": true, "office-pc": true, "attic-nas": false} {
		w, err := e.deps.Workers.Get(ctx, s.Workers[name])
		if err != nil || w.Info.HasFeatures(link.FeatureRcloneCrypt) != want {
			t.Errorf("%s reports %s: %v, want %v (%v)", name, link.FeatureRcloneCrypt, !want, want, err)
		}
	}
}
