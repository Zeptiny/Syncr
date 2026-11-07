package local_test

import (
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/local"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, local.Type{})
}

func TestValidateAndMap(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		path, root string // root "" = invalid
	}{
		{"/srv/photos/", "/srv/photos"},
		{`D:\Backups\`, `D:\Backups`},
		{`\\nas\share\x`, `\\nas\share\x`},
		{"relative/path", ""},
		{"/srv/../etc", ""},
		{"", ""},
	} {
		cfg := backend.Config{Options: map[string]string{"path": tt.path}}
		err := local.Type{}.Validate(cfg)
		if (err != nil) != (tt.root == "") {
			t.Errorf("Validate(%q) = %v", tt.path, err)
		}
		if tt.root == "" {
			continue
		}
		if r, err := (local.Type{}).ToRclone(cfg); err != nil || r.Backend != "" || r.Root != tt.root || len(r.Options) != 0 {
			t.Errorf("ToRclone(%q) = %+v, %v; want root %q and nothing else", tt.path, r, err, tt.root)
		}
	}
}
