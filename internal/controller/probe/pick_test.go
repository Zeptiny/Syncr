package probe_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

func cand(name string, st domain.WorkerStatus, unfinished int, mods ...func(*probe.Candidate)) probe.Candidate {
	c := probe.Candidate{
		ID: domain.NewWorkerID(), Name: name, Status: st, Version: "v2.5.0", Unfinished: unfinished,
		Features: link.Features(), ProbeOps: link.ProbeOps(), JobKinds: []string{"restic_delete_data"},
	}
	for _, m := range mods {
		m(&c)
	}
	return c
}

func preferred(c *probe.Candidate)  { c.Preferred = true }
func noFeatures(c *probe.Candidate) { c.Features = nil }
func noOps(c *probe.Candidate)      { c.ProbeOps = []string{"list_local"} }
func noKinds(c *probe.Candidate)    { c.JobKinds = []string{"rclone_transfer"} }

func TestPick(t *testing.T) {
	t.Parallel()
	const on, off, pending = domain.WorkerOnline, domain.WorkerOffline, domain.WorkerPending
	tests := []struct {
		name  string
		cands []probe.Candidate
		want  string // the picked worker's name; "" for an error
		msg   string
	}{
		{"none", nil, "", "there are no workers"},
		{"fewest unfinished jobs", []probe.Candidate{cand("busy", on, 3), cand("idle", on, 0), cand("some", on, 1)}, "idle", ""},
		{"then by name, case-insensitive", []probe.Candidate{cand("charlie", on, 1), cand("Bravo", on, 1), cand("alpha", on, 2)}, "Bravo", ""},
		{"preferred beats idle", []probe.Candidate{cand("idle", on, 0), cand("warm", on, 9, preferred)}, "warm", ""},
		{"an offline preferred worker is skipped", []probe.Candidate{cand("idle", on, 4), cand("warm", off, 0, preferred)}, "idle", ""},
		{"a preferred worker without the op is skipped", []probe.Candidate{cand("idle", on, 4), cand("warm", on, 0, preferred, noOps)}, "idle", ""},
		{"pending workers are not online", []probe.Candidate{cand("new", pending, 0)}, "", "no worker is online"},
		{"all offline", []probe.Candidate{cand("a", off, 0), cand("b", off, 0)}, "", "no worker is online"},
		{"only outdated workers", []probe.Candidate{cand("old", on, 0, noFeatures), cand("older", on, 0, noOps)}, "", "upgrade a worker"},
		{"the capable ones are offline", []probe.Candidate{cand("old", on, 0, noFeatures), cand("new", off, 0)}, "", "are offline"},
	}
	for _, tt := range tests {
		got, err := probe.Pick(tt.cands, "restic_snapshots")
		switch {
		case tt.want == "":
			if !errors.Is(err, probe.ErrNoWorker) || !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("%s: err = %v, want ErrNoWorker with %q", tt.name, err, tt.msg)
			}
		case err != nil || got.Name != tt.want:
			t.Errorf("%s: picked %q, %v; want %q", tt.name, got.Name, err, tt.want)
		}
	}

	// Ties on everything else: by ID.
	a, b := cand("same", on, 0), cand("same", on, 0)
	first := a
	if b.ID.String() < a.ID.String() {
		first = b
	}
	if got, err := probe.Pick([]probe.Candidate{a, b}, "about"); err != nil || got.ID != first.ID {
		t.Errorf("tie: picked %v, %v", got.ID, err)
	}
}

func TestPickForJob(t *testing.T) {
	t.Parallel()
	const on, off = domain.WorkerOnline, domain.WorkerOffline
	required := []string{link.FeatureResticV2, link.FeatureResticDelete}
	tests := []struct {
		name  string
		cands []probe.Candidate
		want  string
	}{
		{"least busy", []probe.Candidate{cand("busy", on, 2), cand("idle", on, 0)}, "idle"},
		{"preferred", []probe.Candidate{cand("idle", on, 0), cand("warm", on, 5, preferred)}, "warm"},
		{"needs the kind", []probe.Candidate{cand("transfers", on, 0, noKinds), cand("restic", on, 3)}, "restic"},
		{"needs every feature", []probe.Candidate{
			cand("partial", on, 0, func(c *probe.Candidate) { c.Features = []string{link.FeatureResticV2} }), cand("full", on, 3),
		}, "full"},
		// Probe operations do not matter for jobs.
		{"probe ops are irrelevant", []probe.Candidate{cand("jobs-only", on, 0, noOps)}, "jobs-only"},
		{"none usable", []probe.Candidate{cand("old", on, 0, noFeatures), cand("away", off, 0)}, ""},
	}
	for _, tt := range tests {
		got, err := probe.PickForJob(tt.cands, "restic_delete_data", required)
		switch {
		case tt.want == "":
			if !errors.Is(err, probe.ErrNoWorker) {
				t.Errorf("%s: err = %v", tt.name, err)
			}
		case err != nil || got.Name != tt.want:
			t.Errorf("%s: picked %q, %v; want %q", tt.name, got.Name, err, tt.want)
		}
	}
}

func TestCheckExplicit(t *testing.T) {
	t.Parallel()
	const on, off, pending = domain.WorkerOnline, domain.WorkerOffline, domain.WorkerPending
	tests := []struct {
		name  string
		c     probe.Candidate
		found bool
		want  error
		msg   string
	}{
		{"usable", cand("nas", on, 7), true, nil, ""},
		{"missing", probe.Candidate{}, false, domain.ErrNotFound, ""},
		{"offline", cand("nas", off, 0), true, domain.ErrUnavailable, `worker "nas" is offline`},
		{"never connected", cand("nas", pending, 0), true, domain.ErrUnavailable, `worker "nas" has never connected`},
		{"no probe feature", cand("nas", on, 0, noFeatures), true, domain.ErrFailedPrecondition, `worker "nas" (version v2.5.0) cannot run this request; upgrade it`},
		{"no such op", cand("nas", on, 0, noOps), true, domain.ErrFailedPrecondition, "upgrade it"},
		// Offline wins over outdated: the user must bring it back first.
		{"offline and outdated", cand("nas", off, 0, noFeatures), true, domain.ErrUnavailable, "is offline"},
	}
	for _, tt := range tests {
		err := probe.CheckExplicit(tt.c, tt.found, "restic_ls")
		if tt.want == nil {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
			continue
		}
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: err = %v, want %v with %q", tt.name, err, tt.want, tt.msg)
		}
	}
	if err := probe.CheckExplicitForJob(cand("nas", on, 0, noKinds), true, "restic_delete_data", nil); !errors.Is(err, domain.ErrFailedPrecondition) ||
		!strings.Contains(err.Error(), "cannot run restic_delete_data jobs") {
		t.Errorf("job on a worker without the kind: %v", err)
	}
	if err := probe.CheckExplicitForJob(cand("nas", on, 0, noOps), true, "restic_delete_data", []string{link.FeatureResticDelete}); err != nil {
		t.Errorf("job on a capable worker: %v", err)
	}
}
