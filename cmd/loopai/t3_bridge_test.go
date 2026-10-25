package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
	"github.com/umputun/ralphex/pkg/t3"
	"github.com/umputun/ralphex/pkg/t3/t3test"
)

const (
	bridgeThreadID = "8e8b69fc-3359-47c8-a5a3-6dd91c245a86"
	// bridgeNativeThreadID is a provider-native thread id, as T3 gives subagent threads
	bridgeNativeThreadID = "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3AclaudeAgent%3Aagent%3A3"
	bridgeRunID          = "feature-20261022-153000-a1b2"
	bridgeProjectID      = "p1"
	bridgeCheckout       = "/repo/it's here"
	bridgeExecutable     = "/opt/loop ai/loopai"
	bridgeWait           = 10 * time.Second
	// bridgeRunCompletedAt is when the agent turn that launched the run completed, the latest run
	// thread.mark-unread rewinds the thread's visit to
	bridgeRunCompletedAt = "2026-10-23T11:00:00.000Z"
	// bridgeFollow is the follow command of the harness's run
	bridgeFollow = ` cd '/repo/it'\''s here' && '/opt/loop ai/loopai' --attach ` + bridgeRunID + "\r"
)

// bridgeTerminalID is the terminal the harness's bridge opens.
var bridgeTerminalID = t3TerminalIDPrefix + bridgeRunID

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// bridgeLines records the lines a bridge writes into the progress log.
type bridgeLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *bridgeLines) Print(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *bridgeLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// bridgeHarness is a run directory with a journal, a fake T3 server holding the thread and its
// project, and a bridge on a fake clock with fast ticks and backoffs.
type bridgeHarness struct {
	t        *testing.T
	server   *t3test.Server
	home     string
	root     string // the project's workspace root
	threadID string
	dir      string
	journal  *runs.Journal
	clock    *fakeClock
	log      *bridgeLines
	steps    atomic.Int64 // steps the bridge took
	applied  atomic.Int64 // seq of the last journal event the bridge applied
	bridge   *t3Bridge

	mu      sync.Mutex
	handled []t3.TerminalMetadataEvent // the terminal metadata events the bridge handled
}

// newBridgeHarness serves thread, whose project is bridgeProjectID and whose latest run completed at
// bridgeRunCompletedAt unless it names another time, from a fake server whose runtime file the T3 home
// holds unless noRuntime is set.
func newBridgeHarness(t *testing.T, thread t3test.Thread, noRuntime bool, opts ...t3test.Option) *bridgeHarness {
	t.Helper()
	h := &bridgeHarness{t: t, home: t.TempDir(), root: t.TempDir(), threadID: thread.ID, dir: t.TempDir(),
		clock: newFakeClock(), log: &bridgeLines{}}
	t.Setenv(t3.EnvHome, h.home)
	t.Setenv(t3.EnvURL, "")
	thread.ProjectID = bridgeProjectID
	if thread.LatestRunCompletedAt == "" {
		thread.LatestRunCompletedAt = bridgeRunCompletedAt
	}
	opts = append([]t3test.Option{t3test.WithThread(thread),
		t3test.WithProject(t3test.Project{ID: bridgeProjectID, Title: "Repo", WorkspaceRoot: h.root})}, opts...)
	h.server = t3test.NewServer(t, opts...)
	if !noRuntime {
		require.NoError(t, h.server.WriteRuntime(h.home))
	}
	j, err := runs.OpenJournal(h.dir, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = j.Close() })
	h.journal = j
	return h
}

// start records the attempt's start event, two tasks, and starts the bridge after it.
func (h *bridgeHarness) start() {
	h.t.Helper()
	start := h.append(runs.Event{Type: runs.EventAttemptStart, Mode: "full",
		Tasks: []runs.TaskInfo{{N: 1, Title: "One"}, {N: 2, Title: "Two"}}, TaskTotal: 2, Stages: []string{runs.StageReview}})
	timing := t3BridgeTiming{tick: 5 * time.Millisecond, callTimeout: 5 * time.Second,
		backoffMin: 10 * time.Millisecond, backoffMax: 40 * time.Millisecond, retypeIdle: 60 * time.Second,
		retypeQuiet: 90 * time.Second, regenWait: 2 * time.Minute, openWait: 30 * time.Second, now: h.clock.Now,
		afterEvent: func(ev runs.Event) { h.applied.Store(int64(ev.Seq)) }, afterItem: h.recordItem,
		afterStep: func() { h.steps.Add(1) }}
	h.bridge = openT3Bridge(t3BridgeConfig{token: h.server.Token(), threadID: h.threadID, runID: bridgeRunID,
		runDir: h.dir, attempt: 1, fromSeq: start.Seq - 1, checkout: bridgeCheckout, executable: bridgeExecutable,
		log: h.log}, timing)
	h.t.Cleanup(func() { h.bridge.stop(0) }) // a bridge still running is ended at once
}

// append records ev in the run's journal.
func (h *bridgeHarness) append(ev runs.Event) runs.Event {
	h.t.Helper()
	stamped, err := h.journal.Append(ev)
	require.NoError(h.t, err)
	return stamped
}

// task records the task phase entering plan task n of 2 and returns the task event.
func (h *bridgeHarness) task(n int) runs.Event {
	h.t.Helper()
	if n == 1 {
		h.append(runs.Event{Type: runs.EventPhase, Phase: status.PhaseTask})
	}
	return h.append(runs.Event{Type: runs.EventTask, N: n, M: 2, Title: []string{"", "One", "Two"}[n]})
}

