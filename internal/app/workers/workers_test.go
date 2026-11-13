package workers_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

const fingerprint = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

type releaser struct {
	mu    sync.Mutex
	calls []domain.WorkerID
	err   error
}

func (r *releaser) ReleaseWorkerJobs(_ context.Context, id domain.WorkerID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id)
	return r.err
}

type env struct {
	svc    *workers.Service
	store  *memstore.Store
	clock  *clock.Fake
	jobs   *releaser
	cipher *secrets.Keyring
	ids    *idgen.Sequential
}

func newEnv(t *testing.T) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	_, _ = rand.Read(key.Bytes)
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		store: memstore.New(), clock: clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)), jobs: &releaser{},
		cipher: cipher, ids: &idgen.Sequential{},
	}
	e.svc, err = workers.New(workers.Deps{
		Workers: e.store, Tokens: e.store, Cipher: cipher, Clock: e.clock,
		IDs: e.ids, Jobs: e.jobs, Random: rand.Reader,
	}, workers.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) token(t *testing.T) string {
	t.Helper()
	g, err := e.svc.CreateEnrollmentToken(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return g.Token
}

func (e *env) enroll(t *testing.T, name string) workers.Enrollment {
	t.Helper()
	en, err := e.svc.Enroll(context.Background(), workers.EnrollParams{Token: e.token(t), Name: name, OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

func TestNewRejectsMissingDeps(t *testing.T) {
	t.Parallel()
	if _, err := workers.New(workers.Deps{}, workers.Config{}); err == nil {
		t.Error("New with no deps succeeded")
	}
}

func TestEnrollmentTokenIsSingleUseAndExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	g, err := e.svc.CreateEnrollmentToken(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.Token, "syncr_et_") || !g.ExpiresAt.Equal(e.clock.Now().Add(time.Hour)) {
		t.Errorf("grant = %+v, want prefix and default 1h TTL", g)
	}

	en, err := e.svc.Enroll(ctx, workers.EnrollParams{Token: g.Token, Name: " nas ", OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	w := en.Worker
	if w.Name != "nas" || w.Mode != domain.WorkerActive || w.Info.OS != "linux" || w.Info.Arch != "arm64" ||
		!strings.HasPrefix(en.Secret, "syncr_ws_") || len(w.SecretEnc) == 0 || strings.Contains(string(w.SecretEnc), en.Secret) {
		t.Errorf("enrolled worker = %+v, secret %q", w, en.Secret)
	}

	expired, err := e.svc.CreateEnrollmentToken(ctx, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)

	tests := []struct {
		name  string
		token string
	}{
		{"second use", g.Token},
		{"expired", expired.Token},
		{"unknown", "syncr_et_doesnotexist"},
		{"malformed", "not-a-token"},
		{"empty", ""},
	}
	for _, tt := range tests {
		_, err := e.svc.Enroll(ctx, workers.EnrollParams{Token: tt.token})
		if !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("%s: Enroll err = %v, want ErrUnauthenticated", tt.name, err)
		}
	}
	if ws, _ := e.svc.List(ctx, workers.ListFilter{}); len(ws) != 1 {
		t.Errorf("%d workers after failed enrollments, want 1", len(ws))
	}
}

func TestEnrollmentTokenRaceHasOneWinner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	token := e.token(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 16 {
		wg.Go(func() {
			if _, err := e.svc.Enroll(context.Background(), workers.EnrollParams{Token: token}); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, domain.ErrUnauthenticated) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d enrollments with one token, want 1", wins)
	}
}

// blindList hides every worker from ListWorkers while blind is set, so
// Enroll picks names that CreateWorker then finds taken (as if other
// enrollments took them concurrently).
type blindList struct {
	*memstore.Store
	blind atomic.Bool
}

func (b *blindList) ListWorkers(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error) {
	if b.blind.Load() {
		return nil, nil
	}
	return b.Store.ListWorkers(ctx, f)
}

func TestEnrollKeepsTheTokenWhenCreatingFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	repo := &blindList{Store: e.store}
	svc, err := workers.New(workers.Deps{
		Workers: repo, Tokens: e.store, Cipher: e.cipher, Clock: e.clock,
		IDs: e.ids, Jobs: e.jobs, Random: rand.Reader,
	}, workers.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.enroll(t, "nas-01")
	token := e.token(t)

	repo.blind.Store(true)
	if _, err := svc.Enroll(ctx, workers.EnrollParams{Token: token, Name: "nas-01"}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("Enroll with every attempt conflicting: err = %v, want ErrAlreadyExists", err)
	}
	// An invalid token is still reported as such, whatever the name.
	if _, err := svc.Enroll(ctx, workers.EnrollParams{Token: token + "x", Name: "nas-01"}); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("Enroll with an unknown token and a taken name: err = %v, want ErrUnauthenticated", err)
	}

	repo.blind.Store(false)
	en, err := svc.Enroll(ctx, workers.EnrollParams{Token: token, Name: "nas-01"})
	if err != nil {
		t.Fatalf("Enroll retried with the same token: %v, want the token unspent", err)
	}
	if en.Worker.Name != "nas-01-2" {
		t.Errorf("retried enrollment named the worker %q, want nas-01-2", en.Worker.Name)
	}
	if _, err := svc.Enroll(ctx, workers.EnrollParams{Token: token}); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("third use of the token: err = %v, want ErrUnauthenticated", err)
	}
	if live, _ := e.svc.List(ctx, workers.ListFilter{}); len(live) != 2 {
		t.Errorf("%d live workers, want 2", len(live))
	}
}

func TestEnrollValidatesBeforeConsumingTheToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	token := e.token(t)
	bad := []workers.EnrollParams{
		{Token: token, Mode: domain.WorkerPassive, URL: "http://insecure", CertFingerprint: fingerprint},
		{Token: token, Mode: domain.WorkerPassive, URL: "https://w:7443"},
		{Token: token, Mode: domain.WorkerActive, URL: "https://w:7443"},
		{Token: token, Mode: "push"},
		{Token: token, Name: strings.Repeat("x", 65)},
		{Token: token, Name: "bad\nname"},
	}
	for _, p := range bad {
		if _, err := e.svc.Enroll(ctx, p); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Enroll(%+v) err = %v, want ErrInvalidArgument", p, err)
		}
	}
	// The token survived every invalid attempt.
	en, err := e.svc.Enroll(ctx, workers.EnrollParams{
		Token: token, Mode: domain.WorkerPassive, URL: "https://nas.lan:7443/syncr/", CertFingerprint: fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := en.Worker
	if w.Mode != domain.WorkerPassive || w.URL != "https://nas.lan:7443/syncr" ||
		w.CertFingerprint != strings.ToLower(strings.ReplaceAll(fingerprint, ":", "")) || !strings.HasPrefix(w.Name, "worker-") {
		t.Errorf("passive enrollment = %+v", w)
	}
}

func TestCreateEnrollmentTokenTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	for _, ttl := range []time.Duration{30 * time.Second, 8 * 24 * time.Hour, -time.Hour} {
		if _, err := e.svc.CreateEnrollmentToken(ctx, ttl); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ttl %v: err = %v", ttl, err)
		}
	}
	for _, ttl := range []time.Duration{time.Minute, 7 * 24 * time.Hour} {
		g, err := e.svc.CreateEnrollmentToken(ctx, ttl)
		if err != nil || !g.ExpiresAt.Equal(e.clock.Now().Add(ttl)) {
			t.Errorf("ttl %v: (%+v, %v)", ttl, g, err)
		}
	}
	// Tokens are purged when a new one is created, KeepExpiredTokens after
	// they expired.
	e.clock.Advance(2 * time.Minute)
	if _, err := e.svc.CreateEnrollmentToken(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.store.DeleteEnrollmentTokens(ctx, e.clock.Now().Add(-workers.KeepExpiredTokens)); n != 0 {
		t.Errorf("%d long expired tokens were left behind", n)
	}
	e.clock.Advance(workers.KeepExpiredTokens)
	if _, err := e.svc.CreateEnrollmentToken(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.store.DeleteEnrollmentTokens(ctx, e.clock.Now().Add(-workers.KeepExpiredTokens)); n != 0 {
		t.Errorf("%d long expired tokens were left behind", n)
	}
}

func TestGetEnrollmentTokenFollowsTheEnrollment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	g, err := e.svc.CreateEnrollmentToken(ctx, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID.IsZero() {
		t.Fatal("grant without an ID")
	}
	tok, err := e.svc.GetEnrollmentToken(ctx, g.ID)
	if err != nil || tok.ID != g.ID || !tok.ExpiresAt.Equal(g.ExpiresAt) || e.svc.TokenState(tok) != workers.TokenWaiting {
		t.Fatalf("new token = (%+v, %v), state %q", tok, err, e.svc.TokenState(tok))
	}

	en, err := e.svc.Enroll(ctx, workers.EnrollParams{Token: g.Token, Name: "nas", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err = e.svc.GetEnrollmentToken(ctx, g.ID)
	if err != nil || tok.UsedBy != en.Worker.ID || e.svc.TokenState(tok) != workers.TokenUsed {
		t.Errorf("used token = (%+v, %v), state %q", tok, err, e.svc.TokenState(tok))
	}
	// A used token stays used once it has expired, and creating another
	// token does not purge it yet.
	e.clock.Advance(2 * time.Minute)
	if _, err := e.svc.CreateEnrollmentToken(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if tok, err = e.svc.GetEnrollmentToken(ctx, g.ID); err != nil || e.svc.TokenState(tok) != workers.TokenUsed {
		t.Errorf("used token after expiry = (%+v, %v), state %q", tok, err, e.svc.TokenState(tok))
	}
	e.clock.Advance(workers.KeepExpiredTokens + time.Second)
	if _, err := e.svc.CreateEnrollmentToken(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetEnrollmentToken(ctx, g.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("purged token: err = %v, want ErrNotFound", err)
	}

	if _, err := e.svc.GetEnrollmentToken(ctx, domain.EnrollmentTokenID{}); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("zero ID: err = %v, want ErrInvalidID", err)
	}
	if _, err := e.svc.GetEnrollmentToken(ctx, domain.NewEnrollmentTokenID()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrNotFound", err)
	}
}

func TestTokenState(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)
	unused := workers.EnrollmentToken{ExpiresAt: exp}
	used := workers.EnrollmentToken{ExpiresAt: exp, UsedAt: exp.Add(-time.Minute), UsedBy: domain.NewWorkerID()}
	tests := []struct {
		name string
		tok  workers.EnrollmentToken
		now  time.Time
		want workers.TokenState
	}{
		{"unused before expiry", unused, exp.Add(-time.Nanosecond), workers.TokenWaiting},
		// EnrollWorker accepts a token only while at < ExpiresAt.
		{"unused at expiry", unused, exp, workers.TokenExpired},
		{"unused after expiry", unused, exp.Add(time.Hour), workers.TokenExpired},
		{"used before expiry", used, exp.Add(-time.Second), workers.TokenUsed},
		{"used after expiry", used, exp.Add(time.Hour), workers.TokenUsed},
	}
	for _, tt := range tests {
		if got := tt.tok.State(tt.now); got != tt.want {
			t.Errorf("%s: State = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	a := e.enroll(t, "a")
	b := e.enroll(t, "b")
	removed := e.enroll(t, "gone")
	if err := e.svc.Remove(ctx, removed.Worker.ID); err != nil {
		t.Fatal(err)
	}
	passive, err := e.svc.RegisterPassive(ctx, workers.PassiveParams{
		Name: "p", URL: "https://p:7443", CertFingerprint: fingerprint, Secret: strings.Repeat("s", 40),
	})
	if err != nil {
		t.Fatal(err)
	}

	if w, err := e.svc.Authenticate(ctx, a.Worker.ID, a.Secret); err != nil || w.ID != a.Worker.ID {
		t.Fatalf("Authenticate(valid) = (%v, %v)", w.ID, err)
	}
	tests := []struct {
		name   string
		id     domain.WorkerID
		secret string
	}{
		{"wrong secret", a.Worker.ID, b.Secret},
		{"secret with extra byte", a.Worker.ID, a.Secret + "x"},
		{"empty secret", a.Worker.ID, ""},
		{"zero id", domain.WorkerID{}, a.Secret},
		{"unknown worker", domain.NewWorkerID(), a.Secret},
		{"removed worker", removed.Worker.ID, removed.Secret},
		{"passive worker", passive.ID, strings.Repeat("s", 40)},
	}
	for _, tt := range tests {
		if _, err := e.svc.Authenticate(ctx, tt.id, tt.secret); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", tt.name, err)
		}
	}
}

func TestSecretIsBoundToItsWorker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	b := e.enroll(t, "b")
	// A record carrying another worker's encrypted secret (a swapped blob)
	// must not authenticate with that secret: the ciphertext is bound to b.
	wb, _ := e.store.GetWorker(ctx, b.Worker.ID)
	forged := wb
	forged.ID = domain.NewWorkerID()
	forged.Name = "forged" // names are unique among live workers
	if err := e.store.CreateWorker(ctx, forged); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Authenticate(ctx, forged.ID, b.Secret)
	if err == nil || errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("swapped secret: err = %v, want a decryption (internal) error", err)
	}
}

func TestRegisterPassive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	good := workers.PassiveParams{Name: "nas", URL: "https://nas:7443", CertFingerprint: fingerprint, Secret: strings.Repeat("k", 32)}
	tests := []struct {
		name   string
		modify func(*workers.PassiveParams)
	}{
		{"empty name", func(p *workers.PassiveParams) { p.Name = "  " }},
		{"http url", func(p *workers.PassiveParams) { p.URL = "http://nas:7443" }},
		{"url without host", func(p *workers.PassiveParams) { p.URL = "https:///x" }},
		{"url with credentials", func(p *workers.PassiveParams) { p.URL = "https://u:p@nas" }},
		{"url with query", func(p *workers.PassiveParams) { p.URL = "https://nas/?a=b" }},
		{"short fingerprint", func(p *workers.PassiveParams) { p.CertFingerprint = "abcd" }},
		{"non-hex fingerprint", func(p *workers.PassiveParams) { p.CertFingerprint = strings.Repeat("zz", 32) }},
		{"short secret", func(p *workers.PassiveParams) { p.Secret = strings.Repeat("k", 31) }},
		{"secret with space", func(p *workers.PassiveParams) { p.Secret = strings.Repeat("k", 31) + " " }},
		{"secret with non-ASCII", func(p *workers.PassiveParams) { p.Secret = strings.Repeat("k", 31) + "é" }},
	}
	for _, tt := range tests {
		p := good
		tt.modify(&p)
		if _, err := e.svc.RegisterPassive(ctx, p); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", tt.name, err)
		}
	}
	w, err := e.svc.RegisterPassive(ctx, good)
	if err != nil {
		t.Fatal(err)
	}
	target, err := e.svc.PassiveTarget(ctx, w.ID)
	if err != nil || target.Secret != good.Secret || target.URL != "https://nas:7443" || len(target.CertFingerprint) != 64 {
		t.Errorf("PassiveTarget = (%+v, %v)", target, err)
	}
	active := e.enroll(t, "active")
	if _, err := e.svc.PassiveTarget(ctx, active.Worker.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("PassiveTarget(active) err = %v, want ErrNotFound", err)
	}
}

func TestStatusIsDerivedFromLastSeen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	id := e.enroll(t, "w").Worker.ID
	status := func() domain.WorkerStatus {
		w, err := e.svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return e.svc.Status(w)
	}
	if got := status(); got != domain.WorkerPending {
		t.Errorf("new worker: %s", got)
	}
	if err := e.svc.Touch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != domain.WorkerOnline {
		t.Errorf("after contact: %s", got)
	}
	e.clock.Advance(44 * time.Second)
	if got := status(); got != domain.WorkerOnline {
		t.Errorf("44s later: %s", got)
	}
	e.clock.Advance(time.Second)
	if got := status(); got != domain.WorkerOffline {
		t.Errorf("45s later: %s", got)
	}
}

func TestListRenameRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	b := e.enroll(t, "beta")
	a := e.enroll(t, "Alpha")
	c := e.enroll(t, "charlie")
	ws, err := e.svc.List(ctx, workers.ListFilter{})
	if err != nil || len(ws) != 3 || ws[0].ID != a.Worker.ID || ws[1].ID != b.Worker.ID || ws[2].ID != c.Worker.ID {
		t.Errorf("List = %v (%v), want case-insensitive name order", ws, err)
	}

	if _, err := e.svc.Rename(ctx, b.Worker.ID, ""); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Rename(empty) err = %v", err)
	}
	if w, err := e.svc.Rename(ctx, b.Worker.ID, "zulu"); err != nil || w.Name != "zulu" {
		t.Errorf("Rename = (%v, %v)", w.Name, err)
	}

	if err := e.svc.Remove(ctx, c.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, c.Worker.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(removed) err = %v", err)
	}
	if _, err := e.svc.Rename(ctx, c.Worker.ID, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Rename(removed) err = %v", err)
	}
	// Remove is idempotent and releases the jobs every time.
	e.jobs.err = errors.New("store down")
	if err := e.svc.Remove(ctx, c.Worker.ID); err == nil {
		t.Error("Remove did not report the release failure")
	}
	e.jobs.err = nil
	if err := e.svc.Remove(ctx, c.Worker.ID); err != nil {
		t.Errorf("retried Remove: %v", err)
	}
	if len(e.jobs.calls) != 3 || e.jobs.calls[2] != c.Worker.ID {
		t.Errorf("releaser calls = %v", e.jobs.calls)
	}
	if err := e.svc.Remove(ctx, domain.NewWorkerID()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Remove(unknown) err = %v", err)
	}
	if ws, _ := e.svc.List(ctx, workers.ListFilter{}); len(ws) != 2 {
		t.Errorf("List after remove = %d workers", len(ws))
	}
}

func TestAdvanceEpochKeepsBoundedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	id := e.enroll(t, "w").Worker.ID
	for i := range 20 {
		w, _ := e.svc.Get(ctx, id)
		if _, err := e.svc.AdvanceEpoch(ctx, w, fmt.Sprintf("e%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := e.svc.Get(ctx, id)
	if w.Link.Epoch != "e19" || len(w.Link.PastEpochs) != workers.MaxPastEpochs || w.Link.PastEpochs[0] != "e18" || w.Link.PastEpochs[15] != "e3" {
		t.Errorf("link = %+v", w.Link)
	}
	// A stale read loses the race.
	stale := w
	if _, err := e.svc.AdvanceEpoch(ctx, w, "e20"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdvanceEpoch(ctx, stale, "e21"); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("AdvanceEpoch from a stale read: err = %v, want ErrConflict", err)
	}
	for _, bad := range []string{"", "has space", strings.Repeat("x", 129)} {
		if _, err := e.svc.AdvanceEpoch(ctx, w, bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("AdvanceEpoch(%q) err = %v", bad, err)
		}
	}
}

func TestSetInfoClampsAndIgnoresReplacedEpochs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	w := e.enroll(t, "w").Worker
	w, err := e.svc.AdvanceEpoch(ctx, w, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if w.InfoCurrent() {
		t.Error("the enrollment info is current after the first epoch started")
	}
	huge := workers.Info{Epoch: "spoofed", Version: strings.Repeat("v", 1000), MaxConcurrentJobs: -3}
	for i := range 500 {
		huge.LocalRoots = append(huge.LocalRoots, fmt.Sprintf("/r%d", i))
		huge.JobKinds = append(huge.JobKinds, "k")
	}
	if err := e.svc.SetInfo(ctx, w.ID, "e1", huge); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Get(ctx, w.ID)
	if len(got.Info.Version) != 256 || len(got.Info.LocalRoots) != 256 || len(got.Info.JobKinds) != 64 || got.Info.MaxConcurrentJobs != 0 {
		t.Errorf("info not clamped: version %d, roots %d, kinds %d, max %d",
			len(got.Info.Version), len(got.Info.LocalRoots), len(got.Info.JobKinds), got.Info.MaxConcurrentJobs)
	}
	if got.Info.Epoch != "e1" || !got.InfoCurrent() {
		t.Errorf("info epoch = %q, want the Hello's epoch e1", got.Info.Epoch)
	}
	if err := e.svc.SetInfo(ctx, w.ID, "old", workers.Info{Version: "old"}); err != nil {
		t.Errorf("SetInfo(replaced epoch) = %v, want nil (ignored)", err)
	}
	if got, _ := e.svc.Get(ctx, w.ID); got.Info.Version == "old" {
		t.Error("a replaced epoch overwrote the info")
	}
}

func TestEnrollSuffixesTakenNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.enroll(t, "nas-01")
	tests := []struct{ requested, want string }{
		{"NAS-01", "NAS-01-2"},
		{"nas-01", "nas-01-3"},
		{"nas-01-2", "nas-01-2-2"}, // "NAS-01-2" is taken case-insensitively
		{strings.Repeat("x", 64), strings.Repeat("x", 64)},
		{strings.Repeat("x", 64), strings.Repeat("x", 62) + "-2"},
		{strings.Repeat("é", 64), strings.Repeat("é", 64)},
		{strings.Repeat("é", 64), strings.Repeat("é", 62) + "-2"},
	}
	for _, tt := range tests {
		if got := e.enroll(t, tt.requested).Worker.Name; got != tt.want {
			t.Errorf("Enroll(%q) named the worker %q, want %q", tt.requested, got, tt.want)
		}
	}
	// The stored names match, and a removed worker's name is free again.
	if err := e.svc.Remove(ctx, first.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.enroll(t, "nas-01").Worker.Name; got != "nas-01" {
		t.Errorf("Enroll after removing nas-01 named the worker %q", got)
	}
}

func TestRenameAndRegisterPassiveRejectTakenNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a := e.enroll(t, "office-pc")
	b := e.enroll(t, "nas-01")
	if _, err := e.svc.Rename(ctx, b.Worker.ID, " Office-PC "); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("Rename to a taken name: %v, want ErrAlreadyExists", err)
	}
	if w, err := e.svc.Rename(ctx, a.Worker.ID, "OFFICE-PC"); err != nil || w.Name != "OFFICE-PC" {
		t.Errorf("Rename to its own name in another case = (%q, %v)", w.Name, err)
	}
	_, err := e.svc.RegisterPassive(ctx, workers.PassiveParams{
		Name: "nas-01", URL: "https://nas.example:7443", CertFingerprint: fingerprint, Secret: strings.Repeat("s", 40),
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("RegisterPassive with a taken name: %v, want ErrAlreadyExists", err)
	}
}

func TestSetLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a := e.enroll(t, "nas-01").Worker
	b := e.enroll(t, "office-pc").Worker

	w, err := e.svc.SetLabels(ctx, a.ID, []string{"NAS", "linux", "nas", "offsite.eu-1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"linux", "nas", "offsite.eu-1"}; !slices.Equal(w.Labels, want) {
		t.Errorf("labels = %q, want %q", w.Labels, want)
	}
	if _, err := e.svc.SetLabels(ctx, b.ID, []string{"desktop", "nas"}); err != nil {
		t.Fatal(err)
	}
	ws, err := e.svc.List(ctx, workers.ListFilter{Label: "nas"})
	if err != nil || len(ws) != 2 {
		t.Fatalf("List(label nas) = (%d workers, %v)", len(ws), err)
	}
	if ws, _ := e.svc.List(ctx, workers.ListFilter{Label: "desktop"}); len(ws) != 1 || ws[0].ID != b.ID {
		t.Errorf("List(label desktop) = %v", ws)
	}

	tooMany := make([]string, domain.MaxLabels+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("l%d", i)
	}
	for _, tt := range []struct {
		labels []string
		field  string
	}{
		{[]string{"ok", "-bad"}, "labels[1]"},
		{[]string{"has space"}, "labels[0]"},
		{[]string{strings.Repeat("a", 33)}, "labels[0]"},
		{tooMany, "labels"},
	} {
		_, err := e.svc.SetLabels(ctx, a.ID, tt.labels)
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || len(verr.Fields) == 0 || verr.Fields[0].Field != tt.field {
			t.Errorf("SetLabels(%q) = %v, want a validation error on %s", tt.labels, err, tt.field)
		}
	}
	if w, _ := e.svc.Get(ctx, a.ID); !slices.Equal(w.Labels, []string{"linux", "nas", "offsite.eu-1"}) {
		t.Errorf("a rejected SetLabels changed the labels: %q", w.Labels)
	}

	if w, err := e.svc.SetLabels(ctx, a.ID, nil); err != nil || len(w.Labels) != 0 {
		t.Errorf("clearing labels = (%q, %v)", w.Labels, err)
	}
	if _, err := e.svc.SetLabels(ctx, domain.WorkerID{}, nil); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("SetLabels(zero ID) = %v", err)
	}
	if err := e.svc.Remove(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetLabels(ctx, b.ID, []string{"x"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetLabels(removed) = %v, want ErrNotFound", err)
	}
}
