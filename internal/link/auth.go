package link

import (
	"errors"
	"strings"
)

// ErrBadAuthorization reports an Authorization header that is missing or
// not in the link's format.
var ErrBadAuthorization = errors.New("link: malformed authorization header")

const bearerPrefix = "Bearer "

// ActiveAuthorization returns the Authorization header value an active
// worker sends to ControllerLinkService: "Bearer <worker_id>.<secret>".
func ActiveAuthorization(workerID, secret string) string {
	return bearerPrefix + workerID + "." + secret
}

// ParseActiveAuthorization splits an ActiveAuthorization header value. The
// worker ID is everything before the first '.', which UUIDs never contain.
// It does not validate the ID or check the secret.
func ParseActiveAuthorization(header string) (workerID, secret string, err error) {
	token, ok := strings.CutPrefix(header, bearerPrefix)
	if !ok {
		return "", "", ErrBadAuthorization
	}
	workerID, secret, ok = strings.Cut(token, ".")
	if !ok || workerID == "" || secret == "" {
		return "", "", ErrBadAuthorization
	}
	return workerID, secret, nil
}

// PassiveAuthorization returns the Authorization header value the
// controller sends to a passive worker's WorkerLinkService: "Bearer
// <secret>".
func PassiveAuthorization(secret string) string { return bearerPrefix + secret }

// ParsePassiveAuthorization extracts the secret of a PassiveAuthorization
// header value. Compare it with the expected secret in constant time.
func ParsePassiveAuthorization(header string) (secret string, err error) {
	secret, ok := strings.CutPrefix(header, bearerPrefix)
	if !ok || secret == "" {
		return "", ErrBadAuthorization
	}
	return secret, nil
}
