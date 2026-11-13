// Package setup implements "syncr-worker setup": from a clean machine to an
// enrolled worker running as a service.
//
// The steps, in the order they run (everything that can fail locally runs
// before the single-use enrollment token is spent):
//
//  1. validate the request and the machine (paths, rights, existing setup);
//  2. create the directories (only the worker's accounts may change them)
//     and provision the pinned rclone and restic;
//  3. passive mode: create the self-signed certificate (in memory);
//  4. enroll with the controller (unless registering by hand);
//  5. write the certificate, the controller CA copy and the configuration
//     atomically, readable only by the worker (with --force, an existing
//     setup keeps its files until the enrollment has succeeded);
//  6. install and start the service;
//  7. check that everything works, and print a summary.
//
// Every collaborator that touches the network, the service manager or the
// tools is an interface, so tests run the whole flow with fakes.
//
// Allowed dependencies: the standard library, the generated Connect client and
// other worker packages (config, paths, tlscert, atomicfile, service).
package setup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
	"github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/paths"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

// Options is the request, parsed from the command line.
type Options struct {
	// Mode is the link mode: active, or passive for a worker that serves TLS.
	Mode config.LinkMode
	// ControllerURL and Token enroll the worker. A passive worker may omit
	// both: it then prints the values to register by hand.
	ControllerURL string
	Token         redact.Secret
	// Name is the worker's display name; empty means the host name.
	Name string
	// CAFile is a PEM file with the controller's CA or self-signed
	// certificate.
	CAFile string

	// Passive mode. ListenAddr is the address served, AdvertiseURL the URL
	// the controller uses. TLSNames adds subject alternative names to the
	// generated certificate; TLSCertFile and TLSKeyFile use an existing one.
	ListenAddr   string
	AdvertiseURL string
	TLSNames     []string
	TLSCertFile  string
	TLSKeyFile   string

	// LocalRoots are the directories jobs may use (config local_roots).
	LocalRoots []string
	// DataDir and ConfigPath override the platform defaults.
	DataDir    string
	ConfigPath string
	// ToolsDir overrides the managed tools directory (<data dir>/tools).
	// The service runs without setup's environment, so a non-default value
	// is written to the config (tools.dir).
	ToolsDir string

	// User installs for the current user only (no root or Administrator).
	User bool
	// NoService skips installing and starting the service.
	NoService bool
	// Force replaces an existing setup.
	Force bool
	// DryRun prints the plan and changes nothing.
	DryRun bool
}

// System describes the machine.
type System struct {
	GOOS, GOARCH string
	Paths        paths.Env
	Hostname     func() (string, error)
	// Executable is the absolute path of the worker binary the service will
	// run.
	Executable func() (string, error)
	// Stat looks up a file, as os.Stat does (nil means os.Stat). Setup uses
	// it to find an earlier setup's config file.
	Stat func(name string) (fs.FileInfo, error)
}

// stat is System.Stat, or os.Stat.
func (s System) stat(name string) (fs.FileInfo, error) {
	if s.Stat != nil {
		return s.Stat(name)
	}
	return os.Stat(name)
}

// ToolStatus is one provisioned tool.
type ToolStatus struct {
	Name, Version, Source, Path string
}

// ToolProvisioner makes the pinned rclone and restic available, installing a
// missing one into the managed tools directory toolsDir (downloaded and
// verified), and reports each tool. It must use toolsDir and no other
// managed directory: that is the one written to the config for the service.
type ToolProvisioner interface {
	Provision(ctx context.Context, toolsDir string, progress func(msg string)) ([]ToolStatus, error)
}

// TLSProbe connects to a passive worker's listener and returns the
// fingerprint of the certificate it serves.
type TLSProbe func(ctx context.Context, addr string) (fingerprint string, err error)

// Timing controls the self-check; zero values select the defaults.
type Timing struct {
	// Timeout is how long the service gets to start and stay up.
	Timeout time.Duration
	// Interval is the time between status polls.
	Interval time.Duration
	// Stable is how many consecutive polls must find it running.
	Stable int
}

