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
