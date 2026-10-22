package runs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/status"
)

// EventType names the kind of a journal event.
type EventType string

// Journal event types and the fields each one carries.
const (
	// EventAttemptStart opens an attempt: Mode, Plan, Branch, Worktree, WorktreePlan, ProgressLog,
	// ConsoleOffset, Tasks, TaskTotal, and Stages.
	EventAttemptStart EventType = "attempt_start"
	// EventPhase is a phase change: Phase, plus WaitSeconds for a limit wait.
	EventPhase EventType = "phase"
	// EventTask is the task section the run entered: N (the plan position), M, and Title.
	EventTask EventType = "task"
	// EventTasks carries every plan task's status: Statuses.
	EventTasks EventType = "tasks"
	// EventReview is a review section the run entered: Kind, Iteration, and Label.
	EventReview EventType = "review"
	// EventFinalize is the finalize close-out result: Mode, StatusNote, PRURL, Merged, and Incomplete.
	EventFinalize EventType = "finalize"
	// EventReport is the completion report: Summary and Risk.
	EventReport EventType = "report"
	// EventFinish closes an attempt: Status, Reason, and ExitCode.
	EventFinish EventType = "finish"
)

// Stage names an attempt_start event lists, in pipeline order.
const (
	StageReview         = "review"
	StageExternalReview = "external_review"
	StageFinalize       = "finalize"
	StageReport         = "report"
)

// Review kinds a review event records.
const (
	ReviewInternal   = "internal"   // an internal review iteration
	ReviewExternal   = "external"   // an external reviewer iteration
	ReviewCustom     = "custom"     // a custom review script iteration
	ReviewEvaluation = "evaluation" // the review provider evaluating external findings
)

// TaskInfo is one plan task an attempt_start event lists. N is its 1-based plan position.
type TaskInfo struct {
	N     int    `json:"n"`
	Title string `json:"title"`
}

// TaskStatus is one plan task's status in a tasks event: ProgressPending, ProgressActive
// (partially ticked), or ProgressDone.
type TaskStatus struct {
	N      int    `json:"n"`
	Status string `json:"status"`
}

// Event is one line of a run's events.jsonl. Seq, Time, and Attempt are stamped by
// Journal.Append; the other fields belong to the event types that name them in the EventType
// constants, and MarshalJSON writes only the fields of the event's own type.
type Event struct {
	Seq     int       `json:"seq"`
	Time    time.Time `json:"time"`
	Attempt int       `json:"attempt"`
	Type    EventType `json:"type"`

	// Mode is the run mode (full or tasks-only) of an attempt_start event and the finalize mode
	// of a finalize event.
	Mode          string       `json:"mode"`
	Plan          string       `json:"plan"`
	Branch        string       `json:"branch"`
	Worktree      string       `json:"worktree"`
	WorktreePlan  string       `json:"worktree_plan"`
	ProgressLog   string       `json:"progress_log"`
	ConsoleOffset int64        `json:"console_offset"` // console.log size when the attempt started
	Tasks         []TaskInfo   `json:"tasks"`
	TaskTotal     int          `json:"task_total"`
	Stages        []string     `json:"stages"`
	Phase         status.Phase `json:"phase"`
	WaitSeconds   int          `json:"wait_seconds"`
	N             int          `json:"n"`
	M             int          `json:"m"`
	Title         string       `json:"title"`
	Statuses      []TaskStatus `json:"statuses"`
	Kind          string       `json:"kind"`
	Iteration     int          `json:"iteration"`
	Label         string       `json:"label"`
	StatusNote    string       `json:"status_note"`
	PRURL         string       `json:"pr_url"`
	Merged        bool         `json:"merged"`
	Incomplete    string       `json:"incomplete"`
	Summary       string       `json:"summary"`
	Risk          string       `json:"risk"`
	Status        Status       `json:"status"`
	Reason        string       `json:"reason"`
	ExitCode      int          `json:"exit_code"`
}

// eventHeader is the part every journal line carries.
type eventHeader struct {
	Seq     int       `json:"seq"`
	Time    time.Time `json:"time"`
	Attempt int       `json:"attempt"`
	Type    EventType `json:"type"`
}

// MarshalJSON writes the header followed by the fields of e's type only, so a line never carries
// another type's fields. An event of an unknown type is written as its header.
func (e Event) MarshalJSON() ([]byte, error) {
	head, err := json.Marshal(eventHeader{Seq: e.Seq, Time: e.Time, Attempt: e.Attempt, Type: e.Type})
	if err != nil {
		return nil, fmt.Errorf("encode event header: %w", err)
	}
	payload := e.payload()
	if payload == nil {
		return head, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s event: %w", e.Type, err)
	}
	// splice the payload object's members into the header object
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head[:len(head)-1]...)
	out = append(out, ',')
	return append(out, body[1:]...), nil
}

