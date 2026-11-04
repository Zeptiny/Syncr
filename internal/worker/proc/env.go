package proc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// buildEnv returns the child environment: the allowlisted subset of environ
// followed by extra (sorted by name), with extra overriding allowlisted
// variables of the same name. The result is never nil.
func buildEnv(environ []string, extra map[string]string) ([]string, error) {
	if err := validateEnv(extra); err != nil {
		return nil, err
	}

	env := make([]string, 0, len(environ)+len(extra))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || !allowedEnv(name) || hasEnv(extra, name) {
			continue
		}
		env = append(env, kv)
	}

	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		env = append(env, name+"="+extra[name])
	}
	return env, nil
}

// validateEnv rejects variables the OS cannot represent unambiguously.
func validateEnv(extra map[string]string) error {
	var errs []error
	for name, value := range extra {
		switch {
		case name == "":
			errs = append(errs, errors.New("environment variable with empty name"))
		case strings.ContainsAny(name, "=\x00"):
			errs = append(errs, fmt.Errorf("environment variable name %q contains '=' or NUL", name))
		case strings.ContainsRune(value, 0):
			errs = append(errs, fmt.Errorf("environment variable %s: value contains NUL", name))
		}
	}
	return errors.Join(errs...)
}

// allowedEnv reports whether the worker variable name may be passed to
// children.
func allowedEnv(name string) bool {
	for _, allowed := range allowedEnvNames() {
		if envNameEqual(name, allowed) {
			return true
		}
	}
	return false
}

// hasEnv reports whether vars contains name, using the platform's name
// comparison.
func hasEnv(vars map[string]string, name string) bool {
	for k := range vars {
		if envNameEqual(k, name) {
			return true
		}
	}
	return false
}

// commonEnvNames are passed through on every platform: locale and time zone,
// so tools format output predictably; proxy and CA settings, so tools reach
// remotes from networks that need them. They configure the network path, not
// the worker's identity.
func commonEnvNames() []string {
	return []string{
		"LANG", "LC_ALL", "LC_CTYPE", "TZ",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	}
}
