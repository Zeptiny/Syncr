package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/redact"
	workerconfig "github.com/Zeptiny/Syncr/internal/worker/config"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/service"
	"github.com/Zeptiny/Syncr/internal/worker/setup"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
)

// tokenEnv carries the enrollment token without putting it on the command
// line (and so into shell history and process listings).
const tokenEnv = "SYNCR_ENROLLMENT_TOKEN"

// setupOverrides lets tests replace the machine-facing collaborators of
// `setup` and `service`; every nil or zero field selects the real one.
type setupOverrides struct {
	services    service.Manager
	enroller    setup.EnrollerFactory
	provisioner setup.ToolProvisioner
	probe       setup.TLSProbe
	timing      setup.Timing
	executable  func() (string, error)
	goos        string
	hostname    func() (string, error)
}

func (o setupOverrides) manager() service.Manager {
	if o.services != nil {
		return o.services
	}
	return service.NewManager()
}

func (o setupOverrides) goosOrDefault() string {
	if o.goos != "" {
		return o.goos
	}
	return runtime.GOOS
}

func (o setupOverrides) exe() (string, error) {
	if o.executable != nil {
		return o.executable()
	}
	return executablePath()
}

// executablePath returns the absolute path of the running binary with
// symlinks resolved, which is what a service must be pointed at.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// setupCmd enrolls the worker, writes its config, provisions rclone and
// restic, installs and starts the service and checks the result.
func setupCmd(ctx context.Context, e *env, args []string) error {
	fs := newFlagSet(e, "setup")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage:
  %[1]s setup --controller URL --token TOKEN [flags]            enroll an active worker
  %[1]s setup --passive --listen :7443 --advertise-url URL      passive worker, register by hand
  %[1]s setup --passive --listen ... --advertise-url ... --controller URL --token TOKEN

Flags:
`, name)
		fs.PrintDefaults()
	}
	var (
		controller = fs.String("controller", "", "controller base URL, e.g. https://syncr.example.com")
		token      = fs.String("token", "", "single-use enrollment token from the controller (or set $"+tokenEnv+")")
		workerName = fs.String("name", "", "display name of this worker (default: the host name)")
		caFile     = fs.String("ca-file", "", "PEM file with the CA or self-signed certificate of the controller (certificate checks cannot be disabled)")
		localRoots stringList
		dataDir    = fs.String("data-dir", "", "worker data directory (default: platform default, $"+dataDirEnv+")")
		configPath = fs.String("config", "", "where to write the worker config (default: platform default)")
		user       = fs.Bool("user", false, "install for the current user only (no root or Administrator needed)")
		noService  = fs.Bool("no-service", false, "write the config but do not install or start a service")
		force      = fs.Bool("force", false, "replace an existing setup (enroll again, regenerate the certificate)")
		dryRun     = fs.Bool("dry-run", false, "print what would be done and change nothing")
		passive    = fs.Bool("passive", false, "passive worker: serve TLS and let the controller call it")
		listen     = fs.String("listen", "", "passive: address to listen on, e.g. :7443")
		advertise  = fs.String("advertise-url", "", "passive: https URL the controller reaches this worker at")
		tlsNames   stringList
		tlsCert    = fs.String("tls-cert", "", "passive: use this PEM certificate instead of generating one (needs --tls-key)")
		tlsKey     = fs.String("tls-key", "", "passive: private key of --tls-cert")
	)
	fs.Var(&localRoots, "local-root", "directory jobs may read and write (repeatable)")
	fs.Var(&tlsNames, "tls-name", "passive: extra DNS name or IP for the generated certificate (repeatable)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	tok := *token
	if tok == "" {
		tok, _ = e.lookupEnv(tokenEnv)
	}
	dir := *dataDir
	if dir == "" {
		dir, _ = e.lookupEnv(dataDirEnv)
	}
	toolsDir, _ := e.lookupEnv(toolsDirEnv)
	opts := setup.Options{
		Mode:          workerconfig.LinkActive,
		ControllerURL: *controller,
		Token:         redact.Secret(tok),
		Name:          *workerName,
		CAFile:        *caFile,
		ListenAddr:    *listen,
		AdvertiseURL:  *advertise,
		TLSNames:      tlsNames,
		TLSCertFile:   *tlsCert,
		TLSKeyFile:    *tlsKey,
		LocalRoots:    localRoots,
		DataDir:       dir,
		ConfigPath:    *configPath,
		ToolsDir:      toolsDir,
		User:          *user,
		NoService:     *noService,
		Force:         *force,
		DryRun:        *dryRun,
	}
	if *passive {
		opts.Mode = workerconfig.LinkPassive
	}

	o := e.setup
	hostname := o.hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	enroller := o.enroller
	if enroller == nil {
		enroller = setup.NewConnectEnroller
	}
	provisioner := o.provisioner
	if provisioner == nil {
		provisioner = toolProvisioner{e: e}
	}
	probe := o.probe
	if probe == nil {
		probe = probeTLS
	}
	s := setup.New(setup.Deps{
		System: setup.System{
			GOOS: o.goosOrDefault(), GOARCH: runtime.GOARCH,
			Paths:      e.pathsEnv(),
			Hostname:   hostname,
			Executable: o.exe,
		},
		Out:      e.stdout,
		Enroller: enroller,
		Tools:    provisioner,
		Services: o.manager(),
		Probe:    probe,
		Timing:   o.timing,
	})
	_, err := s.Run(ctx, opts)
	return err
}

// toolProvisioner makes the pinned rclone and restic available for setup:
// the same resolution `run` performs, with downloading allowed, in the tools
// directory setup writes to the config.
type toolProvisioner struct{ e *env }

func (p toolProvisioner) Provision(ctx context.Context, toolsDir string, progress func(string)) ([]setup.ToolStatus, error) {
	cfg := workerconfig.Default()
	cfg.Tools.Dir = toolsDir
	cfg.Tools.Download = true
	for tool, target := range map[string]*string{toolRclone: &cfg.Tools.RclonePath, toolRestic: &cfg.Tools.ResticPath} {
		if v, ok := p.e.lookupEnv("SYNCR_" + strings.ToUpper(tool) + "_PATH"); ok {
			*target = v
		}
	}
	progress("looking for rclone and restic (a missing one is downloaded and verified against its pinned checksum)")
	sup := proc.New(proc.Options{})
	quiet := slog.New(slog.DiscardHandler)
	resolved, err := resolveTools(ctx, cfg, workerDeps{
		paths:      p.e.pathsEnv(),
		bundledDir: executableDir(),
		tools:      p.e.tools,
	}, sup, toolsDir, quiet)
	if err != nil {
		return nil, err
	}
	var out []setup.ToolStatus
	for _, n := range []string{toolRclone, toolRestic} {
		r := resolved[n]
		out = append(out, setup.ToolStatus{Name: n, Version: r.Version, Source: string(r.Source), Path: r.Path})
	}
	return out, nil
}

// probeTLS connects to addr and returns the fingerprint of the certificate
// served. It does not trust anything: a controller pins the fingerprint, and
// so does this check.
func probeTLS(ctx context.Context, addr string) (string, error) {
	var fingerprint string
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 3 * time.Second},
		Config: &tls.Config{
			MinVersion:             tls.VersionTLS12,
			SessionTicketsDisabled: true, // every connection runs the fingerprint check
			InsecureSkipVerify:     true, //nolint:gosec // identity is checked by comparing the pinned fingerprint, as the controller does
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("no certificate")
				}
				fingerprint = tlscert.Fingerprint(raw[0])
				return nil
			},
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	_ = conn.Close()
	return fingerprint, nil
}