// waitApplied waits until the bridge applied journal event ev and took the step after it.
func (h *bridgeHarness) waitApplied(ev runs.Event) {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.applied.Load() >= int64(ev.Seq) }, bridgeWait, time.Millisecond,
		"event %d applied", ev.Seq)
	h.settle()
}

// finish records the attempt's finish event with st.
func (h *bridgeHarness) finish(st runs.Status) {
	h.t.Helper()
	h.append(runs.Event{Type: runs.EventFinish, Status: st, ExitCode: detachExitCode(st)})
}

// thread returns the thread as the server holds it.
func (h *bridgeHarness) thread() t3test.Thread {
	h.t.Helper()
	th, ok := h.server.Thread(h.threadID)
	require.True(h.t, ok)
	return th
}

// rename sets the thread's title, as a user renaming it does.
func (h *bridgeHarness) rename(title string) {
	th := h.thread()
	th.Title = title
	h.server.SetThread(th)
}

// waitTitle waits for the thread's title.
func (h *bridgeHarness) waitTitle(want string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return h.thread().Title == want }, bridgeWait, time.Millisecond,
		"title %q; dispatched %v", want, h.titles())
}

// waitDone waits for the bridge to end by itself.
func (h *bridgeHarness) waitDone() {
	h.t.Helper()
	select {
	case <-h.bridge.done:
	case <-time.After(bridgeWait):
		require.FailNow(h.t, "the bridge did not end", "dispatched %v", h.titles())
	}
}

// recordItem is the bridge's afterItem hook.
func (h *bridgeHarness) recordItem(item bridgeItem) {
	if item.err != nil {
		return
	}
	h.mu.Lock()
	h.handled = append(h.handled, item.event)
	h.mu.Unlock()
}

// mark returns how many terminal metadata events the bridge handled so far.
func (h *bridgeHarness) mark() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.handled)
}

// waitHandled waits until the bridge handled a terminal metadata event that match accepts, after the
// first from.
func (h *bridgeHarness) waitHandled(from int, what string, match func(t3.TerminalMetadataEvent) bool) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return slices.ContainsFunc(h.handled[min(from, len(h.handled)):], match)
	}, bridgeWait, time.Millisecond, "the bridge handled %s", what)
}

// isSnapshot, isRemove, and isUpsert match the metadata events of the bridge's terminal.
func isSnapshot(ev t3.TerminalMetadataEvent) bool { return ev.Type == t3.MetadataSnapshot }

func isRemove(ev t3.TerminalMetadataEvent) bool {
	return ev.Type == t3.MetadataRemove && ev.TerminalID == bridgeTerminalID
}

func isUpsert(busy bool) func(t3.TerminalMetadataEvent) bool {
	return func(ev t3.TerminalMetadataEvent) bool {
		return ev.Type == t3.MetadataUpsert && ev.Terminal.TerminalID == bridgeTerminalID &&
			ev.Terminal.Status == t3.TerminalRunning && ev.Terminal.HasRunningSubprocess == busy
	}
}

// settle waits until the bridge took three more steps, so it checked its rules on the current clock.
func (h *bridgeHarness) settle() {
	h.t.Helper()
	from := h.steps.Load()
	require.Eventually(h.t, func() bool { return h.steps.Load() >= from+3 }, bridgeWait, time.Millisecond)
}

// waitWrites waits until n terminal.write calls arrived.
func (h *bridgeHarness) waitWrites(n int) {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return len(h.server.Writes()) >= n }, bridgeWait, time.Millisecond,
		"%d terminal writes", n)
}

// titles returns the titles of every thread.metadata.update dispatched.
func (h *bridgeHarness) titles() []string {
	var titles []string
	for _, d := range h.server.Dispatches() {
		if d["type"] == "thread.metadata.update" {
			titles = append(titles, d["title"].(string))
		}
	}
	return titles
}

// assertMarkedUnread checks that the thread was marked unread by the last command dispatched: its
// visit rewound to just before its latest run completed, so T3 shows that run as unseen.
func (h *bridgeHarness) assertMarkedUnread() {
	h.t.Helper()
	commands := h.commands()
	require.NotEmpty(h.t, commands)
	assert.Equal(h.t, "thread.mark-unread", commands[len(commands)-1], "the mark-unread is the last command")
	th := h.thread()
	require.NotEmpty(h.t, th.LastVisitedAt, "the thread is marked unread; dispatched %v", commands)
	visited, err := time.Parse(time.RFC3339Nano, th.LastVisitedAt)
	require.NoError(h.t, err)
	completed, err := time.Parse(time.RFC3339Nano, th.LatestRunCompletedAt)
	require.NoError(h.t, err)
	assert.True(h.t, visited.Before(completed), "visited %s before the run completed at %s", visited, completed)
}

// commands returns the type of every command dispatched.
func (h *bridgeHarness) commands() []string {
	dispatches := h.server.Dispatches()
	types := make([]string, 0, len(dispatches))
	for _, d := range dispatches {
		types = append(types, d["type"].(string))
	}
	return types
}

func TestT3BridgeTitles(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
	h.start()

	h.task(1)
	h.waitTitle("[loopai: task 1/2] My thread")
	h.task(2)
	h.waitTitle("[loopai: task 2/2] My thread")
	h.finish(runs.StatusSucceeded)
	h.waitDone()

	assert.Equal(t, "[loopai: done] My thread", h.thread().Title, "the final state is left in place")
	assert.Equal(t, []string{"[loopai: task 1/2] My thread", "[loopai: task 2/2] My thread", "[loopai: done] My thread"},
		h.titles())
	for _, d := range h.server.Dispatches() {
		want := []string{"commandId", "threadId", "type"}
		if d["type"] == "thread.metadata.update" {
			want = []string{"commandId", "threadId", "title", "type"}
		}
		assert.Equal(t, want, slices.Sorted(maps.Keys(d)), "title-only updates and bare pin commands: %v", d)
	}
	assert.Empty(t, h.log.all(), "a working bridge writes nothing into the progress log")
}

