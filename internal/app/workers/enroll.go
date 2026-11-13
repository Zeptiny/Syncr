package workers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Credential formats. Prefixes make leaked values recognizable (and
// scannable); the random part is 32 bytes of base64url.
const (
	tokenPrefix  = "syncr_et_" //nolint:gosec // a public prefix, not a credential
	secretPrefix = "syncr_ws_" //nolint:gosec // a public prefix, not a credential
	randomBytes  = 32

	maxNameLen          = 64
	minPairingSecretLen = 32
	maxPairingSecretLen = 512
	maxPlatformLen      = 32
)

// SecretAD is the associated data binding a worker's encrypted secret to its
// record.
func SecretAD(id domain.WorkerID) []byte { return []byte("worker-secret:" + id.String()) }

// HashToken returns the stored form of an enrollment token.
func HashToken(token string) TokenHash { return sha256.Sum256([]byte(token)) }

// CreateEnrollmentToken creates a single-use token valid for ttl (zero means
// Config.TokenTTL; otherwise MinTokenTTL to Config.MaxTokenTTL). Tokens that
// expired more than KeepExpiredTokens ago are purged on the way.
func (s *Service) CreateEnrollmentToken(ctx context.Context, ttl time.Duration) (EnrollmentGrant, error) {
	if ttl == 0 {
		ttl = s.cfg.TokenTTL
	}
	if ttl < MinTokenTTL || ttl > s.cfg.MaxTokenTTL {
		return EnrollmentGrant{}, fmt.Errorf("create enrollment token: %w: ttl %v outside [%v, %v]",
			domain.ErrInvalidArgument, ttl, MinTokenTTL, s.cfg.MaxTokenTTL)
	}
	now := s.clock.Now()
	if n, err := s.tokens.DeleteEnrollmentTokens(ctx, now.Add(-KeepExpiredTokens)); err != nil {
		s.log.WarnContext(ctx, "purging expired enrollment tokens failed", "error", err.Error())
	} else if n > 0 {
		s.log.DebugContext(ctx, "purged expired enrollment tokens", "count", n)
	}
	token, err := s.randomString(tokenPrefix)
	if err != nil {
		return EnrollmentGrant{}, fmt.Errorf("create enrollment token: %w", err)
	}
	t := EnrollmentToken{ID: s.ids.NewEnrollmentTokenID(), Hash: HashToken(token), CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := s.tokens.CreateEnrollmentToken(ctx, t); err != nil {
		return EnrollmentGrant{}, fmt.Errorf("create enrollment token: %w", err)
	}
	s.log.InfoContext(ctx, "enrollment token created", "token_id", t.ID.String(), "expires_at", t.ExpiresAt)
	return EnrollmentGrant{ID: t.ID, Token: token, ExpiresAt: t.ExpiresAt}, nil
}

// GetEnrollmentToken returns the token with this ID, so an admin can follow
// the enrollment it starts: TokenState tells whether it is waiting, used
// (UsedBy is the worker) or expired. ErrNotFound for an unknown ID or a
// token purged KeepExpiredTokens after it expired.
func (s *Service) GetEnrollmentToken(ctx context.Context, id domain.EnrollmentTokenID) (EnrollmentToken, error) {
	if id.IsZero() {
		return EnrollmentToken{}, fmt.Errorf("get enrollment token: %w", domain.ErrInvalidID)
	}
	t, err := s.tokens.GetEnrollmentToken(ctx, id)
	if err != nil {
		return EnrollmentToken{}, fmt.Errorf("get enrollment token %s: %w", id, err)
	}
	return t, nil
}

// TokenState returns t's state as of now.
func (s *Service) TokenState(t EnrollmentToken) TokenState { return t.State(s.clock.Now()) }

// Enroll redeems an enrollment token and creates the worker with a new
// random secret. The request is validated before the token is redeemed, so a
// typo does not burn it, and redeeming the token and creating the worker are
// one atomic store call (TokenRepository.EnrollWorker): if creating fails
// the token stays unused and the worker can retry with it. Invalid, expired
// and used tokens all yield ErrUnauthenticated. Worker names are unique
// among live workers: a taken name gets the first free numeric suffix
// ("nas-01" becomes "nas-01-2"), since the worker cannot ask its operator
// for another.
func (s *Service) Enroll(ctx context.Context, p EnrollParams) (Enrollment, error) {
	if !strings.HasPrefix(p.Token, tokenPrefix) || len(p.Token) > 128 {
		return Enrollment{}, fmt.Errorf("enroll: %w: invalid enrollment token", domain.ErrUnauthenticated)
	}
	mode := p.Mode
	if mode == "" {
		mode = domain.WorkerActive
	}
	if !mode.Valid() {
		return Enrollment{}, fmt.Errorf("enroll: %w: %w: %q", domain.ErrInvalidArgument, domain.ErrInvalidWorkerMode, p.Mode)
	}
	id := s.ids.NewWorkerID()
	w := Worker{ID: id, Mode: mode, Info: Info{OS: clampString(p.OS, maxPlatformLen), Arch: clampString(p.Arch, maxPlatformLen)}}
	var err error
	if strings.TrimSpace(p.Name) == "" {
		w.Name = defaultName(id)
	} else if w.Name, err = validName(p.Name); err != nil {
		return Enrollment{}, fmt.Errorf("enroll: %w", err)
	}
	switch mode {
	case domain.WorkerPassive:
		if w.URL, err = validURL(p.URL); err != nil {
			return Enrollment{}, fmt.Errorf("enroll: %w", err)
		}
		if w.CertFingerprint, err = validFingerprint(p.CertFingerprint); err != nil {
			return Enrollment{}, fmt.Errorf("enroll: %w", err)
		}
	default:
		if p.URL != "" || p.CertFingerprint != "" {
			return Enrollment{}, fmt.Errorf("enroll: %w: url and cert_fingerprint are for passive workers only", domain.ErrInvalidArgument)
		}
	}

	secret, err := s.randomString(secretPrefix)
	if err != nil {
		return Enrollment{}, fmt.Errorf("enroll: %w", err)
	}
	if w.SecretEnc, err = s.cipher.Encrypt([]byte(secret), SecretAD(id)); err != nil {
		return Enrollment{}, fmt.Errorf("enroll: encrypt secret: %w", err)
	}
	w.CreatedAt = s.clock.Now()
	if w, err = s.enrollWithFreeName(ctx, HashToken(p.Token), w); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Enrollment{}, fmt.Errorf("enroll: %w: invalid, expired or used enrollment token", domain.ErrUnauthenticated)
		}
		return Enrollment{}, fmt.Errorf("enroll: %w", err)
	}
	s.log.InfoContext(ctx, "worker enrolled", "worker_id", id.String(), "name", w.Name, "mode", string(mode))
	return Enrollment{Worker: w, Secret: secret}, nil
}

