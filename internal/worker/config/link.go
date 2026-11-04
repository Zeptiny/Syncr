package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// LinkMode selects which side of the worker link initiates calls.
type LinkMode string

// The link modes.
const (
	// LinkUnset means the worker is not enrolled: `run` starts the job
	// machinery but has no link. `syncr-worker setup` sets a mode.
	LinkUnset LinkMode = ""
	// LinkActive makes the worker call the controller. It needs no open
	// port, so it works behind NAT.
	LinkActive LinkMode = "active"
	// LinkPassive makes the worker serve TLS and the controller call it.
	LinkPassive LinkMode = "passive"
)

// MinPairingSecretLen and MaxPairingSecretLen bound LinkConfig.PairingSecret;
// the controller enforces the same limits when a passive worker is registered.
const (
	MinPairingSecretLen = 32
	MaxPairingSecretLen = 512
)

// LinkConfig configures the connection between the worker and the controller.
// The fields of the other mode must stay empty.
//
// Secrets are redact.Secret values: printing or logging a Config shows
// [redacted] in their place. The configuration file holds them in clear text,
// so it must be readable only by the account running the worker (setup writes
// it with mode 0600, or an ACL for SYSTEM and Administrators on Windows).
type LinkConfig struct {
	// Mode is "active" or "passive"; empty means not enrolled.
	Mode LinkMode `yaml:"mode" env:"SYNCR_LINK_MODE"`

	// WorkerID is the ID the controller assigned at enrollment. Required in
	// active mode (it is part of the credentials); informational in passive
	// mode.
	WorkerID string `yaml:"worker_id" env:"SYNCR_WORKER_ID"`

	// Active mode.

	// ControllerURL is the controller's base URL, e.g. https://syncr.example.
	// Plain http is accepted only for loopback hosts (development).
	ControllerURL string `yaml:"controller_url" env:"SYNCR_CONTROLLER_URL"`
	// ControllerCAFile is an optional PEM file with the CA (or the
	// self-signed certificate) that signed the controller's certificate. It
	// is added to the system roots. There is deliberately no option to skip
	// certificate verification.
	ControllerCAFile string `yaml:"controller_ca_file" env:"SYNCR_CONTROLLER_CA_FILE"`
	// WorkerSecret authenticates the worker to the controller.
	WorkerSecret redact.Secret `yaml:"worker_secret" env:"SYNCR_WORKER_SECRET"`

	// Passive mode.

	// ListenAddr is the host:port the worker serves TLS on, e.g. ":7443".
	ListenAddr string `yaml:"listen_addr" env:"SYNCR_LISTEN_ADDR"`
	// AdvertiseURL is the https URL the controller uses to reach the worker.
	// It is informational for the worker (the controller stores it).
	AdvertiseURL string `yaml:"advertise_url" env:"SYNCR_ADVERTISE_URL"`
	// TLSCertFile and TLSKeyFile are the PEM certificate chain and private
	// key the worker serves. The controller pins the SHA-256 fingerprint of
	// the leaf certificate, so a self-signed certificate is fine.
	TLSCertFile string `yaml:"tls_cert_file" env:"SYNCR_TLS_CERT_FILE"`
	TLSKeyFile  string `yaml:"tls_key_file" env:"SYNCR_TLS_KEY_FILE"`
	// PairingSecret is the bearer secret the controller presents to the
	// worker: 32 to 512 printable ASCII characters, no spaces.
	PairingSecret redact.Secret `yaml:"pairing_secret" env:"SYNCR_PAIRING_SECRET"`
}

// Validate reports every invalid link setting, per mode.
func (l *LinkConfig) Validate() error {
	switch l.Mode {
	case LinkUnset:
		return l.validateUnset()
	case LinkActive:
		return l.validateActive()
	case LinkPassive:
		return l.validatePassive()
	default:
		return fmt.Errorf("link.mode: %q is not valid (want %q or %q)", l.Mode, LinkActive, LinkPassive)
	}
}

func (l *LinkConfig) validateUnset() error {
	if l.isZero() {
		return nil
	}
	return errors.New("link: settings are present but link.mode (SYNCR_LINK_MODE) is not set; use \"active\" or \"passive\"")
}

func (l *LinkConfig) isZero() bool {
	return l.WorkerID == "" && l.ControllerURL == "" && l.ControllerCAFile == "" && l.WorkerSecret == "" &&
		l.ListenAddr == "" && l.AdvertiseURL == "" && l.TLSCertFile == "" && l.TLSKeyFile == "" && l.PairingSecret == ""
}

