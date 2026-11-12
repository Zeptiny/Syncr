package tools

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
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
	if art.Format == FormatBzip2 {
		return extractBzip2(archivePath, dst, maxBytes)
	}
	return ExtractFile(archivePath, art.Format, art.Entry, dst, maxBytes)
}

// ExtractFile copies the regular file named entry (a slash-separated path)
// out of the zip (FormatZip) or gzip-compressed tar (FormatTarGz) archive at
// archivePath into dst and returns its size. Use it only on an archive whose
// checksum was verified.
//
// As for the pinned tools, nothing is written to a path taken from the
// archive: the member is found by its exact name and streamed to dst. An
// archive with any unsafe member name (absolute, "..", a backslash or a
// drive letter), with several members named entry, or whose entry is not a
// regular file is rejected. The output is capped at maxBytes while reading;
// a tar.gz archive is also read through at most twice that much in all, so a
// small archive that decompresses to a huge stream is refused too.
func ExtractFile(archivePath string, format Format, entry string, dst io.Writer, maxBytes int64) (int64, error) {
	if maxBytes <= 0 {
		return 0, errors.New("tools: extract: the size limit must be positive")
	}
	switch format {
	case FormatZip:
		return extractZip(archivePath, entry, dst, maxBytes)
	case FormatTarGz:
		return extractTarGz(archivePath, entry, dst, maxBytes)
	default:
		return 0, fmt.Errorf("tools: unknown archive format %q", format)
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

func extractTarGz(archivePath, entry string, dst io.Writer, maxBytes int64) (int64, error) {
	f, err := os.Open(archivePath) //nolint:gosec // archivePath is a temp file the caller created
	if err != nil {
		return 0, fmt.Errorf("tools: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("tools: read tar.gz archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	// Every member is read through, skipped ones too, so the whole stream is
	// bounded: the binary plus a release archive's small text files.
	scanLimit := 2 * maxBytes
	stream := &countingReader{r: io.LimitReader(gz, scanLimit+1)}
	tooLarge := func() error {
		return fmt.Errorf("tools: tar.gz archive decompresses to more than %d bytes (decompression bomb?)", scanLimit)
	}

	tr := tar.NewReader(stream)
	var n int64
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if stream.n > scanLimit {
				return 0, tooLarge()
			}
			return 0, fmt.Errorf("tools: read tar.gz archive: %w", err)
		}
		if err := checkEntryName(hdr.Name); err != nil {
			return 0, err
		}
		if hdr.Name != entry {
			continue
		}
		if found {
			return 0, fmt.Errorf("tools: archive has several entries named %q", entry)
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			return 0, fmt.Errorf("tools: archive entry %q is not a regular file", entry)
		}
		if hdr.Size > maxBytes {
			return 0, fmt.Errorf("tools: archive entry %q declares %d bytes, over the %d byte limit", entry, hdr.Size, maxBytes)
		}
		if n, err = copyLimited(dst, tr, maxBytes, "archive entry "+entry); err != nil {
			return 0, err
		}
	}
	if stream.n > scanLimit {
		return 0, tooLarge()
	}
	if !found {
		return 0, fmt.Errorf("tools: archive does not contain %q", entry)
	}
	return n, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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
