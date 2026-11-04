package tools

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

func TestProcDetector(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: fakeName, Version: fakeVersion, VersionArgs: []string{"version"}, VersionPattern: `^faketool v(\S+)`}

	tests := []struct {
		name    string
		output  string
		want    string
		wantErr string
	}{
		{name: "pinned version", output: "faketool v1.2.3\nextra line", want: "1.2.3"},
		{name: "other version", output: "faketool v2.0.0", want: "2.0.0"},
		{name: "unparsable", output: "usage: faketool", wantErr: "cannot find"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runner := helperRunner{sup: proc.New(proc.Options{}), env: map[string]string{helperEnv: tt.output}}
			got, err := NewProcDetector(runner).Detect(t.Context(), spec, exe)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Detect = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestProcDetectorErrors(t *testing.T) {
	t.Parallel()
	spec := Spec{Name: fakeName, VersionArgs: []string{"version"}, VersionPattern: `^faketool v(\S+)`}
	det := NewProcDetector(proc.New(proc.Options{}))
	_, err := det.Detect(t.Context(), spec, "/definitely/not/here/faketool")
	if _, ok := errors.AsType[*proc.StartError](err); !ok {
		t.Errorf("err = %v, want a wrapped *proc.StartError", err)
	}
}

func TestCheckVersion(t *testing.T) {
	t.Parallel()
	spec := Spec{Name: fakeName, Version: fakeVersion, VersionPattern: `^faketool v(\S+)`}
	dir := t.TempDir()
	good := writeFile(t, dir, "good", fakeBinary(fakeVersion))
	bad := writeFile(t, dir, "bad", fakeBinary("0.0.1"))
	det := &contentDetector{}
	if v, err := checkVersion(t.Context(), det, spec, good); err != nil || v != fakeVersion {
		t.Errorf("good: %q, %v", v, err)
	}
	if _, err := checkVersion(t.Context(), det, spec, bad); err == nil || !strings.Contains(err.Error(), "want pinned 1.2.3") {
		t.Errorf("bad: %v", err)
	}
	if !isContextErr(context.Canceled) || isContextErr(errors.New("x")) {
		t.Error("isContextErr")
	}
}
