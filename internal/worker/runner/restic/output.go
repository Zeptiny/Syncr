package restic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	pathpkg "path"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// message is the union of the fields of restic's --json messages that this
// package reads. Unknown fields and unknown message types are ignored, so a
// newer restic never breaks a run. The same field names carry different
// meanings in different commands; the session picks by command and
// message_type.
type message struct {
	MessageType string `json:"message_type"`

	// backup and restore status
	TotalFiles int64 `json:"total_files"`
	TotalBytes int64 `json:"total_bytes"`
	// backup status
	FilesDone int64 `json:"files_done"`
	BytesDone int64 `json:"bytes_done"`
	// restore status and summary
	FilesRestored int64 `json:"files_restored"`
	BytesRestored int64 `json:"bytes_restored"`
	FilesSkipped  int64 `json:"files_skipped"`
	BytesSkipped  int64 `json:"bytes_skipped"`
	FilesDeleted  int64 `json:"files_deleted"`

	// backup summary
	FilesNew            int64  `json:"files_new"`
	FilesChanged        int64  `json:"files_changed"`
	FilesUnmodified     int64  `json:"files_unmodified"`
	DirsNew             int64  `json:"dirs_new"`
	DirsChanged         int64  `json:"dirs_changed"`
	DirsUnmodified      int64  `json:"dirs_unmodified"`
	DataAdded           int64  `json:"data_added"`
	DataAddedPacked     int64  `json:"data_added_packed"`
	TotalFilesProcessed int64  `json:"total_files_processed"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
	SnapshotID          string `json:"snapshot_id"`
	DryRun              bool   `json:"dry_run"`

	// check summary
	NumErrors          *int64   `json:"num_errors"`
	SuggestRepairIndex bool     `json:"suggest_repair_index"`
	SuggestPrune       bool     `json:"suggest_prune"`
	BrokenPacks        []string `json:"broken_packs"`

	// ls node: {"message_type":"node","name":...,"type":"dir","path":...}
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	Mtime       time.Time `json:"mtime"`
	Permissions string    `json:"permissions"`

	// init
	ID string `json:"id"`

	// error: {"message_type":"error","error":{"message":...},"during":...,"item":...}
	Error  *errorBody `json:"error"`
	During string     `json:"during"`
	Item   string     `json:"item"`

	// exit_error: {"message_type":"exit_error","code":12,"message":"Fatal: ..."}
	Code    *int   `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Message string `json:"message"`
}

// session consumes the output of one restic process. Its methods run on the
// goroutine that called proc.Supervisor.Run, so it needs no locking.
type session struct {
	kind  string
	start time.Time
	emit  runner.EmitFunc
	// sc scrubs the text kept for diagnostics and warnings, whole and
	// before it is shortened.
	sc *scrub.Scrubber

	prog runner.Progress

	// summaries seen
	backup      *message
	restore     *message
	check       *message
	checkErrors *int64
	initialized *message

	// collect keeps standard output as text in raw, up to maxRawBytes.
	collect      bool
	raw          []byte
	rawTruncated bool
	// ls collects the nodes of a listing.
	ls *lsFilter

	// error accounting: distinct failing items (restic reports a file once
	// per phase, e.g. "scan" and "archival"), at most maxErrItems of them;
	// reports of further items are counted in errOverflow without telling
	// them apart, so such an item can count twice. errLines counts reports
	// that name no item.
	errItems    map[string]struct{}
	errOverflow int64
	errLines    int64
	// itemErrors are the first maxItemErrors distinct item errors in order
	// of appearance ("<item>: <message>"), for warnings.
	itemErrors []string

	exit *message // exit_error

	// array holds the last JSON array line (snapshots, forget).
	array          []byte
	arrayTruncated bool

	tail []string // recent non-JSON lines, for diagnostics
}

func newSession(kind string, emit runner.EmitFunc, sc *scrub.Scrubber) *session {
	return &session{kind: kind, start: time.Now(), emit: emit, sc: sc, errItems: make(map[string]struct{})}
}