func TestT3BridgeKeepsARename(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
	h.start()
	h.task(1)
	h.waitTitle("[loopai: task 1/2] My thread")

	h.rename("Renamed")
	h.task(2)
	h.waitTitle("[loopai: task 2/2] Renamed")

	h.rename("[loopai: task 2/2] Renamed again")
	h.finish(runs.StatusFailed)
	h.waitDone()
	assert.Equal(t, "[loopai: failed] Renamed again", h.thread().Title, "the prefix is replaced, not stacked")
}

func TestT3BridgeFinalStates(t *testing.T) {
	for _, tc := range []struct {
		status runs.Status
		want   string
	}{{runs.StatusSucceeded, "done"}, {runs.StatusFailed, "failed"}, {runs.StatusStopped, "stopped"}} {
		t.Run(string(tc.status), func(t *testing.T) {
			h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
			h.start()
			h.task(1)
			h.waitTitle("[loopai: task 1/2] My thread")
			h.settle()
			assert.NotContains(t, h.commands(), "thread.mark-unread", "nothing is marked before the finish")
			assert.Empty(t, h.thread().LastVisitedAt)

			h.finish(tc.status)
			h.waitDone()
			assert.Equal(t, "[loopai: "+tc.want+"] My thread", h.thread().Title)
			assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.metadata.update", "thread.unpin",
				"thread.mark-unread"}, h.commands(), "the mark-unread follows the final title and the unpin")
			h.assertMarkedUnread()
			assert.Empty(t, h.log.all())
		})
	}
}

func TestT3BridgeSendsOnlyChangedTitles(t *testing.T) {
	t.Run("a repeated state is not written again", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.append(runs.Event{Type: runs.EventTask, N: 1, M: 2, Title: "One"}) // a retried iteration of the same task
		h.waitApplied(h.append(runs.Event{Type: runs.EventTasks,
			Statuses: []runs.TaskStatus{{N: 1, Status: runs.ProgressActive}}}))
		assert.Equal(t, []string{"[loopai: task 1/2] My thread"}, h.titles())
		h.append(runs.Event{Type: runs.EventPhase, Phase: status.PhaseReview})
		h.waitTitle("[loopai: review] My thread")
		assert.Equal(t, []string{"[loopai: task 1/2] My thread", "[loopai: review] My thread"}, h.titles())
	})

	t.Run("a title already showing the state is not written", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "[loopai: task 1/2] My thread"}, false)
		h.start()
		h.waitApplied(h.task(1))
		assert.Empty(t, h.titles())
		h.task(2)
		h.waitTitle("[loopai: task 2/2] My thread")
		assert.Equal(t, []string{"[loopai: task 2/2] My thread"}, h.titles())
	})
}

func TestT3BridgePin(t *testing.T) {
	t.Run("an unpinned thread is pinned for the run and unpinned at the end", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		require.Eventually(t, func() bool { return h.thread().PinnedAt != "" }, bridgeWait, time.Millisecond)
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		assert.Empty(t, h.thread().PinnedAt)
		assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.metadata.update", "thread.unpin",
			"thread.mark-unread"}, h.commands())
	})

	t.Run("an already pinned thread is neither pinned nor unpinned", func(t *testing.T) {
		const pinned = "2026-10-01T00:00:00.000Z"
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread", PinnedAt: pinned}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		assert.Equal(t, pinned, h.thread().PinnedAt)
		assert.Equal(t, []string{"thread.metadata.update", "thread.metadata.update", "thread.mark-unread"}, h.commands())
	})

	t.Run("a thread T3 refuses to pin still gets its titles", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread",
			ArchivedAt: "2026-10-01T00:00:00.000Z"}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		assert.Equal(t, "[loopai: done] My thread", h.thread().Title)
		assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.metadata.update", "thread.mark-unread"},
			h.commands(), "no unpin for a pin that failed")
		assert.Empty(t, h.log.all())
	})
}

func TestT3BridgeMarkUnreadFailures(t *testing.T) {
	t.Run("a thread with no completed run is a refusal that ends the bridge quietly", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		th := h.thread()
		th.LatestRunCompletedAt = "" // a new agent turn is running in the thread
		h.server.SetThread(th)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		assert.Equal(t, "[loopai: done] My thread", h.thread().Title)
		assert.Empty(t, h.thread().PinnedAt)
		assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.metadata.update", "thread.unpin",
			"thread.mark-unread"}, h.commands(), "the refused mark-unread is not retried")
		assert.Empty(t, h.thread().LastVisitedAt)
		assert.Empty(t, h.log.all(), "a refused mark-unread does not disable the bridge")
	})

	t.Run("a mark-unread with an unknown outcome is retried after the reconnect", func(t *testing.T) {
		const pinned = "2026-10-01T00:00:00.000Z"
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread", PinnedAt: pinned}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		// the final title is already in place, so the next dispatch is the mark-unread
		h.rename("[loopai: done] My thread")
		handshakes := len(h.server.Handshakes())
		h.server.DieNext("orchestration.dispatchCommand", "lost")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		require.Equal(t, []string{"thread.metadata.update", "thread.mark-unread", "thread.mark-unread"}, h.commands())
		assert.Greater(t, len(h.server.Handshakes()), handshakes, "the bridge reconnected")
		dispatches := h.server.Dispatches()
		assert.Equal(t, dispatches[1]["commandId"], dispatches[2]["commandId"], "the retry reuses the command id")
		h.assertMarkedUnread()
		assert.Empty(t, h.log.all(), "an unknown outcome does not disable the bridge")
	})

	t.Run("an unpin with an unknown outcome is retried before the mark-unread", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		// the final title is already in place, so the next dispatch is the unpin
		h.rename("[loopai: done] My thread")
		h.server.DieNext("orchestration.dispatchCommand", "lost")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		require.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.unpin", "thread.unpin",
			"thread.mark-unread"}, h.commands())
		dispatches := h.server.Dispatches()
		assert.Equal(t, dispatches[2]["commandId"], dispatches[3]["commandId"], "the retry reuses the command id")
		assert.Empty(t, h.thread().PinnedAt)
		h.assertMarkedUnread()
		assert.Empty(t, h.log.all(), "an unknown outcome does not disable the bridge")
	})
}

