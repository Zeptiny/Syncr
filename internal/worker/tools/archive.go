package tools

import (
	"archive/zip"
	"compress/bzip2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// extractBinary copies the single expected binary out of the archive at
// archivePath into dst and returns its size.
//
// Nothing is ever written to a path derived from the archive: the member is
// located by exact name and streamed to dst, which the caller created. That
// makes zip-slip impossible by construction; on top of it, an archive with
// any unsafe member name is rejected outright. The output is capped at
// maxBytes, enforced while reading rather than trusting declared sizes.
func extractBinary(archivePath string, art Artifact, dst io.Writer, maxBytes int64) (int64, error) {
	switch art.Format {
	case FormatZip:
		return extractZip(archivePath, art.Entry, dst, maxBytes)
	case FormatBzip2:
		return extractBzip2(archivePath, dst, maxBytes)
	default:
		return 0, fmt.Errorf("tools: unknown archive format %q", art.Format)
	}
}

func extractZip(archivePath, entry string, dst io.Writer, maxBytes int64) (int64, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		if errors.Is(err, zip.ErrInsecurePath) {
			return 0, fmt.Errorf("tools: archive contains an unsafe path: %w", err)
		}
		return 0, fmt.Errorf("tools: read zip archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var found *zip.File
	for _, f := range zr.File {
		if err := checkEntryName(f.Name); err != nil {
			return 0, err
		}
		if f.Name != entry {
			continue
		}
		if found != nil {
			return 0, fmt.Errorf("tools: archive has several entries named %q", entry)
		}
		found = f
	}
	if found == nil {
		return 0, fmt.Errorf("tools: archive does not contain %q", entry)
	}
	if !found.Mode().IsRegular() {
		return 0, fmt.Errorf("tools: archive entry %q is not a regular file", entry)
	}
	if found.UncompressedSize64 > uint64(maxBytes) { //nolint:gosec // maxBytes is positive
		return 0, fmt.Errorf("tools: archive entry %q declares %d bytes, over the %d byte limit",
			entry, found.UncompressedSize64, maxBytes)
	}
	rc, err := found.Open()
	if err != nil {
		return 0, fmt.Errorf("tools: open archive entry %q: %w", entry, err)
	}
	defer func() { _ = rc.Close() }()
	return copyLimited(dst, rc, maxBytes, "archive entry "+entry)
}

func extractBzip2(archivePath string, dst io.Writer, maxBytes int64) (int64, error) {
	f, err := os.Open(archivePath) //nolint:gosec // archivePath is a temp file this package created
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return copyLimited(dst, bzip2.NewReader(f), maxBytes, "bzip2 stream")
}

// copyLimited copies src to dst, failing if src yields more than maxBytes or
// nothing at all.
func copyLimited(dst io.Writer, src io.Reader, maxBytes int64, what string) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		return n, fmt.Errorf("tools: decompress %s: %w", what, err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("tools: %s exceeds the %d byte limit (decompression bomb?)", what, maxBytes)
	}
	if n == 0 {
		return 0, fmt.Errorf("tools: %s is empty", what)
	}
	return n, nil
}

// checkEntryName rejects member names that could escape a directory if some
// other tool extracted the archive naively.
func checkEntryName(name string) error {
	clean := strings.TrimSuffix(name, "/")
	if strings.ContainsAny(name, "\\\x00") || strings.Contains(name, ":") || !fs.ValidPath(clean) {
		return fmt.Errorf("tools: archive contains an unsafe path %q", name)
	}
	return nil
}
