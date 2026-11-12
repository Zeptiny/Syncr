package tools

import (
	"errors"
	"net"
	"net/http"
	"time"
)

// maxRedirects bounds redirect chains (GitHub release assets redirect once).
const maxRedirects = 5

// NewHTTPClient returns the client used for downloads. It has connection,
// TLS and response-header timeouts but no overall timeout (archives are
// large; the Installer bounds each download with a context), follows a few
// redirects, and refuses to be redirected from https to plain http.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          2,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("refusing redirect from https to http")
			}
			return nil
		},
	}
}
