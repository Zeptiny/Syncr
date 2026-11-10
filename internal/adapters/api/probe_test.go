package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakeProbes is the ProbeService port: it records its arguments and
// answers with err or a fixed response.
type fakeProbes struct {
	mu    sync.Mutex
	err   error
	calls []probeCall
}

type probeCall struct {
	actor    domain.Actor
	worker   domain.WorkerID
	remote   domain.RemoteID
	path     string
	dirsOnly bool
	input    *remotes.Input
}

func (f *fakeProbes) record(c probeCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.err
}

func (f *fakeProbes) last(t *testing.T) probeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the probe service was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeProbes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProbes) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeProbes) ListWorkerFiles(_ context.Context, actor domain.Actor, workerID domain.WorkerID, path string, dirsOnly bool) (*syncrv1.ListWorkerFilesResponse, error) {
	if err := f.record(probeCall{actor: actor, worker: workerID, path: path, dirsOnly: dirsOnly}); err != nil {
		return nil, err
	}
	return &syncrv1.ListWorkerFilesResponse{
		Path: path, Entries: []*syncrv1.ProbeEntry{{Name: "photos", Path: path + "/photos", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR}},
		Worker: &syncrv1.ProbeWorkerRef{Id: workerID.String(), Name: "nas"},
	}, nil
}

func (f *fakeProbes) ListRemoteFiles(_ context.Context, actor domain.Actor, remoteID domain.RemoteID, path string, workerID domain.WorkerID, dirsOnly bool) (*syncrv1.ListRemoteFilesResponse, error) {
	if err := f.record(probeCall{actor: actor, worker: workerID, remote: remoteID, path: path, dirsOnly: dirsOnly}); err != nil {
		return nil, err
	}
	return &syncrv1.ListRemoteFilesResponse{Path: path, Truncated: true}, nil
}

func (f *fakeProbes) TestRemote(_ context.Context, actor domain.Actor, id domain.RemoteID, in *remotes.Input, path string, workerID domain.WorkerID) (*syncrv1.TestRemoteResponse, error) {
	if err := f.record(probeCall{actor: actor, worker: workerID, remote: id, path: path, input: in}); err != nil {
		return nil, err
	}
	return &syncrv1.TestRemoteResponse{Ok: true, Entries: 3}, nil
}

func (e *testEnv) probesFor(a domain.Actor) syncrv1connect.ProbeServiceClient {
	return syncrv1connect.NewProbeServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func probeEnv(t *testing.T) (*testEnv, *fakeProbes) {
	t.Helper()
	f := &fakeProbes{}
	return newEnv(t, func(d *Deps) { d.Probes = f }), f
}

// violation returns the message of err's field violation on field ("" if
// it has none).
func violation(err error, field string) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return ""
	}
	for _, d := range cerr.Details() {
		msg, derr := d.Value()
		if derr != nil {
			continue
		}
		if v, ok := msg.(*syncrv1.ValidationError); ok {
			for _, fv := range v.GetViolations() {
				if fv.GetField() == field {
					return fv.GetMessage()
				}
			}
		}
	}
	return ""
}

func TestProbeListWorkerFiles(t *testing.T) {
	t.Parallel()
	e, f := probeEnv(t)
	ctx := context.Background()
	worker := domain.NewWorkerID()

	// Any signed-in user may browse a worker.
	for _, actor := range []domain.Actor{testUser, testAdmin} {
		resp, err := e.probesFor(actor).ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{
			WorkerId: worker.String(), Path: "/data", DirsOnly: true,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.GetPath() != "/data" || resp.Msg.GetEntries()[0].GetPath() != "/data/photos" || resp.Msg.GetWorker().GetName() != "nas" {
			t.Errorf("response = %v", resp.Msg)
		}
		if c := f.last(t); c.actor.UserID != actor.UserID || c.worker != worker || c.path != "/data" || !c.dirsOnly {
			t.Errorf("call = %+v", c)
		}
	}

	before := f.count()
	cl := e.probesFor(testUser)
	_, err := cl.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{Path: "/data"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if violation(err, "worker_id") != "is required" {
		t.Errorf("no worker: %v", err)
	}
	_, err = cl.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if f.count() != before {
		t.Error("an invalid request reached the service")
	}

	// Without a session: unauthenticated, and nothing is called.
	anon := syncrv1connect.NewProbeServiceClient(e.srv.Client(), e.srv.URL)
	_, err = anon.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: worker.String()}))
	wantCode(t, err, connect.CodeUnauthenticated)
	if f.count() != before {
		t.Error("an unauthenticated request reached the service")
	}
}

