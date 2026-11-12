package probe_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// listing answers list probes with entries named names under the listed
// path, as the worker reports them, and usage requests with a usage.
func listing(names ...string) func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		res := &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Elapsed: durationpb.New(120 * time.Millisecond)}
		var base string
		switch {
		case p.GetSpec().GetListRemote() != nil:
			base = p.GetSpec().GetListRemote().GetLocation().GetPath()
		case p.GetSpec().GetListLocal() != nil:
			base = p.GetSpec().GetListLocal().GetPath()
		case p.GetSpec().GetAbout() != nil:
			res.Output = &syncrv1.ProbeResult_Usage{Usage: &syncrv1.ProbeUsage{Total: proto.Int64(1000), Used: proto.Int64(400)}}
			return res, nil
		}
		l := &syncrv1.ProbeListing{Path: base, Truncated: len(names) > 2}
		for _, n := range names {
			l.Entries = append(l.Entries, &syncrv1.ProbeEntry{Name: n, Path: base + "/" + n, Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, Size: 7})
		}
		res.Output = &syncrv1.ProbeResult_Listing{Listing: l}
		return res, nil
	}
}

func TestListWorkerFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = listing("photos", "docs")
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	other := e.workers.add("other", domain.WorkerOnline, current())
	e.jobs.unfinished[nas] = 9 // browsing never picks another worker
	ctx := context.Background()

	// Any user may browse any worker; admins get nothing more.
	for _, actor := range []domain.Actor{alice, bob} {
		resp, err := e.svc.ListWorkerFiles(ctx, actor, nas, "/data", true)
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetPath() != "/data" || len(resp.GetEntries()) != 2 || resp.GetEntries()[0].GetPath() != "/data/photos" ||
			resp.GetWorker().GetId() != nas.String() || resp.GetWorker().GetName() != "nas" {
			t.Errorf("response = %v", resp)
		}
		sent := e.transport.last(t)
		if op := sent.probe.GetSpec().GetListLocal(); sent.worker != nas || op.GetPath() != "/data" || !op.GetDirsOnly() || op.GetMaxEntries() != 100 ||
			sent.probe.GetTimeout().AsDuration() != 20*time.Second || len(sent.probe.GetSpec().GetRemotes()) != 0 {
			t.Errorf("sent %v to %v", op, sent.worker)
		}
	}
	// The empty path (the local roots) is passed as it is.
	if _, err := e.svc.ListWorkerFiles(ctx, alice, other, "", false); err != nil || e.transport.last(t).probe.GetSpec().GetListLocal().GetPath() != "" {
		t.Errorf("roots: %v", err)
	}

	var verr *domain.ValidationError
	if _, err := e.svc.ListWorkerFiles(ctx, alice, domain.WorkerID{}, "/data", false); !errors.As(err, &verr) || verr.Fields[0].Field != "worker_id" {
		t.Errorf("no worker: %v", err)
	}
	if _, err := e.svc.ListWorkerFiles(ctx, alice, domain.NewWorkerID(), "/data", false); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "/etc is outside the local roots")
	if _, err := e.svc.ListWorkerFiles(ctx, alice, nas, "/etc", false); !errors.As(err, &verr) || verr.Fields[0].Field != "path" ||
		verr.Fields[0].Message != "is outside the worker's allowed folders" {
		t.Errorf("outside the roots: %v", err)
	}
	e.limiter.close()
	if _, err := e.svc.ListWorkerFiles(ctx, alice, nas, "/data", false); !errors.Is(err, domain.ErrRateLimited) {
		t.Errorf("rate limited: %v", err)
	}
}

func TestListRemoteFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = listing("2025", "2026")
	remote, disk := e.remotes.s3("bucket/base"), e.remotes.local("/srv/data")
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	ctx := context.Background()

	// Paths in the response are relative to the remote's root, whatever
	// the worker saw.
	resp, err := e.svc.ListRemoteFiles(ctx, alice, remote, "/photos//", domain.WorkerID{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPath() != "photos" || resp.GetEntries()[0].GetPath() != "photos/2025" || resp.GetEntries()[1].GetName() != "2026" ||
		resp.GetEntries()[0].GetSize() != 7 || resp.GetWorker().GetId() != nas.String() {
		t.Errorf("response = %v", resp)
	}
	sent := e.transport.last(t)
	op := sent.probe.GetSpec().GetListRemote()
	if op.GetLocation().GetPath() != "bucket/base/photos" || op.GetLocation().GetRemote() == "" || !op.GetDirsOnly() || op.GetMaxEntries() != 100 ||
		sent.probe.GetSpec().GetRemotes()[0].GetSecrets()["secret_access_key"] != remoteSecret {
		t.Errorf("sent %v", op.GetLocation())
	}
	// The root.
	if resp, err = e.svc.ListRemoteFiles(ctx, alice, remote, "", domain.WorkerID{}, false); err != nil || resp.GetPath() != "" || resp.GetEntries()[0].GetPath() != "2025" {
		t.Errorf("root = %v, %v", resp, err)
	}

	// A remote on a worker's disk needs the worker and is listed there as
	// a local folder.
	var verr *domain.ValidationError
	if _, err := e.svc.ListRemoteFiles(ctx, alice, disk, "films", domain.WorkerID{}, false); !errors.As(err, &verr) ||
		verr.Fields[0].Field != "worker_id" || verr.Fields[0].Message != "choose the worker whose disk holds this remote" {
		t.Errorf("local remote without a worker: %v", err)
	}
	resp, err = e.svc.ListRemoteFiles(ctx, alice, disk, "films", nas, false)
	if err != nil {
		t.Fatal(err)
	}
	if sent = e.transport.last(t); sent.probe.GetSpec().GetListLocal().GetPath() != "/srv/data/films" || resp.GetPath() != "films" ||
		resp.GetEntries()[0].GetPath() != "films/2025" {
		t.Errorf("local remote: sent %v, response %v", sent.probe.GetSpec().GetListLocal(), resp)
	}

	// Owner scoping: another user (an admin too) cannot list it.
	before := e.transport.count()
	if _, err := e.svc.ListRemoteFiles(ctx, bob, remote, "", domain.WorkerID{}, false); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another user's remote: %v", err)
	}
	if _, err := e.svc.ListRemoteFiles(ctx, alice, remote, "a/../../b", domain.WorkerID{}, false); !errors.As(err, &verr) || verr.Fields[0].Field != "path" {
		t.Errorf("a path with ..: %v", err)
	}
	if _, err := e.svc.ListRemoteFiles(ctx, alice, domain.RemoteID{}, "", domain.WorkerID{}, false); !errors.As(err, &verr) || verr.Fields[0].Field != "remote_id" {
		t.Errorf("no remote: %v", err)
	}
	if e.transport.count() != before {
		t.Error("a refused listing was sent to a worker")
	}

	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found")
	if _, err := e.svc.ListRemoteFiles(ctx, alice, remote, "nope", domain.WorkerID{}, false); !errors.As(err, &verr) || verr.Fields[0].Message != "does not exist" {
		t.Errorf("missing folder: %v", err)
	}
	e.noSecrets()
}