// maxNameAttempts bounds how often Enroll retries when the name it picked
// was taken concurrently.
const maxNameAttempts = 5

// maxNameSuffix bounds the numeric suffixes Enroll tries.
const maxNameSuffix = 1000

// enrollWithFreeName redeems the token hash and stores w under its name or,
// when a live worker has that name (case-insensitively), under the first
// free "<name>-2", "<name>-3", ... (the name is shortened to keep within 64
// characters). A name taken between the check and the insert is retried a
// few times; every attempt is one atomic EnrollWorker call, so a failed one
// leaves the token unused. ErrNotFound if the token cannot be redeemed: the
// store checks the token before the name, so an invalid token is reported
// as such even when every name is taken.
func (s *Service) enrollWithFreeName(ctx context.Context, hash TokenHash, w Worker) (Worker, error) {
	base := w.Name
	for attempt := 1; ; attempt++ {
		live, err := s.repo.ListWorkers(ctx, ListFilter{})
		if err != nil {
			return Worker{}, err
		}
		taken := make(map[string]bool, len(live))
		for _, o := range live {
			taken[strings.ToLower(o.Name)] = true
		}
		name, free := freeName(base, taken)
		if !free {
			name = base // taken, but lets the store report an invalid token first
		}
		w.Name = name
		_, err = s.tokens.EnrollWorker(ctx, hash, w.CreatedAt, w)
		switch {
		case err == nil:
			if name != base {
				s.log.InfoContext(ctx, "worker name was taken; enrolled with a suffix", "requested", base, "name", name)
			}
			return w, nil
		case errors.Is(err, domain.ErrAlreadyExists) && !free:
			return Worker{}, fmt.Errorf("%w: every worker name derived from %q is taken", domain.ErrAlreadyExists, base)
		case !errors.Is(err, domain.ErrAlreadyExists) || attempt == maxNameAttempts:
			return Worker{}, err
		}
	}
}

