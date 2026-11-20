package rcloneenv_test

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// TestCryptPasswordsScrubbed: tool output scrubbed with Env.Secrets, as
// the runners do, keeps neither crypt password, in any form a tool could
// print it: as given, obscured (what rclone gets), quoted or escaped.
func TestCryptPasswordsScrubbed(t *testing.T) {
	t.Parallel()
	const password, salt = `pa"ss wo:rd/ünï`, "salt,with spaces&="
	env, err := rcloneenv.NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
		{Alias: "store", BackendType: "memory"},
		{
			Alias: "vault", BackendType: "crypt",
			Wrapped:     &syncrv1.Location{Remote: "store", Path: "data"},
			Secrets:     map[string]string{"password": password, "password2": salt},
			ObscureKeys: []string{"password", "password2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	vars := env.Vars()
	obscured := []string{vars["RCLONE_CONFIG_VAULT_PASSWORD"], vars["RCLONE_CONFIG_VAULT_PASSWORD2"]}
	var printed []string
	for _, s := range append([]string{password, salt}, obscured...) {
		if s == "" {
			t.Fatalf("a password variable is missing: %q", vars)
		}
		printed = append(printed, s, strconv.Quote(s), url.QueryEscape(s), url.PathEscape(s), fmt.Sprintf("%+q", s))
	}
	out := scrub.New(env.Secrets()).String("rclone said: " + strings.Join(printed, " | "))
	for _, s := range append([]string{password, salt, "wo:rd", "with spaces"}, obscured...) {
		if strings.Contains(out, s) {
			t.Errorf("scrubbed output keeps %q: %s", s, out)
		}
	}
	if !strings.HasPrefix(out, "rclone said: ") {
		t.Errorf("scrubbed output = %q", out)
	}
}
