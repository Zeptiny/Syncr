package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckNetwork(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	var out strings.Builder
	if !checkNetwork(t.Context(), &out, net.DefaultResolver, srv.Client().Transport, srv.URL+"/", 5*time.Second) {
		t.Fatalf("checkNetwork failed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "dns: 127.0.0.1 -> 1 addresses") || !strings.Contains(out.String(), "-> 204 No Content") {
		t.Errorf("output:\n%s", out.String())
	}

	// An untrusted certificate is reported as such.
	out.Reset()
	if checkNetwork(t.Context(), &out, net.DefaultResolver, &http.Transport{}, srv.URL+"/", 5*time.Second) {
		t.Fatal("an untrusted certificate passed")
	}
	if !strings.Contains(out.String(), "do not verify the server") {
		t.Errorf("output:\n%s", out.String())
	}

	out.Reset()
	if checkNetwork(t.Context(), &out, net.DefaultResolver, &http.Transport{}, "http://example.invalid/", time.Second) {
		t.Fatal("a plain http URL passed")
	}
}

func TestReport(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	report(&out)
	for _, want := range []string{"go: ", "page size: ", "host name (os.Hostname): ", "home (os.UserHomeDir): ", "temp (os.TempDir): ", "time zone: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}