// stdout handles one line of restic's standard output.
func (s *session) stdout(l proc.Line) {
	if s.collect {
		if len(s.raw)+len(l.Data)+1 > maxRawBytes {
			s.rawTruncated = true
		} else {
			s.raw = append(append(s.raw, l.Data...), '\n')
		}
	}
	s.line(l)
}

// line handles one line of restic output from either stream.
func (s *session) line(l proc.Line) {
	data := bytes.TrimSpace(l.Data)
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case '{':
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			s.keepTail(data)
			return
		}
		s.message(&m)
	case '[':
		s.array = append(s.array[:0], data...)
		s.arrayTruncated = l.Truncated
	default:
		s.keepTail(data)
	}
}

func (s *session) keepTail(data []byte) {
	if len(s.tail) == maxTailLines {
		copy(s.tail, s.tail[1:])
		s.tail = s.tail[:maxTailLines-1]
	}
	s.tail = append(s.tail, s.sc.Clip(string(data), maxTailLineBytes))
}

func (s *session) message(m *message) {
	switch m.MessageType {
	case "status":
		s.status(m)
	case "summary":
		s.summary(m)
	case "error":
		s.itemError(m)
	case "exit_error":
		s.exit = m
	case "initialized":
		s.initialized = m
	case "node":
		if s.ls != nil {
			s.ls.node(m)
		}
	}
}

// status handles the periodic progress messages of backup and restore.
func (s *session) status(m *message) {
	switch s.kind {
	case kindBackup:
		s.setProgress(m.BytesDone, m.TotalBytes, m.FilesDone, m.TotalFiles)
	case kindRestore:
		s.setProgress(m.BytesRestored+m.BytesSkipped, m.TotalBytes, m.FilesRestored+m.FilesSkipped, m.TotalFiles)
	}
}

func (s *session) summary(m *message) {
	switch s.kind {
	case kindBackup:
		s.backup = m
		s.setProgress(m.TotalBytesProcessed, m.TotalBytesProcessed, m.TotalFilesProcessed, m.TotalFilesProcessed)
	case kindRestore:
		s.restore = m
		s.setProgress(m.BytesRestored+m.BytesSkipped, m.TotalBytes, m.FilesRestored+m.FilesSkipped, m.TotalFiles)
	case kindCheck:
		s.check = m
		if m.NumErrors != nil {
			s.checkErrors = m.NumErrors
			s.prog.Errors = max(s.prog.Errors, *m.NumErrors)
			s.publish()
		}
	}
}

func (s *session) itemError(m *message) {
	if s.kind != kindBackup && s.kind != kindRestore {
		return
	}
	switch _, seen := s.errItems[m.Item]; {
	case m.Item == "":
		s.errLines++
		s.noteItemError("", m)
	case seen:
	case len(s.errItems) < maxErrItems:
		s.errItems[m.Item] = struct{}{}
		s.noteItemError(m.Item, m)
	default:
		s.errOverflow++
	}
	s.prog.Errors = max(s.prog.Errors, s.unreadable())
	s.publish()
}

// noteItemError remembers the first few item errors for warnings. Messages
// usually name the item already; if not, it is prepended.
func (s *session) noteItemError(item string, m *message) {
	if len(s.itemErrors) >= maxItemErrors {
		return
	}
	msg := ""
	if m.Error != nil {
		msg = m.Error.Message
	}
	switch {
	case msg == "":
		msg = item
	case item != "" && !strings.Contains(msg, item):
		msg = item + ": " + msg
	}
	if msg != "" {
		s.itemErrors = append(s.itemErrors, s.sc.Clip(msg, maxTailLineBytes))
	}
}

// unreadable is the number of distinct items restic could not read (an
// estimate beyond maxErrItems failing paths, see errOverflow).
func (s *session) unreadable() int64 {
	return s.errLines + int64(len(s.errItems)) + s.errOverflow
}