// TestProbeErrorMapping: the probe use cases' errors keep their class and,
// where they are written for people, their message.
func TestProbeErrorMapping(t *testing.T) {
	t.Parallel()
	e, f := probeEnv(t)
	ctx := context.Background()
	cl := e.probesFor(testUser)
	req := func() *connect.Request[syncrv1.ListRemoteFilesRequest] {
		return connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: domain.NewRemoteID().String(), Path: "photos"})
	}

	tests := []struct {
		name string
		err  error
		code connect.Code
		msg  string // "" : must not reveal err's text
	}{
		{"offline worker", fmt.Errorf(`%w: worker "nas" is offline`, domain.ErrUnavailable), connect.CodeUnavailable, `worker "nas" is offline`},
		{"outdated worker", fmt.Errorf(`%w: worker "nas" (version v2.4.0) cannot run this request; upgrade it`, domain.ErrFailedPrecondition),
			connect.CodeFailedPrecondition, "upgrade it"},
		{"foreign remote", fmt.Errorf("remote 0190: %w", domain.ErrNotFound), connect.CodeNotFound, ""},
		{"rate limited", fmt.Errorf("%w: too many requests to workers", domain.ErrRateLimited), connect.CodeResourceExhausted, ""},
		{"path outside the roots", &domain.ValidationError{Fields: []domain.FieldError{{Field: "path", Message: "is outside the worker's allowed folders"}}},
			connect.CodeInvalidArgument, "is outside the worker's allowed folders"},
		{"an internal failure", errors.New("pq: connection to 10.0.0.5 refused"), connect.CodeInternal, ""},
	}
	for _, tt := range tests {
		f.fail(tt.err)
		_, err := cl.ListRemoteFiles(ctx, req())
		if codeOf(t, err) != tt.code {
			t.Errorf("%s: code = %v, want %v (%v)", tt.name, codeOf(t, err), tt.code, err)
			continue
		}
		switch {
		case tt.msg != "" && !strings.Contains(err.Error(), tt.msg):
			t.Errorf("%s: message %q lacks %q", tt.name, err, tt.msg)
		case tt.msg == "" && (strings.Contains(err.Error(), "0190") || strings.Contains(err.Error(), "10.0.0.5")):
			t.Errorf("%s: message %q reveals internals", tt.name, err)
		}
	}
	f.fail(&domain.ValidationError{Fields: []domain.FieldError{{Field: "worker_id", Message: "choose the worker whose disk holds this remote"}}})
	_, err := cl.ListRemoteFiles(ctx, req())
	if violation(err, "worker_id") != "choose the worker whose disk holds this remote" {
		t.Errorf("worker_id violation: %v", err)
	}
}

