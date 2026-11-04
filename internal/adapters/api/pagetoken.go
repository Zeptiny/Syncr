package api

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Page token format.
const (
	maxPageToken = 128
	tokenPrefix  = "j1:"
)

// encodePageToken makes the opaque token for the page after the job id.
// Clients must treat it as opaque; the format may change.
func encodePageToken(next domain.JobID) string {
	if next.IsZero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(tokenPrefix + next.String()))
}

// decodePageToken is the inverse of encodePageToken; the empty token is the
// first page (zero ID). Anything else that is not a token this server made
// is an error.
func decodePageToken(token string) (domain.JobID, error) {
	if token == "" {
		return domain.JobID{}, nil
	}
	if len(token) > maxPageToken {
		return domain.JobID{}, errors.New("page_token is malformed")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return domain.JobID{}, errors.New("page_token is malformed")
	}
	rest, ok := strings.CutPrefix(string(raw), tokenPrefix)
	if !ok {
		return domain.JobID{}, errors.New("page_token is malformed")
	}
	id, err := domain.ParseJobID(rest)
	if err != nil || id.IsZero() {
		return domain.JobID{}, errors.New("page_token is malformed")
	}
	return id, nil
}

// clampPageSize applies the page size rules: 0 selects defaultSize, values
// above maxSize are lowered to it, negative values are an error.
func clampPageSize(n, defaultSize, maxSize int32) (int, error) {
	switch {
	case n < 0:
		return 0, errors.New("page_size must not be negative")
	case n == 0:
		return int(defaultSize), nil
	default:
		return int(min(n, maxSize)), nil
	}
}
