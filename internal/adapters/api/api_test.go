package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

func newServer(t *testing.T, info version.Info) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(Deps{BuildInfo: info, Logger: logging.Discard()}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetInfo(t *testing.T) {
	t.Parallel()
	built := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	srv := newServer(t, version.Info{Version: "v2.0.0", Commit: "abc123", BuildTime: built})

	for name, opts := range map[string][]connect.ClientOption{
		"connect json": {connect.WithProtoJSON()},
		"connect get":  {connect.WithHTTPGet()},
		"grpc-web":     {connect.WithGRPCWeb()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := syncrv1connect.NewSystemServiceClient(srv.Client(), srv.URL, opts...)
			resp, err := client.GetInfo(t.Context(), connect.NewRequest(&syncrv1.GetInfoRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.GetVersion() != "v2.0.0" || resp.Msg.GetCommit() != "abc123" || !resp.Msg.GetBuildTime().AsTime().Equal(built) {
				t.Errorf("GetInfo() = %v", resp.Msg)
			}
		})
	}
}

func TestGetInfoUnknownBuildTime(t *testing.T) {
	t.Parallel()
	resp, err := NewSystemService(version.Info{Version: "dev"}).GetInfo(context.Background(), connect.NewRequest(&syncrv1.GetInfoRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetBuildTime() != nil {
		t.Errorf("BuildTime = %v, want unset", resp.Msg.GetBuildTime())
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	srv := newServer(t, version.Info{})

	for method, wantStatus := range map[string]int{
		http.MethodGet:  http.StatusOK,
		http.MethodHead: http.StatusOK,
		http.MethodPost: http.StatusMethodNotAllowed,
	} {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Errorf("%s /healthz: status %d, want %d", method, resp.StatusCode, wantStatus)
		}
		if method == http.MethodGet && string(body) != "ok\n" {
			t.Errorf("GET /healthz body = %q", body)
		}
	}
}
