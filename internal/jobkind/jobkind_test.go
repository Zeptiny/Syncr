package jobkind_test

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// echoKind is a kind that refers to the source remote and builds an empty
// transfer.
type echoKind struct{ name string }

func (k echoKind) Name() string { return k.name }

func (echoKind) Check(def *syncrv1.JobDefinition) (jobkind.Refs, error) {
	src := def.GetRcloneTransfer().GetSource()
	if src.GetPath() == "" {
		return jobkind.Refs{}, &domain.ValidationError{Fields: []domain.FieldError{{Field: "definition.rclone_transfer.source.path", Message: "is required"}}}
	}
	id, err := domain.ParseRemoteID(src.GetRemoteId())
	if err != nil {
		return jobkind.Refs{SearchTerms: []string{src.GetPath()}}, nil
	}
	return jobkind.Refs{RemoteIDs: []domain.RemoteID{id}, SearchTerms: []string{src.GetPath()}}, nil
}

func (echoKind) Build(def *syncrv1.JobDefinition, in jobkind.Resolved) (*syncrv1.JobSpec, error) {
	spec := &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode: def.GetRcloneTransfer().GetMode(),
	}}}
	for _, r := range in.Remotes {
		spec.Remotes = append(spec.Remotes, &syncrv1.RemoteDefinition{Alias: "r1", BackendType: r.Rclone.Backend})
	}
	return spec, nil
}

func (echoKind) Describe(def *syncrv1.JobDefinition, _ jobkind.Names) string {
	return "echo " + def.GetRcloneTransfer().GetSource().GetPath()
}

func transfer(remoteID, path string) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC,
		Source:      &syncrv1.JobLocation{RemoteId: remoteID, Path: path},
		Destination: &syncrv1.JobLocation{Path: "/data/dst"},
		Options:     &syncrv1.RcloneTransferOptions{DryRun: true},
	}}}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(echoKind{"rclone_transfer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("rclone_transfer"); !ok {
		t.Error("Get(rclone_transfer) failed")
	}
	rid := domain.NewRemoteID()
	kind, refs, err := r.Check(transfer(rid.String(), "photos"))
	if err != nil || kind != "rclone_transfer" || len(refs.RemoteIDs) != 1 || refs.RemoteIDs[0] != rid {
		t.Errorf("Check = (%q, %+v, %v)", kind, refs, err)
	}
	if _, _, err := r.Check(transfer("", "")); !isValidation(err, "definition.rclone_transfer.source.path") {
		t.Errorf("Check(no path) = %v", err)
	}
	for name, def := range map[string]*syncrv1.JobDefinition{"nil": nil, "no operation": {}} {
		if _, _, err := r.Check(def); !isValidation(err, "definition.operation") {
			t.Errorf("Check(%s) = %v", name, err)
		}
		if _, err := r.Build(def, jobkind.Resolved{}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Build(%s) = %v", name, err)
		}
	}
	spec, err := r.Build(transfer("", "/src"), jobkind.Resolved{})
	if err != nil || spec.GetRcloneTransfer().GetMode() != syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC {
		t.Errorf("Build = (%v, %v)", spec, err)
	}

	if got := r.Describe(transfer("", "/src"), jobkind.Names{}); got != "echo /src" {
		t.Errorf("Describe = %q, want the kind's description", got)
	}
	if got := r.Describe(nil, jobkind.Names{}); got != "" {
		t.Errorf("Describe(nil) = %q, want empty", got)
	}

	empty, err := jobkind.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := empty.Check(transfer("", "/src")); !isValidation(err, "definition.operation") || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("Check on an empty registry = %v", err)
	}
	if got := empty.Describe(transfer("", "/src"), jobkind.Names{}); got != "rclone_transfer" {
		t.Errorf("Describe on an empty registry = %q, want the kind name", got)
	}
}

func TestBuiltin(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := r.Get("rclone_transfer"); !ok || k.Name() != "rclone_transfer" {
		t.Errorf("Get(rclone_transfer) = (%v, %v)", k, ok)
	}
	kind, refs, err := r.Check(transfer("", "/src"))
	if err != nil || kind != "rclone_transfer" || len(refs.RemoteIDs) != 0 {
		t.Errorf("Check(local to local) = (%q, %+v, %v)", kind, refs, err)
	}
	spec, err := r.Build(transfer("", "/src"), jobkind.Resolved{})
	if err != nil || spec.GetRcloneTransfer().GetSource().GetPath() != "/src" || spec.GetRcloneTransfer().GetDestination().GetPath() != "/data/dst" {
		t.Errorf("Build(local to local) = (%v, %v)", spec, err)
	}
}

func TestNewRegistryRejects(t *testing.T) {
	t.Parallel()
	for name, kinds := range map[string][]jobkind.Kind{
		"nil":           {nil},
		"duplicate":     {echoKind{"rclone_transfer"}, echoKind{"rclone_transfer"}},
		"not a oneof":   {echoKind{"rclone_sync"}},
		"spec-only op":  {echoKind{"restic_init"}},
		"empty name":    {echoKind{""}},
		"message field": {echoKind{"operation"}},
	} {
		if _, err := jobkind.NewRegistry(kinds...); err == nil {
			t.Errorf("%s: NewRegistry accepted it", name)
		}
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	def := transfer(domain.NewRemoteID().String(), "a b/ü")
	b, err := jobkind.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), `{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_SYNC","source":{"remote_id":`) {
		t.Errorf("Marshal = %s, want compact protojson with proto names", b)
	}
	again, err := jobkind.Marshal(def)
	if err != nil || string(again) != string(b) {
		t.Errorf("Marshal is not stable: %s vs %s", again, b)
	}
	got, err := jobkind.Unmarshal(b)
	if err != nil || !proto.Equal(got, def) {
		t.Errorf("Unmarshal = (%v, %v)", got, err)
	}
	for _, bad := range []string{"", "{", `{"unknown_field": 1}`, `{"rclone_transfer": 5}`} {
		if _, err := jobkind.Unmarshal([]byte(bad)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Unmarshal(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
	if _, err := jobkind.Marshal(nil); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Marshal(nil) = %v", err)
	}
	if jobkind.KindOf(nil) != "" || jobkind.KindOf(&syncrv1.JobDefinition{}) != "" || jobkind.KindOf(def) != "rclone_transfer" {
		t.Error("KindOf is wrong")
	}
}

func isValidation(err error, field string) bool {
	var verr *domain.ValidationError
	return errors.As(err, &verr) && len(verr.Fields) == 1 && verr.Fields[0].Field == field
}