func TestTestRemote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	names := make([]string, 12)
	for i := range names {
		names[i] = "dir" + string(rune('a'+i))
	}
	e.transport.answer = listing(names...)
	remote := e.remotes.s3("bucket")
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	ctx := context.Background()

	// A stored remote: the listing, then its usage.
	resp, err := e.svc.TestRemote(ctx, alice, remote, nil, "", domain.WorkerID{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetOk() || resp.GetEntries() != 12 || len(resp.GetSample()) != 10 || resp.GetSample()[0] != "dira" || !resp.GetTruncated() ||
		resp.GetLatency().AsDuration() != 120*time.Millisecond || resp.GetWorker().GetId() != nas.String() ||
		resp.GetUsage().GetTotal() != 1000 || resp.GetUsage().GetUsed() != 400 || resp.GetError() != "" {
		t.Errorf("response = %v", resp)
	}
	if got := e.transport.ops(); !slices.Equal(got, []string{"list_remote", "about"}) {
		t.Errorf("probes = %v", got)
	}
	if op := e.transport.sent[0].probe.GetSpec().GetListRemote(); op.GetMaxEntries() != 50 || op.GetLocation().GetPath() != "bucket" {
		t.Errorf("list op = %v", op)
	}
	if len(e.limiter.keys) != 1 {
		t.Errorf("a test took %d rate-limit tokens, want 1", len(e.limiter.keys))
	}

	// Usage is best effort: a remote that cannot tell still passes.
	e.transport.answer = func(w domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		if p.GetSpec().GetAbout() != nil {
			return failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "about failed")(w, p)
		}
		return listing("x")(w, p)
	}
	if resp, err = e.svc.TestRemote(ctx, alice, remote, nil, "sub", domain.WorkerID{}); err != nil || !resp.GetOk() || resp.GetUsage() != nil {
		t.Errorf("without usage: %v, %v", resp, err)
	}

	// The worker's failure is the answer, scrubbed, not an error.
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "AccessDenied for key "+remoteSecret)
	resp, err = e.svc.TestRemote(ctx, alice, remote, nil, "", domain.WorkerID{})
	if err != nil || resp.GetOk() || resp.GetErrorCode() != "execution_failed" || !strings.Contains(resp.GetError(), "AccessDenied") ||
		resp.GetWorker().GetName() != "nas" {
		t.Errorf("failed test = %v, %v", resp, err)
	}
	e.noSecrets(resp.GetError())
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT, "")
	if resp, err = e.svc.TestRemote(ctx, alice, remote, nil, "", domain.WorkerID{}); err != nil || resp.GetOk() || resp.GetErrorCode() != "timeout" ||
		resp.GetError() != `worker "nas" did not finish in time` {
		t.Errorf("timed out test = %v, %v", resp, err)
	}
	// So is a worker that went away, without the sentinel's prefix.
	e.transport.answer = func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		return nil, errors.Join(domain.ErrUnavailable)
	}
	if resp, err = e.svc.TestRemote(ctx, alice, remote, nil, "", domain.WorkerID{}); err != nil || resp.GetOk() || resp.GetErrorCode() != "unavailable" {
		t.Errorf("unavailable = %v, %v", resp, err)
	}

	// Errors: scoping, validation, rate limiting.
	var verr *domain.ValidationError
	if _, err := e.svc.TestRemote(ctx, bob, remote, nil, "", domain.WorkerID{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another user's remote: %v", err)
	}
	if _, err := e.svc.TestRemote(ctx, alice, domain.RemoteID{}, nil, "", domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "remote_id" {
		t.Errorf("nothing to test: %v", err)
	}
	if _, err := e.svc.TestRemote(ctx, alice, remote, nil, "..", domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "path" {
		t.Errorf("bad path: %v", err)
	}
	if _, err := e.svc.TestRemote(ctx, alice, remote, nil, "", domain.NewWorkerID()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
	e.limiter.close()
	if _, err := e.svc.TestRemote(ctx, alice, remote, nil, "", domain.WorkerID{}); !errors.Is(err, domain.ErrRateLimited) {
		t.Errorf("rate limited: %v", err)
	}
}

func TestTestRemoteDraftsAndWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = listing("x")
	remote, disk := e.remotes.s3("bucket"), e.remotes.local("/srv/data")
	ctx := context.Background()

	// No worker can run it: an answer, not an error.
	e.workers.add("old", domain.WorkerOnline, workers.Info{Version: "v2.4.0"})
	resp, err := e.svc.TestRemote(ctx, alice, remote, nil, "", domain.WorkerID{})
	if err != nil || resp.GetOk() || resp.GetErrorCode() != "no_worker" || resp.GetWorker() != nil || !strings.Contains(resp.GetError(), "upgrade a worker") ||
		strings.Contains(resp.GetError(), "failed precondition") {
		t.Errorf("no worker = %v, %v", resp, err)
	}
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	away := e.workers.add("away", domain.WorkerOffline, current())
	if resp, err = e.svc.TestRemote(ctx, alice, remote, nil, "", away); err != nil || resp.GetOk() || !strings.Contains(resp.GetError(), `worker "away" is offline`) {
		t.Errorf("offline worker = %v, %v", resp, err)
	}

	// An unsaved new remote: the draft is tested, with its secret.
	const draftSecret = "draft-secret-sentinel"
	in := &remotes.Input{Name: "New", Type: "s3", Options: map[string]string{"bucket": "fresh"}, Secrets: map[string]string{"secret_access_key": draftSecret}}
	if resp, err = e.svc.TestRemote(ctx, alice, domain.RemoteID{}, in, "", domain.WorkerID{}); err != nil || !resp.GetOk() {
		t.Fatalf("new draft = %v, %v", resp, err)
	}
	list := e.transport.sent[len(e.transport.sent)-2].probe.GetSpec()
	if list.GetListRemote().GetLocation().GetPath() != "fresh" || list.GetRemotes()[0].GetSecrets()["secret_access_key"] != draftSecret {
		t.Errorf("draft spec location = %v", list.GetListRemote().GetLocation())
	}
	// A failure that echoes the draft's secret is scrubbed too.
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "bad key "+draftSecret)
	if resp, err = e.svc.TestRemote(ctx, alice, domain.RemoteID{}, in, "", domain.WorkerID{}); err != nil || strings.Contains(resp.GetError(), draftSecret) ||
		strings.Contains(e.logs.String(), draftSecret) {
		t.Errorf("draft failure = %v, %v", resp, err)
	}
	e.transport.answer = listing("x")

	// A stored remote with unsaved changes goes through Draft with its ID.
	if resp, err = e.svc.TestRemote(ctx, alice, remote, &remotes.Input{Secrets: map[string]string{"secret_access_key": "rotated"}}, "", domain.WorkerID{}); err != nil || !resp.GetOk() {
		t.Fatalf("edited remote = %v, %v", resp, err)
	}
	if got := e.transport.sent[len(e.transport.sent)-2].probe.GetSpec().GetRemotes()[0].GetSecrets()["secret_access_key"]; got != "rotated" {
		t.Errorf("edited remote tested with secret %q", got)
	}
	// An invalid draft is the caller's error.
	e.remotes.draftErr = &domain.ValidationError{Fields: []domain.FieldError{{Field: "options.bucket", Message: "is required"}}}
	var verr *domain.ValidationError
	if _, err := e.svc.TestRemote(ctx, alice, domain.RemoteID{}, in, "", domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "options.bucket" {
		t.Errorf("invalid draft: %v", err)
	}
	e.remotes.draftErr = nil

	// A folder of a worker: needs the worker, has no usage.
	if _, err := e.svc.TestRemote(ctx, alice, disk, nil, "", domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "worker_id" {
		t.Errorf("local remote without a worker: %v", err)
	}
	before := e.transport.count()
	if resp, err = e.svc.TestRemote(ctx, alice, disk, nil, "", nas); err != nil || !resp.GetOk() || resp.GetUsage() != nil {
		t.Errorf("local remote = %v, %v", resp, err)
	}
	if got := e.transport.ops()[before:]; !slices.Equal(got, []string{"list_local"}) {
		t.Errorf("local remote probes = %v", got)
	}
}