// Deps are the collaborators of Setup.
type Deps struct {
	System   System
	Out      io.Writer
	Enroller EnrollerFactory
	Tools    ToolProvisioner
	Services service.Manager
	// Random is the entropy source for the pairing secret; nil means
	// crypto/rand.
	Random io.Reader
	Probe  TLSProbe
	Timing Timing
}

// Setup runs the setup flow.
type Setup struct {
	Deps
}

// New returns a Setup, filling defaults.
func New(d Deps) *Setup {
	if d.Random == nil {
		d.Random = rand.Reader
	}
	if d.Out == nil {
		d.Out = io.Discard
	}
	if d.Timing.Timeout == 0 {
		d.Timing.Timeout = 20 * time.Second
	}
	if d.Timing.Interval == 0 {
		d.Timing.Interval = time.Second
	}
	if d.Timing.Stable == 0 {
		d.Timing.Stable = 3
	}
	return &Setup{Deps: d}
}

// Result is the outcome of a successful setup.
type Result struct {
	DryRun     bool
	ConfigPath string
	DataDir    string
	WorkerID   string
	// Fingerprint and PairingSecret are set for passive workers.
	Fingerprint   string
	PairingSecret redact.Secret
	Tools         []ToolStatus
	Service       service.State
}

// ErrAlreadySetUp is returned (wrapped) when a setup exists and Force is not
// set.
var ErrAlreadySetUp = errors.New("this worker is already set up")

// plan is the validated, fully resolved request.
type plan struct {
	opts       Options
	pathsMode  paths.Mode
	scope      service.Scope
	name       string
	dataDir    string
	configPath string
	toolsDir   string // the managed tools directory, as the service resolves it
	tlsDir     string
	certPath   string
	keyPath    string
	caCopy     string
	// ownCert is true when setup creates the certificate.
	ownCert bool
	// enroll is true when the controller is contacted.
	enroll bool
	spec   service.Spec
	// existingConfig and existingService describe an earlier setup.
	existingConfig  bool
	existingService service.State
	listenProbe     string
	// toolPaths are explicit tool binaries (name to path) that setup found
	// through SYNCR_RCLONE_PATH or SYNCR_RESTIC_PATH; the service has no
	// such environment, so they are written to the config.
	toolPaths map[string]string
}

// Run executes the setup.
func (s *Setup) Run(ctx context.Context, opts Options) (*Result, error) {
	p, err := s.plan(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		s.printPlan(p)
		return &Result{DryRun: true, ConfigPath: p.configPath, DataDir: p.dataDir}, nil
	}
	return s.execute(ctx, p)
}

func (s *Setup) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.Out, format, args...)
}

func (s *Setup) step(title string) { s.printf("==> %s\n", title) }

