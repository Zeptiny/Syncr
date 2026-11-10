// Package active is the worker's transport in active mode: the worker calls
// the controller's ControllerLinkService (Report, Poll) over HTTP(S), as
// Connect unary calls that work through ordinary proxies. It moves the
// envelopes of internal/worker/linkcore and follows its sequencing contract
// exactly:
//
//   - The Report loop sends core.BuildReport, applies the answer with
//     core.ApplyReportResponse and reports again at once while a full batch
//     left more events behind. Otherwise it waits for core.Notify() (new
//     events), sending at most one Report per Options.ReportInterval (about
//     1 s), or for the heartbeat (Options.Heartbeat, about 15 s).
//   - The Poll loop long-polls with core.BuildPoll(Options.PollWait), hands
//     the commands to core.ApplyPollResponse and polls again at once, which
//     delivers the new command acks.
//   - The probe loop waits for core.ProbeNotify() and delivers each result
//     of core.PendingProbeResults() with CompleteProbe.
//   - The loops never run two Reports or two Polls at once; a Report and a
//     Poll may overlap.
//   - On a transport error a loop backs off exponentially (with jitter,
//     Options.MinBackoff to Options.MaxBackoff) and rebuilds its request,
//     so the same events and acks are sent again; the controller
//     deduplicates. Errors retrying cannot fix (bad credentials, a stale
//     epoch, an invalid request) are logged as errors and retried after
//     MaxBackoff: the loops never spin.
//
// # Shutdown
//
// The composition root cancels Run's context, calls agent.Shutdown (which
// leaves every job's CANCELLED result in the outbox) and then Drain with a
// deadline: Drain reports until core.Drained() or the deadline.
//
// # Security
//
// Every call carries link.ActiveAuthorization(worker ID, secret). The
// default HTTP client verifies the controller's certificate against the
// system roots, or against Options.RootCAs for a controller with a private
// CA; there is no way to disable verification. Redirects are refused. An
// http:// controller URL is accepted (for a controller behind a local
// TLS-terminating proxy) but logged as a warning, because the secret then
// travels in clear text.
//
// Allowed dependencies: the standard library, connect, the generated code,
// internal/link and internal/worker/linkcore.
package active

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
)

// Defaults for Options.
const (
	DefaultRequestTimeout = 30 * time.Second
	DefaultPollWait       = 25 * time.Second
	DefaultPollGrace      = 15 * time.Second
	DefaultReportInterval = time.Second
	DefaultHeartbeat      = 15 * time.Second
	DefaultMinBackoff     = time.Second
	DefaultMaxBackoff     = 30 * time.Second
	// DefaultReadMaxBytes caps a controller response. A PollResponse may
	// carry up to 64 commands with job specs of up to 1 MiB each.
	DefaultReadMaxBytes = 64 << 20
)

// Core is the link core the transport drives; *linkcore.Core implements it.
type Core interface {
	BuildReport(maxEvents int) *syncrv1.ReportRequest
	ApplyReportResponse(req *syncrv1.ReportRequest, resp *syncrv1.ReportResponse) (more bool, err error)
	BuildPoll(wait time.Duration) *syncrv1.PollRequest
	ApplyPollResponse(ctx context.Context, req *syncrv1.PollRequest, resp *syncrv1.PollResponse) []*syncrv1.CommandAck
	Notify() <-chan struct{}
	Drained() bool
}

// ProbeCore is the part of the link core that runs probes. When the Core
// also implements it (*linkcore.Core does), the transport delivers the
// probes' results.
type ProbeCore interface {
	Epoch() string
	ProbeNotify() <-chan struct{}
	PendingProbeResults() []*syncrv1.ProbeResult
	ProbeDelivered(probeID string)
}