// freeName returns base or the first "<base>-<n>" (n >= 2) whose lower case
// is not in taken, cutting base so the result has at most maxNameLen runes.
func freeName(base string, taken map[string]bool) (string, bool) {
	if !taken[strings.ToLower(base)] {
		return base, true
	}
	runes := []rune(base)
	for n := 2; n <= maxNameSuffix; n++ {
		suffix := "-" + strconv.Itoa(n)
		keep := min(len(runes), maxNameLen-len(suffix))
		name := strings.TrimRightFunc(string(runes[:keep]), unicode.IsSpace) + suffix
		if !taken[strings.ToLower(name)] {
			return name, true
		}
	}
	return "", false
}

// RegisterPassive adds a passive worker from the values its setup printed.
// ErrAlreadyExists if a worker that is not removed has the name
// (case-insensitively); unlike Enroll, the admin picks another.
func (s *Service) RegisterPassive(ctx context.Context, p PassiveParams) (Worker, error) {
	name, err := validName(p.Name)
	if err != nil {
		return Worker{}, fmt.Errorf("register passive worker: %w", err)
	}
	u, err := validURL(p.URL)
	if err != nil {
		return Worker{}, fmt.Errorf("register passive worker: %w", err)
	}
	fp, err := validFingerprint(p.CertFingerprint)
	if err != nil {
		return Worker{}, fmt.Errorf("register passive worker: %w", err)
	}
	if err := validPairingSecret(p.Secret); err != nil {
		return Worker{}, fmt.Errorf("register passive worker: %w", err)
	}
	id := s.ids.NewWorkerID()
	enc, err := s.cipher.Encrypt([]byte(p.Secret), SecretAD(id))
	if err != nil {
		return Worker{}, fmt.Errorf("register passive worker: encrypt secret: %w", err)
	}
	w := Worker{
		ID: id, Name: name, Mode: domain.WorkerPassive, URL: u, CertFingerprint: fp,
		SecretEnc: enc, CreatedAt: s.clock.Now(),
	}
	if err := s.repo.CreateWorker(ctx, w); err != nil {
		return Worker{}, fmt.Errorf("register passive worker: %w", err)
	}
	s.log.InfoContext(ctx, "passive worker registered", "worker_id", id.String(), "name", name, "url", u)
	return w, nil
}

// Authenticate checks an active worker's credentials and returns the worker.
// Unknown, removed and passive workers and wrong secrets all yield
// ErrUnauthenticated (the reason is logged at debug level). A secret that
// cannot be decrypted (wrong master key) is an internal error.
func (s *Service) Authenticate(ctx context.Context, id domain.WorkerID, secret string) (Worker, error) {
	deny := func(reason string) (Worker, error) {
		s.log.DebugContext(ctx, "worker authentication failed", "worker_id", id.String(), "reason", reason)
		return Worker{}, fmt.Errorf("authenticate worker: %w", domain.ErrUnauthenticated)
	}
	if id.IsZero() || secret == "" {
		return deny("missing credentials")
	}
	w, err := s.repo.GetWorker(ctx, id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return deny("unknown worker")
	case err != nil:
		return Worker{}, fmt.Errorf("authenticate worker %s: %w", id, err)
	case w.Removed():
		return deny("worker removed")
	case w.Mode != domain.WorkerActive:
		return deny("not an active worker")
	}
	stored, err := s.cipher.Decrypt(w.SecretEnc, SecretAD(id))
	if err != nil {
		return Worker{}, fmt.Errorf("authenticate worker %s: decrypt secret: %w", id, err)
	}
	if subtle.ConstantTimeCompare(stored, []byte(secret)) != 1 {
		return deny("wrong secret")
	}
	return w, nil
}