// setProgress folds a snapshot into the cumulative progress: counters never
// decrease; totals may grow.
func (s *session) setProgress(bytes, totalBytes, files, totalFiles int64) {
	s.prog.Bytes = max(s.prog.Bytes, bytes)
	s.prog.Files = max(s.prog.Files, files)
	s.prog.TotalBytes = max(s.prog.TotalBytes, totalBytes)
	s.prog.TotalFiles = max(s.prog.TotalFiles, totalFiles)
	s.publish()
}

func (s *session) publish() {
	s.prog.Elapsed = time.Since(s.start)
	s.emit(s.prog)
}

// stats returns the final cumulative counters.
func (s *session) stats() runner.Progress {
	p := s.prog
	p.Elapsed = time.Since(s.start)
	return p
}

// detail is restic's explanation of a failure: its exit_error message, or
// the recent diagnostic lines.
func (s *session) detail() string {
	var msg string
	switch {
	case s.exit != nil && s.exit.Message != "":
		msg = s.exit.Message
	case len(s.tail) > 0:
		msg = joinLines(s.tail)
	}
	if s.kind == kindCheck && s.checkErrors != nil && *s.checkErrors > 0 {
		prefix := fmt.Sprintf("the check found %d error(s)", *s.checkErrors)
		if msg == "" {
			return prefix
		}
		return prefix + "; " + msg
	}
	return msg
}

func joinLines(lines []string) string {
	var b bytes.Buffer
	for i, l := range lines {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(l)
	}
	return b.String()
}

// output builds the kind-specific JobOutput after a successful run (exit 0,
// or exit 3 for backup). It returns nil for kinds without output. An error
// means restic's output could not be understood.
func (s *session) output(spec *syncrv1.JobSpec) (*syncrv1.JobOutput, error) {
	switch s.kind {
	case kindBackup:
		return s.backupOutput(spec.GetResticBackup())
	case kindForget:
		return s.forgetOutput()
	case kindSnapshots:
		out, err := s.snapshotsOutput(0)
		if err != nil {
			return nil, err
		}
		return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticSnapshots{ResticSnapshots: out}}, nil
	case kindCheck:
		return s.checkOutput(spec.GetResticCheck()), nil
	case kindRestore:
		return s.restoreOutput(), nil
	}
	return nil, nil
}

func (s *session) backupOutput(op *syncrv1.ResticBackup) (*syncrv1.JobOutput, error) {
	m := s.backup
	if m == nil {
		return nil, errors.New("restic backup finished without a summary")
	}
	id := m.SnapshotID
	if op.GetDryRun() || m.DryRun {
		id = "" // a dry run creates no snapshot
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
		SnapshotId:           id,
		DataAddedBytes:       m.DataAdded,
		FilesNew:             m.FilesNew,
		FilesChanged:         m.FilesChanged,
		FilesUnmodified:      m.FilesUnmodified,
		DirsNew:              m.DirsNew,
		DirsChanged:          m.DirsChanged,
		DirsUnmodified:       m.DirsUnmodified,
		DataAddedPackedBytes: m.DataAddedPacked,
		TotalFilesProcessed:  m.TotalFilesProcessed,
		TotalBytesProcessed:  m.TotalBytesProcessed,
	}}}, nil
}

// checkOutput is the output of a check that ran to completion; nil if
// restic printed no summary.
func (s *session) checkOutput(op *syncrv1.ResticCheck) *syncrv1.JobOutput {
	m := s.check
	if m == nil {
		return nil
	}
	out := &syncrv1.ResticCheckOutput{
		SuggestRepairIndex: m.SuggestRepairIndex,
		SuggestPrune:       m.SuggestPrune,
		BrokenPacks:        m.BrokenPacks,
		ReadDataPercent:    op.GetReadDataPercent(),
	}
	if m.NumErrors != nil {
		out.NumErrors = *m.NumErrors
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticCheck{ResticCheck: out}}
}

// restoreOutput is restic's restore summary; nil if it printed none.
func (s *session) restoreOutput() *syncrv1.JobOutput {
	m := s.restore
	if m == nil {
		return nil
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticRestore{ResticRestore: &syncrv1.ResticRestoreOutput{
		TotalFiles:    m.TotalFiles,
		FilesRestored: m.FilesRestored,
		FilesSkipped:  m.FilesSkipped,
		FilesDeleted:  m.FilesDeleted,
		TotalBytes:    m.TotalBytes,
		BytesRestored: m.BytesRestored,
		BytesSkipped:  m.BytesSkipped,
	}}}
}

