package jobkind

import (
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// deleteDataField is the field path of a data deletion in a definition.
const deleteDataField = "definition.restic_delete_data"

// ResticDeleteData is the "restic_delete_data" kind: delete restic's own
// files of a repository from its backend. Only the controller creates its
// jobs, when the owner deletes a repository with its data (SystemOnly):
// RunJob and schedules reject it. The definition carries the repository ID
// only; the worker verifies the repository's restic ID (from the resolved
// repository) before it deletes anything, and removes the repository's
// folder only when the repository is not at the remote's root.
type ResticDeleteData struct{}

var (
	_ Kind          = ResticDeleteData{}
	_ Keyed         = ResticDeleteData{}
	_ Requirer      = ResticDeleteData{}
	_ Labeled       = ResticDeleteData{}
	_ Unschedulable = ResticDeleteData{}
	_ SystemOnly    = ResticDeleteData{}
	_ Summarizer    = ResticDeleteData{}
)

// Name implements Kind.
func (ResticDeleteData) Name() string { return "restic_delete_data" }

// Label implements Labeled.
func (ResticDeleteData) Label() string { return "Delete data" }

// Unschedulable implements Unschedulable.
func (ResticDeleteData) Unschedulable() string { return "is started by deleting the repository" }

// SystemOnly implements SystemOnly.
func (ResticDeleteData) SystemOnly() string { return "is started by deleting the repository" }

// RequiredFeatures implements Requirer: the Milestone 5 restic specs and
// the deletion runner.
func (ResticDeleteData) RequiredFeatures() []string {
	return []string{link.FeatureResticV2, link.FeatureResticDelete}
}

// ConcurrencyKeys implements Keyed: the repository's key, so the deletion
// waits for every older job on the repository.
func (ResticDeleteData) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	return repositoryKeys(def.GetResticDeleteData().GetRepositoryId())
}

// Check implements Kind: the repository ID. The search term is "delete
// data".
func (ResticDeleteData) Check(def *syncrv1.JobDefinition) (Refs, error) {
	op := def.GetResticDeleteData()
	var v domain.Validation
	if op == nil {
		v.Add(deleteDataField, "is required")
		return Refs{}, v.Err()
	}
	refs := Refs{RepositoryID: checkRepositoryID(&v, deleteDataField, op.GetRepositoryId()), SearchTerms: []string{"delete data"}}
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	return refs, nil
}

// Build implements Kind: the deletion of the located repository, expecting
// the restic ID the controller stored, and removing the repository's
// folder only if it is not the remote's root.
func (k ResticDeleteData) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	if _, err := k.Check(def); err != nil {
		return nil, err
	}
	defs, rr, repo, err := repositorySpec(k.Name(), def.GetResticDeleteData().GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	op := &syncrv1.ResticDeleteData{
		Repository:       rr,
		ExpectedResticId: repo.ResticID,
		RemoveDirectory:  repo.Path != "",
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: op}}, nil
}

// Describe implements Kind: "Delete the data of Photos vault".
func (ResticDeleteData) Describe(def *syncrv1.JobDefinition, names Names) string {
	return "Delete the data of " + repositoryName(def.GetResticDeleteData().GetRepositoryId(), names)
}

// Summarize implements Summarizer: the objects deleted, the repository
// parts removed and what happened to the folder; or that the data was
// already gone.
func (ResticDeleteData) Summarize(_ *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail {
	o := out.GetResticDeleteData()
	if o == nil {
		return nil
	}
	if o.GetAlreadyGone() {
		return []Detail{{"Result", "Already gone"}}
	}
	ds := []Detail{{"Deleted", countOf(o.GetDeletedObjects(), "object")}}
	if parts := o.GetRemovedParts(); len(parts) > 0 {
		ds = append(ds, Detail{"Removed", strings.Join(parts, ", ")})
	}
	folder := "kept (other files remain, or it is the remote's root)"
	if o.GetDirectoryRemoved() {
		folder = "removed"
	}
	return append(ds, Detail{"Folder", folder})
}
