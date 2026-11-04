package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
	"github.com/Zeptiny/Syncr/internal/worker/atomicfile"
	"github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

// execute performs the steps of a validated plan.
func (s *Setup) execute(ctx context.Context, p *plan) (*Result, error) {
	res := &Result{ConfigPath: p.configPath, DataDir: p.dataDir}
	info := s.Services.Info(p.spec)
	s.printf("Syncr worker setup: %s worker, %s scope\n\n", p.opts.Mode, p.scope)

	// 1. Directories.
	s.step("Preparing directories")
	if err := s.prepareDirs(p); err != nil {
		return nil, err
	}

	// 2. Tools.
	s.step("Provisioning rclone and restic (pinned versions, checksum-verified)")
	tools, err := s.Tools.Provision(ctx, p.dataDir, func(msg string) { s.printf("    %s\n", msg) })
	if err != nil {
		return nil, fmt.Errorf("provision the tools: %w\n"+
			"The pinned binaries are downloaded from GitHub over HTTPS. If this machine has no internet access, "+
			"copy rclone and restic of the pinned versions next to syncr-worker and run setup again", err)
	}
	res.Tools = tools
	p.toolPaths = map[string]string{}
	for _, t := range tools {
		if t.Source == "configured" {
			p.toolPaths[t.Name] = t.Path
		}
		s.printf("    %s %s (%s) %s\n", t.Name, t.Version, t.Source, t.Path)
	}

	// 3. CA and certificate.
	if err := s.stageCA(p); err != nil {
		return nil, err
	}
	if p.opts.Mode == config.LinkPassive {
		s.step("Preparing the TLS certificate")
		fp, err := s.prepareCertificate(p)
		if err != nil {
			return nil, err
		}
		res.Fingerprint = fp
		s.printf("    fingerprint (SHA-256): %s\n", fp)
	}

	// 4. Credentials: enroll, or invent the pairing secret.
	workerID, secret, err := s.credentials(ctx, p, res.Fingerprint)
	if err != nil {
		return nil, err
	}
	res.WorkerID = workerID
	if p.opts.Mode == config.LinkPassive {
		res.PairingSecret = secret
	}

	// 5. Config.
	s.step("Writing the configuration")
	cfg := s.buildConfig(p, workerID, secret)
	body, err := config.Marshal(cfg)
	if err != nil {
		return nil, s.afterEnrollError(p, workerID, err)
	}
	written, err := atomicfile.Write(p.configPath, body, atomicfile.Options{RestrictACL: true, IncludeCurrentUser: p.scope == service.ScopeUser})
	if err != nil {
		return nil, s.afterEnrollError(p, workerID, err)
	}
	s.printf("    %s (private to the worker)\n", p.configPath)
	if written.ACLWarning != nil {
		s.printf("    warning: could not restrict the file to SYSTEM and Administrators: %v\n"+
			"    The file holds secrets; restrict access to it yourself.\n", written.ACLWarning)
	}

	// 6. Service.
	if p.opts.NoService {
		s.printf("==> Skipping the service (--no-service)\n")
	} else if err := s.installService(ctx, p, info); err != nil {
		return nil, s.serviceError(p, info, err)
	}

	// 7. Self-check.
	s.step("Checking the setup")
	state, err := s.selfCheck(ctx, p, res)
	res.Service = state
	if err != nil {
		return res, fmt.Errorf("%w\nSetup wrote the configuration and installed the service, so after fixing the cause you can run: %s service restart%s",
			err, "syncr-worker", scopeFlag(p))
	}

	s.summary(p, res, info)
	return res, nil
}

func scopeFlag(p *plan) string {
	if p.scope == service.ScopeUser {
		return " --user"
	}
	return ""
}

