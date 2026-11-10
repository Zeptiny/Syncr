package restic_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
		bad      bool
	}{
		{in: "", want: ""},
		{in: "/", want: ""},
		{in: "  restic/photos  ", want: "restic/photos"},
		{in: "/restic//photos/", want: "restic/photos"},
		{in: "./restic/./photos", want: "restic/photos"},
		{in: "a b/ü", want: "a b/ü"},
		{in: "restic/../other", bad: true},
		{in: "..", bad: true},
		{in: `restic\photos`, bad: true},
		{in: "restic/\x00", bad: true},
		{in: "restic\nphotos", bad: true},
		{in: "\xff", bad: true},
		{in: strings.Repeat("a", restic.MaxPathLen), want: strings.Repeat("a", restic.MaxPathLen)},
		{in: strings.Repeat("a", restic.MaxPathLen+1), bad: true},
	}
	for _, tt := range tests {
		got, err := restic.NormalizePath(tt.in)
		if tt.bad {
			var verr *domain.ValidationError
			if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != "path" {
				t.Errorf("NormalizePath(%q) = (%q, %v), want a field error on path", tt.in, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizePath(%q) = (%q, %v), want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestRepoBasics(t *testing.T) {
	t.Parallel()
	id, err := domain.ParseResticRepoID("0195f0a4-3b7c-7d1e-9f00-123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(restic.PasswordAD(id)); got != "restic-password:0195f0a4-3b7c-7d1e-9f00-123456789abc" {
		t.Errorf("PasswordAD = %q", got)
	}
	if (restic.Repo{}).Deleted() {
		t.Error("a live repository is deleted")
	}
	if !restic.ModeInit.Valid() || !restic.ModeConnect.Valid() || restic.Mode("").Valid() {
		t.Error("Mode.Valid is wrong")
	}
	if !errors.Is(restic.ErrUnknownRepository, domain.ErrNotFound) || !errors.Is(restic.ErrRepositoryDeleting, domain.ErrFailedPrecondition) {
		t.Error("the sentinels do not match their domain errors")
	}
	// Every kind that can remove or add snapshots, the data deletion too.
	if got := restic.MutatingKinds(); !slices.Equal(got, []string{restic.KindBackup, "restic_forget", "restic_prune", restic.KindDeleteData}) {
		t.Errorf("MutatingKinds = %q", got)
	}
	pe := &restic.ProbeError{Code: "wrong_password", Message: "the password is wrong"}
	if pe.Error() != "wrong_password: the password is wrong" || (&restic.ProbeError{Code: "timeout"}).Error() != "timeout" {
		t.Errorf("ProbeError.Error = %q", pe.Error())
	}
}
