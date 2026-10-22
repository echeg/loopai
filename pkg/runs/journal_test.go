package runs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

func TestJournalSeqContinuesAfterReopen(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, 1)
	require.NoError(t, err)
	first, err := j.Append(Event{Type: EventPhase, Phase: status.PhaseTask})
	require.NoError(t, err)
	second, err := j.Append(Event{Type: EventTask, N: 1, M: 2, Title: "one"})
	require.NoError(t, err)
	require.NoError(t, j.Close())
	assert.Equal(t, 1, first.Seq)
	assert.Equal(t, 2, second.Seq)
	assert.Equal(t, 1, second.Attempt)
	assert.WithinDuration(t, time.Now(), second.Time, time.Minute)
	assert.Equal(t, time.UTC, second.Time.Location())

	info, err := os.Stat(filepath.Join(dir, JournalFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	j, err = OpenJournal(dir, 2)
	require.NoError(t, err)
	third, err := j.Append(Event{Type: EventAttemptStart, Mode: "full"})
	require.NoError(t, err)
	require.NoError(t, j.Close())
	assert.Equal(t, 3, third.Seq, "seq numbers continue across attempts")
	assert.Equal(t, 2, third.Attempt)

	events, err := ReadFrom(dir, 0)
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, []int{1, 2, 3}, eventSeqs(events))
	assert.Equal(t, []int{1, 1, 2}, []int{events[0].Attempt, events[1].Attempt, events[2].Attempt})
	assert.Equal(t, "one", events[1].Title)
}

func TestJournalConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, 1)
	require.NoError(t, err)
	defer j.Close()

	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				_, appendErr := j.Append(Event{Type: EventReview, Kind: ReviewExternal, Iteration: i,
					Label: "writer " + string(rune('a'+w))})
				assert.NoError(t, appendErr)
			}
		})
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(dir, JournalFile)) //nolint:gosec // test temp path
	require.NoError(t, err)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var seqs []int
	for scanner.Scan() {
		var ev Event
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &ev), "every line is one complete JSON event: %s", scanner.Text())
		assert.Equal(t, EventReview, ev.Type)
		seqs = append(seqs, ev.Seq)
	}
	require.NoError(t, scanner.Err())
	require.Len(t, seqs, writers*perWriter)
	assert.True(t, slices.IsSorted(seqs), "lines are written in seq order")
	assert.Equal(t, writers*perWriter, seqs[len(seqs)-1], "no seq number is used twice or skipped")
}

func TestJournalTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, 1)
	require.NoError(t, err)
	_, err = j.Append(Event{Type: EventPhase, Phase: status.PhaseTask})
	require.NoError(t, err)
	_, err = j.Append(Event{Type: EventTask, N: 1, M: 1, Title: "only"})
	require.NoError(t, err)
	require.NoError(t, j.Close())

	// a crash in the middle of the third write
	path := filepath.Join(dir, JournalFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test temp path
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq":3,"time":"2026-10-22T12:00:00Z","attempt":1,"type":"fin`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	events, err := ReadFrom(dir, 0)
	require.NoError(t, err, "a truncated final line is not fatal")
	assert.Equal(t, []int{1, 2}, eventSeqs(events))

	j, err = OpenJournal(dir, 2)
	require.NoError(t, err)
	next, err := j.Append(Event{Type: EventAttemptStart, Mode: "full"})
	require.NoError(t, err)
	require.NoError(t, j.Close())
	assert.Equal(t, 3, next.Seq, "the seq of the incomplete line is reused")

	events, err = ReadFrom(dir, 0)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, eventSeqs(events), "the next event starts on its own line")
	assert.Equal(t, EventAttemptStart, events[2].Type)
}

func TestReadFrom(t *testing.T) {
	dir := t.TempDir()
	events, err := ReadFrom(dir, 0)
	require.NoError(t, err)
	assert.Empty(t, events, "a missing journal has no events")

	j, err := OpenJournal(dir, 1)
	require.NoError(t, err)
	for i := range 5 {
		_, appendErr := j.Append(Event{Type: EventTask, N: i + 1, M: 5})
		require.NoError(t, appendErr)
	}
	require.NoError(t, j.Close())

	// a corrupt complete line in the middle is skipped, not fatal
	path := filepath.Join(dir, JournalFile)
	data, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	lines := bytes.SplitAfter(data, []byte("\n"))
	corrupt := slices.Concat(lines[0], lines[1], []byte("not json\n"), lines[2], lines[3], lines[4])
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))

	tests := []struct {
		name string
		seq  int
		want []int
	}{
		{name: "all", seq: 0, want: []int{1, 2, 3, 4, 5}},
		{name: "mid journal", seq: 3, want: []int{4, 5}},
		{name: "last", seq: 5, want: []int{}},
		{name: "beyond the end", seq: 9, want: []int{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadFrom(dir, tc.seq)
			require.NoError(t, err)
			assert.Equal(t, tc.want, eventSeqs(got))
		})
	}
}

func TestReadFromError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, JournalFile), 0o750))
	_, err := ReadFrom(dir, 0)
	require.ErrorContains(t, err, "read journal")
}

func TestJournalErrors(t *testing.T) {
	_, err := OpenJournal(filepath.Join(t.TempDir(), "missing"), 1)
	require.ErrorContains(t, err, "open journal", "the run directory is never created")

	j, err := OpenJournal(t.TempDir(), 1)
	require.NoError(t, err)
	require.NoError(t, j.Close())
	require.NoError(t, j.Close(), "closing twice is harmless")
	_, err = j.Append(Event{Type: EventPhase, Phase: status.PhaseTask})
	require.ErrorContains(t, err, "journal is closed")
}

// faultyJournalFile is a journal file whose next failWrites writes stop halfway with an error and
// whose next failTruncates truncates fail.
type faultyJournalFile struct {
	*os.File
	failWrites    int
	failTruncates int
}

func (f *faultyJournalFile) Write(p []byte) (int, error) {
	if f.failWrites > 0 {
		f.failWrites--
		n, _ := f.File.Write(p[:len(p)/2])
		return n, errors.New("disk full")
	}
	return f.File.Write(p) //nolint:wrapcheck // test double forwards the file's own error
}

func (f *faultyJournalFile) Truncate(size int64) error {
	if f.failTruncates > 0 {
		f.failTruncates--
		return errors.New("truncate refused")
	}
	return f.File.Truncate(size) //nolint:wrapcheck // test double forwards the file's own error
}

func TestJournalFailedWrite(t *testing.T) {
	tests := []struct {
		name          string
		failTruncates int
		wantLines     int // newline-terminated lines in the file, corrupt ones included
	}{
		{name: "a cut-off failed write leaves no trace", failTruncates: 0, wantLines: 3},
		{name: "a failed write that cannot be cut off is cut on the next append", failTruncates: 1, wantLines: 3},
		{name: "torn bytes that stay end as a line of their own", failTruncates: 2, wantLines: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			j, err := OpenJournal(dir, 1)
			require.NoError(t, err)
			defer j.Close()
			_, err = j.Append(Event{Type: EventPhase, Phase: status.PhaseTask})
			require.NoError(t, err)

			file, ok := j.f.(*os.File)
			require.True(t, ok)
			j.f = &faultyJournalFile{File: file, failWrites: 1, failTruncates: tc.failTruncates}
			_, err = j.Append(Event{Type: EventTask, N: 1, M: 1, Title: "torn"})
			require.ErrorContains(t, err, "append journal event: disk full")

			second, err := j.Append(Event{Type: EventTask, N: 1, M: 1, Title: "after"})
			require.NoError(t, err)
			assert.Equal(t, 2, second.Seq, "the failed write did not use up its seq number")
			third, err := j.Append(Event{Type: EventFinish, Status: StatusSucceeded})
			require.NoError(t, err)
			assert.Equal(t, 3, third.Seq)

			events, err := ReadFrom(dir, 0)
			require.NoError(t, err)
			assert.Equal(t, []int{1, 2, 3}, eventSeqs(events))
			assert.Equal(t, "after", events[1].Title, "the event after the torn bytes decodes")
			assert.Equal(t, EventFinish, events[2].Type)

			data, err := os.ReadFile(filepath.Join(dir, JournalFile)) //nolint:gosec // test temp path
			require.NoError(t, err)
			assert.Equal(t, tc.wantLines, bytes.Count(data, []byte("\n")))
			assert.True(t, bytes.HasSuffix(data, []byte("\n")))

			// a reopened journal continues after the complete lines, torn ones included
			require.NoError(t, j.Close())
			j, err = OpenJournal(dir, 2)
			require.NoError(t, err)
			next, err := j.Append(Event{Type: EventAttemptStart, Mode: "full"})
			require.NoError(t, err)
			require.NoError(t, j.Close())
			assert.Equal(t, 4, next.Seq)
			events, err = ReadFrom(dir, 0)
			require.NoError(t, err)
			assert.Equal(t, []int{1, 2, 3, 4}, eventSeqs(events))
		})
	}
}

func TestEventMarshalJSON(t *testing.T) {
	stamp := time.Date(2026, 10, 22, 15, 30, 0, 123, time.UTC)
	head := []string{"seq", "time", "attempt", "type"}
	// several cases set fields of another type: the exact key sets prove they never leak
	tests := []struct {
		name string
		ev   Event
		keys []string // payload keys after the header
		want map[string]any
	}{
		{name: "attempt start", ev: Event{Type: EventAttemptStart, Mode: "full", Plan: "/r/p.md", Branch: "b",
			Worktree: "/r/wt", WorktreePlan: "/r/wt/p.md", ProgressLog: "/r/log.txt", ConsoleOffset: 0,
			Tasks: []TaskInfo{{N: 1, Title: "one"}}, TaskTotal: 1, Stages: []string{StageReview}},
			keys: []string{"mode", "plan", "branch", "worktree", "worktree_plan", "progress_log", "console_offset",
				"tasks", "task_total", "stages"},
			want: map[string]any{"console_offset": float64(0), "tasks": []any{map[string]any{"n": float64(1),
				"title": "one"}}, "stages": []any{"review"}}},
		{name: "attempt start without tasks or stages", ev: Event{Type: EventAttemptStart, Mode: "tasks-only"},
			keys: []string{"mode", "plan", "branch", "worktree", "worktree_plan", "progress_log", "console_offset",
				"tasks", "task_total", "stages"},
			want: map[string]any{"tasks": []any{}, "stages": []any{}, "task_total": float64(0)}},
		{name: "phase", ev: Event{Type: EventPhase, Phase: status.PhaseReview, Title: "foreign", ExitCode: 3},
			keys: []string{"phase"},
			want: map[string]any{"phase": "review"}},
		{name: "limit wait phase", ev: Event{Type: EventPhase, Phase: status.PhaseLimitWait, WaitSeconds: 600},
			keys: []string{"phase", "wait_seconds"}, want: map[string]any{"wait_seconds": float64(600)}},
		{name: "task", ev: Event{Type: EventTask, N: 2, M: 3, Title: "two", Kind: "foreign", Plan: "/p.md"},
			keys: []string{"n", "m", "title"},
			want: map[string]any{"n": float64(2), "m": float64(3), "title": "two"}},
		{name: "tasks", ev: Event{Type: EventTasks, Statuses: []TaskStatus{{N: 1, Status: ProgressDone}}},
			keys: []string{"statuses"},
			want: map[string]any{"statuses": []any{map[string]any{"n": float64(1), "status": "done"}}}},
		{name: "review", ev: Event{Type: EventReview, Kind: ReviewInternal, Label: "review 0"},
			keys: []string{"kind", "iteration", "label"}, want: map[string]any{"iteration": float64(0)}},
		{name: "finalize", ev: Event{Type: EventFinalize, Mode: "sync", StatusNote: "synced"},
			keys: []string{"mode", "status_note", "pr_url", "merged", "incomplete"},
			want: map[string]any{"merged": false, "mode": "sync"}},
		{name: "report", ev: Event{Type: EventReport, Summary: "did it", Risk: "low"}, keys: []string{"summary", "risk"},
			want: map[string]any{"risk": "low"}},
		{name: "finish", ev: Event{Type: EventFinish, Status: StatusSucceeded, Phase: status.PhaseTask, Mode: "full"},
			keys: []string{"status", "reason", "exit_code"},
			want: map[string]any{"status": "succeeded", "exit_code": float64(0)}},
		{name: "unknown type", ev: Event{Type: "other", Phase: status.PhaseTask, N: 4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.ev.Seq, tc.ev.Time, tc.ev.Attempt = 7, stamp, 2
			data, err := json.Marshal(tc.ev)
			require.NoError(t, err)

			var obj map[string]any
			require.NoError(t, json.Unmarshal(data, &obj))
			assert.ElementsMatch(t, append(slices.Clone(head), tc.keys...), mapKeys(obj))
			assert.InDelta(t, 7, obj["seq"], 0)
			assert.Equal(t, "2026-10-22T15:30:00.000000123Z", obj["time"], "time is RFC3339Nano")
			assert.InDelta(t, 2, obj["attempt"], 0)
			for k, v := range tc.want {
				assert.Equal(t, v, obj[k], k)
			}

			var back Event
			require.NoError(t, json.Unmarshal(data, &back))
			assert.Equal(t, tc.ev.Seq, back.Seq)
			assert.Equal(t, tc.ev.Type, back.Type)
		})
	}
}

func TestEventRoundTrip(t *testing.T) {
	ev := Event{Seq: 3, Time: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), Attempt: 1, Type: EventFinish,
		Status: StatusFailed, Reason: "runner: task 3: boom", ExitCode: 1}
	data, err := json.Marshal(ev)
	require.NoError(t, err)
	var back Event
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, ev, back)
}

// eventSeqs returns the seq numbers of events.
func eventSeqs(events []Event) []int {
	seqs := make([]int, 0, len(events))
	for _, ev := range events {
		seqs = append(seqs, ev.Seq)
	}
	return seqs
}
