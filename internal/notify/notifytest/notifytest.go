// Package notifytest is the contract suite of notification channels
// (notify.Channel). Every channel package runs it from its tests, with a
// configuration whose Send fails:
//
//	func TestContract(t *testing.T) {
//		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
//			w.WriteHeader(http.StatusInternalServerError)
//		}))
//		defer srv.Close()
//		notifytest.Run(t, newChannel(srv.Client()), notifytest.WithFailingTarget(cfgFor(srv.URL)))
//	}
//
// It checks what users and operators rely on regardless of the channel:
//
//  1. the ID and label shape and the fields' descriptor shape
//     (notify.CheckChannel);
//  2. Validate accepts every example (notify.Exampler, required) and rejects
//     an unknown key, a secret sent in Options, a non-secret sent in Secrets
//     and every missing required field, naming the field;
//  3. validation errors and Summary never contain a secret value (the
//     examples' secrets and injected sentinels);
//  4. Send honors a cancelled context: it returns a temporary *SendError
//     promptly;
//  5. a failing Send returns a *SendError whose text contains no secret of
//     its configuration (WithFailingTarget; skipped without one).
//
// It is test support: it imports only the standard library, internal/domain,
// internal/descriptor and internal/notify.
package notifytest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// Option configures Run and RunRegistry.
type Option func(*config)

type config struct {
	failing     *descriptor.Values
	sendTimeout time.Duration
	scoped      map[string][]Option
}

// WithFailingTarget gives the configuration of check 5: Send with it must
// fail (e.g. it points at a test server that answers 500 or refuses the
// connection). Its secrets should be distinctive (e.g. a sentinel token in
// the URL), so that leaking them is detected.
func WithFailingTarget(cfg descriptor.Values) Option {
	return func(c *config) {
		v := cfg.Clone()
		c.failing = &v
	}
}

// WithSendTimeout bounds each Send of the suite (default 10s).
func WithSendTimeout(d time.Duration) Option {
	return func(c *config) { c.sendTimeout = d }
}

// For scopes opts to the channel with id, for RunRegistry (e.g. a failing
// target per built-in channel). Run applies the options scoped to its
// channel's ID too.
func For(id string, opts ...Option) Option {
	return func(c *config) {
		if c.scoped == nil {
			c.scoped = map[string][]Option{}
		}
		c.scoped[id] = append(c.scoped[id], opts...)
	}
}

func newConfig(id string, opts []Option) config {
	c := config{sendTimeout: 10 * time.Second}
	for _, o := range opts {
		o(&c)
	}
	for _, o := range c.scoped[id] {
		o(&c)
	}
	return c
}

// sentinelPrefix starts the values the suite injects as secrets.
const sentinelPrefix = "notifytest-sentinel-"

// minSecretLen is the length from which example secret values are searched
// for in errors and summaries (shorter values match by accident).
const minSecretLen = 6

// SampleMessage returns a realistic message (a scheduled job that failed),
// for the suite's sends and for channel tests.
func SampleMessage() notify.Message {
	finished := time.Date(2026, 11, 4, 2, 31, 7, 0, time.UTC)
	m := notify.Message{
		Version:    notify.MessageVersion,
		ID:         "0195f0a4-3b7c-7d1e-9f00-123456789abc",
		Event:      domain.EventFailed,
		OccurredAt: finished,
		URL:        "https://syncr.example/jobs/0195f0a4-3b7c-7d1e-9f00-000000000001",
		Schedule:   &notify.ScheduleInfo{ID: "0195f0a4-3b7c-7d1e-9f00-000000000002", Name: "Nightly photos → B2"},
		Job: &notify.JobInfo{
			ID: "0195f0a4-3b7c-7d1e-9f00-000000000001", Kind: "rclone_transfer", Status: domain.JobFailed,
			WorkerID: "0195f0a4-3b7c-7d1e-9f00-000000000003", WorkerName: "nas-01",
			ErrorCode: "path_not_allowed", ErrorMessage: "the source is outside the worker's local_roots",
			Bytes: 1 << 20, Files: 12, Errors: 1,
			StartedAt: finished.Add(-90 * time.Second), FinishedAt: finished, DurationSeconds: 90,
		},
	}
	m.Title = notify.Title(m)
	return m
}

// RunRegistry runs the suite for every channel of reg, each as a subtest
// named by its ID.
func RunRegistry(t *testing.T, reg *notify.Registry, opts ...Option) {
	t.Helper()
	chs := reg.Channels()
	if len(chs) == 0 {
		t.Fatal("the registry has no channels")
	}
	for _, ch := range chs {
		t.Run(ch.ID(), func(t *testing.T) { Run(t, ch, opts...) })
	}
}