func TestT3BridgeOpensTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		worktree string
	}{{name: "thread in its project root"}, {name: "thread in a worktree", worktree: "/wt/feature"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread", WorktreePath: tc.worktree}, false)
			h.start()
			h.waitWrites(1)
			h.settle()

			opens := h.server.Opens()
			require.Len(t, opens, 1, "the terminal is opened once")
			want := map[string]any{"threadId": bridgeThreadID, "terminalId": bridgeTerminalID, "cwd": h.root,
				"env": map[string]any{"T3CODE_PROJECT_ROOT": h.root}}
			if tc.worktree != "" {
				want["cwd"], want["worktreePath"] = tc.worktree, tc.worktree
				want["env"] = map[string]any{"T3CODE_PROJECT_ROOT": h.root, "T3CODE_WORKTREE_PATH": tc.worktree}
			}
			assert.Equal(t, want, opens[0], "the UI's launch context, no size and no provider instance")
			writes := h.server.Writes()
			require.Len(t, writes, 1)
			assert.Equal(t, map[string]any{"threadId": bridgeThreadID, "terminalId": bridgeTerminalID,
				"data": bridgeFollow}, writes[0])
		})
	}
}

func TestT3BridgeReusesTerminal(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false,
		t3test.WithTerminal(t3test.Terminal{ThreadID: bridgeThreadID, TerminalID: bridgeTerminalID, Cwd: "/r"}))
	h.start()
	h.waitHandled(0, "the snapshot", isSnapshot)
	h.settle()
	assert.Empty(t, h.server.Opens(), "a terminal the server has is reused")
	assert.Empty(t, h.server.Writes(), "an idle terminal is typed into only after a minute")

	h.clock.Advance(59 * time.Second)
	h.settle()
	assert.Empty(t, h.server.Writes())
	h.clock.Advance(time.Second)
	h.waitWrites(1)
	assert.Equal(t, bridgeFollow, h.server.Writes()[0]["data"])
}

func TestT3BridgeReopensTerminalAfterRestart(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread", WorktreePath: "/wt"}, false)
	h.start()
	h.waitWrites(1)
	h.settle()

	h.server.OpenNextStarting()
	require.NoError(t, h.server.Restart())
	require.Eventually(t, func() bool { return len(h.server.Opens()) == 2 }, bridgeWait, time.Millisecond)
	opens := h.server.Opens()
	assert.Equal(t, opens[0], opens[1], "reopened once with the identical launch context")

	// the terminal is starting: the follow command is retried, and refused, until it runs
	h.waitWrites(3)
	require.True(t, h.server.SetTerminalState(bridgeThreadID, bridgeTerminalID, "running", false))
	h.settle()
	typed := len(h.server.Writes())
	h.settle()
	assert.Len(t, h.server.Writes(), typed, "typed once it runs, then nothing more")
	assert.Equal(t, bridgeFollow, h.server.Writes()[typed-1]["data"])
	assert.Len(t, h.server.Opens(), 2, "at most one open per server start")

	t.Run("an idle terminal is typed into again once", func(t *testing.T) {
		// the follow command runs, then ends (the user pressed Ctrl+C)
		mark := h.mark()
		require.True(t, h.server.SetTerminalState(bridgeThreadID, bridgeTerminalID, "running", true))
		h.waitHandled(mark, "the busy upsert", isUpsert(true))
		h.clock.Advance(90 * time.Second) // the quiet period after typing has passed
		mark = h.mark()
		require.True(t, h.server.SetTerminalState(bridgeThreadID, bridgeTerminalID, "running", false))
		h.waitHandled(mark, "the idle upsert", isUpsert(false))

		h.clock.Advance(59 * time.Second)
		h.settle()
		assert.Len(t, h.server.Writes(), typed, "idle for less than a minute")
		h.clock.Advance(time.Second)
		h.waitWrites(typed + 1)
		h.clock.Advance(5 * time.Minute)
		h.settle()
		assert.Len(t, h.server.Writes(), typed+1, "re-typed once: the command was not seen running since")
	})
}

