package restic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// echoSupervisor makes restic's rclone "print" the secrets the child was
// given, in the forms tools print them, and fail.
type echoSupervisor struct {
	lines func(env map[string]string) []string
}

func (e echoSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	for _, l := range e.lines(spec.Env) {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	return proc.Result{ExitCode: 1}, &proc.ExitError{Path: fakeResticBin, ExitCode: 1}
}

// TestTransformedSecretsAreScrubbed checks the forms of a secret other than
// the verbatim one: rclone's obscured form of an obscure_keys secret (only
// known from the environment the real rcloneenv builder made), Go-quoted,
// JSON-escaped and URL-escaped forms, and a multi-line key printed line by
// line.
func TestTransformedSecretsAreScrubbed(t *testing.T) {
	t.Parallel()
	const pass = `sftp "pa$$" w/ord&<x>`
	const key = "-----BEGIN KEY-----\nAAAAB3NzaC1yc2EAAAADAQAB\nQ9F0y7wJxJvTIQm1ZkL2\n-----END KEY-----"
	remote := &syncrv1.RemoteDefinition{
		Alias:       "bk",
		BackendType: "sftp",
		Options:     map[string]string{"host": "sftp.example.com", "user": "syncr"},
		Secrets:     map[string]string{"pass": pass, "key_pem": key},
		ObscureKeys: []string{"pass"},
	}
	quotedJSON, _ := json.Marshal(pass)
	keyLines := strings.Split(key, "\n")
	tests := map[string]func(env map[string]string) []string{
		"obscured": func(env map[string]string) []string {
			return []string{"rclone: ERROR : obscured " + env[rcloneenv.EnvName("bk", "pass")] + " rejected"}
		},
		"go quoted": func(map[string]string) []string {
			return []string{fmt.Sprintf("rclone: ERROR : quoted %q rejected", pass)}
		},
		"json": func(map[string]string) []string {
			return []string{"rclone: ERROR : json " + string(quotedJSON) + " rejected"}
		},
		"url escaped": func(map[string]string) []string {
			return []string{"rclone: ERROR : url sftp://syncr:" + url.QueryEscape(pass) + "@host/"}
		},
		"key lines": func(map[string]string) []string { return keyLines[1:3] },
	}
	for name, lines := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var obscured string
			te := newTestEnv(t, func(c *Config) {
				c.Remotes = rcloneenv.NewBuilder(nil)
				c.Supervisor = echoSupervisor{lines: func(env map[string]string) []string {
					obscured = env[rcloneenv.EnvName("bk", "pass")]
					return lines(env)
				}}
			})
			spec := specWith([]*syncrv1.RemoteDefinition{remote}, func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: remoteRepo()}}
			})
			_, err := te.runners[kindPrune].Run(t.Context(), runner.Job{ID: "job-1", Spec: spec}, func(runner.Progress) {})
			if err == nil {
				t.Fatal("expected an error")
			}
			if obscured == "" || obscured == pass {
				t.Fatalf("the remote was not obscured: %q", obscured)
			}
			msg := err.Error()
			if !strings.Contains(msg, "[redacted]") {
				t.Errorf("error %q has no redaction", msg)
			}
			for _, leak := range []string{obscured, "pa$$", "w/ord", "w%2Ford", keyLines[1], keyLines[2]} {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks %q: %q", leak, msg)
				}
			}
		})
	}
}