// Options configure a Transport. ControllerURL, WorkerID and Secret are
// required; zero values elsewhere select the defaults.
type Options struct {
	// ControllerURL is the controller's base URL, e.g.
	// "https://syncr.example.com" (a path prefix is kept).
	ControllerURL string
	// WorkerID and Secret are the worker's credentials from enrollment.
	WorkerID string
	Secret   string

	// RootCAs, if set, replaces the system roots for verifying the
	// controller's certificate (a controller with a private CA).
	RootCAs *x509.CertPool
	// HTTPClient, if set, is used instead of the default client; RootCAs
	// must then be nil. It must not follow redirects to other hosts with
	// the Authorization header, and must not set a client-wide Timeout
	// shorter than PollWait + PollGrace.
	HTTPClient *http.Client

	// RequestTimeout bounds a Report.
	RequestTimeout time.Duration
	// PollWait is the long-poll wait asked of the controller (which caps
	// it, at 25 s by default); PollGrace is added to it for the Poll's
	// timeout.
	PollWait  time.Duration
	PollGrace time.Duration
	// ReportInterval is the minimum time between two Reports triggered by
	// new events; Heartbeat the longest time between two Reports.
	ReportInterval time.Duration
	Heartbeat      time.Duration
	// MinBackoff and MaxBackoff bound the pause after a failed call.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// MaxEvents bounds the events of one Report (0: linkcore default).
	MaxEvents int
	// ReadMaxBytes caps a decoded controller response.
	ReadMaxBytes int

	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Transport runs the active link of one worker process. Create it with New.
type Transport struct {
	core           Core
	rpc            syncrv1connect.ControllerLinkServiceClient
	auth           string
	requestTimeout time.Duration
	pollWait       time.Duration
	pollGrace      time.Duration
	reportInterval time.Duration
	heartbeat      time.Duration
	minBackoff     time.Duration
	maxBackoff     time.Duration
	maxEvents      int
	log            *slog.Logger
}

// New returns a Transport for core.
func New(core Core, opts Options) (*Transport, error) {
	if core == nil {
		return nil, errors.New("active transport: core is nil")
	}
	if opts.WorkerID == "" || opts.Secret == "" {
		return nil, errors.New("active transport: worker id and secret are required")
	}
	u, err := url.Parse(opts.ControllerURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("active transport: controller url %q must be an http(s):// base URL", opts.ControllerURL)
	}
	if opts.HTTPClient != nil && opts.RootCAs != nil {
		return nil, errors.New("active transport: set either HTTPClient or RootCAs")
	}
	for _, d := range []time.Duration{opts.RequestTimeout, opts.PollWait, opts.PollGrace, opts.ReportInterval, opts.Heartbeat, opts.MinBackoff, opts.MaxBackoff} {
		if d < 0 {
			return nil, fmt.Errorf("active transport: negative duration %v", d)
		}
	}
	t := &Transport{
		core:           core,
		auth:           link.ActiveAuthorization(opts.WorkerID, opts.Secret),
		requestTimeout: cmp.Or(opts.RequestTimeout, DefaultRequestTimeout),
		pollWait:       cmp.Or(opts.PollWait, DefaultPollWait),
		pollGrace:      cmp.Or(opts.PollGrace, DefaultPollGrace),
		reportInterval: cmp.Or(opts.ReportInterval, DefaultReportInterval),
		heartbeat:      cmp.Or(opts.Heartbeat, DefaultHeartbeat),
		minBackoff:     cmp.Or(opts.MinBackoff, DefaultMinBackoff),
		maxBackoff:     cmp.Or(opts.MaxBackoff, DefaultMaxBackoff),
		maxEvents:      opts.MaxEvents,
		log:            opts.Logger,
	}
	t.maxBackoff = max(t.maxBackoff, t.minBackoff)
	switch {
	case t.maxEvents <= 0:
		t.maxEvents = linkcore.DefaultMaxEvents
	case t.maxEvents > linkcore.MaxEvents:
		t.maxEvents = linkcore.MaxEvents
	}
	if t.log == nil {
		t.log = slog.New(slog.DiscardHandler)
	}
	if u.Scheme == "http" {
		t.log.Warn("the controller URL uses http: the worker secret travels unencrypted; use https", "controller_url", u.Redacted())
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = defaultClient(opts.RootCAs)
	}
	t.rpc = syncrv1connect.NewControllerLinkServiceClient(hc, strings.TrimRight(u.String(), "/"),
		connect.WithReadMaxBytes(cmp.Or(opts.ReadMaxBytes, DefaultReadMaxBytes)))
	return t, nil
}

// defaultClient verifies the controller against rootCAs (nil: the system
// roots), uses the environment's proxy settings and refuses redirects.
func defaultClient(rootCAs *x509.CertPool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck,forcetypeassert // DefaultTransport is an *http.Transport
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Run runs the Report and Poll loops until ctx ends, then returns nil.
func (t *Transport) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { t.reportLoop(ctx) })
	wg.Go(func() { t.pollLoop(ctx) })
	if pc, ok := t.core.(ProbeCore); ok {
		wg.Go(func() { t.probeLoop(ctx, pc) })
	}
	wg.Wait()
	return nil
}