// Run runs the suite for ch.
func Run(t *testing.T, ch notify.Channel, opts ...Option) {
	t.Helper()
	cfg := newConfig(ch.ID(), opts)
	if err := notify.CheckChannel(ch); err != nil {
		t.Fatalf("CheckChannel: %v", err)
	}
	ex, ok := ch.(notify.Exampler)
	if !ok || len(ex.Examples()) == 0 {
		t.Fatalf("channel %q must implement notify.Exampler with at least one example", ch.ID())
	}
	examples := ex.Examples()
	t.Run("Fields", func(t *testing.T) { checkFields(t, ch) })
	t.Run("ValidateExamples", func(t *testing.T) { checkExamples(t, ch, examples) })
	t.Run("ValidateRejects", func(t *testing.T) { checkRejects(t, ch, examples) })
	t.Run("NoSecretsInErrorsOrSummary", func(t *testing.T) { checkNoSecrets(t, ch, examples) })
	t.Run("SendHonorsCancel", func(t *testing.T) { checkCancel(t, ch, examples[0], cfg) })
	t.Run("SendErrorHasNoSecrets", func(t *testing.T) {
		if cfg.failing == nil {
			t.Skip("no failing target (notifytest.WithFailingTarget)")
		}
		checkFailingSend(t, ch, *cfg.failing, cfg)
	})
}

func checkFields(t *testing.T, ch notify.Channel) {
	fields := ch.Fields()
	if len(fields) == 0 {
		t.Fatal("the channel has no fields")
	}
	want := slices.Clone(fields)
	fields[0].Name = "mutated"
	if got := ch.Fields(); got[0].Name != want[0].Name {
		t.Error("Fields returns a slice shared with the channel")
	}
}

func checkExamples(t *testing.T, ch notify.Channel, examples []descriptor.Values) {
	for i, ex := range examples {
		if err := ch.Validate(ex.Clone()); err != nil {
			t.Errorf("example %d: Validate: %v", i, err)
		}
		if s := ch.Summary(ex.Clone()); s == "" || strings.ContainsAny(s, "\r\n") {
			t.Errorf("example %d: Summary = %q, want one non-empty line", i, s)
		}
	}
	// Validate and Summary must cope with nil maps.
	_ = ch.Validate(descriptor.Values{})
	_ = ch.Summary(descriptor.Values{})
}

// fieldPaths returns the field paths of a validation error.
func fieldPaths(t *testing.T, what string, err error) []string {
	t.Helper()
	if err == nil {
		t.Errorf("%s: Validate accepted it", what)
		return nil
	}
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("%s: error %v does not match domain.ErrInvalidArgument", what, err)
	}
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Errorf("%s: error %v is not a *domain.ValidationError", what, err)
		return nil
	}
	out := make([]string, 0, len(verr.Fields))
	for _, f := range verr.Fields {
		out = append(out, f.Field)
	}
	return out
}

func wantPath(t *testing.T, what string, err error, path string) {
	t.Helper()
	paths := fieldPaths(t, what, err)
	if err != nil && paths != nil && !slices.Contains(paths, path) {
		t.Errorf("%s: error fields %q do not include %q", what, paths, path)
	}
}

func checkRejects(t *testing.T, ch notify.Channel, examples []descriptor.Values) {
	fields := ch.Fields()
	for i, ex := range examples {
		v := withMaps(ex)
		v.Options["notifytest_unknown"] = "x"
		wantPath(t, "unknown option", ch.Validate(v), "options.notifytest_unknown")

		v = withMaps(ex)
		v.Secrets["notifytest_unknown"] = "x"
		wantPath(t, "unknown secret", ch.Validate(v), "secrets.notifytest_unknown")

		for _, f := range fields {
			if f.Secret && ex.Secrets[f.Name] != "" {
				v = withMaps(ex)
				v.Options[f.Name] = v.Secrets[f.Name]
				delete(v.Secrets, f.Name)
				wantPath(t, "example "+strconv.Itoa(i)+": secret "+f.Name+" in options", ch.Validate(v), "options."+f.Name)
			}
			if !f.Secret && ex.Options[f.Name] != "" {
				v = withMaps(ex)
				v.Secrets[f.Name] = v.Options[f.Name]
				delete(v.Options, f.Name)
				wantPath(t, "example "+strconv.Itoa(i)+": option "+f.Name+" in secrets", ch.Validate(v), "secrets."+f.Name)
			}
			if f.Required {
				v = withMaps(ex)
				delete(v.Options, f.Name)
				delete(v.Secrets, f.Name)
				path := "options." + f.Name
				if f.Secret {
					path = "secrets." + f.Name
				}
				wantPath(t, "example "+strconv.Itoa(i)+": missing required "+f.Name, ch.Validate(v), path)
			}
		}
	}
}