func TestT3BridgeReopensTerminalAfterRestartOfAnUnknownServer(t *testing.T) {
	// with LOOPAI_T3_URL and no runtime file the server's pid and start time are unknown, so a lost
	// connection is what tells the bridge the server may have restarted
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, true)
	t.Setenv(t3.EnvURL, h.server.URL())
	h.start()
	h.waitWrites(1)
	h.settle()

	mark := h.mark()
	require.NoError(t, h.server.Restart())
	h.waitHandled(mark, "the new server's snapshot", isSnapshot)
	require.Eventually(t, func() bool { return len(h.server.Opens()) == 2 }, bridgeWait, time.Millisecond,
		"the absent terminal is reopened")
	opens := h.server.Opens()
	assert.Equal(t, opens[0], opens[1], "reopened with the identical launch context")
	h.waitWrites(2)
	assert.Equal(t, bridgeFollow, h.server.Writes()[1]["data"])

	t.Run("a connection lost while the terminal lives reuses it", func(t *testing.T) {
		mark := h.mark()
		h.server.SendDefect("t3test: dropped")
		h.waitHandled(mark, "the next connection's snapshot", isSnapshot)
		h.settle()
		assert.Len(t, h.server.Opens(), 2, "a terminal the snapshot holds is not opened again")
		assert.Len(t, h.server.Writes(), 2)
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		assert.Empty(t, h.log.all())
	})
}

func TestT3BridgeSurvivesAWriteIntoADeadShell(t *testing.T) {
	t.Run("into the terminal it opened", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.server.BreakNextWrite()
		h.start()
		h.waitWrites(1)
		h.settle()

		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.finish(runs.StatusSucceeded)
		h.waitDone()
		assert.Equal(t, "[loopai: done] My thread", h.thread().Title)
		assert.Empty(t, h.thread().PinnedAt, "the pin the bridge took is released")
		assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.metadata.update", "thread.unpin",
			"thread.mark-unread"}, h.commands())
		assert.Len(t, h.server.Writes(), 1)
		assert.Empty(t, h.log.all(), "a terminal gone does not disable the bridge")
	})

	t.Run("on a re-type", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		h.waitWrites(1)
		h.settle()
		// the follow command runs, then ends, and the shell dies just as the bridge types again
		mark := h.mark()
		require.True(t, h.server.SetTerminalState(bridgeThreadID, bridgeTerminalID, "running", true))
		h.waitHandled(mark, "the busy upsert", isUpsert(true))
		h.clock.Advance(90 * time.Second)
		mark = h.mark()
		require.True(t, h.server.SetTerminalState(bridgeThreadID, bridgeTerminalID, "running", false))
		h.waitHandled(mark, "the idle upsert", isUpsert(false))
		h.server.BreakNextWrite()
		h.clock.Advance(60 * time.Second)
		h.waitWrites(2)
		h.clock.Advance(10 * time.Minute)
		h.settle()
		assert.Len(t, h.server.Writes(), 2, "a terminal gone is not typed into until T3 reports it again")

		// T3 reports it running again: the idle wait starts afresh
		mark = h.mark()
		require.True(t, h.server.SetTerminalState(bridgeThreadID, bridgeTerminalID, "running", false))
		h.waitHandled(mark, "the terminal reported again", isUpsert(false))
		h.clock.Advance(59 * time.Second)
		h.settle()
		assert.Len(t, h.server.Writes(), 2)
		h.clock.Advance(time.Second)
		h.waitWrites(3)

		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		h.finish(runs.StatusFailed)
		h.waitDone()
		assert.Equal(t, "[loopai: failed] My thread", h.thread().Title)
		assert.Empty(t, h.thread().PinnedAt, "the pin the bridge took is released")
		assert.Empty(t, h.log.all(), "a terminal gone does not disable the bridge")
	})
}

func TestT3BridgeNeverRetypesAClosedTerminal(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
	h.start()
	h.waitWrites(1)
	h.settle()

	mark := h.mark()
	h.server.Remove(bridgeThreadID, bridgeTerminalID)
	h.waitHandled(mark, "the remove", isRemove)
	h.clock.Advance(10 * time.Minute)
	h.settle()

	mark = h.mark()
	require.NoError(t, h.server.Restart())
	h.waitHandled(mark, "the new server's snapshot", isSnapshot)
	h.clock.Advance(10 * time.Minute)
	h.settle()
	assert.Len(t, h.server.Opens(), 1, "a terminal the user closed is not reopened, not even after a restart")
	assert.Len(t, h.server.Writes(), 1, "nor typed into")

	h.task(1)
	h.waitTitle("[loopai: task 1/2] My thread")
}

func TestT3BridgeWaitsForTitleRegeneration(t *testing.T) {
	t.Run("until the generated title arrives", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "New thread", TitleRegenerating: true}, false)
		h.start()
		h.waitApplied(h.task(1))
		assert.Empty(t, h.titles(), "the first prefix waits for the generated title")

		th := h.thread()
		th.Title, th.TitleRegenerating = "Generated", false
		h.server.SetThread(th)
		h.waitTitle("[loopai: task 1/2] Generated")
	})

	t.Run("for at most two minutes", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "New thread", TitleRegenerating: true}, false)
		h.start()
		h.waitApplied(h.task(1))
		h.clock.Advance(2*time.Minute - time.Second)
		h.settle()
		assert.Empty(t, h.titles())
		h.clock.Advance(time.Second)
		h.waitTitle("[loopai: task 1/2] New thread")
		assert.False(t, h.thread().TitleRegenerating, "the write cancels the generation")

		th := h.thread()
		th.TitleRegenerating = true // a later generation is simply overwritten
		h.server.SetThread(th)
		h.task(2)
		h.waitTitle("[loopai: task 2/2] New thread")
	})
}

