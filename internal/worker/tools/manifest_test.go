package tools

import (
	"strings"
	"testing"
)

func TestDefaultManifest(t *testing.T) {
	t.Parallel()
	m := DefaultManifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"rclone": "1.75.2", "restic": "0.19.1"}
	platforms := []Platform{{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}}
	for name, version := range want {
		spec, ok := m.Lookup(name)
		if !ok {
			t.Fatalf("manifest lacks %s", name)
		}
		if spec.Version != version {
			t.Errorf("%s version = %s, want %s", name, spec.Version, version)
		}
		for _, p := range platforms {
			art, err := spec.Artifact(p)
			if err != nil {
				t.Errorf("%s %s: %v", name, p, err)
				continue
			}
			if !strings.Contains(art.URL, version) {
				t.Errorf("%s %s: URL %s does not mention version %s", name, p, art.URL, version)
			}
		}
	}
	if _, ok := m.Lookup("nope"); ok {
		t.Error("Lookup found a tool that is not pinned")
	}
	if got := len(m.Platforms()); got != len(platforms) {
		t.Errorf("Platforms() = %d entries, want %d", got, len(platforms))
	}
}

func TestSpecBinary(t *testing.T) {
	t.Parallel()
	s := Spec{Name: "rclone"}
	if s.Binary("linux") != "rclone" || s.Binary("windows") != "rclone.exe" {
		t.Errorf("Binary: %s, %s", s.Binary("linux"), s.Binary("windows"))
	}
}

func TestManifestValidate(t *testing.T) {
	t.Parallel()
	const digest = "349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702"
	good := func() Spec {
		return Spec{
			Name: "tool", Version: "1.0.0", VersionArgs: []string{"version"}, VersionPattern: `^tool (\S+)`,
			Artifacts: map[Platform]Artifact{
				{"linux", "amd64"}: {URL: "https://example.com/t.zip", SHA256: digest, Format: FormatZip, Entry: "t/tool"},
			},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Spec)
		wantErr string
	}{
		{name: "valid", mutate: func(*Spec) {}},
		{name: "bad name", mutate: func(s *Spec) { s.Name = "Tool!" }, wantErr: "name"},
		{name: "leading v", mutate: func(s *Spec) { s.Version = "v1.0.0" }, wantErr: "leading v"},
		{name: "no version args", mutate: func(s *Spec) { s.VersionArgs = nil }, wantErr: "version args"},
		{name: "bad regexp", mutate: func(s *Spec) { s.VersionPattern = "(" }, wantErr: "version pattern"},
		{name: "no capture group", mutate: func(s *Spec) { s.VersionPattern = "tool" }, wantErr: "capture group"},
		{name: "no artifacts", mutate: func(s *Spec) { s.Artifacts = nil }, wantErr: "no artifacts"},
		{name: "http url", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.URL = "http://example.com/t.zip" }) }, wantErr: "https"},
		{name: "short digest", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.SHA256 = "abc" }) }, wantErr: "sha256"},
		{name: "upper-case digest", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.SHA256 = strings.ToUpper(digest) }) }, wantErr: "sha256"},
		{name: "unknown format", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.Format = "rar" }) }, wantErr: "unknown format"},
		{name: "traversal entry", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.Entry = "../tool" }) }, wantErr: "clean relative path"},
		{name: "missing zip entry", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.Entry = "" }) }, wantErr: "clean relative path"},
		{name: "bz2 with entry", mutate: func(s *Spec) { setArtifact(s, func(a *Artifact) { a.Format = FormatBzip2 }) }, wantErr: "no entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := good()
			tt.mutate(&s)
			err := Manifest{Tools: []Spec{s}}.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	if err := (Manifest{}).Validate(); err == nil {
		t.Error("empty manifest accepted")
	}
	if err := (Manifest{Tools: []Spec{good(), good()}}).Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate tools: %v", err)
	}
}

func setArtifact(s *Spec, f func(*Artifact)) {
	for p, a := range s.Artifacts {
		f(&a)
		s.Artifacts[p] = a
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	m := DefaultManifest()
	rclone, _ := m.Lookup("rclone")
	restic, _ := m.Lookup("restic")
	tests := []struct {
		name    string
		spec    Spec
		output  string
		want    string
		wantErr bool
	}{
		{"rclone", rclone, "rclone v1.75.2\n- os/version: ubuntu 24.04 (64 bit)\n- os/kernel: 6.8.0\n", "1.75.2", false},
		{"rclone crlf", rclone, "rclone v1.75.2\r\n- os/version: windows 11\r\n", "1.75.2", false},
		{"rclone beta", rclone, "rclone v1.76.0-beta.8123.gabc\n", "1.76.0-beta.8123.gabc", false},
		{"restic", restic, "restic 0.19.1 compiled with go1.26.4 on linux/amd64\n", "0.19.1", false},
		{"restic after warning", restic, "warning: something\nrestic 0.19.1 compiled with go1.26.4 on linux/amd64\n", "0.19.1", false},
		{"garbage", rclone, "command not found\n", "", true},
		{"empty", restic, "", "", true},
		{"other tool", rclone, "restic 0.19.1\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseVersion(tt.spec, tt.output)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseVersion = %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