// plan validates opts and resolves every path. It changes nothing.
func (s *Setup) plan(ctx context.Context, opts Options) (*plan, error) {
	p := &plan{opts: opts, scope: service.ScopeSystem, pathsMode: paths.ModeSystem}
	if opts.User {
		p.scope, p.pathsMode = service.ScopeUser, paths.ModeUser
	}
	if err := s.validateOptions(&opts); err != nil {
		return nil, err
	}
	p.opts = opts

	var err error
	if p.name = strings.TrimSpace(opts.Name); p.name == "" {
		host, herr := s.System.Hostname()
		if herr != nil || host == "" {
			host = "syncr-worker"
		}
		p.name = host
	}
	if r := []rune(p.name); len(r) > 64 {
		p.name = string(r[:64])
	}

	if p.dataDir = opts.DataDir; p.dataDir == "" {
		if p.dataDir, err = paths.DataDir(p.pathsMode, s.System.Paths); err != nil {
			return nil, fmt.Errorf("find the data directory: %w (use --data-dir)", err)
		}
	}
	if p.configPath = opts.ConfigPath; p.configPath == "" {
		if p.configPath, err = paths.ConfigFile(p.pathsMode, s.System.Paths); err != nil {
			return nil, fmt.Errorf("find the config file location: %w (use --config)", err)
		}
	}
	// The defaults derive from the environment (HOME, XDG_*), which could be
	// relative; the flags were resolved by validateOptions.
	if p.dataDir, err = filepath.Abs(p.dataDir); err != nil {
		return nil, fmt.Errorf("find the data directory: %w (use --data-dir)", err)
	}
	if p.configPath, err = filepath.Abs(p.configPath); err != nil {
		return nil, fmt.Errorf("find the config file location: %w (use --config)", err)
	}
	if p.toolsDir = opts.ToolsDir; p.toolsDir == "" {
		p.toolsDir = paths.ToolsDir(p.dataDir)
	}
	p.tlsDir = filepath.Join(p.dataDir, "tls")
	if opts.CAFile != "" {
		p.caCopy = filepath.Join(p.tlsDir, "controller-ca.pem")
	}
	if opts.Mode == config.LinkPassive {
		p.certPath, p.keyPath = opts.TLSCertFile, opts.TLSKeyFile
		if p.certPath == "" {
			p.ownCert = true
			p.certPath, p.keyPath = filepath.Join(p.tlsDir, "worker.crt"), filepath.Join(p.tlsDir, "worker.key")
		}
		p.listenProbe = probeAddr(opts.ListenAddr)
	}
	p.enroll = opts.Token != ""

	exe, err := s.System.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the worker executable: %w", err)
	}
	p.spec = service.NewSpec(s.System.GOOS, p.scope, exe, p.configPath, p.dataDir, filepath.Join(p.dataDir, "logs", "worker.log"))

	// A configuration candidate with placeholder credentials proves that
	// the request is valid before a token is spent.
	if _, err := config.Marshal(s.buildConfig(p, "00000000-0000-0000-0000-000000000000", redact.Secret(placeholderSecret))); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if opts.CAFile != "" {
		if _, err := LoadCAPool(opts.CAFile); err != nil {
			return nil, err
		}
	}
	if err := s.detectExisting(ctx, p); err != nil {
		return nil, err
	}
	if !opts.NoService {
		if err := s.Services.Preflight(p.spec); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// placeholderSecret satisfies the validation of every secret field.
const placeholderSecret = "placeholder-secret-for-validation-0123456789"

func (s *Setup) validateOptions(opts *Options) error {
	switch opts.Mode {
	case config.LinkActive, config.LinkPassive:
	default:
		return errors.New("choose a mode: active (default) or --passive")
	}
	if opts.Mode == config.LinkActive {
		if opts.ControllerURL == "" {
			return errors.New("--controller is required: the controller's https:// address, shown on its Add worker page")
		}
		if opts.Token == "" {
			return errors.New("--token is required: an enrollment token from the controller (Workers, Add worker). " +
				"To keep it out of your shell history, set SYNCR_ENROLLMENT_TOKEN instead")
		}
		for flag, v := range map[string]string{"--listen": opts.ListenAddr, "--advertise-url": opts.AdvertiseURL, "--tls-cert": opts.TLSCertFile, "--tls-key": opts.TLSKeyFile} {
			if v != "" {
				return fmt.Errorf("%s only applies to a passive worker (add --passive)", flag)
			}
		}
		if len(opts.TLSNames) > 0 {
			return errors.New("--tls-name only applies to a passive worker (add --passive)")
		}
	} else {
		if opts.ListenAddr == "" || opts.AdvertiseURL == "" {
			return errors.New("a passive worker needs --listen (for example :7443) and --advertise-url (the https:// address the controller will use)")
		}
		if (opts.ControllerURL == "") != (opts.Token == "") {
			return errors.New("to enroll a passive worker automatically give both --controller and --token; " +
				"to register it by hand in the controller, give neither")
		}
		if (opts.TLSCertFile == "") != (opts.TLSKeyFile == "") {
			return errors.New("--tls-cert and --tls-key go together")
		}
		if opts.CAFile != "" && opts.ControllerURL == "" {
			return errors.New("--ca-file is for talking to a controller; it has no use without --controller")
		}
		if opts.TLSCertFile != "" && len(opts.TLSNames) > 0 {
			return errors.New("--tls-name is for generated certificates; remove it when you provide --tls-cert")
		}
	}
	if opts.ControllerURL != "" {
		opts.ControllerURL = strings.TrimRight(opts.ControllerURL, "/")
		if err := config.CheckControllerURL("--controller", opts.ControllerURL); err != nil {
			return err
		}
	}
	if opts.Mode == config.LinkPassive {
		if err := config.CheckListenAddr("--listen", opts.ListenAddr); err != nil {
			return err
		}
		opts.AdvertiseURL = strings.TrimRight(opts.AdvertiseURL, "/")
		if err := config.CheckAdvertiseURL("--advertise-url", opts.AdvertiseURL); err != nil {
			return err
		}
		if _, _, err := tlscert.SplitHosts(opts.TLSNames); err != nil {
			return fmt.Errorf("--tls-name: %w", err)
		}
	}
	// Every path ends up in the config or the service definition, and the
	// service starts in a directory of its own (WorkingDirectory, or
	// System32 for the Windows SCM): resolve them against the directory
	// setup runs in now.
	for flag, p := range map[string]*string{
		"--data-dir": &opts.DataDir, "--config": &opts.ConfigPath, "--ca-file": &opts.CAFile,
		"--tls-cert": &opts.TLSCertFile, "--tls-key": &opts.TLSKeyFile, "SYNCR_TOOLS_DIR": &opts.ToolsDir,
	} {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return fmt.Errorf("%s %q: %w", flag, *p, err)
		}
		*p = abs
	}
	roots := make([]string, 0, len(opts.LocalRoots))
	for _, r := range opts.LocalRoots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return fmt.Errorf("--local-root %q: %w", r, err)
		}
		roots = append(roots, abs)
	}
	opts.LocalRoots = roots
	return nil
}