// prepareDirs creates the data and config directories, failing early with
// advice when the rights are missing.
func (s *Setup) prepareDirs(p *plan) error {
	configPerm := fs.FileMode(0o755)
	if p.scope == service.ScopeUser {
		configPerm = 0o700
	}
	for _, d := range []struct {
		path string
		perm fs.FileMode
	}{
		{p.dataDir, 0o700},
		{filepath.Dir(p.configPath), configPerm},
	} {
		if err := os.MkdirAll(d.path, d.perm); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return fmt.Errorf("cannot create %s: %w\nSetting up for the whole machine needs root (Linux) or Administrator (Windows) rights. "+
					"Run the command with sudo or from an elevated terminal, or add --user to set up for your account only, "+
					"or choose directories you own with --data-dir and --config", d.path, err)
			}
			return fmt.Errorf("create %s: %w", d.path, err)
		}
	}
	return nil
}

// stageCA copies the controller CA next to the other worker files, so the
// service does not depend on where the operator keeps the original.
func (s *Setup) stageCA(p *plan) error {
	if p.opts.CAFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.opts.CAFile)
	if err != nil {
		return fmt.Errorf("read the CA file: %w", err)
	}
	if err := os.MkdirAll(p.tlsDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", p.tlsDir, err)
	}
	if _, err := atomicfile.Write(p.caCopy, data, atomicfile.Options{Perm: 0o644}); err != nil {
		return fmt.Errorf("store the CA file: %w", err)
	}
	return nil
}

// prepareCertificate creates the passive worker's certificate, or reuses what
// is already in place (so that re-running a failed setup keeps the
// fingerprint), and returns its fingerprint.
func (s *Setup) prepareCertificate(p *plan) (string, error) {
	if !p.ownCert {
		fp, err := tlscert.LoadFingerprint(p.certPath, p.keyPath)
		if err != nil {
			return "", fmt.Errorf("the certificate you provided cannot be used: %w", err)
		}
		return fp, nil
	}
	if !p.opts.Force {
		if fp, err := tlscert.LoadFingerprint(p.certPath, p.keyPath); err == nil {
			s.printf("    reusing the existing certificate %s\n", p.certPath)
			return fp, nil
		}
	}
	if err := os.MkdirAll(p.tlsDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", p.tlsDir, err)
	}
	dns, ips, err := tlscert.SplitHosts(p.opts.TLSNames)
	if err != nil {
		return "", err
	}
	host := p.opts.AdvertiseURL
	if u, perr := url.Parse(p.opts.AdvertiseURL); perr == nil {
		host = u.Hostname()
	}
	if local, herr := s.System.Hostname(); herr == nil && local != "" {
		dns = append(dns, local)
	}
	pair, err := tlscert.Generate(tlscert.Options{Hostname: host, DNSNames: dns, IPs: ips})
	if err != nil {
		return "", err
	}
	w, err := tlscert.Write(p.certPath, p.keyPath, pair, p.scope == service.ScopeUser)
	if err != nil {
		return "", err
	}
	s.printf("    created %s (valid until %s)\n", p.certPath, pair.NotAfter.Format(time.DateOnly))
	if w.ACLWarning != nil {
		s.printf("    warning: could not restrict the private key to SYSTEM and Administrators: %v\n", w.ACLWarning)
	}
	return pair.Fingerprint, nil
}

// credentials returns the worker ID (empty when registering by hand) and
// the secret: from the controller, or a fresh pairing secret.
func (s *Setup) credentials(ctx context.Context, p *plan, fingerprint string) (string, redact.Secret, error) {
	if !p.enroll {
		secret, err := s.randomSecret()
		return "", secret, err
	}
	s.step("Enrolling with " + p.opts.ControllerURL)
	enroller, err := s.Enroller(p.opts.ControllerURL, p.opts.CAFile)
	if err != nil {
		return "", "", err
	}
	req := EnrollRequest{
		Token: p.opts.Token, Name: p.name, OS: s.System.GOOS, Arch: s.System.GOARCH, Mode: p.opts.Mode,
	}
	if p.opts.Mode == config.LinkPassive {
		req.URL, req.Fingerprint = p.opts.AdvertiseURL, fingerprint
	}
	creds, err := enroller.Enroll(ctx, req)
	if err != nil {
		return "", "", err
	}
	s.printf("    enrolled as %q, worker ID %s\n", p.name, creds.WorkerID)
	if p.opts.Mode == config.LinkPassive {
		if err := config.ValidatePairingSecret(creds.Secret.Reveal()); err != nil {
			return "", "", s.afterEnrollError(p, creds.WorkerID,
				fmt.Errorf("the controller returned a secret a passive worker cannot use (%w)", err))
		}
	}
	return creds.WorkerID, creds.Secret, nil
}