func TestT3BridgeFinalTitleSkipsRegenerationWait(t *testing.T) {
	t.Run("for an attempt that finishes before any title write", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "New thread", TitleRegenerating: true}, false)
		h.start()
		h.waitWrites(1) // connected and pinned, with no state to show yet
		h.finish(runs.StatusFailed)
		h.waitDone()
		assert.Equal(t, "[loopai: failed] New thread", h.thread().Title, "the final title does not wait")
		assert.False(t, h.thread().TitleRegenerating, "the write cancels the generation")
		assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.unpin", "thread.mark-unread"},
			h.commands())
		h.assertMarkedUnread()
	})

	t.Run("for a finish arriving while a progress title waits", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "New thread", TitleRegenerating: true}, false)
		h.start()
		h.waitApplied(h.task(1))
		assert.Empty(t, h.titles(), "the progress title waits for the generated one")

		h.finish(runs.StatusFailed)
		h.waitDone()
		assert.Equal(t, []string{"[loopai: failed] New thread"}, h.titles(),
			"the waiting progress title is never written")
		assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.unpin", "thread.mark-unread"},
			h.commands())
		h.assertMarkedUnread()
	})
}

func TestT3BridgeThreadIDReachesEveryRequestUnchanged(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeNativeThreadID, Title: "Subagent"}, false)
	h.start()
	h.waitWrites(1)
	h.task(1)
	h.waitTitle("[loopai: task 1/2] Subagent")
	h.finish(runs.StatusSucceeded)
	h.waitDone()
	assert.Equal(t, "[loopai: done] Subagent", h.thread().Title)

	methods := map[string]bool{}
	for _, req := range h.server.Requests() {
		methods[req.Method] = true
		if req.Method == "subscribeTerminalMetadata" {
			continue
		}
		assert.Contains(t, string(req.Payload), `"threadId":"`+bridgeNativeThreadID+`"`, "%s", req.Method)
	}
	for _, method := range []string{"orchestration.getThreadProjection", "orchestration.dispatchCommand",
		"terminal.open", "terminal.write"} {
		assert.True(t, methods[method], "%s was sent", method)
	}
}

func TestT3BridgeRetriesUntilT3Runs(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, true)
	h.start()
	h.task(1)
	from := h.steps.Load()
	require.Eventually(t, func() bool { return h.steps.Load() >= from+5 }, bridgeWait, time.Millisecond,
		"the bridge keeps retrying while T3 is not running")
	assert.Empty(t, h.server.Handshakes())

	require.NoError(t, h.server.WriteRuntime(h.home))
	h.waitTitle("[loopai: task 1/2] My thread")
	assert.Empty(t, h.log.all(), "a server that is not running yet does not disable the bridge")
}

func TestT3BridgeDisables(t *testing.T) {
	t.Run("a rejected token on reconnect", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")

		h.server.RejectNextConnection()
		require.NoError(t, h.server.Restart())
		h.waitDone()
		lines := h.log.all()
		require.Len(t, lines, 1, "exactly one progress-log line")
		assert.Contains(t, lines[0], "T3 thread updates stopped, the run continues without them")
		assert.Contains(t, lines[0], "401 auth_invalid (invalid_credential)")
		assert.NotContains(t, lines[0], h.server.Token())

		handshakes := len(h.server.Handshakes())
		h.task(2)
		time.Sleep(100 * time.Millisecond)
		assert.Len(t, h.server.Handshakes(), handshakes, "a disabled bridge never reconnects")
		assert.Equal(t, "[loopai: task 1/2] My thread", h.thread().Title)
	})

	t.Run("a server that does not speak protocol 2", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false,
			t3test.WithProtocolVersion("3"))
		h.start()
		h.waitDone()
		lines := h.log.all()
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], "426")
	})

	t.Run("a thread T3 does not know", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.threadID = "missing-thread"
		h.start()
		h.waitDone()
		lines := h.log.all()
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], "OrchestrationV2GetThreadProjectionError")
	})
}

// TestT3BridgeTerminalErrorStopsOnlyTheTerminal makes T3 refuse the bridge's terminal with a typed
// error, on the open and on the write of the follow command: the terminal half stops with one
// progress-log line, while the titles, the pin, and the unpin at the end carry on.
func TestT3BridgeTerminalErrorStopsOnlyTheTerminal(t *testing.T) {
	tests := []struct {
		name, method, tag string
		writes            int
	}{
		{name: "the open", method: "terminal.open", tag: "TerminalCwdNotFoundError"},
		{name: "the write", method: "terminal.write", tag: "TerminalUnexpectedError", writes: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
			h.server.FailNext(tc.method, tc.tag)
			h.start()
			require.Eventually(t, func() bool { return len(h.log.all()) == 1 }, bridgeWait, time.Millisecond)
			lines := h.log.all()
			assert.Contains(t, lines[0], "T3 terminal updates stopped, the thread's title and the run continue")
			assert.Contains(t, lines[0], tc.tag)

			h.task(1)
			h.waitTitle("[loopai: task 1/2] My thread")
			h.clock.Advance(10 * time.Minute) // past every re-type wait
			h.settle()
			h.finish(runs.StatusSucceeded)
			h.waitDone()
			assert.Equal(t, "[loopai: done] My thread", h.thread().Title)
			assert.Empty(t, h.thread().PinnedAt)
			assert.Equal(t, []string{"thread.pin", "thread.metadata.update", "thread.metadata.update", "thread.unpin",
				"thread.mark-unread"}, h.commands())
			assert.Len(t, h.server.Opens(), 1, "the terminal is never opened again")
			assert.Len(t, h.server.Writes(), tc.writes, "the follow command is never typed again")
			assert.Len(t, h.log.all(), 1, "one progress-log line")
		})
	}
}

