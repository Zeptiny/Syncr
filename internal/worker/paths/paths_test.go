package paths

import (
	"errors"
	"path/filepath"
	"testing"
)

func fakeEnv(goos string, vars map[string]string, home string, euid int) Env {
	return Env{
		GOOS:      goos,
		LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
		UserHomeDir: func() (string, error) {
			if home == "" {
				return "", errors.New("no home")
			}
			return home, nil
		},
		Geteuid: func() int { return euid },
	}
}

func TestDataDir(t *testing.T) {
	t.Parallel()
	xdg := t.TempDir() // an absolute path on every OS
	home := t.TempDir()
	tests := []struct {
		name    string
		mode    Mode
		env     Env
		want    string
		wantErr bool
	}{
		{name: "linux system", mode: ModeSystem, env: fakeEnv("linux", nil, home, 0), want: "/var/lib/syncr"},
		{name: "linux user xdg", mode: ModeUser, env: fakeEnv("linux", map[string]string{"XDG_DATA_HOME": xdg}, home, 1000), want: filepath.Join(xdg, "syncr")},
		{name: "linux user relative xdg ignored", mode: ModeUser, env: fakeEnv("linux", map[string]string{"XDG_DATA_HOME": "rel"}, home, 1000), want: filepath.Join(home, ".local", "share", "syncr")},
		{name: "linux user empty xdg ignored", mode: ModeUser, env: fakeEnv("linux", map[string]string{"XDG_DATA_HOME": ""}, home, 1000), want: filepath.Join(home, ".local", "share", "syncr")},
		{name: "linux user fallback", mode: ModeUser, env: fakeEnv("linux", nil, home, 1000), want: filepath.Join(home, ".local", "share", "syncr")},
		{name: "linux user no home", mode: ModeUser, env: fakeEnv("linux", nil, "", 1000), wantErr: true},
		{name: "windows system", mode: ModeSystem, env: fakeEnv("windows", map[string]string{"ProgramData": `C:\ProgramData`}, home, -1), want: filepath.Join(`C:\ProgramData`, "Syncr")},
		{name: "windows user", mode: ModeUser, env: fakeEnv("windows", map[string]string{"LocalAppData": `C:\Users\a\AppData\Local`}, home, -1), want: filepath.Join(`C:\Users\a\AppData\Local`, "Syncr")},
		{name: "windows missing ProgramData", mode: ModeSystem, env: fakeEnv("windows", nil, home, -1), wantErr: true},
		{name: "darwin user", mode: ModeUser, env: fakeEnv("darwin", nil, home, 501), want: filepath.Join(home, "Library", "Application Support", "syncr")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DataDir(tt.mode, tt.env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("DataDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  Env
		want Mode
	}{
		{"linux root", fakeEnv("linux", nil, "", 0), ModeSystem},
		{"linux user", fakeEnv("linux", nil, "", 1000), ModeUser},
		{"windows", fakeEnv("windows", nil, "", -1), ModeSystem},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DefaultMode(tt.env); got != tt.want {
				t.Errorf("DefaultMode = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolsDirAndString(t *testing.T) {
	t.Parallel()
	if got, want := ToolsDir(filepath.Join("a", "b")), filepath.Join("a", "b", "tools"); got != want {
		t.Errorf("ToolsDir = %q, want %q", got, want)
	}
	data := filepath.Join("a", "b")
	if got, want := ScratchDir(data), filepath.Join(data, "scratch"); got != want {
		t.Errorf("ScratchDir = %q, want %q", got, want)
	}
	if got, want := CacheDir(data), filepath.Join(data, "cache"); got != want {
		t.Errorf("CacheDir = %q, want %q", got, want)
	}
	if got, want := BisyncDir(data), filepath.Join(data, "bisync"); got != want {
		t.Errorf("BisyncDir = %q, want %q", got, want)
	}
	if ModeUser.String() != "user" || ModeSystem.String() != "system" {
		t.Error("unexpected Mode.String")
	}
	if _, err := DataDir(DefaultMode(OSEnv()), OSEnv()); err != nil {
		t.Logf("DataDir on this host: %v", err) // environment dependent, informational
	}
}

func TestConfigFile(t *testing.T) {
	t.Parallel()
	xdg := t.TempDir()
	home := t.TempDir()
	tests := []struct {
		name    string
		mode    Mode
		env     Env
		want    string
		wantErr bool
	}{
		{name: "linux system", mode: ModeSystem, env: fakeEnv("linux", nil, home, 0), want: "/etc/syncr/worker.yaml"},
		{name: "linux user xdg", mode: ModeUser, env: fakeEnv("linux", map[string]string{"XDG_CONFIG_HOME": xdg}, home, 1000), want: filepath.Join(xdg, "syncr", "worker.yaml")},
		{name: "linux user relative xdg ignored", mode: ModeUser, env: fakeEnv("linux", map[string]string{"XDG_CONFIG_HOME": "rel"}, home, 1000), want: filepath.Join(home, ".config", "syncr", "worker.yaml")},
		{name: "linux user fallback", mode: ModeUser, env: fakeEnv("linux", nil, home, 1000), want: filepath.Join(home, ".config", "syncr", "worker.yaml")},
		{name: "linux user no home", mode: ModeUser, env: fakeEnv("linux", nil, "", 1000), wantErr: true},
		{name: "windows system", mode: ModeSystem, env: fakeEnv("windows", map[string]string{"ProgramData": `C:\ProgramData`}, home, -1), want: filepath.Join(`C:\ProgramData`, "Syncr", "worker.yaml")},
		{name: "windows user", mode: ModeUser, env: fakeEnv("windows", map[string]string{"LocalAppData": `C:\Users\a\AppData\Local`}, home, -1), want: filepath.Join(`C:\Users\a\AppData\Local`, "Syncr", "worker.yaml")},
		{name: "windows missing ProgramData", mode: ModeSystem, env: fakeEnv("windows", nil, home, -1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ConfigFile(tt.mode, tt.env)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ConfigFile = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSourcesDirs: the folders of backups of a remote, all inside
// SourcesDir (whose refusal covers them), one per method and source key.
func TestSourcesDirs(t *testing.T) {
	t.Parallel()
	data := filepath.Join("a", "b")
	const key = "0123456789abcdef0123456789abcdef"
	for _, tt := range []struct {
		name, got, want string
	}{
		{"SourcesDir", SourcesDir(data), filepath.Join(data, "sources")},
		{"MirrorsDir", MirrorsDir(data), filepath.Join(data, "sources", "mirror")},
		{"MirrorDir", MirrorDir(data, key), filepath.Join(data, "sources", "mirror", key)},
		{"MountsDir", MountsDir(data), filepath.Join(data, "sources", "mount")},
		{"MountDir", MountDir(data, key), filepath.Join(data, "sources", "mount", key)},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	// A mirror and a mount of the same key never share a folder, and
	// neither is the bisync state root or the scratch space.
	if MirrorDir(data, key) == MountDir(data, key) {
		t.Error("MirrorDir and MountDir of one key are the same folder")
	}
	for _, other := range []string{BisyncDir(data), ScratchDir(data), CacheDir(data), ToolsDir(data)} {
		if other == SourcesDir(data) {
			t.Errorf("SourcesDir = %q is another directory's", other)
		}
	}
}