// probeLoop delivers the results of finished probes with CompleteProbe. A
// result the controller accepted, or refuses for good (it no longer knows
// the probe, the epoch is stale, the controller has no probes), is dropped;
// after a transport error the loop backs off and tries again, for as long
// as the core keeps the result.
func (t *Transport) probeLoop(ctx context.Context, pc ProbeCore) {
	var backoff time.Duration
	for {
		results := pc.PendingProbeResults()
		if len(results) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-pc.ProbeNotify():
			}
			continue
		}
		failed := false
		for _, res := range results {
			err := t.completeProbe(ctx, &syncrv1.CompleteProbeRequest{Epoch: pc.Epoch(), Result: res})
			if ctx.Err() != nil {
				return
			}
			if err == nil || probeRefused(err) {
				if err != nil {
					t.log.InfoContext(ctx, "the controller did not take a probe result", "probe_id", res.GetProbeId(), "error", err.Error())
				}
				pc.ProbeDelivered(res.GetProbeId())
				continue
			}
			failed = true
			backoff = t.nextBackoff(backoff, err)
			t.logFailure(ctx, "complete probe", err, backoff)
			break
		}
		if !failed {
			backoff = 0
			continue
		}
		if !sleep(ctx, jitter(backoff)) {
			return
		}
	}
}

// probeRefused reports whether the controller answered that it will never
// take the result, so sending it again is pointless.
func probeRefused(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeInvalidArgument, connect.CodeUnimplemented:
		return true
	}
	return false
}

func (t *Transport) completeProbe(ctx context.Context, req *syncrv1.CompleteProbeRequest) error {
	ctx, cancel := context.WithTimeout(ctx, t.requestTimeout)
	defer cancel()
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", t.auth)
	if _, err := t.rpc.CompleteProbe(ctx, r); err != nil {
		return fmt.Errorf("complete probe: %w", err)
	}
	return nil
}

// Drain reports until every event has been acknowledged or ctx ends, in
// which case it returns an error. Call it after Run has returned and the
// agent has shut down.
func (t *Transport) Drain(ctx context.Context) error {
	var backoff time.Duration
	for !t.core.Drained() {
		req := t.core.BuildReport(t.maxEvents)
		resp, err := t.report(ctx, req)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			backoff = t.nextBackoff(backoff, err)
			t.logFailure(ctx, "final report", err, backoff)
			if !sleep(ctx, jitter(backoff)) {
				break
			}
			continue
		}
		backoff = 0
		_, _ = t.core.ApplyReportResponse(req, resp)
		pushOn := t.full(req) && ackedAll(req, resp) // a backlog that the controller accepts
		if !t.core.Drained() && !pushOn && !sleep(ctx, t.reportInterval) {
			break // do not spin on a controller that acks slowly
		}
	}
	if !t.core.Drained() {
		return fmt.Errorf("active transport: events not delivered before the deadline: %w", context.Cause(ctx))
	}
	return nil
}

// reportLoop is the Report loop of the linkcore contract.
func (t *Transport) reportLoop(ctx context.Context) {
	var backoff time.Duration
	for {
		req := t.core.BuildReport(t.maxEvents)
		start := time.Now()
		resp, err := t.report(ctx, req)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			backoff = t.nextBackoff(backoff, err)
			t.logFailure(ctx, "report", err, backoff)
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			continue
		}
		if backoff > 0 {
			t.log.InfoContext(ctx, "report succeeded again")
			backoff = 0
		}
		more, _ := t.core.ApplyReportResponse(req, resp) // implausible acks are logged by the core
		pending := more && ackedAll(req, resp)           // the controller took the batch and events remain
		if pending && t.full(req) {
			continue // a full, accepted batch left events behind: send the next one now
		}
		if !t.waitForReport(ctx, start, pending) {
			return
		}
	}
}