// probeAddr turns a listen address into one that can be dialed locally.
func probeAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// detectExisting finds an earlier setup and refuses to touch it without
// --force.
func (s *Setup) detectExisting(ctx context.Context, p *plan) error {
	if _, err := s.System.stat(p.configPath); err == nil {
		p.existingConfig = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check for an existing setup at %s: %w", p.configPath, err)
	}
	if !p.opts.NoService {
		st, err := s.Services.Status(ctx, p.spec)
		if err != nil {
			return fmt.Errorf("query the service manager: %w (use --no-service to set up without a service)", err)
		}
		p.existingService = st
	}
	if p.opts.Force || (!p.existingConfig && p.existingService == service.StateNotInstalled) {
		return nil
	}
	var found []string
	if p.existingConfig {
		found = append(found, "the config file "+p.configPath+describeExisting(p.configPath))
	}
	if p.existingService != service.StateNotInstalled {
		found = append(found, "the "+p.spec.Name+" service ("+p.existingService.String()+")")
	}
	return fmt.Errorf("%w: found %s.\n"+
		"To set it up again with a new enrollment, run the same command with --force. "+
		"The old worker entry stays on the controller; remove it there.\n"+
		"To only manage the service, use: %s service start|stop|restart|status|uninstall",
		ErrAlreadySetUp, strings.Join(found, " and "), "syncr-worker")
}

// describeExisting summarizes an existing config for the message, best
// effort.
func describeExisting(path string) string {
	cfg := config.Default()
	if err := platformconfig.Load(path, nil, &cfg); err != nil {
		return ""
	}
	switch cfg.Link.Mode {
	case config.LinkActive:
		return fmt.Sprintf(" (active worker %s of %s)", cfg.Link.WorkerID, cfg.Link.ControllerURL)
	case config.LinkPassive:
		return fmt.Sprintf(" (passive worker at %s)", cfg.Link.AdvertiseURL)
	}
	return ""
}

