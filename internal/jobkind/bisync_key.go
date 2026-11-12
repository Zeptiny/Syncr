package jobkind

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// BisyncKeyPrefix starts the concurrency key of a two-way sync location:
// every two-way sync job holds BisyncKeyPrefix plus 40 hex digits for each
// of its two sides (Bisync.ConcurrencyKeys).
const BisyncKeyPrefix = "bisync:"

// bisyncKeyDigits is how many hex digits of the hash a location key keeps
// (160 bits): "bisync:" plus 40 is 47 characters, within ADR 0013's limit.
const bisyncKeyDigits = 40

// Versions of the hashed encodings. A new encoding takes a new version, so
// that it can never produce an older encoding's key for another input.
const (
	bisyncStateVersion    = "syncr-bisync-state-v1"
	bisyncLocationVersion = "syncr-bisync-location-v1"
)

// keyEncoder writes the canonical encoding a key hashes: a sequence of
// fields, each its length in decimal bytes, ":", the bytes and ",". The
// length prefix makes it unambiguous whatever the bytes are, so two
// different inputs never share an encoding.
type keyEncoder struct{ b []byte }

func (e *keyEncoder) field(s string) {
	e.b = strconv.AppendInt(e.b, int64(len(s)), 10)
	e.b = append(e.b, ':')
	e.b = append(e.b, s...)
	e.b = append(e.b, ',')
}

// count writes a number of entries as a field.
func (e *keyEncoder) count(n int) { e.field(strconv.Itoa(n)) }

// bisyncLocationKey is the concurrency key of a checked location: the
// remote ID and the path on it (without leading or trailing "/", which
// the worker ignores when it compares paths on a remote), or the local
// path, lower-cased if it is a Windows path (Windows ignores case). It
// does not depend on the worker, the remote's settings or the other side,
// so every pair that uses the location holds it.
func bisyncLocationKey(l location) string {
	e := keyEncoder{}
	e.field(bisyncLocationVersion)
	if l.remote.IsZero() {
		key := l.key
		if absoluteWindows(key) {
			key = strings.ToLower(key)
		}
		e.field("local")
		e.field(key)
	} else {
		e.field("remote")
		e.field(l.remote.String())
		e.field(strings.Trim(l.key, "/"))
	}
	sum := sha256.Sum256(e.b)
	return BisyncKeyPrefix + hex.EncodeToString(sum[:])[:bisyncKeyDigits]
}

// bisyncSide is one side of a pair as Build resolved it: its spec location
// and, when the definition names one, its remote.
type bisyncSide struct {
	loc    *syncrv1.Location
	remote domain.RemoteID
}

// bisyncStateKey is the pair's state key (RcloneBisync.state_key): the
// lower-case hex SHA-256 of bisyncStateInput.
func (s *specRemotes) bisyncStateKey(sides [2]bisyncSide) (string, error) {
	in, err := s.bisyncStateInput(sides)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(in)
	return hex.EncodeToString(sum[:]), nil
}

// bisyncStateInput is the canonical encoding (keyEncoder) the state key
// hashes: bisyncStateVersion, then each side in order (path1, path2). A
// side on the worker's filesystem (a worker path, or a path on a local
// remote) is "local" and its cleaned path; a side on a remote is "remote",
// the remote (stateRemote) and the joined path (root and path) on it.
//
// It never contains a secret or an obscured value: only the remotes'
// options, which hold no secret, enter it, and even an option named like a
// secret or an obscured key is left out. So a rotated credential keeps the
// key, and any other change of a side (its remote, the remote's type or
// settings, its root, the path, a union member) or swapping the sides
// gives a new key, which the worker treats as a new pair (its first run
// is a resync).
func (s *specRemotes) bisyncStateInput(sides [2]bisyncSide) ([]byte, error) {
	e := &keyEncoder{}
	e.field(bisyncStateVersion)
	for _, side := range sides {
		if side.loc.GetRemote() == "" {
			e.field("local")
			e.field(side.loc.GetPath())
			continue
		}
		r, err := s.remote(side.remote)
		if err != nil {
			return nil, err
		}
		e.field("remote")
		if err := s.stateRemote(e, r); err != nil {
			return nil, err
		}
		e.field(side.loc.GetPath())
	}
	return e.b, nil
}

// stateRemote encodes remote r for the state key: its ID, backend type and
// non-secret options sorted by name, then its union members in order, each
// with its mode and its location (local, or the member remote the same
// way and the joined path on it).
func (s *specRemotes) stateRemote(e *keyEncoder, r Remote) error {
	e.field(r.ID.String())
	e.field(r.Rclone.Backend)
	opts := nonSecretOptions(r)
	e.count(len(opts))
	for _, k := range slices.Sorted(maps.Keys(opts)) {
		e.field(k)
		e.field(opts[k])
	}
	e.count(len(r.Members))
	for i, m := range r.Members {
		mr, err := s.remote(m.RemoteID)
		if err != nil {
			return fmt.Errorf("member %d of union remote %s: %w", i, r.ID, err)
		}
		loc, err := s.on(mr, m.Path)
		if err != nil {
			return fmt.Errorf("member %d of union remote %s: %w", i, r.ID, err)
		}
		e.field(string(m.Mode))
		if loc.GetRemote() == "" {
			e.field("local")
		} else {
			e.field("remote")
			if err := s.stateRemote(e, mr); err != nil {
				return err
			}
		}
		e.field(loc.GetPath())
	}
	return nil
}

// nonSecretOptions are r's options without any that shares its name with
// a secret or an obscured key (options never hold secrets; this keeps a
// misfiled one out of the state key anyway).
func nonSecretOptions(r Remote) map[string]string {
	out := make(map[string]string, len(r.Rclone.Options))
	for k, v := range r.Rclone.Options {
		if _, secret := r.Rclone.Secrets[k]; secret || slices.Contains(r.Rclone.ObscureKeys, k) {
			continue
		}
		out[k] = v
	}
	return out
}
