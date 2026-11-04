package restic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
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

	// backup summary
	FilesNew            int64  `json:"files_new"`
	FilesChanged        int64  `json:"files_changed"`
	FilesUnmodified     int64  `json:"files_unmodified"`
	DataAdded           int64  `json:"data_added"`
	TotalFilesProcessed int64  `json:"total_files_processed"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
	SnapshotID          string `json:"snapshot_id"`
	DryRun              bool   `json:"dry_run"`

	// check summary
	NumErrors *int64 `json:"num_errors"`

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

	prog runner.Progress

	// summaries seen
	backup      *message
	restore     *message
	checkErrors *int64
	initialized *message

	// error accounting: distinct failing items (restic reports a file once
	// per phase, e.g. "scan" and "archival").
	errItems map[string]struct{}
	errLines int64

	exit *message // exit_error

	// array holds the last JSON array line (snapshots, forget).
	array          []byte
	arrayTruncated bool

	tail []string // recent non-JSON lines, for diagnostics
}

func newSession(kind string, emit runner.EmitFunc) *session {
	return &session{kind: kind, start: time.Now(), emit: emit, errItems: make(map[string]struct{})}
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
	s.tail = append(s.tail, clip(string(data), maxTailLineBytes))
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
	if m.Item == "" {
		s.errLines++
	} else {
		s.errItems[m.Item] = struct{}{}
	}
	s.prog.Errors = max(s.prog.Errors, s.errLines+int64(len(s.errItems)))
	s.publish()
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
		return s.snapshotsOutput()
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
		SnapshotId:      id,
		DataAddedBytes:  m.DataAdded,
		FilesNew:        m.FilesNew,
		FilesChanged:    m.FilesChanged,
		FilesUnmodified: m.FilesUnmodified,
	}}}, nil
}

// forgetGroup is the part of a "restic forget --json" group that is read.
type forgetGroup struct {
	Remove []struct {
		ID string `json:"id"`
	} `json:"remove"`
}

func (s *session) forgetOutput() (*syncrv1.JobOutput, error) {
	out := &syncrv1.ResticForgetOutput{}
	if s.array == nil {
		return nil, errors.New("restic forget produced no result")
	}
	// A listing cut at the supervisor's line limit still yields the groups
	// that fit; the snapshots were removed either way.
	_, err := decodeArray(s.array, func(g forgetGroup) {
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
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
}

func (s *session) snapshotsOutput() (*syncrv1.JobOutput, error) {
	if s.array == nil {
		return nil, errors.New("restic snapshots produced no result")
	}
	if s.arrayTruncated {
		return nil, errors.New("snapshot listing exceeds the supervisor's line limit; raise proc.Options.MaxLineBytes")
	}
	out := &syncrv1.ResticSnapshotsOutput{}
	_, err := decodeArray(s.array, func(sn snapshotJSON) {
		out.Snapshots = append(out.Snapshots, &syncrv1.ResticSnapshot{
			Id:    sn.ID,
			Time:  timestamppb.New(sn.Time),
			Host:  sn.Hostname,
			Paths: sn.Paths,
			Tags:  sn.Tags,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("parse restic snapshots result: %w", err)
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticSnapshots{ResticSnapshots: out}}, nil
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
