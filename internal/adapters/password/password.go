// Package password hashes and verifies passwords with argon2id
// (golang.org/x/crypto/argon2), encoded as PHC strings:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// where salt and hash are standard base64 without padding. The default
// parameters are the second recommended option of RFC 9106: 64 MiB of
// memory, 3 passes, 4 lanes, a 16-byte salt and a 32-byte key.
//
// Hashing is deliberately expensive in memory, so a Hasher runs at most
// Config.MaxConcurrent hashes at a time (by default 4, capping the memory
// at 256 MiB with the default parameters); callers beyond that wait,
// honoring their context. Verify accepts the parameters stored in the
// encoded hash (within bounds that keep a forged hash from exhausting
// memory) and reports when they differ from the current ones, so the
// caller can store a new hash on the next successful login.
//
// The Hasher satisfies users.PasswordHasher structurally.
//
// Allowed dependencies: the standard library and golang.org/x/crypto/argon2.
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters.
type Params struct {
	// Memory is the memory cost in KiB.
	Memory uint32
	// Time is the number of passes.
	Time uint32
	// Threads is the degree of parallelism (lanes).
	Threads uint8
	// SaltLen and KeyLen are the salt and derived key lengths in bytes.
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams returns the RFC 9106 second recommended option: m = 64 MiB,
// t = 3, p = 4, 16-byte salt, 32-byte key.
func DefaultParams() Params {
	return Params{Memory: 64 * 1024, Time: 3, Threads: 4, SaltLen: 16, KeyLen: 32}
}

// Bounds of the parameters New accepts and Verify decodes. They keep a
// forged or corrupted stored hash from making Verify allocate unbounded
// memory or loop for minutes.
const (
	minMemory  = 8           // KiB per lane, the argon2 minimum
	maxMemory  = 1024 * 1024 // 1 GiB
	maxTime    = 64
	minSaltLen = 8
	maxSaltLen = 64
	minKeyLen  = 16
	maxKeyLen  = 128
)

// Validate reports parameters outside the supported bounds.
func (p Params) Validate() error {
	switch {
	case p.Threads < 1:
		return errors.New("password: threads must be at least 1")
	case p.Memory < minMemory*uint32(p.Threads) || p.Memory > maxMemory:
		return fmt.Errorf("password: memory must be between %d KiB per thread and %d KiB", minMemory, maxMemory)
	case p.Time < 1 || p.Time > maxTime:
		return fmt.Errorf("password: time must be between 1 and %d", maxTime)
	case p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen:
		return fmt.Errorf("password: salt length must be between %d and %d bytes", minSaltLen, maxSaltLen)
	case p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen:
		return fmt.Errorf("password: key length must be between %d and %d bytes", minKeyLen, maxKeyLen)
	}
	return nil
}

// DefaultMaxConcurrent is the default number of concurrent hashes.
const DefaultMaxConcurrent = 4

// Config configures a Hasher. Zero fields take their defaults.
type Config struct {
	// Params are the parameters of new hashes (default DefaultParams).
	Params Params
	// MaxConcurrent bounds the hashes computed at the same time (default
	// DefaultMaxConcurrent).
	MaxConcurrent int
	// Random is the salt source (default crypto/rand.Reader).
	Random io.Reader
}

// Hasher hashes and verifies passwords. Create it with New; it is safe for
// concurrent use.
type Hasher struct {
	params Params
	random io.Reader
	// sem holds one token per hash in progress.
	sem chan struct{}
}

// New returns a Hasher.
func New(cfg Config) (*Hasher, error) {
	if cfg.Params == (Params{}) {
		cfg.Params = DefaultParams()
	}
	if err := cfg.Params.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = DefaultMaxConcurrent
	}
	if cfg.MaxConcurrent < 1 {
		return nil, errors.New("password: max concurrent hashes must be at least 1")
	}
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	return &Hasher{params: cfg.Params, random: cfg.Random, sem: make(chan struct{}, cfg.MaxConcurrent)}, nil
}

// Params returns the parameters of new hashes.
func (h *Hasher) Params() Params { return h.params }

// Hash returns the PHC string of password with a new random salt. It waits
// for a free hashing slot, or fails with ctx's error.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := io.ReadFull(h.random, salt); err != nil {
		return "", fmt.Errorf("password: read salt: %w", err)
	}
	key, err := h.derive(ctx, password, salt, h.params)
	if err != nil {
		return "", err
	}
	return encode(h.params, salt, key), nil
}

// Verify reports whether password matches encoded (compared in constant
// time) and whether encoded was made with parameters other than the
// current ones, so that the caller should store a new Hash. A malformed or
// unsupported encoding is an error. It waits for a free hashing slot, or
// fails with ctx's error.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, false, err
	}
	got, err := h.derive(ctx, password, salt, p)
	if err != nil {
		return false, false, err
	}
	ok = subtle.ConstantTimeCompare(got, want) == 1
	return ok, p != h.params, nil
}

// derive runs argon2id in a hashing slot.
func (h *Hasher) derive(ctx context.Context, password string, salt []byte, p Params) ([]byte, error) {
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("password: wait for a hashing slot: %w", ctx.Err())
	}
	defer func() { <-h.sem }()
	return argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen), nil
}

// encode returns the PHC string. Its base64 is the standard alphabet
// without padding.
func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// errMalformed is the error of an encoded hash Verify cannot use. It never
// contains the encoded value.
var errMalformed = errors.New("password: malformed or unsupported password hash")

// decode parses a PHC argon2id string strictly: version 19 only, the
// parameters in the order m, t, p, and every value within the bounds of
// Params.Validate.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, errMalformed
	}
	var p Params
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return Params{}, nil, nil, errMalformed
	}
	for i, name := range []string{"m", "t", "p"} {
		v, ok := strings.CutPrefix(fields[i], name+"=")
		if !ok {
			return Params{}, nil, nil, errMalformed
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || (len(v) > 1 && v[0] == '0') {
			return Params{}, nil, nil, errMalformed
		}
		switch name {
		case "m":
			p.Memory = uint32(n)
		case "t":
			p.Time = uint32(n)
		case "p":
			if n > 255 {
				return Params{}, nil, nil, errMalformed
			}
			p.Threads = uint8(n)
		}
	}
	enc := base64.RawStdEncoding.Strict()
	salt, err := enc.DecodeString(parts[4])
	if err != nil || len(salt) > maxSaltLen {
		return Params{}, nil, nil, errMalformed
	}
	key, err := enc.DecodeString(parts[5])
	if err != nil || len(key) > maxKeyLen {
		return Params{}, nil, nil, errMalformed
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key)) //nolint:gosec // G115: at most maxSaltLen and maxKeyLen
	if p.Validate() != nil {
		return Params{}, nil, nil, errMalformed
	}
	return p, salt, key, nil
}
