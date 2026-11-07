package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// workerAdminHandler implements syncr.v1.WorkerAdminService. Workers are
// shared by all users: the policy table lets every user list and get them
// and keeps the other procedures to admins.
type workerAdminHandler struct {
	syncrv1connect.UnimplementedWorkerAdminServiceHandler

	workers WorkerService
	install InstallConfig
	errs    errorMapper
}

var _ syncrv1connect.WorkerAdminServiceHandler = (*workerAdminHandler)(nil)

func (h *workerAdminHandler) fail(ctx context.Context, procedure string, err error) error {
	return h.errs.toConnect(ctx, procedure, "worker", err)
}

func (h *workerAdminHandler) worker(w workers.Worker) *syncrv1.Worker {
	return workerToProto(w, h.workers.Status(w))
}

// CreateEnrollmentToken returns a new token with the commands that
// install a worker and enroll it with the token. It is the only time the
// token is visible: the service stores its hash. The commands use the
// configured public URL or, without one, the request's controller_url
// (checked before the token is created); without either there are none.
func (h *workerAdminHandler) CreateEnrollmentToken(ctx context.Context, req *connect.Request[syncrv1.CreateEnrollmentTokenRequest]) (*connect.Response[syncrv1.CreateEnrollmentTokenResponse], error) {
	const procedure = syncrv1connect.WorkerAdminServiceCreateEnrollmentTokenProcedure
	ttl := req.Msg.GetTtl()
	if ttl != nil {
		if err := ttl.CheckValid(); err != nil {
			return nil, invalid("ttl: " + err.Error())
		}
	}
	controllerURL, haveURL, err := h.install.controllerURL(req.Msg.GetControllerUrl())
	if err != nil {
		if h.install.PublicURL != "" {
			// The configuration is wrong, not the request: still hand out
			// the token, without commands.
			h.errs.log.WarnContext(ctx, "cannot render install commands: http.public_url is not usable", "error", err.Error())
			haveURL = false
		} else {
			return nil, invalidField("controller_url", err.Error())
		}
	}
	grant, err := h.workers.CreateEnrollmentToken(ctx, ttl.AsDuration())
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg := &syncrv1.CreateEnrollmentTokenResponse{
		Token:     grant.Token,
		ExpiresAt: timestamppb.New(grant.ExpiresAt),
	}
	if haveURL {
		cmds, err := enrollCommands(h.install, controllerURL, grant.Token)
		if err != nil {
			h.errs.log.WarnContext(ctx, "cannot render install commands", "error", err.Error())
		}
		msg.InstallCommands = cmds
	}
	resp := connect.NewResponse(msg)
	resp.Header().Set("Cache-Control", "no-store")
	return resp, nil
}

// GetPassiveInstallCommands returns the commands that install a passive
// worker; its setup prints what RegisterPassiveWorker needs.
func (h *workerAdminHandler) GetPassiveInstallCommands(ctx context.Context, _ *connect.Request[syncrv1.GetPassiveInstallCommandsRequest]) (*connect.Response[syncrv1.GetPassiveInstallCommandsResponse], error) {
	cmds, err := passiveCommands(h.install)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.WorkerAdminServiceGetPassiveInstallCommandsProcedure, fmt.Errorf("render passive install commands: %w", err))
	}
	return connect.NewResponse(&syncrv1.GetPassiveInstallCommandsResponse{Commands: cmds}), nil
}

// RegisterPassiveWorker validates and registers a passive worker. The URL
// (https only), the fingerprint (SHA-256, 64 hex digits; colons and upper
// case are normalized) and the pairing secret (32 to 512 printable ASCII
// characters) are checked by the workers service, whose messages reach the
// caller as InvalidArgument.
func (h *workerAdminHandler) RegisterPassiveWorker(ctx context.Context, req *connect.Request[syncrv1.RegisterPassiveWorkerRequest]) (*connect.Response[syncrv1.RegisterPassiveWorkerResponse], error) {
	m := req.Msg
	w, err := h.workers.RegisterPassive(ctx, workers.PassiveParams{
		Name:            m.GetName(),
		URL:             m.GetUrl(),
		CertFingerprint: m.GetCertFingerprint(),
		Secret:          m.GetPairingSecret(),
	})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.WorkerAdminServiceRegisterPassiveWorkerProcedure, err)
	}
	return connect.NewResponse(&syncrv1.RegisterPassiveWorkerResponse{Worker: h.worker(w)}), nil
}

// ListWorkers lists the workers that are not removed, by name.
func (h *workerAdminHandler) ListWorkers(ctx context.Context, _ *connect.Request[syncrv1.ListWorkersRequest]) (*connect.Response[syncrv1.ListWorkersResponse], error) {
	ws, err := h.workers.List(ctx, workers.ListFilter{})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.WorkerAdminServiceListWorkersProcedure, err)
	}
	resp := &syncrv1.ListWorkersResponse{Workers: make([]*syncrv1.Worker, 0, len(ws))}
	for _, w := range ws {
		resp.Workers = append(resp.Workers, h.worker(w))
	}
	return connect.NewResponse(resp), nil
}

// GetWorker returns one worker.
func (h *workerAdminHandler) GetWorker(ctx context.Context, req *connect.Request[syncrv1.GetWorkerRequest]) (*connect.Response[syncrv1.GetWorkerResponse], error) {
	id, err := parseWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	w, err := h.workers.Get(ctx, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.WorkerAdminServiceGetWorkerProcedure, err)
	}
	return connect.NewResponse(&syncrv1.GetWorkerResponse{Worker: h.worker(w)}), nil
}

// RenameWorker changes a worker's display name.
func (h *workerAdminHandler) RenameWorker(ctx context.Context, req *connect.Request[syncrv1.RenameWorkerRequest]) (*connect.Response[syncrv1.RenameWorkerResponse], error) {
	id, err := parseWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	w, err := h.workers.Rename(ctx, id, req.Msg.GetName())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.WorkerAdminServiceRenameWorkerProcedure, err)
	}
	return connect.NewResponse(&syncrv1.RenameWorkerResponse{Worker: h.worker(w)}), nil
}

// RemoveWorker removes a worker and ends its unfinished jobs.
func (h *workerAdminHandler) RemoveWorker(ctx context.Context, req *connect.Request[syncrv1.RemoveWorkerRequest]) (*connect.Response[syncrv1.RemoveWorkerResponse], error) {
	id, err := parseWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	if err := h.workers.Remove(ctx, id); err != nil {
		return nil, h.fail(ctx, syncrv1connect.WorkerAdminServiceRemoveWorkerProcedure, err)
	}
	return connect.NewResponse(&syncrv1.RemoveWorkerResponse{}), nil
}

// parseWorkerID parses a request's worker ID into an InvalidArgument error
// naming the field.
func parseWorkerID(s string) (domain.WorkerID, error) {
	if s == "" {
		return domain.WorkerID{}, invalid("worker_id is required")
	}
	id, err := domain.ParseWorkerID(s)
	if err != nil || id.IsZero() {
		return domain.WorkerID{}, invalid("worker_id is not a valid ID")
	}
	return id, nil
}
