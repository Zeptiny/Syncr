package jobkind_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// fullKind is a restic_check stand-in implementing every optional
// interface (the restic kinds arrive with Track C).
type fullKind struct {
	echoKind
	keys []string
}

// Check accepts any check definition and refers to no remote.
func (fullKind) Check(*syncrv1.JobDefinition) (jobkind.Refs, error) {
	return jobkind.Refs{SearchTerms: []string{"check"}}, nil
}

func (k fullKind) ConcurrencyKeys(*syncrv1.JobDefinition) []string { return k.keys }
func (fullKind) Unschedulable() string                             { return "checks are started by hand" }
func (fullKind) SystemOnly() string                                { return "is started by the controller" }
func (fullKind) RequiredFeatures() []string                        { return []string{link.FeatureResticV2} }
func (fullKind) Label() string                                     { return "Check" }

// Summarize reports whether it got an output, and the backup's snapshot.
func (fullKind) Summarize(_ *syncrv1.JobDefinition, out *syncrv1.JobOutput) []jobkind.Detail {
	if out == nil {
		return []jobkind.Detail{{Label: "Output", Value: "none"}}
	}
	return []jobkind.Detail{{Label: "Snapshot", Value: out.GetResticBackup().GetSnapshotId()}}
}

var (
	_ jobkind.Keyed         = fullKind{}
	_ jobkind.Unschedulable = fullKind{}
	_ jobkind.SystemOnly    = fullKind{}
	_ jobkind.Requirer      = fullKind{}
	_ jobkind.Labeled       = fullKind{}
	_ jobkind.Summarizer    = fullKind{}
)

func TestRegistryOptionalInterfaces(t *testing.T) {
	t.Parallel()
	repo := domain.NewResticRepoID()
	key := jobkind.ResticRepoKey(repo)
	full := fullKind{echoKind: echoKind{"restic_check"}, keys: []string{"b-key", key, "b-key"}}
	r, err := jobkind.NewRegistry(jobkind.Transfer{}, full)
	if err != nil {
		t.Fatal(err)
	}

	// Check fills in the keys of a Keyed kind, sorted and deduplicated, as
	// a copy.
	_, refs, err := r.Check(checkDef())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b-key", key}; !slices.Equal(refs.ConcurrencyKeys, want) {
		t.Errorf("ConcurrencyKeys = %q, want %q", refs.ConcurrencyKeys, want)
	}
	refs.ConcurrencyKeys[0] = "mutated"
	if full.keys[0] != "b-key" {
		t.Error("Check aliases the kind's keys")
	}
	if _, refs, _ := r.Check(transfer("", "/src")); refs.ConcurrencyKeys != nil {
		t.Errorf("transfer keys = %q, want none", refs.ConcurrencyKeys)
	}

	tests := []struct {
		kind                   string
		schedulable, startable bool
		scheduleWhy, startWhy  string
		features               []string
		label                  string
	}{
		{kind: "rclone_transfer", schedulable: true, startable: true, label: "Transfer"},
		{kind: "restic_check", scheduleWhy: "checks are started by hand", startWhy: "is started by the controller",
			features: []string{"restic.v2"}, label: "Check"},
		{kind: "restic_backup", scheduleWhy: `job kind "restic_backup" is not supported`,
			startWhy: `job kind "restic_backup" is not supported`, label: "restic_backup"},
	}
	for _, tt := range tests {
		if ok, why := r.Schedulable(tt.kind); ok != tt.schedulable || why != tt.scheduleWhy {
			t.Errorf("Schedulable(%s) = (%v, %q), want (%v, %q)", tt.kind, ok, why, tt.schedulable, tt.scheduleWhy)
		}
		if ok, why := r.UserStartable(tt.kind); ok != tt.startable || why != tt.startWhy {
			t.Errorf("UserStartable(%s) = (%v, %q), want (%v, %q)", tt.kind, ok, why, tt.startable, tt.startWhy)
		}
		if got := r.RequiredFeatures(tt.kind); !slices.Equal(got, tt.features) {
			t.Errorf("RequiredFeatures(%s) = %q, want %q", tt.kind, got, tt.features)
		}
		if got := r.Label(tt.kind); got != tt.label {
			t.Errorf("Label(%s) = %q, want %q", tt.kind, got, tt.label)
		}
	}

	// Summarize decodes the stored output; nothing or garbage is nil.
	out, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "1a2b3c4d"}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		kind   string
		output []byte
		want   []jobkind.Detail
	}{
		"output":        {kind: "restic_check", output: out, want: []jobkind.Detail{{Label: "Snapshot", Value: "1a2b3c4d"}}},
		"no output":     {kind: "restic_check", want: []jobkind.Detail{{Label: "Output", Value: "none"}}},
		"garbage":       {kind: "restic_check", output: []byte{0xff, 0xff, 0xff}, want: []jobkind.Detail{{Label: "Output", Value: "none"}}},
		"no summarizer": {kind: "rclone_transfer", output: out},
		"unknown kind":  {kind: "restic_prune", output: out},
	} {
		if got := r.Summarize(tt.kind, checkDef(), tt.output); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Summarize = %v, want %v", name, got, tt.want)
		}
	}
}

func checkDef() *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{}}}
}
