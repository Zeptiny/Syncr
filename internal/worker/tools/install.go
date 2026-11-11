package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/filelock"
)

// Installer limits and file names.
const (
	// DefaultMaxArchiveBytes caps the size of a downloaded archive. The
	// largest pinned archive is about 30 MiB.
	DefaultMaxArchiveBytes int64 = 256 << 20
	// DefaultMaxBinaryBytes caps the size of the extracted binary. The
	// largest pinned binary is about 85 MiB.
	DefaultMaxBinaryBytes int64 = 512 << 20
	// DefaultDownloadTimeout bounds one whole download.
	DefaultDownloadTimeout = 10 * time.Minute

	lockFileName    = ".lock"
	sidecarFileName = "install.json"
	tempPrefix      = ".syncr-tmp-"
	sidecarSchema   = 1
)

// InstallerOptions configures an Installer.
type InstallerOptions struct {
	// Dir is the tools directory. Required.
	Dir string
	// HTTPClient downloads archives; nil means NewHTTPClient().
	HTTPClient *http.Client
	// BaseURL, if set, replaces the location of every artifact: the archive
	// is fetched from BaseURL + "/" + the last path element of its pinned
	// URL. It is for mirrors and tests; the pinned SHA-256 still applies.
	BaseURL string
	// Detector, if set, is run on installed binaries of the host platform to
	// confirm that they execute and report the pinned version.
	Detector VersionDetector
	// Host is the platform whose binaries Detector can run; the zero value
	// means HostPlatform().
	Host Platform
	// MaxArchiveBytes and MaxBinaryBytes override the size caps when
	// positive.
	MaxArchiveBytes int64
	MaxBinaryBytes  int64
	// Timeout overrides DefaultDownloadTimeout when positive.
	Timeout time.Duration
	// Now overrides the clock used for sidecar timestamps.
	Now func() time.Time
}

// Installer downloads, verifies and installs pinned tool binaries into a
// tools directory laid out as
//
//	<dir>/.lock
//	<dir>/<tool>/<version>/<os>-<arch>/<binary>
//	<dir>/<tool>/<version>/<os>-<arch>/install.json
//
// It is safe for concurrent use, including by several processes.
type Installer struct {
	dir        string
	client     *http.Client
	baseURL    string
	detector   VersionDetector
	host       Platform
	maxArchive int64
	maxBinary  int64
	timeout    time.Duration
	now        func() time.Time
}

