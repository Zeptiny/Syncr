package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// absProgram is an absolute program path of this host.
func absProgram() string {
	if runtime.GOOS == "windows" {
		return `C:\Syncr\hooks\dump.cmd`
	}
	return "/usr/local/bin/dump-db"
}

func TestHooksFromYAML(t *testing.T) {
	t.Parallel()
	yaml := "hooks:\n" +
		"  dump-db:\n" +
		"    description: Dump the app database\n" +
		"    command: [" + quote(absProgram()) + ", --all, \"two words\"]\n" +
		"    timeout: 15m\n" +
		"    env:\n" +
		"      PGPASSFILE: /etc/syncr/pgpass\n" +
		"  start-app:\n" +
		"    command: [docker, start, app]\n"
	cfg, err := load(t, yaml, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]HookConfig{
		"dump-db": {
			Description: "Dump the app database", Command: []string{absProgram(), "--all", "two words"},
			Timeout: 15 * time.Minute, Env: map[string]string{"PGPASSFILE": "/etc/syncr/pgpass"},
		},
		"start-app": {Command: []string{"docker", "start", "app"}},
	}
	if !reflect.DeepEqual(cfg.Hooks, want) {
		t.Errorf("Hooks = %+v, want %+v", cfg.Hooks, want)
	}
	if got := cfg.Hooks["dump-db"].EffectiveTimeout(); got != 15*time.Minute {
		t.Errorf("EffectiveTimeout = %s, want the configured 15m", got)
	}
	if got := cfg.Hooks["start-app"].EffectiveTimeout(); got != DefaultHookTimeout {
		t.Errorf("EffectiveTimeout = %s, want the default %s", got, DefaultHookTimeout)
	}
	// A hook has exactly these settings: a typo is an error, not a hook
	// that silently runs without what the operator meant.
	for _, bad := range []string{
		"hooks:\n  x:\n    command: [a]\n    shell: true\n",
		"hooks:\n  x:\n    cmd: [a]\n",
		"hooks:\n  x:\n    command: a b\n",
	} {
		if _, err := load(t, bad, nil); err == nil {
			t.Errorf("config %q accepted", bad)
		}
	}
	// Hooks cannot come from the environment.
	cfg, err = load(t, "", map[string]string{"SYNCR_HOOKS": "x"})
	if err != nil || len(cfg.Hooks) != 0 {
		t.Errorf("SYNCR_HOOKS: hooks = %v, err = %v; want none", cfg.Hooks, err)
	}
}

