package jobkind_test

import (
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// fullKind is a restic_check stand-in implementing every optional
// interface.
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

// RequiredOS asks for Linux when the check reads all data.
func (fullKind) RequiredOS(def *syncrv1.JobDefinition) jobkind.OSRequirement {
	if def.GetResticCheck().GetReadDataPercent() != 100 {
		return jobkind.OSRequirement{}
	}
	return jobkind.OSRequirement{OS: "linux", Field: "definition.restic_check.read_data_percent", What: "Reading all data"}
}

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
	_ jobkind.OSBound       = fullKind{}
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

	// RequiredOS asks the definition's kind; others require nothing.
	all := checkDef()
	all.GetResticCheck().ReadDataPercent = 100
	for name, tt := range map[string]struct {
		def  *syncrv1.JobDefinition
		want jobkind.OSRequirement
	}{
		"bound":        {all, jobkind.OSRequirement{OS: "linux", Field: "definition.restic_check.read_data_percent", What: "Reading all data"}},
		"not bound":    {checkDef(), jobkind.OSRequirement{}},
		"no OSBound":   {transfer("", "/src"), jobkind.OSRequirement{}},
		"unknown kind": {&syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticPrune{ResticPrune: &syncrv1.ResticPruneDefinition{}}}, jobkind.OSRequirement{}},
		"nothing":      {nil, jobkind.OSRequirement{}},
	} {
		if got := r.RequiredOS(tt.def); got != tt.want {
			t.Errorf("%s: RequiredOS = %+v, want %+v", name, got, tt.want)
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

// TestOSRequirement: what a requirement allows and how it explains itself.
func TestOSRequirement(t *testing.T) {
	t.Parallel()
	vss := jobkind.OSRequirement{OS: "windows", Field: "definition.restic_backup.use_fs_snapshot", What: "Windows VSS (use_fs_snapshot)"}
	for goos, want := range map[string]bool{"windows": true, "linux": false, "": false} {
		if got := vss.Allows(goos); got != want {
			t.Errorf("Allows(%q) = %v, want %v", goos, got, want)
		}
	}
	if !(jobkind.OSRequirement{}).Allows("") || !(jobkind.OSRequirement{}).Allows("linux") {
		t.Error("the zero requirement does not allow everything")
	}
	if got := vss.FieldMessage(); got != "is only available on Windows workers" {
		t.Errorf("FieldMessage = %q", got)
	}
	if got := vss.Explain(); got != "Windows VSS (use_fs_snapshot) is only available on Windows workers" {
		t.Errorf("Explain = %q", got)
	}

	// The built-in backup asks for Windows with VSS only.
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	backup := &syncrv1.ResticBackupDefinition{RepositoryId: domain.NewResticRepoID().String(), Paths: []string{`C:\Users`}}
	def := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: backup}}
	if got := r.RequiredOS(def); got != (jobkind.OSRequirement{}) {
		t.Errorf("backup without VSS: RequiredOS = %+v", got)
	}
	backup.UseFsSnapshot = true
	if got := r.RequiredOS(def); got != vss {
		t.Errorf("backup with VSS: RequiredOS = %+v, want %+v", got, vss)
	}
}

// twoWayKind is a rclone_bisync stand-in: one-shot resync and force, and
// state on one worker.
type twoWayKind struct {
	echoKind
	// shared is returned by OneShotFields for a definition with force set,
	// to show that CheckSchedulable copies it.
	shared []domain.FieldError
}

// Check accepts any definition and refers to no remote.
func (twoWayKind) Check(*syncrv1.JobDefinition) (jobkind.Refs, error) { return jobkind.Refs{}, nil }

func (k twoWayKind) OneShotFields(def *syncrv1.JobDefinition) []domain.FieldError {
	d := def.GetRcloneBisync()
	if d.GetForce() {
		return k.shared
	}
	if d.GetResync() {
		return []domain.FieldError{{Field: "definition.rclone_bisync.resync", Message: "can only be set for a single run"}}
	}
	return nil
}

func (twoWayKind) SingleWorker() string {
	return "a two-way sync keeps its state on one worker; choose a worker"
}

// pinnedKind is a restic_prune stand-in that is only SingleWorker.
type pinnedKind struct{ echoKind }

func (pinnedKind) SingleWorker() string { return "prunes stay on one worker" }

// onceKind is a restic_forget stand-in that is only OneShot: a dry run is
// for a single run.
type onceKind struct{ echoKind }

func (onceKind) OneShotFields(def *syncrv1.JobDefinition) []domain.FieldError {
	if def.GetResticForget().GetDryRun() {
		return []domain.FieldError{{Field: "definition.restic_forget.dry_run", Message: "is for a single run"}}
	}
	return nil
}

var (
	_ jobkind.OneShot      = twoWayKind{}
	_ jobkind.SingleWorker = twoWayKind{}
	_ jobkind.SingleWorker = pinnedKind{}
	_ jobkind.OneShot      = onceKind{}
)

func bisyncDef(resync, force bool) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{
		Path1:  &syncrv1.JobLocation{Path: "/data/a"},
		Path2:  &syncrv1.JobLocation{Path: "/data/b"},
		Resync: resync,
		Force:  force,
	}}}
}