func TestProbeListRemoteFiles(t *testing.T) {
	t.Parallel()
	e, f := probeEnv(t)
	ctx := context.Background()
	cl := e.probesFor(testUser)
	remote, worker := domain.NewRemoteID(), domain.NewWorkerID()

	resp, err := cl.ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: remote.String(), Path: "photos"}))
	if err != nil || resp.Msg.GetPath() != "photos" || !resp.Msg.GetTruncated() {
		t.Fatalf("response = %v, %v", resp, err)
	}
	if c := f.last(t); c.remote != remote || !c.worker.IsZero() || c.path != "photos" || c.dirsOnly || c.actor.UserID != testUser.UserID {
		t.Errorf("call = %+v", c)
	}
	if _, err = cl.ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{
		RemoteId: remote.String(), WorkerId: worker.String(), DirsOnly: true,
	})); err != nil {
		t.Fatal(err)
	}
	if c := f.last(t); c.worker != worker || !c.dirsOnly {
		t.Errorf("call = %+v", c)
	}

	before := f.count()
	for name, msg := range map[string]*syncrv1.ListRemoteFilesRequest{
		"no remote":    {Path: "x"},
		"bad remote":   {RemoteId: "nope"},
		"bad worker":   {RemoteId: remote.String(), WorkerId: "nope"},
		"zero remote":  {RemoteId: domain.RemoteID{}.String()},
		"zero worker ": {RemoteId: remote.String(), WorkerId: domain.WorkerID{}.String()},
	} {
		_, err := cl.ListRemoteFiles(ctx, connect.NewRequest(msg))
		if codeOf(t, err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.count() != before {
		t.Error("an invalid request reached the service")
	}
}

func TestProbeTestRemote(t *testing.T) {
	t.Parallel()
	e, f := probeEnv(t)
	ctx := context.Background()
	cl := e.probesFor(testUser)
	remote, worker, member := domain.NewRemoteID(), domain.NewWorkerID(), domain.NewRemoteID()
	const secret = "tested-secret-sentinel"

	// A stored remote.
	resp, err := cl.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{RemoteId: remote.String(), Path: "sub", WorkerId: worker.String()}))
	if err != nil || !resp.Msg.GetOk() || resp.Msg.GetEntries() != 3 {
		t.Fatalf("response = %v, %v", resp, err)
	}
	if c := f.last(t); c.remote != remote || c.input != nil || c.path != "sub" || c.worker != worker {
		t.Errorf("call = %+v", c)
	}

	// An unsaved new remote.
	input := &syncrv1.RemoteInput{
		Name: "New", BackendType: "union", Options: map[string]string{"policy": "epmfs"}, Secrets: map[string]string{"secret_access_key": secret},
		Members: []*syncrv1.UnionMember{{RemoteId: member.String(), Path: "a", Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY}},
	}
	if _, err = cl.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{Input: input})); err != nil {
		t.Fatal(err)
	}
	c := f.last(t)
	if !c.remote.IsZero() || c.input == nil || c.input.Name != "New" || c.input.Type != "union" || c.input.Secrets["secret_access_key"] != secret ||
		len(c.input.Members) != 1 || c.input.Members[0].RemoteID != member || c.input.Members[0].Path != "a" {
		t.Errorf("call = %+v, input %+v", c, c.input)
	}
	// A stored remote with unsaved values.
	if _, err = cl.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{RemoteId: remote.String(), Input: input})); err != nil {
		t.Fatal(err)
	}
	if c := f.last(t); c.remote != remote || c.input == nil {
		t.Errorf("call = %+v", c)
	}

	before := f.count()
	_, err = cl.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if violation(err, "remote_id") == "" {
		t.Errorf("nothing to test: %v", err)
	}
	_, err = cl.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{Input: &syncrv1.RemoteInput{
		Members: []*syncrv1.UnionMember{{RemoteId: "nope", Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY}},
	}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if violation(err, "members[0].remote_id") == "" {
		t.Errorf("malformed member: %v", err)
	}
	if f.count() != before {
		t.Error("an invalid request reached the service")
	}
	// The tested secret is in no log line.
	if strings.Contains(e.logs.String(), secret) {
		t.Error("the tested secret was logged")
	}
}

// TestProbeServiceDisabled: without a probe service the procedures answer
// Unimplemented.
func TestProbeServiceDisabled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.probesFor(testUser).ListWorkerFiles(context.Background(), connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: domain.NewWorkerID().String()}))
	wantCode(t, err, connect.CodeUnimplemented)
}
