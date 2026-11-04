package api

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestPageTokenRoundTrip(t *testing.T) {
	t.Parallel()
	id := domain.NewJobID()
	token := encodePageToken(id)
	if token == "" || strings.ContainsAny(token, "=+/ ") {
		t.Errorf("token = %q, want non-empty URL-safe", token)
	}
	got, err := decodePageToken(token)
	if err != nil || got != id {
		t.Errorf("decode = %v, %v, want %v", got, err, id)
	}
	if encodePageToken(domain.JobID{}) != "" {
		t.Error("the zero ID encodes to a token")
	}
	if got, err := decodePageToken(""); err != nil || !got.IsZero() {
		t.Errorf("decode(empty) = %v, %v", got, err)
	}
}

func TestPageTokenMalformed(t *testing.T) {
	t.Parallel()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, token := range map[string]string{
		"not base64":       "!!!",
		"padded":           encodePageToken(domain.NewJobID()) + "==",
		"std alphabet":     "a+b/",
		"wrong prefix":     enc("x9:" + domain.NewJobID().String()),
		"no prefix":        enc(domain.NewJobID().String()),
		"bad id":           enc("j1:not-a-uuid"),
		"nil id":           enc("j1:00000000-0000-0000-0000-000000000000"),
		"too long":         strings.Repeat("A", maxPageToken+1),
		"empty id":         enc("j1:"),
		"trailing garbage": enc("j1:" + domain.NewJobID().String() + "!"),
	} {
		if _, err := decodePageToken(token); err == nil {
			t.Errorf("%s: decodePageToken(%q) succeeded", name, token)
		}
	}
}

func TestClampPageSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[int32]int{0: 50, 1: 1, 49: 49, 500: 500, 501: 500, 1 << 30: 500} {
		got, err := clampPageSize(in, 50, 500)
		if err != nil || got != want {
			t.Errorf("clampPageSize(%d) = %d, %v, want %d", in, got, err, want)
		}
	}
	if _, err := clampPageSize(-1, 50, 500); err == nil {
		t.Error("negative page size accepted")
	}
}