// forgetGroup is the part of a "restic forget --json" group that is read.
type forgetGroup struct {
	Remove []struct {
		ID string `json:"id"`
	} `json:"remove"`
	// Keep is counted only; its elements are whole snapshots.
	Keep []json.RawMessage `json:"keep"`
}

func (s *session) forgetOutput() (*syncrv1.JobOutput, error) {
	out := &syncrv1.ResticForgetOutput{}
	if s.array == nil {
		return nil, errors.New("restic forget produced no result")
	}
	// A listing cut at the supervisor's line limit still yields the groups
	// that fit; the snapshots were removed either way.
	_, err := decodeArray(s.array, func(g forgetGroup) {
		out.KeptCount += int32(min(len(g.Keep), math.MaxInt32-int(out.KeptCount))) //nolint:gosec // bounded by min
		for _, r := range g.Remove {
			if r.ID != "" {
				out.RemovedSnapshotIds = append(out.RemovedSnapshotIds, r.ID)
			}
		}
	})
	if err != nil && !s.arrayTruncated {
		return nil, fmt.Errorf("parse restic forget result: %w", err)
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: out}}, nil
}

// snapshotJSON is the part of a "restic snapshots --json" entry that is read.
type snapshotJSON struct {
	ID             string       `json:"id"`
	Time           time.Time    `json:"time"`
	Hostname       string       `json:"hostname"`
	Username       string       `json:"username"`
	Parent         string       `json:"parent"`
	ProgramVersion string       `json:"program_version"`
	Paths          []string     `json:"paths"`
	Tags           []string     `json:"tags"`
	Summary        *summaryJSON `json:"summary"`
}

