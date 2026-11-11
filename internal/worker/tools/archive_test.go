package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestExtractBinary(t *testing.T) {
	t.Parallel()
	zipArt := Artifact{Format: FormatZip, Entry: fakeEntry}
	tgzArt := Artifact{Format: FormatTarGz, Entry: "syncr-worker"}
	bin := fakeBinary(fakeVersion)
	const limit = 1 << 20

	tests := []struct {
		name    string
		art     Artifact
		archive []byte
		max     int64
		want    []byte // expected output; nil when an error is expected
		wantErr string
	}{
		{
			name: "zip with binary among other files", art: zipArt, max: limit, want: bin,
			archive: buildZip(t,
				zipMember{name: "faketool-1.2.3/", mode: os.ModeDir | 0o755},
				zipMember{name: "faketool-1.2.3/README.txt", body: []byte("hi")},
				zipMember{name: fakeEntry, body: bin, mode: 0o755},
			),
		},
		{
			name: "zip with nameless-mode entry (Windows zips)", art: zipArt, max: limit, want: bin,
			archive: buildZip(t, zipMember{name: fakeEntry, body: bin}),
		},
		{
			name: "zip-slip alongside the binary", art: zipArt, max: limit, wantErr: "unsafe path",
			archive: buildZip(t, zipMember{name: "../../evil", body: []byte("x")}, zipMember{name: fakeEntry, body: bin}),
		},
		{
			name: "zip-slip as the only entry", art: zipArt, max: limit, wantErr: "unsafe path",
			archive: buildZip(t, zipMember{name: "../faketool", body: bin}),
		},
		{
			name: "absolute entry", art: zipArt, max: limit, wantErr: "unsafe path",
			archive: buildZip(t, zipMember{name: "/etc/cron.d/x", body: bin}, zipMember{name: fakeEntry, body: bin}),
		},
		{
			name: "backslash entry", art: zipArt, max: limit, wantErr: "unsafe path",
			archive: buildZip(t, zipMember{name: `..\evil.exe`, body: bin}, zipMember{name: fakeEntry, body: bin}),
		},
		{
			name: "drive letter entry", art: zipArt, max: limit, wantErr: "unsafe path",
			archive: buildZip(t, zipMember{name: `C:evil.exe`, body: bin}, zipMember{name: fakeEntry, body: bin}),
		},
		{
			name: "binary missing", art: zipArt, max: limit, wantErr: "does not contain",
			archive: buildZip(t, zipMember{name: "faketool-1.2.3/README.txt", body: bin}),
		},
		{
			name: "entry has wrong case", art: zipArt, max: limit, wantErr: "does not contain",
			archive: buildZip(t, zipMember{name: "faketool-1.2.3/FakeTool", body: bin}),
		},
		{
			name: "duplicate entries", art: zipArt, max: limit, wantErr: "several entries",
			archive: buildZip(t, zipMember{name: fakeEntry, body: bin}, zipMember{name: fakeEntry, body: []byte("evil")}),
		},
		{
			name: "entry is a symlink", art: zipArt, max: limit, wantErr: "not a regular file",
			archive: buildZip(t, zipMember{name: fakeEntry, body: []byte("/bin/sh"), mode: os.ModeSymlink | 0o777}),
		},
		{
			name: "entry is empty", art: zipArt, max: limit, wantErr: "empty",
			archive: buildZip(t, zipMember{name: fakeEntry}),
		},
		{
			name: "zip bomb", art: zipArt, max: 1 << 10, wantErr: "limit",
			archive: buildZip(t, zipMember{name: fakeEntry, body: make([]byte, 1<<20)}),
		},
		{
			name: "not a zip", art: zipArt, max: limit, wantErr: "zip",
			archive: []byte("this is not a zip archive at all"),
		},
		{
			name: "truncated zip", art: zipArt, max: limit, wantErr: "zip",
			archive: buildZip(t, zipMember{name: fakeEntry, body: bin})[:30],
		},
		{
			name: "bz2", art: Artifact{Format: FormatBzip2}, max: limit,
			archive: mustB64(t, goodBz2B64), want: append([]byte("faketool v1.2.3\n"), bytes.Repeat([]byte("payload\n"), 4)...),
		},
		{
			name: "bz2 bomb", art: Artifact{Format: FormatBzip2}, max: 1 << 20, wantErr: "limit",
			archive: mustB64(t, bombBz2B64),
		},
		{
			name: "bz2 corrupt", art: Artifact{Format: FormatBzip2}, max: limit, wantErr: "decompress",
			archive: corrupt(mustB64(t, goodBz2B64)),
		},
		{
			name: "bz2 garbage", art: Artifact{Format: FormatBzip2}, max: limit, wantErr: "decompress",
			archive: []byte("definitely not bzip2"),
		},
		{
			name: "tar.gz with binary among other files", art: tgzArt, max: limit, want: bin,
			archive: buildTarGz(t,
				tarMember{name: "README.md", body: []byte("read me")},
				tarMember{name: "docs/", typ: tar.TypeDir},
				tarMember{name: "syncr-worker", body: bin},
				tarMember{name: "LICENSE-rclone.txt", body: []byte("text")},
			),
		},
		{
			name: "tar.gz slip alongside the binary", art: tgzArt, max: limit, wantErr: "unsafe path",
			archive: buildTarGz(t, tarMember{name: "syncr-worker", body: bin}, tarMember{name: "../../etc/evil", body: []byte("x")}),
		},
		{
			name: "tar.gz absolute entry", art: tgzArt, max: limit, wantErr: "unsafe path",
			archive: buildTarGz(t, tarMember{name: "/usr/local/bin/syncr-worker", body: bin}),
		},
		{
			name: "tar.gz backslash entry", art: tgzArt, max: limit, wantErr: "unsafe path",
			archive: buildTarGz(t, tarMember{name: `..\syncr-worker`, body: bin}),
		},
		{
			name: "tar.gz binary missing", art: tgzArt, max: limit, wantErr: "does not contain",
			archive: buildTarGz(t, tarMember{name: "syncr-worker-other", body: bin}, tarMember{name: "bin/syncr-worker", body: bin}),
		},
		{
			name: "tar.gz duplicate entries", art: tgzArt, max: limit, wantErr: "several entries",
			archive: buildTarGz(t, tarMember{name: "syncr-worker", body: bin}, tarMember{name: "syncr-worker", body: []byte("evil")}),
		},
		{
			name: "tar.gz entry is a symlink", art: tgzArt, max: limit, wantErr: "not a regular file",
			archive: buildTarGz(t, tarMember{name: "syncr-worker", typ: tar.TypeSymlink, link: "/bin/sh"}),
		},
		{
			name: "tar.gz entry is a hard link", art: tgzArt, max: limit, wantErr: "not a regular file",
			archive: buildTarGz(t, tarMember{name: "a", body: bin}, tarMember{name: "syncr-worker", typ: tar.TypeLink, link: "a"}),
		},
		{
			name: "tar.gz entry is empty", art: tgzArt, max: limit, wantErr: "empty",
			archive: buildTarGz(t, tarMember{name: "syncr-worker"}),
		},
		{
			name: "tar.gz entry over the limit", art: tgzArt, max: 1 << 10, wantErr: "limit",
			archive: buildTarGz(t, tarMember{name: "syncr-worker", body: make([]byte, 1<<20)}),
		},
		{
			name: "tar.gz bomb in another member", art: tgzArt, max: 1 << 10, wantErr: "decompression bomb",
			archive: buildTarGz(t, tarMember{name: "syncr-worker", body: []byte("small")}, tarMember{name: "padding", body: make([]byte, 1<<20)}),
		},
		{
			name: "not a tar.gz", art: tgzArt, max: limit, wantErr: "tar.gz",
			archive: []byte("this is not a gzip stream"),
		},
		{
			name: "truncated tar.gz", art: tgzArt, max: limit, wantErr: "tar.gz",
			archive: buildTarGz(t, tarMember{name: "syncr-worker", body: bytes.Repeat(bin, 100)})[:60],
		},
		{
			name: "unknown format", art: Artifact{Format: "rar"}, max: limit, wantErr: "unknown archive format",
			archive: []byte("x"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "archive")
			if err := os.WriteFile(path, tt.archive, 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			n, err := extractBinary(path, tt.art, &out, tt.max)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), tt.want) || n != int64(len(tt.want)) {
				t.Errorf("extracted %q (%d bytes), want %q", out.Bytes(), n, tt.want)
			}
		})
	}
}

