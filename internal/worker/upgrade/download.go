package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// Size caps and timeouts of the downloads.
const (
	// MaxChecksumsBytes caps checksums.txt.
	MaxChecksumsBytes int64 = 1 << 20
	// MaxArchiveBytes caps the downloaded archive.
	MaxArchiveBytes int64 = 256 << 20
	// MaxBinaryBytes caps the extracted worker binary.
	MaxBinaryBytes int64 = 256 << 20
	// DownloadTimeout bounds each download.
	DownloadTimeout = 10 * time.Minute
)

// HTTPDoer sends HTTP requests; *http.Client implements it. The default is
// the tools download client (internal/worker/tools.NewHTTPClient): proxy
// from the environment, and no redirect from https to plain http.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// releaseBase returns the base URL of the release assets: from, or the
// GitHub release of version, or GitHub's latest release.
func releaseBase(from, version string) (string, error) {
	if from == "" {
		if version == "" {
			return DefaultReleases + "/latest/download", nil
		}
		return DefaultReleases + "/download/" + version, nil
	}
	u, err := url.Parse(from)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: --from %q is not an absolute URL", ErrInsecureSource, from)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return "", fmt.Errorf("%w: --from must be an https URL (plain http only to this machine, e.g. http://127.0.0.1:8000), not %q", ErrInsecureSource, from)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: --from %q must not have a query or fragment", ErrInsecureSource, from)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// isLoopback reports whether host is localhost or a loopback address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// get downloads url into w, at most limit bytes, and returns the SHA-256 of
// what it wrote.
func (u *upgrader) get(ctx context.Context, target string, w io.Writer, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", target, err)
	}
	req.Header.Set("User-Agent", "syncr-worker")
	resp, err := u.cfg.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", target, resp.Status)
	}
	if resp.ContentLength > limit {
		return "", fmt.Errorf("download %s: %d bytes is over the %d byte limit", target, resp.ContentLength, limit)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", target, err)
	}
	if n > limit {
		return "", fmt.Errorf("download %s: over the %d byte limit", target, limit)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checksumOf returns the SHA-256 that checksums (a sha256sum-style
// checksums.txt: "<hex>  <name>" per line, the name optionally marked
// binary with "*") lists for name.
func checksumOf(checksums []byte, name string) (string, error) {
	var found string
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != sha256.Size*2 {
			return "", fmt.Errorf("%w: the line for %s is not a SHA-256", ErrNoChecksum, name)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("%w: the line for %s is not a SHA-256", ErrNoChecksum, name)
		}
		if found != "" && found != sum {
			return "", fmt.Errorf("%w: %s is listed twice with different checksums", ErrNoChecksum, name)
		}
		found = sum
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", ChecksumsFile, err)
	}
	if found == "" {
		return "", fmt.Errorf("%w: %s has no line for %s", ErrNoChecksum, ChecksumsFile, name)
	}
	return found, nil
}

// download fetches checksums.txt and the archive, verifies the archive and
// extracts the worker binary to a new private file next to the installed
// one, whose path it returns. The caller removes that file unless it is
// swapped in. Nothing else is left behind.
func (u *upgrader) download(ctx context.Context) (newBinary string, err error) {
	base, asset := u.base, u.asset
	u.say("Downloading %s from %s", asset, base)

	var checksums bytes.Buffer
	if _, err := u.get(ctx, base+"/"+ChecksumsFile, &checksums, MaxChecksumsBytes); err != nil {
		return "", err
	}
	want, err := checksumOf(checksums.Bytes(), asset)
	if err != nil {
		return "", err
	}

	archive, err := u.tempFile(tempDownload)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	got, err := u.get(ctx, base+"/"+asset, archive, MaxArchiveBytes)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("%w: %s has SHA-256 %s, %s lists %s; nothing was changed", ErrChecksumMismatch, asset, got, ChecksumsFile, want)
	}
	if err := archive.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", archive.Name(), err)
	}
	u.say("Verified %s (SHA-256 %s)", asset, got)

	bin, err := u.tempFile(tempBinary + u.exeSuffix())
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = bin.Close()
			_ = os.Remove(bin.Name())
		}
	}()
	if _, err := tools.ExtractFile(archive.Name(), u.format, u.member, bin, MaxBinaryBytes); err != nil {
		return "", fmt.Errorf("extract %s from %s: %w", u.member, asset, err)
	}
	if err := bin.Sync(); err != nil {
		return "", fmt.Errorf("write %s: %w", bin.Name(), err)
	}
	if err := bin.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", bin.Name(), err)
	}
	if err := os.Chmod(bin.Name(), u.mode); err != nil {
		return "", fmt.Errorf("make %s executable: %w", bin.Name(), err)
	}
	return bin.Name(), nil
}

// tempFile creates a private file next to the installed binary, so the
// swap is a rename within one directory. A directory the upgrade cannot
// write to (a worker in a container image, or a binary installed by another
// user) is refused here, before anything changed.
func (u *upgrader) tempFile(pattern string) (*os.File, error) {
	f, err := os.CreateTemp(u.dir, pattern)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: cannot write to %s, where the worker binary is: run the upgrade as the user that installed the worker "+
				"(root, or an Administrator); a worker in a container is upgraded by pulling the new image instead", ErrNotWritable, u.dir)
		}
		return nil, fmt.Errorf("create a file next to %s: %w", u.exe, err)
	}
	return f, nil
}