// afterEnrollError explains how to recover when the credentials could not be
// stored. The token is spent and the secret is gone with this process.
func (s *Setup) afterEnrollError(p *plan, workerID string, err error) error {
	if !p.enroll {
		return fmt.Errorf("write the configuration: %w", err)
	}
	return fmt.Errorf("the controller enrolled this worker (ID %s) but its credentials could not be saved: %w\n"+
		"The enrollment token is spent and the secret was not stored. Remove the worker %s in the controller, "+
		"fix the problem above, and run setup again with a new token", workerID, err, workerID)
}

func (s *Setup) installService(ctx context.Context, p *plan, info service.Info) error {
	if p.existingService != service.StateNotInstalled {
		s.step("Removing the existing service (--force)")
		if err := s.Services.Uninstall(ctx, p.spec); err != nil && !errors.Is(err, service.ErrNotInstalled) {
			return fmt.Errorf("remove the existing service: %w", err)
		}
	}
	s.step(fmt.Sprintf("Installing the service (%s)", info.Platform))
	if err := s.Services.Install(ctx, p.spec); err != nil {
		return err
	}
	s.printf("    %s: %s\n", p.spec.Name, info.Location)
	s.step("Starting the service")
	if err := s.Services.Start(ctx, p.spec); err != nil {
		return fmt.Errorf("start the service: %w", err)
	}
	return nil
}

// serviceError adds the recovery path: the config is in place, only the
// service needs fixing, and enrolling again would waste a token.
func (s *Setup) serviceError(p *plan, info service.Info, err error) error {
	var perm *service.PermissionError
	if errors.As(err, &perm) {
		return fmt.Errorf("%w\nThe configuration was saved to %s. After fixing this, install the service with: syncr-worker service install --start%s",
			err, p.configPath, scopeFlag(p))
	}
	hint := ""
	if info.LogHint != "" {
		hint = "\nLogs: " + info.LogHint
	}
	return fmt.Errorf("%w\nThe configuration was saved to %s, so do not enroll again. After fixing this, run: syncr-worker service install --start%s%s",
		err, p.configPath, scopeFlag(p), hint)
}