// tarMember is one member of a test tar.gz archive; typ defaults to a
// regular file.
type tarMember struct {
	name string
	body []byte
	typ  byte
	link string
}

func buildTarGz(t testing.TB, members ...tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		typ := m.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: m.name, Typeflag: typ, Linkname: m.link, Mode: 0o755}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(m.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write(m.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractFileNeedsALimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(path, buildTarGz(t, tarMember{name: "x", body: []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractFile(path, FormatTarGz, "x", &bytes.Buffer{}, 0); err == nil {
		t.Error("ExtractFile without a size limit succeeded")
	}
	if _, err := ExtractFile(path, FormatBzip2, "", &bytes.Buffer{}, 1<<10); err == nil {
		t.Error("ExtractFile of a bz2 stream (no members) succeeded")
	}
}

func corrupt(b []byte) []byte {
	c := bytes.Clone(b)
	for i := 12; i < len(c); i += 3 {
		c[i] ^= 0xff
	}
	return c
}

func TestCopyLimitedStopsReading(t *testing.T) {
	t.Parallel()
	// A source that lies about its size by never ending must be cut off.
	var out bytes.Buffer
	_, err := copyLimited(&out, iotest.OneByteReader(bytes.NewReader(make([]byte, 4096))), 100, "test")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %v", err)
	}
	if out.Len() > 101 {
		t.Errorf("wrote %d bytes past the cap", out.Len())
	}
}
