package proc

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestBuildEnv(t *testing.T) {
	t.Parallel()
	environ := []string{
		"PATH=/usr/bin",
		"HOME=/home/worker",
		"LANG=C.UTF-8",
		"SYNCR_WORKER_TOKEN=secret",
		"AWS_ACCESS_KEY_ID=leak",
		"RESTIC_PASSWORD=leak",
		"RCLONE_CONFIG_PASS=leak",
		"malformed",
		"=C:=C:\\",
	}
	if runtime.GOOS == "windows" {
		environ = []string{"Path=C:\\Windows", "SystemRoot=C:\\Windows", "SYNCR_WORKER_TOKEN=secret", "RESTIC_PASSWORD=leak", "lang=en"}
	}

	got, err := buildEnv(environ, map[string]string{"RCLONE_CONFIG_SRC_TYPE": "local", "LANG": "en_US.UTF-8", "B": "2", "A": "1"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"PATH=/usr/bin", "HOME=/home/worker", "A=1", "B=2", "LANG=en_US.UTF-8", "RCLONE_CONFIG_SRC_TYPE=local"}
	if runtime.GOOS == "windows" {
		// Names compare case-insensitively: "lang" is overridden by "LANG".
		want = []string{"Path=C:\\Windows", "SystemRoot=C:\\Windows", "A=1", "B=2", "LANG=en_US.UTF-8", "RCLONE_CONFIG_SRC_TYPE=local"}
	}
	if !slices.Equal(got, want) {
		t.Errorf("buildEnv() =\n%q\nwant\n%q", got, want)
	}
	for _, kv := range got {
		if strings.Contains(kv, "secret") || strings.Contains(kv, "leak") {
			t.Errorf("worker secret leaked: %q", kv)
		}
	}
}

func TestBuildEnvNeverNil(t *testing.T) {
	t.Parallel()
	got, err := buildEnv(nil, nil)
	if err != nil || got == nil {
		t.Fatalf("buildEnv(nil, nil) = (%v, %v); a nil environment would make exec inherit the worker's", got, err)
	}
}

func TestBuildEnvRejectsInvalidVariables(t *testing.T) {
	t.Parallel()
	for _, extra := range []map[string]string{
		{"": "x"},
		{"A=B": "x"},
		{"A\x00": "x"},
		{"A": "x\x00y"},
	} {
		if _, err := buildEnv(nil, extra); err == nil {
			t.Errorf("buildEnv(%q) = nil error", extra)
		}
	}
}