// waitForReport waits until the next Report is due: on new events, but not
// before ReportInterval after the previous Report started, or after the
// heartbeat. With pending set, events are already waiting and the Report
// is due after ReportInterval: they arrived during the previous Report, or
// the byte bound kept them out of it (a small batch cut before a large
// event). The latter were announced before that Report, so no Notify is
// left for them, and without this they would wait for the heartbeat. It
// returns false when ctx ends.
func (t *Transport) waitForReport(ctx context.Context, last time.Time, pending bool) bool {
	if pending {
		select {
		case <-t.core.Notify(): // announces the events that are about to be sent
		default:
		}
		return sleep(ctx, t.reportInterval-time.Since(last))
	}
	hb := time.NewTimer(t.heartbeat - time.Since(last))
	defer hb.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-hb.C:
		return true
	case <-t.core.Notify():
		return sleep(ctx, t.reportInterval-time.Since(last))
	}
}

// full reports whether req's batch was cut by the event or byte limit (so
// the remaining events are a backlog, not events that arrived meanwhile).
func (t *Transport) full(req *syncrv1.ReportRequest) bool {
	if len(req.GetEvents()) >= t.maxEvents {
		return true
	}
	size := 0
	for _, ev := range req.GetEvents() {
		size += proto.Size(ev)
	}
	return size >= linkcore.DefaultMaxBytes/2
}

// ackedAll reports whether resp acknowledged every event of req, i.e. the
// Report made progress (otherwise sending the next one at once could spin).
func ackedAll(req *syncrv1.ReportRequest, resp *syncrv1.ReportResponse) bool {
	evs := req.GetEvents()
	return len(evs) > 0 && resp.GetAckSeq() >= evs[len(evs)-1].GetSeq()
}

// pollLoop is the Poll loop of the linkcore contract.
func (t *Transport) pollLoop(ctx context.Context) {
	var backoff time.Duration
	for {
		req := t.core.BuildPoll(t.pollWait)
		start := time.Now()
		resp, err := t.poll(ctx, req)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			backoff = t.nextBackoff(backoff, err)
			t.logFailure(ctx, "poll", err, backoff)
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			continue
		}
		if backoff > 0 {
			t.log.InfoContext(ctx, "poll succeeded again")
			backoff = 0
		}
		if acks := t.core.ApplyPollResponse(ctx, req, resp); len(acks) > 0 {
			continue // poll again at once: it delivers the acks
		}
		// An empty answer before the wait ended means the controller is
		// going away or a proxy cut the call; do not hammer it.
		if !sleep(ctx, t.reportInterval-time.Since(start)) {
			return
		}
	}
}

func (t *Transport) report(ctx context.Context, req *syncrv1.ReportRequest) (*syncrv1.ReportResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, t.requestTimeout)
	defer cancel()
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", t.auth)
	resp, err := t.rpc.Report(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	return resp.Msg, nil
}

func (t *Transport) poll(ctx context.Context, req *syncrv1.PollRequest) (*syncrv1.PollResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, t.pollWait+t.pollGrace)
	defer cancel()
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", t.auth)
	resp, err := t.rpc.Poll(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("poll: %w", err)
	}
	return resp.Msg, nil
}

// permanent reports whether retrying err cannot help without a change on
// either side (credentials, configuration, another worker process).
func permanent(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeFailedPrecondition,
		connect.CodeInvalidArgument, connect.CodeUnimplemented, connect.CodeNotFound:
		return true
	}
	return false
}

// nextBackoff doubles prev within [MinBackoff, MaxBackoff]; a permanent
// error waits MaxBackoff at once.
func (t *Transport) nextBackoff(prev time.Duration, err error) time.Duration {
	if permanent(err) {
		return t.maxBackoff
	}
	return min(max(2*prev, t.minBackoff), t.maxBackoff)
}

func (t *Transport) logFailure(ctx context.Context, call string, err error, retryIn time.Duration) {
	if permanent(err) {
		hint := "check the worker's credentials and the controller URL"
		if connect.CodeOf(err) == connect.CodeFailedPrecondition {
			hint = "the controller talks to a newer process of this worker: is it running twice with the same credentials?"
		}
		t.log.ErrorContext(ctx, "link call rejected", "call", call, "error", err.Error(), "hint", hint, "retry_in", retryIn.String())
		return
	}
	t.log.WarnContext(ctx, "link call failed", "call", call, "error", err.Error(), "retry_in", retryIn.String())
}

// jitter returns a random duration in [d/2, d].
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter needs no cryptographic randomness
}

// sleep waits for d (nothing if d <= 0) and reports whether ctx is still
// alive.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