func (l *LinkConfig) validateActive() error {
	errs := []error{
		requireURL("link.controller_url", l.ControllerURL, true),
		checkAbs("link.controller_ca_file", l.ControllerCAFile, true),
		validateWorkerID(l.WorkerID, true),
	}
	if l.WorkerSecret == "" {
		errs = append(errs, errors.New("link.worker_secret: required in active mode (SYNCR_WORKER_SECRET)"))
	}
	errs = append(errs, forbid("active", map[string]string{
		"link.listen_addr":    l.ListenAddr,
		"link.advertise_url":  l.AdvertiseURL,
		"link.tls_cert_file":  l.TLSCertFile,
		"link.tls_key_file":   l.TLSKeyFile,
		"link.pairing_secret": l.PairingSecret.Reveal(),
	}))
	return errors.Join(errs...)
}

func (l *LinkConfig) validatePassive() error {
	errs := []error{
		validateListenAddr("link.listen_addr", l.ListenAddr),
		requireURL("link.advertise_url", l.AdvertiseURL, false),
		checkAbs("link.tls_cert_file", l.TLSCertFile, false),
		checkAbs("link.tls_key_file", l.TLSKeyFile, false),
		validateWorkerID(l.WorkerID, false),
		validatePairingSecret(l.PairingSecret.Reveal()),
	}
	errs = append(errs, forbid("passive", map[string]string{
		"link.controller_url":     l.ControllerURL,
		"link.controller_ca_file": l.ControllerCAFile,
		"link.worker_secret":      l.WorkerSecret.Reveal(),
	}))
	return errors.Join(errs...)
}

// forbid reports the fields that do not belong to mode, in a stable order.
func forbid(mode string, fields map[string]string) error {
	var names []string
	for name, v := range fields {
		if v != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	return fmt.Errorf("%s: not valid in %s mode (remove it or change link.mode)", strings.Join(names, ", "), mode)
}

// requireURL checks a base URL: http(s) with a host, no credentials, query or
// fragment. Plain http is allowed only for loopback hosts, and only when
// allowLoopbackHTTP is set (the controller URL; the advertise URL must be https).
func requireURL(field, raw string, allowLoopbackHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: required", field)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", field, err)
	case u.Hostname() == "":
		return fmt.Errorf("%s: %q has no host", field, raw)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return fmt.Errorf("%s: %q must not contain credentials, a query or a fragment", field, raw)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && allowLoopbackHTTP && IsLoopbackHost(u.Hostname()):
		return nil
	case u.Scheme == "http" && allowLoopbackHTTP:
		return fmt.Errorf("%s: %q uses plain http, which is allowed only for localhost; use https", field, raw)
	default:
		return fmt.Errorf("%s: %q must use https", field, raw)
	}
}

// IsLoopbackHost reports whether host is "localhost" or a loopback IP.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func validateListenAddr(field, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s: required in passive mode, e.g. \":7443\"", field)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %q is not a host:port address (e.g. \":7443\"): %w", field, addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s: %q has an invalid port %q (want 1-65535)", field, addr, port)
	}
	return nil
}

func validateWorkerID(id string, required bool) error {
	if id == "" {
		if required {
			return errors.New("link.worker_id: required in active mode (SYNCR_WORKER_ID)")
		}
		return nil
	}
	if !isUUID(id) {
		return fmt.Errorf("link.worker_id: %q is not a UUID", id)
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// ValidatePairingSecret checks the pairing secret rules shared with the
// controller: 32 to 512 printable ASCII characters without spaces.
func ValidatePairingSecret(secret string) error {
	return validatePairingSecret(secret)
}

func validatePairingSecret(secret string) error {
	if secret == "" {
		return errors.New("link.pairing_secret: required in passive mode (SYNCR_PAIRING_SECRET)")
	}
	if len(secret) < MinPairingSecretLen || len(secret) > MaxPairingSecretLen {
		return fmt.Errorf("link.pairing_secret: must be %d to %d characters (it is %d)", MinPairingSecretLen, MaxPairingSecretLen, len(secret))
	}
	for i := range len(secret) {
		if c := secret[i]; c <= ' ' || c > '~' {
			return errors.New("link.pairing_secret: must be printable ASCII without spaces")
		}
	}
	return nil
}

// CheckControllerURL validates a controller base URL for field (the name
// shown in the error): https, or http for loopback hosts only.
func CheckControllerURL(field, raw string) error { return requireURL(field, raw, true) }

// CheckAdvertiseURL validates the https URL a passive worker is reached at.
func CheckAdvertiseURL(field, raw string) error { return requireURL(field, raw, false) }

// CheckListenAddr validates a host:port listen address.
func CheckListenAddr(field, addr string) error { return validateListenAddr(field, addr) }