// summaryJSON is the backup summary restic 0.17 and later store in a
// snapshot.
type summaryJSON struct {
	BackupStart         time.Time `json:"backup_start"`
	BackupEnd           time.Time `json:"backup_end"`
	FilesNew            int64     `json:"files_new"`
	FilesChanged        int64     `json:"files_changed"`
	FilesUnmodified     int64     `json:"files_unmodified"`
	DirsNew             int64     `json:"dirs_new"`
	DirsChanged         int64     `json:"dirs_changed"`
	DirsUnmodified      int64     `json:"dirs_unmodified"`
	DataBlobs           int64     `json:"data_blobs"`
	TreeBlobs           int64     `json:"tree_blobs"`
	DataAdded           int64     `json:"data_added"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
}

func (sn snapshotJSON) proto() *syncrv1.ResticSnapshot {
	out := &syncrv1.ResticSnapshot{
		Id:             sn.ID,
		Time:           timestamppb.New(sn.Time),
		Host:           sn.Hostname,
		Paths:          sn.Paths,
		Tags:           sn.Tags,
		Parent:         sn.Parent,
		Username:       sn.Username,
		ProgramVersion: strings.TrimPrefix(sn.ProgramVersion, "restic "),
	}
	if m := sn.Summary; m != nil {
		out.Summary = &syncrv1.ResticSnapshotSummary{
			FilesNew: m.FilesNew, FilesChanged: m.FilesChanged, FilesUnmodified: m.FilesUnmodified,
			DirsNew: m.DirsNew, DirsChanged: m.DirsChanged, DirsUnmodified: m.DirsUnmodified,
			DataBlobs: m.DataBlobs, TreeBlobs: m.TreeBlobs,
			DataAddedBytes: m.DataAdded, DataAddedPackedBytes: m.DataAddedPacked,
			TotalFilesProcessed: m.TotalFilesProcessed, TotalBytesProcessed: m.TotalBytesProcessed,
		}
		if !m.BackupStart.IsZero() {
			out.Summary.BackupStart = timestamppb.New(m.BackupStart)
		}
		if !m.BackupEnd.IsZero() {
			out.Summary.BackupEnd = timestamppb.New(m.BackupEnd)
		}
	}
	return out
}

// snapshotsOutput parses the listing, in restic's order (oldest first). A
// positive limit keeps only the newest limit snapshots and reports whether
// older ones were dropped.
func (s *session) snapshotsOutput(limit int) (*syncrv1.ResticSnapshotsOutput, error) {
	if s.array == nil {
		return nil, errors.New("restic snapshots produced no result")
	}
	if s.arrayTruncated {
		return nil, errors.New("snapshot listing exceeds the supervisor's line limit; raise proc.Options.MaxLineBytes")
	}
	out := &syncrv1.ResticSnapshotsOutput{}
	_, err := decodeArray(s.array, func(sn snapshotJSON) {
		out.Snapshots = append(out.Snapshots, sn.proto())
	})
	if err != nil {
		return nil, fmt.Errorf("parse restic snapshots result: %w", err)
	}
	if limit > 0 && len(out.Snapshots) > limit {
		slices.SortStableFunc(out.Snapshots, func(a, b *syncrv1.ResticSnapshot) int {
			return a.GetTime().AsTime().Compare(b.GetTime().AsTime())
		})
		out.Snapshots = slices.Clone(out.Snapshots[len(out.Snapshots)-limit:])
		out.Truncated = true
	}
	return out, nil
}

// maxRawBytes bounds the standard output kept by a collecting command.
const maxRawBytes = 64 << 10

// lsFilter collects the direct children of dir from a "restic ls" listing,
// which also lists dir itself.
type lsFilter struct {
	dir string // cleaned tree path
	max int
	// found reports that dir itself was listed (the root never is).
	found     bool
	entries   []*syncrv1.ProbeEntry
	truncated bool
}

func (f *lsFilter) node(m *message) {
	p := pathpkg.Clean(m.Path)
	if p == f.dir {
		f.found = true
		return
	}
	if pathpkg.Dir(p) != f.dir {
		return
	}
	if len(f.entries) >= f.max {
		f.truncated = true
		return
	}
	e := &syncrv1.ProbeEntry{
		Name: runner.ValidText(m.Name),
		Path: runner.ValidText(p),
		Mode: m.Permissions,
	}
	switch m.Type {
	case "file":
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE
		e.Size = m.Size
	case "dir":
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
	case "symlink":
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK
	default:
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER
	}
	if !m.Mtime.IsZero() {
		e.ModTime = timestamppb.New(m.Mtime)
	}
	f.entries = append(f.entries, e)
}

// sorted returns the entries, directories first, then by name.
func (f *lsFilter) sorted() []*syncrv1.ProbeEntry {
	slices.SortStableFunc(f.entries, func(a, b *syncrv1.ProbeEntry) int {
		ad, bd := a.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, b.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
		if ad != bd {
			if ad {
				return -1
			}
			return 1
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	return f.entries
}

// repositoryConfig parses the collected output of "restic cat config".
func (s *session) repositoryConfig() (*syncrv1.ResticRepositoryConfig, error) {
	var cfg struct {
		Version int32  `json:"version"`
		ID      string `json:"id"`
	}
	if s.rawTruncated {
		return nil, errors.New("the repository config is too large")
	}
	if err := json.Unmarshal(bytes.TrimSpace(s.raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse the repository config: %w", err)
	}
	if !resticIDPattern.MatchString(cfg.ID) {
		return nil, errors.New("the repository config has no valid ID")
	}
	return &syncrv1.ResticRepositoryConfig{Id: cfg.ID, Version: cfg.Version}, nil
}

// decodeArray streams the elements of a JSON array into fn, one at a time, so
// elements before a truncation point are still delivered. It returns the
// number of elements decoded.
func decodeArray[T any](data []byte, fn func(T)) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return 0, errors.New("not a JSON array")
	}
	n := 0
	for dec.More() {
		var v T
		if err := dec.Decode(&v); err != nil {
			return n, err
		}
		fn(v)
		n++
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return n, err
	}
	return n, nil
}
