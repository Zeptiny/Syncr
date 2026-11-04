package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestRunServesAndShutsDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var logs bytes.Buffer
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	env := envOf(map[string]string{"SYNCR_HTTP_ADDR": "127.0.0.1:0", "SYNCR_LOG_FORMAT": "json"})
	go func() { done <- run(ctx, nil, io.Discard, &logs, env, ready) }()

	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run() exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not start")
	}
	base := "http://" + addr.String()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status %d", resp.StatusCode)
	}

	client := syncrv1connect.NewSystemServiceClient(http.DefaultClient, base)
	info, err := client.GetInfo(t.Context(), connect.NewRequest(&syncrv1.GetInfoRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if info.Msg.GetVersion() == "" {
		t.Error("GetInfo returned an empty version")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v after shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not shut down")
	}
	if !strings.Contains(logs.String(), `"msg":"controller stopped"`) {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := run(t.Context(), []string{"-version"}, &out, io.Discard, envOf(nil), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "syncr-controller ") {
		t.Errorf("output %q", out.String())
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), nil, io.Discard, io.Discard, envOf(map[string]string{"SYNCR_HTTP_ADDR": "nope"}), nil)
	if err == nil || !strings.Contains(err.Error(), "http.addr") {
		t.Fatalf("run() = %v, want config validation error", err)
	}
	err = run(t.Context(), []string{"-config", "/does/not/exist.yaml"}, io.Discard, io.Discard, envOf(nil), nil)
	if err == nil {
		t.Fatal("run() with a missing config file = nil")
	}
	if err := run(t.Context(), []string{"-bogus"}, io.Discard, io.Discard, envOf(nil), nil); !errors.Is(err, errUsage) {
		t.Fatalf("run(-bogus) = %v, want errUsage", err)
	}
}