// selfCheck verifies the result and returns the service state.
func (s *Setup) selfCheck(ctx context.Context, p *plan, res *Result) (service.State, error) {
	ok := func(format string, args ...any) { s.printf("    ok    %s\n", fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) { s.printf("    warn  %s\n", fmt.Sprintf(format, args...)) }

	// The configuration loads as the service will load it.
	loaded := config.Default()
	if err := platformconfig.Load(p.configPath, nil, &loaded); err != nil {
		return service.StateNotInstalled, fmt.Errorf("self-check: the configuration does not load: %w", err)
	}
	ok("configuration %s is valid (link mode: %s)", p.configPath, loaded.Link.Mode)
	for _, t := range res.Tools {
		ok("%s %s available", t.Name, t.Version)
	}

	state := service.StateNotInstalled
	if !p.opts.NoService {
		var err error
		if state, err = s.waitRunning(ctx, p); err != nil {
			return state, err
		}
		ok("service %s is running", p.spec.Name)
	}

	if p.opts.Mode == config.LinkPassive && !p.opts.NoService && s.Probe != nil {
		fp, err := s.Probe(ctx, p.listenProbe)
		switch {
		case err != nil:
			warn("no TLS answer from %s yet (%v); look at the service log if the controller cannot connect", p.listenProbe, err)
		case fp != res.Fingerprint:
			warn("%s serves a certificate with fingerprint %s, not %s; another program may own that port", p.listenProbe, fp, res.Fingerprint)
		default:
			ok("the worker serves the certificate with the printed fingerprint on %s", p.listenProbe)
		}
	}
	return state, nil
}

// waitRunning polls until the service has been seen running Stable times in
// a row, which tells a healthy start from a crash loop.
func (s *Setup) waitRunning(ctx context.Context, p *plan) (service.State, error) {
	deadline := time.Now().Add(s.Timing.Timeout)
	stable := 0
	state := service.StateNotInstalled
	for {
		st, err := s.Services.Status(ctx, p.spec)
		if err != nil {
			return st, fmt.Errorf("self-check: query the service: %w", err)
		}
		state = st
		if st == service.StateRunning {
			stable++
			if stable >= s.Timing.Stable {
				return st, nil
			}
		} else {
			stable = 0
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-time.After(s.Timing.Interval):
		}
	}
	info := s.Services.Info(p.spec)
	msg := fmt.Sprintf("self-check: the service did not stay running (state: %s)", state)
	if info.LogHint != "" {
		msg += "\nSee why: " + info.LogHint
	}
	return state, errors.New(msg)
}

// summary prints the final report.
func (s *Setup) summary(p *plan, res *Result, info service.Info) {
	s.printf("\nSetup complete.\n\n")
	s.printf("  Mode:        %s\n", p.opts.Mode)
	s.printf("  Name:        %s\n", p.name)
	if res.WorkerID != "" {
		s.printf("  Worker ID:   %s\n", res.WorkerID)
	}
	if p.opts.Mode == config.LinkActive {
		s.printf("  Controller:  %s\n", p.opts.ControllerURL)
	}
	s.printf("  Config:      %s\n", p.configPath)
	s.printf("  Data:        %s\n", p.dataDir)
	for _, t := range res.Tools {
		s.printf("  %-11s  %s (%s)\n", t.Name+":", t.Version, t.Source)
	}
	if !p.opts.NoService {
		s.printf("  Service:     %s, %s (%s)\n", p.spec.Name, res.Service, info.Platform)
	}

	if p.opts.Mode == config.LinkPassive && !p.enroll {
		s.printf("\nRegister this worker in the controller (Workers, Add worker, Passive worker) with these values:\n\n")
		s.printf("  URL:             %s\n", p.opts.AdvertiseURL)
		s.printf("  Fingerprint:     %s\n", res.Fingerprint)
		s.printf("  Pairing secret:  %s\n\n", res.PairingSecret.Reveal())
		s.printf("The pairing secret is also stored in %s; this is the only time it is printed.\n", p.configPath)
		s.printf("Make sure the controller can reach %s (open the port in your firewall).\n", p.opts.AdvertiseURL)
	} else if p.opts.Mode == config.LinkPassive {
		s.printf("\nThe controller pinned fingerprint %s for %s.\n", res.Fingerprint, p.opts.AdvertiseURL)
		s.printf("Make sure the controller can reach it (open the port in your firewall).\n")
	} else {
		s.printf("\nThe worker should show as online in the controller within a few seconds.\n")
	}
	if p.opts.NoService {
		s.printf("\nNo service was installed. Run the worker with: syncr-worker run --config %s\n", p.configPath)
		s.printf("Install the service later with: syncr-worker service install --start%s\n", scopeFlag(p))
	}
	if info.LogHint != "" && !p.opts.NoService {
		s.printf("\nLogs:    %s\n", info.LogHint)
		s.printf("Manage:  syncr-worker service status|stop|restart|uninstall%s\n", scopeFlag(p))
	}
	for _, note := range info.Notes {
		s.printf("\nNote: %s\n", note)
	}
	if len(p.opts.LocalRoots) == 0 {
		s.printf("\nNote: no --local-root was given, so jobs cannot read or write local files on this machine.\n"+
			"Add directories under local_roots in %s and restart the service to allow them.\n", p.configPath)
	}
	if strings.TrimSpace(p.opts.CAFile) != "" {
		s.printf("\nThe controller CA was copied to %s.\n", p.caCopy)
	}
}
