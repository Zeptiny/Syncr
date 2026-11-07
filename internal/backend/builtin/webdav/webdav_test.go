package webdav_test

import (
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/webdav"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, webdav.Type{})
}

func TestValidateURL(t *testing.T) {
	t.Parallel()
	for url, ok := range map[string]bool{
		"https://dav.example.com/files": true,
		"http://10.0.0.2:8080":          true,
		"ftp://dav.example.com":         false,
		"dav.example.com/files":         false,
		"https://":                      false,
	} {
		err := webdav.Type{}.Validate(backend.Config{Options: map[string]string{"url": url}})
		if (err == nil) != ok {
			t.Errorf("Validate(url %q) = %v, want ok %v", url, err, ok)
		}
	}
	r, err := webdav.Type{}.ToRclone(backend.Config{Options: map[string]string{"url": "https://d"}, Secrets: map[string]string{"pass": "p"}})
	if err != nil || r.Options["vendor"] != "other" || len(r.ObscureKeys) != 1 || r.ObscureKeys[0] != "pass" {
		t.Errorf("ToRclone = %+v, %v", r, err)
	}
}
