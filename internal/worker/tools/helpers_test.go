package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

const (
	fakeName    = "faketool"
	fakeVersion = "1.2.3"
	fakeEntry   = "faketool-1.2.3/faketool"
)

// fakeBinary is the content of a fake tool "binary": its first line is what
// contentDetector reports as the version output.
func fakeBinary(version string) []byte {
	return []byte(fakeName + " v" + version + "\npayload payload payload\n")
}

// The bz2 test vectors were produced with Python's bz2 module. goodBz2
// decompresses to "faketool v1.2.3\n" followed by "payload\n" four times;
// bombBz2 decompresses to 4 MiB of zero bytes.
const (
	goodBz2B64 = "QlpoOTFBWSZTWSj/cRgAAAzZgAAQQAE4ACcMxSAgADFDTTAAFVRozUNqaepBjOI+6ek6JNTa9GS1XKqD8XckU4UJAo/3EYA="
	bombBz2B64 = "QlpoOTFBWSZTWVYC4ogAICBIAMAAAAQACCAAMMwFKaYCAbECAeLuSKcKEgrAXFEA"
)

func mustB64(t testing.TB, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type zipMember struct {
	name string
	body []byte
	mode os.FileMode // zero means a regular file
}

func buildZip(t testing.TB, members ...zipMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		if m.mode != 0 {
			h.SetMode(m.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(m.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// archiveServer serves files over TLS and counts requests.
type archiveServer struct {
	*httptest.Server
	hits atomic.Int32
}

func newArchiveServer(t testing.TB, files map[string][]byte) *archiveServer {
	t.Helper()
	as := &archiveServer{}
	as.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits.Add(1)
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(as.Close)
	return as
}

// testSpec pins faketool for p to the archive served at srv/name.
func testSpec(srv *archiveServer, p Platform, name string, format Format, entry string, archive []byte) Spec {
	return Spec{
		Name:           fakeName,
		Version:        fakeVersion,
		VersionArgs:    []string{"version"},
		VersionPattern: `^faketool v(\S+)`,
		Artifacts: map[Platform]Artifact{
			p: {URL: srv.URL + "/" + name, SHA256: sum(archive), Format: format, Entry: entry},
		},
	}
}

// contentDetector "runs" a fake binary by parsing its first bytes, so tests
// need no real executables and behave the same on every OS.
type contentDetector struct {
	calls atomic.Int32
}

func (d *contentDetector) Detect(_ context.Context, spec Spec, path string) (string, error) {
	d.calls.Add(1)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return ParseVersion(spec, string(raw))
}

// newTestInstaller returns an installer wired to srv's TLS client.
func newTestInstaller(t testing.TB, srv *archiveServer, dir string, det VersionDetector) *Installer {
	t.Helper()
	inst, err := NewInstaller(InstallerOptions{Dir: dir, HTTPClient: srv.Client(), Detector: det})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func writeFile(t testing.TB, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// helperRunner runs the test binary itself as a fake tool.
type helperRunner struct {
	sup *proc.Supervisor
	env map[string]string
}

func (r helperRunner) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	spec.Env = r.env
	return r.sup.Run(ctx, spec)
}
