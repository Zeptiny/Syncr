// Command gofacts prints what a Go program built like syncr-worker sees on
// the machine it runs on: the host name, home and temporary directories,
// the time zone, the page size, and whether DNS and TLS work from Go
// itself (the resolver cgo selects on Android, and the system's CA
// certificates). It is part of the Android spike kit (android/spike) and
// prints nothing secret.
//
//	gofacts [-url https://downloads.rclone.org/] [-timeout 20s]
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"
)

func main() {
	target := flag.String("url", "https://downloads.rclone.org/", "https URL to resolve and fetch")
	timeout := flag.Duration("timeout", 20*time.Second, "limit for the DNS lookup and for the request")
	flag.Parse()

	ctx := context.Background()
	report(os.Stdout)
	ok := checkNetwork(ctx, os.Stdout, net.DefaultResolver, http.DefaultTransport, *target, *timeout)
	if !ok {
		os.Exit(1)
	}
}

// report prints the process's view of the machine.
func report(w io.Writer) {
	fmt.Fprintf(w, "go: %s %s/%s, %d CPUs\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Fprintf(w, "page size: %d\n", os.Getpagesize())
	fmt.Fprintf(w, "host name (os.Hostname): %s\n", valueOrError(os.Hostname()))
	fmt.Fprintf(w, "home (os.UserHomeDir): %s\n", valueOrError(os.UserHomeDir()))
	fmt.Fprintf(w, "temp (os.TempDir): %s\n", os.TempDir())
	name, offset := time.Now().Zone()
	fmt.Fprintf(w, "time zone: Local=%s, now %s (UTC%+03d:%02d)\n", time.Local, name, offset/3600, abs(offset%3600)/60)
}

// checkNetwork resolves the URL's host and fetches the URL (HEAD) through
// rt, which verifies the server's certificate against the system's CA
// certificates. It prints what happened and reports success.
func checkNetwork(ctx context.Context, w io.Writer, res *net.Resolver, rt http.RoundTripper, target string, timeout time.Duration) bool {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		fmt.Fprintf(w, "network: %q is not an https URL\n", target)
		return false
	}
	ok := true

	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	start := time.Now()
	addrs, err := res.LookupHost(lookupCtx, u.Hostname())
	cancel()
	if err != nil {
		fmt.Fprintf(w, "dns: %s FAILED after %s: %v\n", u.Hostname(), since(start), err)
		ok = false
	} else {
		fmt.Fprintf(w, "dns: %s -> %d addresses in %s\n", u.Hostname(), len(addrs), since(start))
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, u.String(), http.NoBody)
	if err != nil {
		fmt.Fprintf(w, "https: %v\n", err)
		return false
	}
	start = time.Now()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		var verr *tls.CertificateVerificationError
		if errors.As(err, &verr) {
			fmt.Fprintf(w, "https: %s FAILED: the system's CA certificates do not verify the server: %v\n", u.Host, err)
		} else {
			fmt.Fprintf(w, "https: %s FAILED after %s: %v\n", u.Host, since(start), err)
		}
		return false
	}
	_ = resp.Body.Close()
	fmt.Fprintf(w, "https: HEAD %s -> %s in %s, %s, verified chain: %s\n",
		u.String(), resp.Status, since(start), tlsVersion(resp.TLS), chainSummary(resp.TLS))
	return ok
}

func tlsVersion(st *tls.ConnectionState) string {
	if st == nil {
		return "no TLS"
	}
	return tls.VersionName(st.Version)
}

// chainSummary names the subject of the first verified chain's root.
func chainSummary(st *tls.ConnectionState) string {
	if st == nil || len(st.VerifiedChains) == 0 || len(st.VerifiedChains[0]) == 0 {
		return "none"
	}
	chain := st.VerifiedChains[0]
	return fmt.Sprintf("%d certificates, root %q", len(chain), chain[len(chain)-1].Subject.CommonName)
}

func valueOrError(v string, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return v
}

func since(t time.Time) time.Duration { return time.Since(t).Round(time.Millisecond) }

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
