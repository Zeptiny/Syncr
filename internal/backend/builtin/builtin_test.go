package builtin_test

import (
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
)

func TestTypes(t *testing.T) {
	t.Parallel()
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s3", "sftp", "local", "union", "b2", "webdav"} {
		typ, ok := reg.Get(id)
		if !ok {
			t.Errorf("type %q is not built in", id)
			continue
		}
		if _, ok := typ.(backend.Exampler); !ok {
			t.Errorf("type %q has no examples", id)
		}
	}
	backendtest.RunRegistry(t, reg)
}