// PassiveTarget returns what a transport needs to call a passive worker,
// including its decrypted secret. ErrNotFound for unknown, removed or active
// workers.
func (s *Service) PassiveTarget(ctx context.Context, id domain.WorkerID) (PassiveTarget, error) {
	w, err := s.Get(ctx, id)
	if err != nil {
		return PassiveTarget{}, fmt.Errorf("passive target: %w", err)
	}
	if w.Mode != domain.WorkerPassive {
		return PassiveTarget{}, fmt.Errorf("passive target %s: %w: not a passive worker", id, domain.ErrNotFound)
	}
	secret, err := s.cipher.Decrypt(w.SecretEnc, SecretAD(id))
	if err != nil {
		return PassiveTarget{}, fmt.Errorf("passive target %s: decrypt secret: %w", id, err)
	}
	return PassiveTarget{WorkerID: id, URL: w.URL, CertFingerprint: w.CertFingerprint, Secret: string(secret)}, nil
}

func (s *Service) randomString(prefix string) (string, error) {
	b := make([]byte, randomBytes)
	if _, err := io.ReadFull(s.random, b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func defaultName(id domain.WorkerID) string {
	s := id.String()
	return "worker-" + s[len(s)-6:]
}

// validName trims and checks a display name: 1 to 64 characters, no
// control characters.
func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: name is empty", domain.ErrInvalidArgument)
	case !utf8.ValidString(name):
		return "", fmt.Errorf("%w: name is not valid UTF-8", domain.ErrInvalidArgument)
	case utf8.RuneCountInString(name) > maxNameLen:
		return "", fmt.Errorf("%w: name is longer than %d characters", domain.ErrInvalidArgument, maxNameLen)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%w: name contains control characters", domain.ErrInvalidArgument)
	}
	return name, nil
}

// validURL checks a passive worker's base URL: https, a host, no
// credentials, query or fragment. A trailing slash is removed.
func validURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: url: %w", domain.ErrInvalidArgument, err)
	case u.Scheme != "https":
		return "", fmt.Errorf("%w: url must use https", domain.ErrInvalidArgument)
	case u.Hostname() == "":
		return "", fmt.Errorf("%w: url has no host", domain.ErrInvalidArgument)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return "", fmt.Errorf("%w: url must not contain credentials, a query or a fragment", domain.ErrInvalidArgument)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// validFingerprint normalizes a SHA-256 certificate fingerprint to 64
// lower-case hex digits; colons, spaces and upper case are accepted.
func validFingerprint(fp string) (string, error) {
	clean := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(fp)))
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("%w: cert_fingerprint must be a SHA-256 fingerprint (64 hex digits)", domain.ErrInvalidArgument)
	}
	return clean, nil
}

// validPairingSecret checks a worker-generated secret: 32 to 512 printable
// ASCII characters without spaces.
func validPairingSecret(secret string) error {
	if len(secret) < minPairingSecretLen || len(secret) > maxPairingSecretLen {
		return fmt.Errorf("%w: pairing secret must be %d to %d characters", domain.ErrInvalidArgument, minPairingSecretLen, maxPairingSecretLen)
	}
	for i := range len(secret) {
		if c := secret[i]; c <= ' ' || c > '~' {
			return fmt.Errorf("%w: pairing secret must be printable ASCII without spaces", domain.ErrInvalidArgument)
		}
	}
	return nil
}

// clampString cuts s to at most n bytes on a rune boundary and drops
// invalid UTF-8.
func clampString(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