func TestHooksValidate(t *testing.T) {
	t.Parallel()
	ok := HookConfig{Command: []string{absProgram()}}
	many := make(map[string]HookConfig)
	for i := range 65 {
		many[fmt.Sprintf("hook-%02d", i)] = ok
	}
	tests := []struct {
		name  string
		hooks map[string]HookConfig
		want  []string // substrings, all must appear; none means valid
	}{
		{name: "valid", hooks: map[string]HookConfig{
			"a":                     ok,
			"dump.db_1-x":           {Command: []string{"docker", "exec", "db", "pg_dump"}, Timeout: MinHookTimeout, Description: strings.Repeat("é", 200)},
			strings.Repeat("a", 63): {Command: []string{absProgram(), ""}, Timeout: MaxHookTimeout, Env: map[string]string{"A": "", "sync": "1"}},
		}},
		{name: "none", hooks: nil},
		{name: "bad names", hooks: map[string]HookConfig{"Dump": ok, "-x": ok, "a b": ok, "": ok, strings.Repeat("a", 64): ok},
			want: []string{`"Dump"`, `"-x"`, `"a b"`, `""`, "is not valid"}},
		{name: "too many", hooks: many, want: []string{"65 hooks", "at most 64"}},
		{name: "no command", hooks: map[string]HookConfig{"x": {}}, want: []string{"hooks.x.command", "is required"}},
		{name: "empty program", hooks: map[string]HookConfig{"x": {Command: []string{" ", "a"}}}, want: []string{"hooks.x.command", "is empty"}},
		{name: "relative program", hooks: map[string]HookConfig{"x": {Command: []string{filepath.Join("bin", "dump")}}},
			want: []string{"hooks.x.command", "absolute path", "bare program name"}},
		{name: "dot program", hooks: map[string]HookConfig{"x": {Command: []string{"." + string(filepath.Separator) + "dump"}}}, want: []string{"hooks.x.command"}},
		{name: "NUL argument", hooks: map[string]HookConfig{"x": {Command: []string{absProgram(), "a\x00b"}}}, want: []string{"hooks.x.command[1]", "NUL"}},
		{name: "timeout too short", hooks: map[string]HookConfig{"x": {Command: ok.Command, Timeout: 10 * time.Millisecond}}, want: []string{"hooks.x.timeout", "outside"}},
		{name: "timeout negative", hooks: map[string]HookConfig{"x": {Command: ok.Command, Timeout: -time.Second}}, want: []string{"hooks.x.timeout"}},
		{name: "timeout too long", hooks: map[string]HookConfig{"x": {Command: ok.Command, Timeout: 25 * time.Hour}}, want: []string{"hooks.x.timeout"}},
		{name: "long description", hooks: map[string]HookConfig{"x": {Command: ok.Command, Description: strings.Repeat("a", 201)}}, want: []string{"hooks.x.description", "longer than 200"}},
		{name: "multi-line description", hooks: map[string]HookConfig{"x": {Command: ok.Command, Description: "a\nb"}}, want: []string{"hooks.x.description", "one line"}},
		{name: "invalid description", hooks: map[string]HookConfig{"x": {Command: ok.Command, Description: "a\xffb"}}, want: []string{"hooks.x.description", "UTF-8"}},
		{name: "reserved env", hooks: map[string]HookConfig{"x": {Command: ok.Command, Env: map[string]string{"SYNCR_JOB_ID": "1", "syncr_x": "2"}}},
			want: []string{"hooks.x.env", "SYNCR_JOB_ID", "syncr_x", "reserved"}},
		{name: "bad env names", hooks: map[string]HookConfig{"x": {Command: ok.Command, Env: map[string]string{"": "1", "A=B": "2"}}},
			want: []string{"hooks.x.env", `"A=B"`, `""`}},
		{name: "NUL env value", hooks: map[string]HookConfig{"x": {Command: ok.Command, Env: map[string]string{"A": "x\x00"}}}, want: []string{"hooks.x.env", "NUL"}},
		{name: "several at once", hooks: map[string]HookConfig{"a": {}, "b": {Command: ok.Command, Timeout: -1}},
			want: []string{"hooks.a.command", "hooks.b.timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Hooks = tt.hooks
			err := cfg.Validate()
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

// An env value may be a secret: a validation error names the variable,
// never its value.
func TestHooksValidationNeverEchoesEnvValues(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Hooks = map[string]HookConfig{"x": {
		Command: []string{absProgram()},
		Env:     map[string]string{"SYNCR_TOKEN": "hunter2-secret", "PGPASSWORD": "hunter2-nul\x00"},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error quotes an env value: %v", err)
	}
}

// Setup writes the configuration with Marshal: hooks an operator added must
// survive a rewrite, in a file Load reads back to the same Config.
func TestMarshalKeepsHooks(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Hooks = map[string]HookConfig{
		"stop-app": {Command: []string{"docker", "stop", "app"}, Description: "Stop the app: its files stay still"},
		"dump-db": {
			Command: []string{absProgram(), "--format", "custom"}, Timeout: 90 * time.Second,
			Env: map[string]string{"PGUSER": "backup", "EMPTY": ""},
		},
	}
	body, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := load2(path)
	if err != nil {
		t.Fatalf("Load(Marshal(cfg)): %v\n%s", err, body)
	}
	if !reflect.DeepEqual(got.Hooks, cfg.Hooks) {
		t.Errorf("round trip changed the hooks\n got %+v\nwant %+v\nfile:\n%s", got.Hooks, cfg.Hooks, body)
	}
	// Sorted by name, so the file does not change between two writes.
	if i, j := strings.Index(string(body), "dump-db:"), strings.Index(string(body), "stop-app:"); i < 0 || j < i {
		t.Errorf("hooks are not written in name order:\n%s", body)
	}

	// A configuration without hooks has no hooks key.
	body, err = Marshal(Default())
	if err != nil || strings.Contains(string(body), "hooks") {
		t.Errorf("Marshal(Default()) = %v\n%s\nwant no hooks key", err, body)
	}
	cfg.Hooks = map[string]HookConfig{"x": {}}
	if _, err := Marshal(cfg); err == nil {
		t.Error("Marshal accepted a hook without a command")
	}
}
