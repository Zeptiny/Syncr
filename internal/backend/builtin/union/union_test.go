package union_test

import (
	"maps"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/union"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, union.Type{})
}

func TestValidateAndMap(t *testing.T) {
	t.Parallel()
	if err := (union.Type{}).Validate(backend.Config{Options: map[string]string{"cache_time": "-1"}}); err == nil {
		t.Error("a negative cache time is accepted")
	}
	if err := (union.Type{}).Validate(backend.Config{Options: map[string]string{"create_policy": "bogus"}}); err == nil {
		t.Error("an unknown policy is accepted")
	}
	r, err := union.Type{}.ToRclone(backend.Config{})
	want := map[string]string{"action_policy": "epall", "create_policy": "epmfs", "search_policy": "ff", "cache_time": "120"}
	if err != nil || r.Backend != "union" || !maps.Equal(r.Options, want) || len(r.Secrets) != 0 {
		t.Errorf("ToRclone(defaults) = %+v, %v; want the default policies only", r, err)
	}
}