// secretsOf returns the secret values of v long enough to search for.
func secretsOf(v descriptor.Values) []string {
	var out []string
	for _, s := range slices.Sorted(maps.Values(v.Secrets)) {
		if len(s) >= minSecretLen {
			out = append(out, s)
		}
	}
	return out
}

func leaks(text string, secrets []string) string {
	for _, s := range secrets {
		if strings.Contains(text, s) {
			return s
		}
	}
	return ""
}

func checkNoSecrets(t *testing.T, ch notify.Channel, examples []descriptor.Values) {
	fields := ch.Fields()
	for i, ex := range examples {
		secrets := secretsOf(ex)
		if s := leaks(ch.Summary(ex.Clone()), secrets); s != "" {
			t.Errorf("example %d: Summary contains the secret %q", i, s)
		}
		// Errors of invalid variants never quote the secrets that remain.
		v := withMaps(ex)
		v.Options["notifytest_unknown"] = "x"
		if err := ch.Validate(v); err != nil {
			if s := leaks(err.Error(), secrets); s != "" {
				t.Errorf("example %d: validation error contains the secret %q", i, s)
			}
		}

		// Sentinels in every secret field, alone and appended to the
		// example's value.
		for _, mode := range []string{"replace", "append"} {
			v := withMaps(ex)
			var sentinels []string
			for _, f := range fields {
				if !f.Secret {
					continue
				}
				s := sentinelPrefix + f.Name + "-" + mode
				sentinels = append(sentinels, s)
				if mode == "append" {
					v.Secrets[f.Name] += s
				} else {
					v.Secrets[f.Name] = s
				}
			}
			if len(sentinels) == 0 {
				continue
			}
			all := append(slices.Clone(sentinels), secrets...)
			if err := ch.Validate(v.Clone()); err != nil {
				if s := leaks(err.Error(), all); s != "" {
					t.Errorf("example %d (%s sentinels): validation error contains the secret %q: %v", i, mode, s, err)
				}
			}
			if s := leaks(ch.Summary(v.Clone()), all); s != "" {
				t.Errorf("example %d (%s sentinels): Summary contains the secret %q", i, mode, s)
			}
		}
	}
}

// send runs ch.Send with a deadline and fails the test if it does not
// return in time.
func send(ctx context.Context, t *testing.T, ch notify.Channel, v descriptor.Values, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- ch.Send(ctx, v, SampleMessage()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("Send did not return within %v", timeout)
		return nil
	}
}

// checkSendError checks that err is a well-formed *notify.SendError
// without secrets.
func checkSendError(t *testing.T, err error, secrets []string) *notify.SendError {
	t.Helper()
	var serr *notify.SendError
	if !errors.As(err, &serr) {
		t.Errorf("Send error %v (%T) is not a *notify.SendError", err, err)
		return nil
	}
	if utf8.RuneCountInString(serr.Reason) > notify.MaxReasonRunes {
		t.Errorf("Reason has %d runes, more than %d", utf8.RuneCountInString(serr.Reason), notify.MaxReasonRunes)
	}
	if s := leaks(serr.Reason, secrets); s != "" {
		t.Errorf("Reason contains the secret %q: %q", s, serr.Reason)
	}
	if s := leaks(err.Error(), secrets); s != "" {
		t.Errorf("the error text contains the secret %q", s)
	}
	if serr.RetryAfter < 0 {
		t.Errorf("RetryAfter = %v", serr.RetryAfter)
	}
	return serr
}

func checkCancel(t *testing.T, ch notify.Channel, example descriptor.Values, cfg config) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := send(ctx, t, ch, example.Clone(), cfg.sendTimeout)
	if err == nil {
		t.Fatal("Send with a cancelled context succeeded")
	}
	if serr := checkSendError(t, err, secretsOf(example)); serr != nil && serr.Permanent {
		t.Error("a cancelled send is a permanent failure; it must be retried")
	}
}

func checkFailingSend(t *testing.T, ch notify.Channel, failing descriptor.Values, cfg config) {
	if err := ch.Validate(failing.Clone()); err != nil {
		t.Fatalf("the failing target is not a valid configuration: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.sendTimeout)
	defer cancel()
	err := send(ctx, t, ch, failing.Clone(), cfg.sendTimeout+time.Second)
	if err == nil {
		t.Fatal("Send to the failing target succeeded")
	}
	_ = checkSendError(t, err, secretsOf(failing))
	if s := leaks(ch.Summary(failing.Clone()), secretsOf(failing)); s != "" {
		t.Errorf("Summary of the failing target contains the secret %q", s)
	}
}

// withMaps returns a copy of v with non-nil maps.
func withMaps(v descriptor.Values) descriptor.Values {
	c := v.Clone()
	if c.Options == nil {
		c.Options = map[string]string{}
	}
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	return c
}
