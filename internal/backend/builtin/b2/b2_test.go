package b2_test

import (
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/b2"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, b2.Type{})
}

func TestValidateAndMap(t *testing.T) {
	t.Parallel()
	secrets := map[string]string{"key": "k"}
	if err := (b2.Type{}).Validate(backend.Config{Options: map[string]string{"account": "a", "bucket": "../x"}, Secrets: secrets}); err == nil {
		t.Error("a bucket with a dot-dot segment is accepted")
	}
	r, err := b2.Type{}.ToRclone(backend.Config{Options: map[string]string{"account": "a", "bucket": "b/p/"}, Secrets: secrets})
	if err != nil || r.Backend != "b2" || r.Root != "b/p" || r.Options["account"] != "a" || r.Secrets["key"] != "k" || len(r.Options) != 1 {
		t.Errorf("ToRclone = %+v, %v", r, err)
	}
}