// TestT3BridgeDisableLeavesTheThread makes T3 refuse a title update with a typed error, which
// disables the bridge: before it stops, it removes its progress prefix from a run still going, or
// writes the final state of a finished one, and unpins the thread it pinned, so T3 can settle it; a
// finished thread is then marked unread.
func TestT3BridgeDisableLeavesTheThread(t *testing.T) {
	t.Run("a run still going", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")
		require.NotEmpty(t, h.thread().PinnedAt)

		h.server.FailNext("orchestration.dispatchCommand", "OrchestrationV2DispatchCommandError")
		h.task(2)
		h.waitDone()
		assert.Equal(t, "My thread", h.thread().Title, "no progress state stays behind")
		assert.Empty(t, h.thread().PinnedAt, "the thread the bridge pinned is unpinned")
		assert.NotContains(t, h.commands(), "thread.mark-unread", "a run still going is not marked unread")
		assert.Empty(t, h.thread().LastVisitedAt)
		lines := h.log.all()
		require.Len(t, lines, 1, "exactly one progress-log line")
		assert.Contains(t, lines[0], "T3 thread updates stopped, the run continues without them")
		assert.Contains(t, lines[0], "OrchestrationV2DispatchCommandError")
	})

	t.Run("a finished run", func(t *testing.T) {
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")

		h.server.FailNext("orchestration.dispatchCommand", "OrchestrationV2DispatchCommandError")
		h.finish(runs.StatusFailed)
		h.waitDone()
		assert.Equal(t, "[loopai: failed] My thread", h.thread().Title, "the final state is written once more")
		assert.Empty(t, h.thread().PinnedAt)
		commands := h.commands()
		assert.Equal(t, "thread.mark-unread", commands[len(commands)-1], "the finished thread is marked unread last")
		h.assertMarkedUnread()
		assert.Len(t, h.log.all(), 1)
	})

	t.Run("a thread the bridge did not pin stays pinned", func(t *testing.T) {
		const pinned = "2026-10-01T00:00:00.000Z"
		h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread", PinnedAt: pinned}, false)
		h.start()
		h.task(1)
		h.waitTitle("[loopai: task 1/2] My thread")

		h.server.FailNext("orchestration.dispatchCommand", "OrchestrationV2DispatchCommandError")
		h.task(2)
		h.waitDone()
		assert.Equal(t, "My thread", h.thread().Title)
		assert.Equal(t, pinned, h.thread().PinnedAt)
		assert.NotContains(t, h.commands(), "thread.unpin")
	})
}

func TestT3BridgeStopIsBounded(t *testing.T) {
	h := newBridgeHarness(t, t3test.Thread{ID: bridgeThreadID, Title: "My thread"}, false)
	h.start()
	h.task(1)
	h.waitTitle("[loopai: task 1/2] My thread")
	h.server.Stall("orchestration.getThreadProjection")
	h.finish(runs.StatusSucceeded)

	started := time.Now()
	h.bridge.stop(200 * time.Millisecond)
	assert.Less(t, time.Since(started), 5*time.Second, "a server that never answers does not hold the run's exit")
	assert.Equal(t, "[loopai: task 1/2] My thread", h.thread().Title)
}

func TestT3BridgeFatal(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		fatal bool
	}{
		{name: "rejected token", err: &t3.HandshakeError{StatusCode: http.StatusUnauthorized, Code: "auth_invalid"}, fatal: true},
		{name: "forbidden", err: &t3.HandshakeError{StatusCode: http.StatusForbidden}, fatal: true},
		{name: "protocol 426", err: fmt.Errorf("connect: %w", &t3.HandshakeError{StatusCode: http.StatusUpgradeRequired}),
			fatal: true},
		{name: "undecodable reply", err: fmt.Errorf("%w: bad", t3.ErrProtocolUnsupported), fatal: true},
		{name: "oversized reply", err: &t3.TransportError{Method: "m", Reason: "connection lost",
			Err: fmt.Errorf("%w: %w", t3.ErrClosed, t3.ErrReplyTooLarge)}, fatal: true},
		{name: "typed server error", err: &t3.RemoteError{Method: "m", Tag: "EnvironmentAuthorizationError"}, fatal: true},
		{name: "project not listed", err: fmt.Errorf("%w: p1", errT3ProjectNotListed), fatal: true},
		{name: "server error", err: &t3.HandshakeError{StatusCode: http.StatusBadGateway}},
		{name: "request timeout", err: &t3.HandshakeError{StatusCode: http.StatusRequestTimeout}},
		{name: "rate limited", err: &t3.HandshakeError{StatusCode: http.StatusTooManyRequests}},
		{name: "not running", err: fmt.Errorf("%w: missing", t3.ErrNotRunning)},
		{name: "lost connection", err: &t3.TransportError{Method: "m", Reason: "connection lost", Err: t3.ErrClosed}},
		{name: "timeout", err: fmt.Errorf("t3 m: %w", context.DeadlineExceeded)},
		{name: "refused", err: errors.New("t3: connect ws://127.0.0.1:1/ws: connection refused")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.fatal, t3BridgeFatal(tc.err))
		})
	}
}