// TestRegistryScheduleRules: what schedules may not do with a kind
// (SingleWorker, LabelTargetAllowed) or a definition (OneShot,
// CheckSchedulable), and that RunJob's Check is not affected.
func TestRegistryScheduleRules(t *testing.T) {
	t.Parallel()
	forceErr := domain.FieldError{Field: "definition.rclone_bisync.force", Message: "can only be set for a single run"}
	twoWay := twoWayKind{echoKind: echoKind{"rclone_bisync"}, shared: []domain.FieldError{forceErr}}
	r, err := jobkind.NewRegistry(jobkind.Transfer{}, twoWay, pinnedKind{echoKind{"restic_prune"}}, onceKind{echoKind{"restic_forget"}})
	if err != nil {
		t.Fatal(err)
	}

	labels := []struct {
		kind string
		ok   bool
		why  string
	}{
		{kind: "rclone_transfer", ok: true},
		{kind: "rclone_bisync", why: "a two-way sync keeps its state on one worker; choose a worker"},
		{kind: "restic_prune", why: "prunes stay on one worker"},
		{kind: "restic_forget", ok: true},
		{kind: "restic_backup", why: `job kind "restic_backup" is not supported`},
		{kind: "", why: `job kind "" is not supported`},
	}
	for _, tt := range labels {
		if ok, why := r.LabelTargetAllowed(tt.kind); ok != tt.ok || why != tt.why {
			t.Errorf("LabelTargetAllowed(%q) = (%v, %q), want (%v, %q)", tt.kind, ok, why, tt.ok, tt.why)
		}
	}

	forget := func(dryRun bool) *syncrv1.JobDefinition {
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticForget{ResticForget: &syncrv1.ResticForgetDefinition{DryRun: dryRun}}}
	}
	backup := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{}}}
	prune := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticPrune{ResticPrune: &syncrv1.ResticPruneDefinition{}}}
	defs := []struct {
		name string
		def  *syncrv1.JobDefinition
		want []domain.FieldError // nil: schedulable
	}{
		{name: "not one-shot", def: transfer("", "/src")},
		{name: "single worker only", def: prune},
		{name: "two-way, plain", def: bisyncDef(false, false)},
		{name: "two-way, resync", def: bisyncDef(true, false),
			want: []domain.FieldError{{Field: "definition.rclone_bisync.resync", Message: "can only be set for a single run"}}},
		{name: "two-way, force", def: bisyncDef(false, true), want: []domain.FieldError{forceErr}},
		{name: "one-shot only, plain", def: forget(false)},
		{name: "one-shot only, set", def: forget(true),
			want: []domain.FieldError{{Field: "definition.restic_forget.dry_run", Message: "is for a single run"}}},
		{name: "unregistered kind", def: backup,
			want: []domain.FieldError{{Field: "definition.operation", Message: `job kind "restic_backup" is not supported`}}},
		{name: "no operation", def: &syncrv1.JobDefinition{},
			want: []domain.FieldError{{Field: "definition.operation", Message: "is required"}}},
		{name: "nil", want: []domain.FieldError{{Field: "definition.operation", Message: "is required"}}},
	}
	for _, tt := range defs {
		err := r.CheckSchedulable(tt.def)
		if tt.want == nil {
			if err != nil {
				t.Errorf("%s: CheckSchedulable = %v, want nil", tt.name, err)
			}
			continue
		}
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || !slices.Equal(verr.Fields, tt.want) {
			t.Errorf("%s: CheckSchedulable = %v, want the field errors %v", tt.name, err, tt.want)
		}
	}

	// The error is a copy of what the kind returned.
	var verr *domain.ValidationError
	if err := r.CheckSchedulable(bisyncDef(true, true)); !errors.As(err, &verr) {
		t.Fatalf("CheckSchedulable(resync and force) = %v", err)
	}
	verr.Fields[0].Message = "mutated"
	if twoWay.shared[0] != forceErr {
		t.Error("CheckSchedulable aliases the kind's field errors")
	}

	// An unregistered kind cannot be scheduled by either answer.
	if ok, why := r.Schedulable("restic_backup"); ok || why != onlyFieldMessage(t, r.CheckSchedulable(backup)) {
		t.Errorf("Schedulable(restic_backup) = (%v, %q), want the reason CheckSchedulable gives", ok, why)
	}

	// RunJob accepts one-shot fields: Check does not consult OneShot.
	if kind, _, err := r.Check(bisyncDef(true, true)); err != nil || kind != "rclone_bisync" {
		t.Errorf("Check(resync and force) = (%q, %v), want it accepted", kind, err)
	}

	// The built-in kinds allow labels and have no one-shot fields; a
	// restore is refused by Schedulable alone.
	builtin, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	restore := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: &syncrv1.ResticRestoreDefinition{}}}
	for _, def := range []*syncrv1.JobDefinition{transfer("", "/src"), checkDef(), forget(true), restore} {
		kind := jobkind.KindOf(def)
		if err := builtin.CheckSchedulable(def); err != nil {
			t.Errorf("built-in CheckSchedulable(%s) = %v", kind, err)
		}
		if ok, why := builtin.LabelTargetAllowed(kind); !ok {
			t.Errorf("built-in LabelTargetAllowed(%s) = (false, %q)", kind, why)
		}
	}
}

// onlyFieldMessage returns the message of err's only field error.
func onlyFieldMessage(t *testing.T, err error) string {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || len(verr.Fields) != 1 {
		t.Fatalf("got %v, want one field error", err)
	}
	return verr.Fields[0].Message
}
