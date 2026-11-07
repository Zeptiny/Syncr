package password

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastParams keep the tests quick; DefaultParams is exercised once.
func fastParams() Params {
	return Params{Memory: 64, Time: 1, Threads: 2, SaltLen: 16, KeyLen: 32}
}

func newHasher(t *testing.T, p Params) *Hasher {
	t.Helper()
	h, err := New(Config{Params: p})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDefaultParamsRoundTrip(t *testing.T) {
	t.Parallel()
	h, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Params() != DefaultParams() || cap(h.sem) != DefaultMaxConcurrent {
		t.Fatalf("defaults: %+v, %d slots", h.Params(), cap(h.sem))
	}
	encoded, err := h.Hash(t.Context(), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^\$argon2id\$v=19\$m=65536,t=3,p=4\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	if !re.MatchString(encoded) {
		t.Fatalf("encoded = %q", encoded)
	}
	ok, rehash, err := h.Verify(t.Context(), "correct horse battery staple", encoded)
	if err != nil || !ok || rehash {
		t.Errorf("Verify(right) = (%v, %v, %v)", ok, rehash, err)
	}
	ok, _, err = h.Verify(t.Context(), "correct horse battery stapler", encoded)
	if err != nil || ok {
		t.Errorf("Verify(wrong) = (%v, %v)", ok, err)
	}
}

// TestKnownAnswer checks the encoding against the argon2 reference
// implementation's test vector for argon2id v1.3 (password "password",
// salt "somesalt", t=2, m=64 MiB, p=1).
func TestKnownAnswer(t *testing.T) {
	t.Parallel()
	const vector = "$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"
	h := newHasher(t, fastParams())
	ok, rehash, err := h.Verify(t.Context(), "password", vector)
	if err != nil || !ok {
		t.Fatalf("Verify(reference vector) = (%v, %v)", ok, err)
	}
	if !rehash {
		t.Error("parameters differ from the hasher's, want needsRehash")
	}
	if ok, _, _ := h.Verify(t.Context(), "Password", vector); ok {
		t.Error("a different password matched the reference vector")
	}
	// The encoder produces the same string for the same inputs.
	p, salt, key, err := decode(vector)
	if err != nil {
		t.Fatal(err)
	}
	if got := encode(p, salt, key); got != vector {
		t.Errorf("encode(decode(v)) = %q", got)
	}
}

func TestSaltsAreRandom(t *testing.T) {
	t.Parallel()
	h := newHasher(t, fastParams())
	a, err := h.Hash(t.Context(), "same password here")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash(t.Context(), "same password here")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of one password are equal")
	}
	for _, enc := range []string{a, b} {
		if ok, _, err := h.Verify(t.Context(), "same password here", enc); !ok || err != nil {
			t.Errorf("Verify(%q) = (%v, %v)", enc, ok, err)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	t.Parallel()
	old := newHasher(t, fastParams())
	enc, err := old.Hash(t.Context(), "a password to upgrade")
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Params{
		"memory":  {Memory: 128, Time: 1, Threads: 2, SaltLen: 16, KeyLen: 32},
		"time":    {Memory: 64, Time: 2, Threads: 2, SaltLen: 16, KeyLen: 32},
		"threads": {Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		"salt":    {Memory: 64, Time: 1, Threads: 2, SaltLen: 32, KeyLen: 32},
		"key":     {Memory: 64, Time: 1, Threads: 2, SaltLen: 16, KeyLen: 64},
	} {
		ok, rehash, err := newHasher(t, p).Verify(t.Context(), "a password to upgrade", enc)
		if err != nil || !ok || !rehash {
			t.Errorf("%s changed: Verify = (%v, %v, %v), want ok and needsRehash", name, ok, rehash, err)
		}
	}
	if _, rehash, _ := old.Verify(t.Context(), "a password to upgrade", enc); rehash {
		t.Error("same parameters report needsRehash")
	}
}

func TestMalformedHashes(t *testing.T) {
	t.Parallel()
	h := newHasher(t, fastParams())
	good, err := h.Hash(t.Context(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	for name, enc := range map[string]string{
		"empty":              "",
		"bcrypt":             "$2a$10$abcdefghijklmnopqrstuv",
		"argon2i":            with(1, "argon2i"),
		"old version":        with(2, "v=16"),
		"no version":         "$argon2id$m=64,t=1,p=2$" + parts[4] + "$" + parts[5],
		"params reordered":   with(3, "t=1,m=64,p=2"),
		"param missing":      with(3, "m=64,t=1"),
		"extra param":        with(3, "m=64,t=1,p=2,k=1"),
		"leading zero":       with(3, "m=064,t=1,p=2"),
		"negative":           with(3, "m=-64,t=1,p=2"),
		"zero time":          with(3, "m=64,t=0,p=2"),
		"huge memory":        with(3, "m=4194304,t=1,p=2"),
		"huge time":          with(3, "m=64,t=100000,p=2"),
		"zero threads":       with(3, "m=64,t=1,p=0"),
		"threads overflow":   with(3, "m=64,t=1,p=256"),
		"memory below lanes": with(3, "m=8,t=1,p=2"),
		"padded salt":        with(4, parts[4]+"=="),
		"bad salt":           with(4, "!!!!"),
		"short salt":         with(4, "c2FsdA"),
		"short key":          with(5, "c2hvcnQ"),
		"trailing part":      good + "$x",
		"no leading dollar":  strings.TrimPrefix(good, "$"),
	} {
		ok, _, err := h.Verify(t.Context(), "pw", enc)
		if err == nil || ok {
			t.Errorf("%s: Verify(%q) = (%v, %v), want an error", name, enc, ok, err)
		}
		if err != nil && enc != "" && strings.Contains(err.Error(), enc) {
			t.Errorf("%s: the error repeats the hash: %v", name, err)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]Config{
		"threads":        {Params: Params{Memory: 64, Time: 1, Threads: 0, SaltLen: 16, KeyLen: 32}},
		"memory":         {Params: Params{Memory: 4, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}},
		"time":           {Params: Params{Memory: 64, Time: 0, Threads: 1, SaltLen: 16, KeyLen: 32}},
		"salt":           {Params: Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 4, KeyLen: 32}},
		"key":            {Params: Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 8}},
		"max concurrent": {Params: fastParams(), MaxConcurrent: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New(%+v) succeeded", name, cfg)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestHashFailsWithoutRandomness(t *testing.T) {
	t.Parallel()
	h, err := New(Config{Params: fastParams(), Random: failingReader{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Hash(t.Context(), "pw"); err == nil {
		t.Error("Hash succeeded without a salt")
	}
}

// TestConcurrencyIsBounded fills every hashing slot: further calls wait and
// give up with their context.
func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()
	h, err := New(Config{Params: fastParams(), MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := h.Hash(t.Context(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	h.sem <- struct{}{}
	h.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "pw"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Hash with every slot taken = %v, want DeadlineExceeded", err)
	}
	if _, _, err := h.Verify(ctx, "pw", enc); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Verify with every slot taken = %v, want DeadlineExceeded", err)
	}

	// Freeing a slot lets a waiting call through.
	done := make(chan error, 1)
	go func() {
		_, _, err := h.Verify(t.Context(), "pw", enc)
		done <- err
	}()
	<-h.sem
	if err := <-done; err != nil {
		t.Errorf("Verify after a slot was freed: %v", err)
	}
	<-h.sem
	if len(h.sem) != 0 {
		t.Errorf("%d slots still taken", len(h.sem))
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	h := newHasher(t, fastParams())
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			enc, err := h.Hash(t.Context(), "concurrent password")
			if err != nil {
				t.Error(err)
				return
			}
			if ok, _, err := h.Verify(t.Context(), "concurrent password", enc); !ok || err != nil {
				t.Errorf("Verify = (%v, %v)", ok, err)
			}
		})
	}
	wg.Wait()
}