// NewInstaller returns an Installer for opts.
func NewInstaller(opts InstallerOptions) (*Installer, error) {
	if opts.Dir == "" {
		return nil, errors.New("tools: installer needs a tools directory")
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	i := &Installer{
		dir:        dir,
		client:     opts.HTTPClient,
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		detector:   opts.Detector,
		host:       opts.Host,
		maxArchive: opts.MaxArchiveBytes,
		maxBinary:  opts.MaxBinaryBytes,
		timeout:    opts.Timeout,
		now:        opts.Now,
	}
	if i.client == nil {
		i.client = NewHTTPClient()
	}
	if i.host == (Platform{}) {
		i.host = HostPlatform()
	}
	if i.maxArchive <= 0 {
		i.maxArchive = DefaultMaxArchiveBytes
	}
	if i.maxBinary <= 0 {
		i.maxBinary = DefaultMaxBinaryBytes
	}
	if i.timeout <= 0 {
		i.timeout = DefaultDownloadTimeout
	}
	if i.now == nil {
		i.now = time.Now
	}
	return i, nil
}

// Dir returns the absolute tools directory.
func (i *Installer) Dir() string { return i.dir }

// Installed describes a verified managed install.
type Installed struct {
	Tool     string
	Version  string
	Platform Platform
	// Path is the absolute path of the binary.
	Path string
	// SHA256 is the hex SHA-256 of the binary.
	SHA256 string
	// Reused is true if Install found a valid install and downloaded nothing.
	Reused bool
}

// sidecar is the JSON record stored next to an installed binary.
type sidecar struct {
	Schema        int       `json:"schema"`
	Tool          string    `json:"tool"`
	Version       string    `json:"version"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	Binary        string    `json:"binary"`
	BinarySHA256  string    `json:"binary_sha256"`
	BinarySize    int64     `json:"binary_size"`
	ArchiveSHA256 string    `json:"archive_sha256"`
	SourceURL     string    `json:"source_url"`
	InstalledAt   time.Time `json:"installed_at"`
}

func (i *Installer) targetDir(spec Spec, p Platform) string {
	return filepath.Join(i.dir, spec.Name, spec.Version, p.OS+"-"+p.Arch)
}

// Installed verifies the existing install of spec for p without touching the
// network. It returns an error wrapping ErrNotInstalled if there is none and
// one wrapping ErrInstallInvalid if the files do not match their record or
// the pin.
func (i *Installer) Installed(ctx context.Context, spec Spec, p Platform) (Installed, error) {
	art, err := spec.Artifact(p)
	if err != nil {
		return Installed{}, err
	}
	inst, err := i.verifyFiles(spec, p, art)
	if err != nil {
		return Installed{}, err
	}
	if err := i.checkRuns(ctx, spec, inst); err != nil {
		return Installed{}, err
	}
	return inst, nil
}

// checkRuns executes the binary if the platform is runnable here.
func (i *Installer) checkRuns(ctx context.Context, spec Spec, inst Installed) error {
	if i.detector == nil || inst.Platform != i.host {
		return nil
	}
	_, err := checkVersion(ctx, i.detector, spec, inst.Path)
	return err
}

// verifyFiles checks the install against its sidecar and the pin by hashing
// the binary.
func (i *Installer) verifyFiles(spec Spec, p Platform, art Artifact) (Installed, error) {
	dir := i.targetDir(spec, p)
	binPath := filepath.Join(dir, spec.Binary(p.OS))

	raw, err := os.ReadFile(filepath.Join(dir, sidecarFileName)) //nolint:gosec // built from the configured tools directory
	if errors.Is(err, fs.ErrNotExist) {
		return Installed{}, fmt.Errorf("%w: %s %s for %s", ErrNotInstalled, spec.Name, spec.Version, p)
	}
	if err != nil {
		return Installed{}, fmt.Errorf("tools: read install record: %w", err)
	}
	var sc sidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		return Installed{}, fmt.Errorf("%w: unreadable install record: %w", ErrInstallInvalid, err)
	}
	switch {
	case sc.Schema != sidecarSchema,
		sc.Tool != spec.Name, sc.Version != spec.Version,
		sc.OS != p.OS, sc.Arch != p.Arch,
		sc.Binary != spec.Binary(p.OS),
		sc.ArchiveSHA256 != art.SHA256:
		return Installed{}, fmt.Errorf("%w: install record does not match the pinned %s %s", ErrInstallInvalid, spec.Name, spec.Version)
	}

	info, err := os.Stat(binPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Installed{}, fmt.Errorf("%w: %s is missing", ErrInstallInvalid, binPath)
	}
	if err != nil {
		return Installed{}, fmt.Errorf("tools: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != sc.BinarySize {
		return Installed{}, fmt.Errorf("%w: %s changed after installation", ErrInstallInvalid, binPath)
	}
	sum, err := hashFile(binPath)
	if err != nil {
		return Installed{}, err
	}
	if sum != sc.BinarySHA256 {
		return Installed{}, fmt.Errorf("%w: %s does not match its recorded sha256", ErrInstallInvalid, binPath)
	}
	return Installed{Tool: spec.Name, Version: spec.Version, Platform: p, Path: binPath, SHA256: sum}, nil
}

// Install makes sure the pinned spec is installed for p and returns it. If a
// valid install exists nothing is downloaded (Installed.Reused is set).
// Otherwise the archive is downloaded, verified against the pinned SHA-256,
// and only then opened to extract the one expected binary.
//
// A lock file in the tools directory serializes concurrent installs across
// goroutines and processes; the install is re-checked after the lock is
// acquired, so racing callers download at most once.
func (i *Installer) Install(ctx context.Context, spec Spec, p Platform) (Installed, error) {
	art, err := spec.Artifact(p)
	if err != nil {
		return Installed{}, err
	}
	if err := os.MkdirAll(i.dir, 0o755); err != nil { //nolint:gosec // the tools directory must be readable by the worker service user
		return Installed{}, fmt.Errorf("tools: create tools directory: %w", err)
	}
	release, err := i.lock(ctx)
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = release() }()

	inst, err := i.verifyFiles(spec, p, art)
	if err == nil {
		if err := i.checkRuns(ctx, spec, inst); err == nil {
			inst.Reused = true
			return inst, nil
		}
		// Right bytes but not runnable: fall through and reinstall once.
	} else if !errors.Is(err, ErrNotInstalled) && !errors.Is(err, ErrInstallInvalid) {
		return Installed{}, err
	}

	inst, err = i.install(ctx, spec, p, art)
	if err != nil {
		return Installed{}, err
	}
	if err := i.checkRuns(ctx, spec, inst); err != nil {
		return Installed{}, fmt.Errorf("tools: freshly installed %s is unusable: %w", spec.Name, err)
	}
	return inst, nil
}

// lock takes the tools directory's install lock, waiting for other holders
// (another worker, a concurrent setup or tools fetch) until ctx ends.
func (i *Installer) lock(ctx context.Context) (release func() error, err error) {
	release, err = filelock.Acquire(ctx, filepath.Join(i.dir, lockFileName))
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	return release, nil
}

// install performs the download and replace; the caller holds the lock.
func (i *Installer) install(ctx context.Context, spec Spec, p Platform, art Artifact) (Installed, error) {
	dir := i.targetDir(spec, p)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the tools directory must be readable by the worker service user
		return Installed{}, fmt.Errorf("tools: create %s: %w", dir, err)
	}
	// The lock is held, so every temp file here is left over from a crash.
	removeTemps(dir)
	// Drop the record first: until the new one is written, the directory
	// reads as "not installed" rather than describing a different binary.
	if err := os.Remove(filepath.Join(dir, sidecarFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Installed{}, fmt.Errorf("tools: remove stale install record: %w", err)
	}

	archivePath, err := i.download(ctx, dir, art)
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = os.Remove(archivePath) }()

	binName := spec.Binary(p.OS)
	sum, size, err := i.extractTo(archivePath, art, dir, binName)
	if err != nil {
		return Installed{}, err
	}

	sc := sidecar{
		Schema: sidecarSchema, Tool: spec.Name, Version: spec.Version,
		OS: p.OS, Arch: p.Arch, Binary: binName,
		BinarySHA256: sum, BinarySize: size, ArchiveSHA256: art.SHA256,
		SourceURL: art.URL, InstalledAt: i.now().UTC(),
	}
	raw, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return Installed{}, fmt.Errorf("tools: encode install record: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, sidecarFileName), append(raw, '\n'), 0o644); err != nil {
		return Installed{}, err
	}
	syncDir(dir)
	return Installed{Tool: spec.Name, Version: spec.Version, Platform: p, Path: filepath.Join(dir, binName), SHA256: sum}, nil
}

// downloadURL applies BaseURL.
func (i *Installer) downloadURL(art Artifact) (string, error) {
	if i.baseURL == "" {
		return art.URL, nil
	}
	u, err := url.Parse(art.URL)
	if err != nil {
		return "", fmt.Errorf("tools: %w", err)
	}
	return i.baseURL + "/" + path.Base(u.Path), nil
}

// download streams the artifact into a temp file in dir and verifies its
// SHA-256 against the pinned digest. On any error the temp file is removed.
func (i *Installer) download(ctx context.Context, dir string, art Artifact) (name string, err error) {
	target, err := i.downloadURL(art)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("tools: %w", err)
	}
	req.Header.Set("User-Agent", "syncr-worker")
	resp, err := i.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tools: download %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tools: download %s: %s", target, resp.Status)
	}
	if resp.ContentLength > i.maxArchive {
		return "", fmt.Errorf("tools: download %s: %d bytes exceeds the %d byte limit", target, resp.ContentLength, i.maxArchive)
	}

	f, err := os.CreateTemp(dir, tempPrefix+"dl-*")
	if err != nil {
		return "", fmt.Errorf("tools: create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, i.maxArchive+1))
	if err != nil {
		return "", fmt.Errorf("tools: download %s: %w", target, err)
	}
	if n > i.maxArchive {
		return "", fmt.Errorf("tools: download %s exceeds the %d byte limit", target, i.maxArchive)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != art.SHA256 {
		return "", &ChecksumError{URL: target, Want: art.SHA256, Got: got}
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("tools: flush download: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("tools: close download: %w", err)
	}
	return f.Name(), nil
}

// extractTo writes the binary from the verified archive to dir/binName
// atomically and returns its SHA-256 and size.
func (i *Installer) extractTo(archivePath string, art Artifact, dir, binName string) (sum string, size int64, err error) {
	f, err := os.CreateTemp(dir, tempPrefix+"bin-*")
	if err != nil {
		return "", 0, fmt.Errorf("tools: create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	size, err = extractBinary(archivePath, art, io.MultiWriter(f, h), i.maxBinary)
	if err != nil {
		return "", 0, err
	}
	if err = f.Sync(); err != nil {
		return "", 0, fmt.Errorf("tools: flush binary: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", 0, fmt.Errorf("tools: close binary: %w", err)
	}
	if err = os.Chmod(f.Name(), 0o755); err != nil { //nolint:gosec // an executable must be runnable by the worker user
		return "", 0, fmt.Errorf("tools: make binary executable: %w", err)
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, binName)); err != nil {
		return "", 0, fmt.Errorf("tools: install binary: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// writeFileAtomic writes data to name via a temp file, fsync and rename.
func writeFileAtomic(name string, data []byte, perm fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(name), tempPrefix+"meta-*")
	if err != nil {
		return fmt.Errorf("tools: create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("tools: write %s: %w", name, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("tools: flush %s: %w", name, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("tools: close %s: %w", name, err)
	}
	if err = os.Chmod(f.Name(), perm); err != nil {
		return fmt.Errorf("tools: chmod %s: %w", name, err)
	}
	if err = os.Rename(f.Name(), name); err != nil {
		return fmt.Errorf("tools: write %s: %w", name, err)
	}
	return nil
}

// syncDir flushes directory metadata (the renames) where the OS supports it;
// it is best effort because Windows cannot sync directories.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // built from the configured tools directory
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func removeTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func hashFile(name string) (string, error) {
	f, err := os.Open(name) //nolint:gosec // built from the configured tools directory
	if err != nil {
		return "", fmt.Errorf("tools: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("tools: hash %s: %w", name, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
