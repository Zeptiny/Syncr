package workers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Limits applied to what workers report, so a misbehaving worker cannot
// bloat its record.
const (
	maxEpochLen   = 128
	maxInfoString = 256
	maxPathLen    = 4096
	maxTools      = 16
	maxJobKinds   = 64
	maxLocalRoots = 256
	maxFeatures   = 64
	maxProbeOps   = 64
)

// ValidEpoch checks a worker-supplied epoch: 1 to 128 printable ASCII
// characters.
func ValidEpoch(epoch string) error {
	if epoch == "" || len(epoch) > maxEpochLen {
		return fmt.Errorf("%w: epoch must be 1 to %d characters", domain.ErrInvalidArgument, maxEpochLen)
	}
	for i := range len(epoch) {
		if c := epoch[i]; c <= ' ' || c > '~' {
			return fmt.Errorf("%w: epoch must be printable ASCII", domain.ErrInvalidArgument)
		}
	}
	return nil
}

// AdvanceEpoch moves w (as read by the caller) to epoch, recording w's
// epoch in the history of replaced epochs. It fails with ErrConflict if the
// stored epoch is no longer w.Link.Epoch; the caller re-reads and decides
// again. Deciding whether epoch is new or stale is the caller's job.
func (s *Service) AdvanceEpoch(ctx context.Context, w Worker, epoch string) (Worker, error) {
	if err := ValidEpoch(epoch); err != nil {
		return Worker{}, fmt.Errorf("advance epoch of worker %s: %w", w.ID, err)
	}
	past := w.Link.PastEpochs
	if w.Link.Epoch != "" {
		past = append([]string{w.Link.Epoch}, slices.DeleteFunc(slices.Clone(past), func(e string) bool { return e == w.Link.Epoch })...)
	}
	past = slices.DeleteFunc(past, func(e string) bool { return e == epoch })
	if len(past) > MaxPastEpochs {
		past = past[:MaxPastEpochs]
	}
	got, err := s.repo.AdvanceWorkerEpoch(ctx, w.ID, w.Link.Epoch, NewEpoch{Epoch: epoch, StartedAt: s.clock.Now(), PastEpochs: past})
	if err != nil {
		return Worker{}, fmt.Errorf("advance epoch of worker %s: %w", w.ID, err)
	}
	s.log.InfoContext(ctx, "worker epoch changed", "worker_id", w.ID.String(), "epoch", epoch, "previous_epoch", w.Link.Epoch)
	return got, nil
}

// AdvanceAckSeq raises the ingest cursor of epoch to at least seq and
// returns the resulting cursor.
func (s *Service) AdvanceAckSeq(ctx context.Context, id domain.WorkerID, epoch string, seq int64) (int64, error) {
	got, err := s.repo.AdvanceWorkerAckSeq(ctx, id, epoch, seq)
	if err != nil {
		return 0, fmt.Errorf("advance ack seq of worker %s: %w", id, err)
	}
	return got, nil
}

// SetInfo stores what the worker reported in the Hello of epoch, clamped to
// sane sizes, with Info.Epoch = epoch (Worker.InfoCurrent). A Hello of an
// epoch that is no longer current is ignored.
func (s *Service) SetInfo(ctx context.Context, id domain.WorkerID, epoch string, info Info) error {
	in := sanitizeInfo(info)
	in.Epoch = epoch
	err := s.repo.SetWorkerInfo(ctx, id, epoch, in)
	if errors.Is(err, domain.ErrConflict) {
		s.log.DebugContext(ctx, "ignoring hello of a replaced epoch", "worker_id", id.String(), "epoch", epoch)
		return nil
	}
	if err != nil {
		return fmt.Errorf("set info of worker %s: %w", id, err)
	}
	return nil
}

// Touch records an authenticated contact with the worker now.
func (s *Service) Touch(ctx context.Context, id domain.WorkerID) error {
	if err := s.repo.TouchWorker(ctx, id, s.clock.Now()); err != nil {
		return fmt.Errorf("touch worker %s: %w", id, err)
	}
	return nil
}

func sanitizeInfo(in Info) Info {
	out := Info{
		Version:           clampString(in.Version, maxInfoString),
		OS:                clampString(in.OS, maxPlatformLen),
		Arch:              clampString(in.Arch, maxPlatformLen),
		Hostname:          clampString(in.Hostname, maxInfoString),
		MaxConcurrentJobs: max(in.MaxConcurrentJobs, 0),
		ReportedAt:        in.ReportedAt,
	}
	for _, t := range in.Tools[:min(len(in.Tools), maxTools)] {
		out.Tools = append(out.Tools, Tool{Name: clampString(t.Name, maxInfoString), Version: clampString(t.Version, maxInfoString)})
	}
	for _, k := range in.JobKinds[:min(len(in.JobKinds), maxJobKinds)] {
		out.JobKinds = append(out.JobKinds, clampString(k, maxInfoString))
	}
	for _, r := range in.LocalRoots[:min(len(in.LocalRoots), maxLocalRoots)] {
		out.LocalRoots = append(out.LocalRoots, clampString(r, maxPathLen))
	}
	for _, f := range in.Features[:min(len(in.Features), maxFeatures)] {
		out.Features = append(out.Features, clampString(f, maxInfoString))
	}
	for _, op := range in.ProbeOps[:min(len(in.ProbeOps), maxProbeOps)] {
		out.ProbeOps = append(out.ProbeOps, clampString(op, maxInfoString))
	}
	out.Hooks = sanitizeHooks(in.Hooks)
	return out
}

// sanitizeHooks returns the hooks a worker reported that a job could name:
// those with a valid name, each once, at most domain.MaxWorkerHooks of
// them, sorted by name, with the description as one line of at most
// domain.MaxHookDescriptionLen characters and a timeout that is not
// negative.
func sanitizeHooks(in []Hook) []Hook {
	var out []Hook
	seen := make(map[string]struct{}, len(in))
	for _, h := range in {
		if len(out) == domain.MaxWorkerHooks {
			break
		}
		if _, dup := seen[h.Name]; dup || !domain.ValidHookName(h.Name) {
			continue
		}
		seen[h.Name] = struct{}{}
		out = append(out, Hook{Name: h.Name, Description: oneLine(h.Description, domain.MaxHookDescriptionLen), Timeout: max(h.Timeout, 0)})
	}
	slices.SortFunc(out, func(a, b Hook) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// oneLine returns s as valid UTF-8 without control characters (each is
// replaced by a space), cut to at most n characters.
func oneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