func TestT3TitleState(t *testing.T) {
	tests := []struct {
		name string
		snap runs.Snapshot
		want string
	}{
		{name: "nothing yet", want: ""},
		{name: "task phase before a task", snap: runs.Snapshot{Phase: status.PhaseTask}, want: ""},
		{name: "task", snap: runs.Snapshot{Phase: status.PhaseTask, Task: 3, TaskTotal: 7}, want: "task 3/7"},
		{name: "task before the phase event", snap: runs.Snapshot{Task: 1, TaskTotal: 2}, want: "task 1/2"},
		{name: "section past the last task", snap: runs.Snapshot{Phase: status.PhaseTask, Task: 3, TaskTotal: 2},
			want: "task 2/2"},
		{name: "task count unknown", snap: runs.Snapshot{Phase: status.PhaseTask, Task: 2}, want: "task 2"},
		{name: "review", snap: runs.Snapshot{Phase: status.PhaseReview, Task: 2, TaskTotal: 2}, want: "review"},
		{name: "external review", snap: runs.Snapshot{Phase: status.PhaseExternalReview}, want: "external review"},
		{name: "external evaluation", snap: runs.Snapshot{Phase: status.PhaseExternalEval}, want: "external review"},
		{name: "finalize", snap: runs.Snapshot{Phase: status.PhaseFinalize}, want: "finalize"},
		{name: "report", snap: runs.Snapshot{Phase: status.PhaseReport}, want: "report"},
		{name: "limit wait", snap: runs.Snapshot{Phase: status.PhaseLimitWait, Task: 1, TaskTotal: 2}, want: "limit wait"},
		{name: "plan phase", snap: runs.Snapshot{Phase: status.PhasePlan}, want: ""},
		{name: "succeeded", snap: runs.Snapshot{Phase: status.PhaseReport,
			Finish: &runs.Finish{Status: runs.StatusSucceeded}}, want: "done"},
		{name: "failed", snap: runs.Snapshot{Finish: &runs.Finish{Status: runs.StatusFailed}}, want: "failed"},
		{name: "stopped", snap: runs.Snapshot{Finish: &runs.Finish{Status: runs.StatusStopped}}, want: "stopped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, t3TitleState(&tc.snap))
		})
	}
}

func TestT3PrefixedTitle(t *testing.T) {
	tests := []struct{ title, want string }{
		{"My thread", "[loopai: done] My thread"},
		{"[loopai: task 1/2] My thread", "[loopai: done] My thread"},
		{"[loopai: task 1/2]My thread", "[loopai: done] My thread"},
		{"[loopai: task 1/2] [loopai: review] My thread", "[loopai: done] My thread"},
		{"[loopai: task 1/2]", "[loopai: done]"},
		{"", "[loopai: done]"},
		{"My [loopai: x] thread", "[loopai: done] My [loopai: x] thread"},
		{"[loopai] thread", "[loopai: done] [loopai] thread"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, t3PrefixedTitle("done", tc.title), "title %q", tc.title)
	}
}

func TestT3BaseTitle(t *testing.T) {
	tests := []struct{ title, want string }{
		{"My thread", "My thread"},
		{"[loopai: task 1/2] My thread", "My thread"},
		{"[loopai: task 1/2] [loopai: review] My thread", "My thread"},
		{"[loopai: task 1/2]", ""},
		{"My [loopai: x] thread", "My [loopai: x] thread"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, t3BaseTitle(tc.title), "title %q", tc.title)
	}
}

func TestT3BridgeUnreachable(t *testing.T) {
	assert.True(t, t3BridgeUnreachable(&t3.HandshakeError{StatusCode: http.StatusUnauthorized}))
	assert.True(t, t3BridgeUnreachable(fmt.Errorf("%w: bad", t3.ErrProtocolUnsupported)))
	assert.False(t, t3BridgeUnreachable(&t3.RemoteError{Method: "m", Tag: "OrchestrationV2DispatchCommandError"}))
	assert.False(t, t3BridgeUnreachable(fmt.Errorf("%w: p1", errT3ProjectNotListed)))
}

func TestT3FollowCommand(t *testing.T) {
	assert.Equal(t, bridgeFollow, t3FollowCommand(bridgeCheckout, bridgeExecutable, bridgeRunID))
	assert.Equal(t, " cd '/repo' && '/bin/loopai' --attach r-1\r", t3FollowCommand("/repo", "/bin/loopai", "r-1"))
	assert.False(t, strings.Contains(bridgeFollow, "exec") || strings.Contains(bridgeFollow, "exit"),
		"the command is never exec'd and never ends the shell")
}

func TestT3ServerStart(t *testing.T) {
	a := t3ServerStart(t3.Runtime{PID: 40000, StartedAt: "2026-10-22T12:00:00Z"})
	assert.Equal(t, "40000@2026-10-22T12:00:00Z", a)
	assert.NotEqual(t, a, t3ServerStart(t3.Runtime{PID: 40001, StartedAt: "2026-10-22T12:00:00Z"}))
	assert.NotEqual(t, a, t3ServerStart(t3.Runtime{PID: 40000, StartedAt: "2026-10-22T12:01:00Z"}))
	assert.Equal(t, "40000@", t3ServerStart(t3.Runtime{PID: 40000}), "a pid alone identifies a start")
	assert.Equal(t, "0@2026-10-22T12:00:00Z", t3ServerStart(t3.Runtime{StartedAt: "2026-10-22T12:00:00Z"}))
	assert.Empty(t, t3ServerStart(t3.Runtime{Origin: "http://127.0.0.1:1"}), "LOOPAI_T3_URL without a runtime file")

	b := &t3Bridge{}
	known := t3.Runtime{PID: 40000, StartedAt: "2026-10-22T12:00:00Z"}
	assert.Equal(t, a, b.serverStart(known))
	assert.Equal(t, a, b.serverStart(known), "a known start is the same on every connection")
	unknown := t3.Runtime{Origin: "http://127.0.0.1:1"}
	first, second := b.serverStart(unknown), b.serverStart(unknown)
	assert.NotEqual(t, first, second, "an unknown start counts each connection as a new one")
	assert.NotEqual(t, a, first)
}