// payload returns the type-specific fields of e as a value to encode, or nil for an unknown type.
// Empty lists are written as [] rather than null.
func (e Event) payload() any {
	switch e.Type {
	case EventAttemptStart:
		return struct {
			Mode          string     `json:"mode"`
			Plan          string     `json:"plan"`
			Branch        string     `json:"branch"`
			Worktree      string     `json:"worktree"`
			WorktreePlan  string     `json:"worktree_plan"`
			ProgressLog   string     `json:"progress_log"`
			ConsoleOffset int64      `json:"console_offset"`
			Tasks         []TaskInfo `json:"tasks"`
			TaskTotal     int        `json:"task_total"`
			Stages        []string   `json:"stages"`
		}{e.Mode, e.Plan, e.Branch, e.Worktree, e.WorktreePlan, e.ProgressLog, e.ConsoleOffset,
			nonNil(e.Tasks), e.TaskTotal, nonNil(e.Stages)}
	case EventPhase:
		return struct {
			Phase       status.Phase `json:"phase"`
			WaitSeconds int          `json:"wait_seconds,omitempty"`
		}{e.Phase, e.WaitSeconds}
	case EventTask:
		return struct {
			N     int    `json:"n"`
			M     int    `json:"m"`
			Title string `json:"title"`
		}{e.N, e.M, e.Title}
	case EventTasks:
		return struct {
			Statuses []TaskStatus `json:"statuses"`
		}{nonNil(e.Statuses)}
	case EventReview:
		return struct {
			Kind      string `json:"kind"`
			Iteration int    `json:"iteration"`
			Label     string `json:"label"`
		}{e.Kind, e.Iteration, e.Label}
	case EventFinalize:
		return struct {
			Mode       string `json:"mode"`
			StatusNote string `json:"status_note"`
			PRURL      string `json:"pr_url"`
			Merged     bool   `json:"merged"`
			Incomplete string `json:"incomplete"`
		}{e.Mode, e.StatusNote, e.PRURL, e.Merged, e.Incomplete}
	case EventReport:
		return struct {
			Summary string `json:"summary"`
			Risk    string `json:"risk"`
		}{e.Summary, e.Risk}
	case EventFinish:
		return struct {
			Status   Status `json:"status"`
			Reason   string `json:"reason"`
			ExitCode int    `json:"exit_code"`
		}{e.Status, e.Reason, e.ExitCode}
	default:
		return nil
	}
}

// nonNil returns s, or an empty slice when s is nil, so it encodes as [].
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Journal appends events to a run's events.jsonl. Only the run process, which holds the lifetime
// lock, writes the journal, so seq numbers stay unique; a Journal is safe for concurrent use by
// that process's goroutines.
type Journal struct {
	mu      sync.Mutex
	f       *os.File
	attempt int
	seq     int   // seq of the last event written
	size    int64 // length of the journal's complete lines
}

// OpenJournal opens the journal of run directory dir for attempt, creating it with mode 0600 when
// it is missing; the directory must exist. Seq numbers continue from the journal's last event, so
// they increase across attempts. A final line left incomplete by a crash mid-write is cut off,
// so the next event starts on a line of its own.
func OpenJournal(dir string, attempt int) (*Journal, error) {
	path := filepath.Join(dir, JournalFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // journal path under the runs directory
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read journal: %w", err)
	}
	events, size := parseEvents(data)
	if size < int64(len(data)) {
		if err := f.Truncate(size); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cut incomplete journal line: %w", err)
		}
	}
	seq := 0
	for _, ev := range events {
		seq = max(seq, ev.Seq)
	}
	return &Journal{f: f, attempt: attempt, seq: seq, size: size}, nil
}

// Append stamps ev with the next seq number, the current time, and the journal's attempt, writes
// it as one line in a single write, and returns the stamped event. A failed write is cut off
// again, so the journal keeps only complete lines and the seq number is not used up.
func (j *Journal) Append(ev Event) (Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return Event{}, errors.New("append journal event: journal is closed")
	}
	ev.Seq = j.seq + 1
	ev.Time = time.Now().UTC()
	ev.Attempt = j.attempt
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("append journal event: %w", err)
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		_ = j.f.Truncate(j.size)
		return Event{}, fmt.Errorf("append journal event: %w", err)
	}
	j.seq = ev.Seq
	j.size += int64(len(line))
	return ev, nil
}

// Close closes the journal; later appends fail.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	if err != nil {
		return fmt.Errorf("close journal: %w", err)
	}
	return nil
}

// ReadFrom returns the events of run directory dir's journal whose seq is greater than seq, in
// journal order; ReadFrom(dir, 0) returns them all. A missing journal has no events. A final line
// still being written, or left incomplete by a crash, is not returned, and a corrupt line is
// skipped rather than failing the read.
func ReadFrom(dir string, seq int) ([]Event, error) {
	events, _, err := readJournal(filepath.Join(dir, JournalFile), 0)
	if err != nil {
		return nil, err
	}
	result := make([]Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq > seq {
			result = append(result, ev)
		}
	}
	return result, nil
}

// readJournal parses the complete lines of the journal at path from byte offset on and returns
// their events with the offset just past the last complete line. A missing journal has no events.
func readJournal(path string, offset int64) ([]Event, int64, error) {
	f, err := os.Open(path) //nolint:gosec // journal path under the runs directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()
	if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
		return nil, offset, fmt.Errorf("seek journal: %w", serr)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, fmt.Errorf("read journal: %w", err)
	}
	events, consumed := parseEvents(data)
	return events, offset + consumed, nil
}

// parseEvents decodes the newline-terminated lines of data and returns their events with the
// length of those lines. Trailing bytes without a newline are an incomplete line and are left
// out of both; a complete line that does not decode is skipped.
func parseEvents(data []byte) ([]Event, int64) {
	var events []Event
	consumed := 0
	for {
		end := bytes.IndexByte(data[consumed:], '\n')
		if end < 0 {
			return events, int64(consumed)
		}
		line := bytes.TrimSpace(data[consumed : consumed+end])
		consumed += end + 1
		var ev Event
		if len(line) == 0 || json.Unmarshal(line, &ev) != nil {
			continue
		}
		events = append(events, ev)
	}
}