// buildConfig assembles the worker configuration for a plan.
func (s *Setup) buildConfig(p *plan, workerID string, secret redact.Secret) config.Config {
	cfg := config.Default()
	cfg.DataDir = p.dataDir
	cfg.LocalRoots = p.opts.LocalRoots
	// The default follows data_dir; anything else (SYNCR_TOOLS_DIR during
	// setup) must be spelled out for the service, which has no such
	// environment.
	if p.toolsDir != paths.ToolsDir(p.dataDir) {
		cfg.Tools.Dir = p.toolsDir
	}
	cfg.Tools.RclonePath = p.toolPaths["rclone"]
	cfg.Tools.ResticPath = p.toolPaths["restic"]
	l := &cfg.Link
	l.Mode = p.opts.Mode
	l.WorkerID = workerID
	if p.opts.Mode == config.LinkActive {
		l.ControllerURL = p.opts.ControllerURL
		l.ControllerCAFile = p.caCopy
		l.WorkerSecret = secret
		return cfg
	}
	l.ListenAddr = p.opts.ListenAddr
	l.AdvertiseURL = p.opts.AdvertiseURL
	l.TLSCertFile, l.TLSKeyFile = p.certPath, p.keyPath
	l.PairingSecret = secret
	if !p.enroll {
		l.WorkerID = "" // unknown until the admin registers the worker
	}
	return cfg
}

// printPlan lists what a real run would do.
func (s *Setup) printPlan(p *plan) {
	s.printf("Dry run: nothing is changed. A real run would:\n")
	n := 0
	do := func(format string, args ...any) {
		n++
		s.printf("  %d. %s\n", n, fmt.Sprintf(format, args...))
	}
	info := s.Services.Info(p.spec)
	do("create %s (data) and %s (config)", p.dataDir, filepath.Dir(p.configPath))
	do("provision the pinned rclone and restic under %s (download and verify what is missing)", p.toolsDir)
	if p.opts.Mode == config.LinkPassive {
		switch {
		case p.ownCert:
			do("create a self-signed TLS certificate for %s in %s", certHosts(p), p.tlsDir)
		default:
			do("use the TLS certificate %s", p.certPath)
		}
	}
	if p.opts.CAFile != "" {
		do("copy the CA file %s to %s", p.opts.CAFile, p.caCopy)
	}
	switch {
	case p.enroll && p.opts.Mode == config.LinkActive:
		do("enroll with %s as %q (active), using the enrollment token", p.opts.ControllerURL, p.name)
	case p.enroll:
		do("enroll with %s as %q (passive, %s), using the enrollment token", p.opts.ControllerURL, p.name, p.opts.AdvertiseURL)
	default:
		do("generate a pairing secret and print the values to register in the controller")
	}
	do("write %s (readable only by the worker)", p.configPath)
	if p.opts.NoService {
		do("skip the service (--no-service)")
	} else {
		if p.existingService != service.StateNotInstalled {
			do("remove the existing %s service (--force)", p.spec.Name)
		}
		do("install and start the %s as %q: %s", info.Platform, p.spec.Name, info.Location)
		do("check that the service stays up")
	}
	if p.existingConfig {
		s.printf("\nThe existing config %s would be replaced (--force).\n", p.configPath)
	}
	for _, note := range info.Notes {
		s.printf("\nNote: %s\n", note)
	}
}

func certHosts(p *plan) string {
	u, err := url.Parse(p.opts.AdvertiseURL)
	host := p.opts.AdvertiseURL
	if err == nil {
		host = u.Hostname()
	}
	return strings.Join(append([]string{host}, p.opts.TLSNames...), ", ")
}

// randomSecret returns 32 random bytes as base64url (43 characters), which
// satisfies the pairing secret rules.
func (s *Setup) randomSecret() (redact.Secret, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(s.Random, b); err != nil {
		return "", fmt.Errorf("generate a pairing secret: %w", err)
	}
	return redact.Secret(base64.RawURLEncoding.EncodeToString(b)), nil
}
