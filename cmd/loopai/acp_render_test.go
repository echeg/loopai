//go:build !windows

// The rendering tests follow runs whose locks they hold and share the scripted sessions of
// acp_provider_test.go, so like that file they build on Unix only.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

// acpSentUpdates records what a renderer sends, one line per update.
type acpSentUpdates struct{ lines []string }

func (u *acpSentUpdates) Plan(entries []acp.PlanEntry) error {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Content+"="+e.Status+"/"+e.Priority)
	}
	u.lines = append(u.lines, "plan "+strings.Join(parts, "; "))
	return nil
}

func (u *acpSentUpdates) ToolCall(id, title string) error {
	u.lines = append(u.lines, "open "+id[strings.LastIndex(id, "-")+1:]+" "+title)
	return nil
}

func (u *acpSentUpdates) ToolCallUpdate(id, st, title string) error {
	u.lines = append(u.lines, "update "+id[strings.LastIndex(id, "-")+1:]+" "+st+"|"+title)
	return nil
}

func (u *acpSentUpdates) AgentMessageChunk(text string) error {
	u.lines = append(u.lines, "message "+text)
	return nil
}

// take returns the lines recorded since the last call.
func (u *acpSentUpdates) take() []string {
	lines := u.lines
	u.lines = nil
	return lines
}

// acpRenderEpoch is the time the renderer tests' runs start at.
var acpRenderEpoch = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// acpRenderEvents are the journal events of a run of two tasks entering its first task.
func acpRenderEvents() []runs.Event {
	return []runs.Event{
		{Seq: 1, Time: acpRenderEpoch, Attempt: 1, Type: runs.EventAttemptStart, TaskTotal: 2,
			Tasks:  []runs.TaskInfo{{N: 1, Title: "Add the parser"}, {N: 2, Title: "Wire the CLI"}},
			Stages: []string{runs.StageReview, runs.StageFinalize}},
		{Seq: 2, Time: acpRenderEpoch, Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseTask},
		{Seq: 3, Time: acpRenderEpoch, Attempt: 1, Type: runs.EventTask, N: 1, M: 2, Title: "Add the parser"},
	}
}

func TestACPRendererSendsOnlyTransitionsAndTheLivenessTick(t *testing.T) {
	now := acpRenderEpoch
	sent := &acpSentUpdates{}
	r := newACPRenderer(sent, "run-1", func() time.Time { return now })

	r.start(acpRenderEvents())
	assert.Equal(t, []string{
		"plan Task 1: Add the parser=in_progress/medium; Task 2: Wire the CLI=pending/medium; " +
			"Review=pending/medium; Finalize=pending/medium",
		"open 1 Task 1/2: Add the parser",
	}, sent.take(), "the plan and the current phase are shown at once")

	at := func(minutes int) time.Time { return acpRenderEpoch.Add(time.Duration(minutes) * time.Minute) }
	for _, m := range []int{1, 19} {
		now = at(m)
		r.tick()
	}
	r.apply(runs.Event{Seq: 4, Time: at(19), Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseTask})
	r.apply(runs.Event{Seq: 5, Time: at(19), Attempt: 1, Type: runs.EventTasks,
		Statuses: []runs.TaskStatus{{N: 1, Status: runs.ProgressActive}, {N: 2, Status: runs.ProgressPending}}})
	assert.Empty(t, sent.take(), "neither the ticks before 20 minutes nor events that change nothing send updates")

	now = at(20)
	r.tick()
	assert.Equal(t, []string{"update 1 |Task 1/2: Add the parser (20m elapsed)"}, sent.take())
	now = at(39)
	r.tick()
	assert.Empty(t, sent.take(), "the next liveness retitle is due 20 minutes after the last update")
	now = at(80)
	r.tick()
	assert.Equal(t, []string{"update 1 |Task 1/2: Add the parser (1h20m elapsed)"}, sent.take())

	now = at(81)
	r.apply(runs.Event{Seq: 6, Time: at(81), Attempt: 1, Type: runs.EventTasks,
		Statuses: []runs.TaskStatus{{N: 1, Status: runs.ProgressDone}, {N: 2, Status: runs.ProgressPending}}})
	r.apply(runs.Event{Seq: 7, Time: at(81), Attempt: 1, Type: runs.EventTask, N: 2, M: 2, Title: "Wire the CLI"})
	assert.Equal(t, []string{
		"plan Task 1: Add the parser=completed/medium; Task 2: Wire the CLI=pending/medium; " +
			"Review=pending/medium; Finalize=pending/medium",
		"plan Task 1: Add the parser=completed/medium; Task 2: Wire the CLI=in_progress/medium; " +
			"Review=pending/medium; Finalize=pending/medium",
		"update 1 |Task 2/2: Wire the CLI",
	}, sent.take(), "a new task section retitles the phase's tool call")

	now = at(90)
	r.apply(runs.Event{Seq: 8, Time: at(90), Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseLimitWait, WaitSeconds: 600})
	r.apply(runs.Event{Seq: 9, Time: at(100), Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseTask})
	assert.Equal(t, []string{
		"update 1 |Task 2/2: Wire the CLI: waiting for the usage limit",
		"update 1 |Task 2/2: Wire the CLI",
	}, sent.take(), "a limit wait retitles the tool call it interrupts")

	now = at(110)
	r.apply(runs.Event{Seq: 10, Time: at(110), Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseReview})
	r.apply(runs.Event{Seq: 11, Time: at(110), Attempt: 1, Type: runs.EventReview, Kind: runs.ReviewInternal,
		Iteration: 1, Label: "claude review iteration 1"})
	assert.Equal(t, []string{
		"plan Task 1: Add the parser=completed/medium; Task 2: Wire the CLI=completed/medium; " +
			"Review=in_progress/medium; Finalize=pending/medium",
		"update 1 completed|",
		"open 2 Review",
		"update 2 |Review: claude review iteration 1",
	}, sent.take(), "a phase change closes the tool call and opens the next one")

	now = at(130)
	r.tick()
	assert.Equal(t, []string{"update 2 |Review: claude review iteration 1 (20m elapsed)"}, sent.take())

	r.apply(runs.Event{Seq: 12, Time: at(131), Attempt: 1, Type: runs.EventFinish, Status: runs.StatusSucceeded})
	now = at(200)
	r.tick()
	r.finish(acp.ToolCompleted, "loopai run run-1 succeeded.")
	assert.Equal(t, []string{
		"plan Task 1: Add the parser=completed/medium; Task 2: Wire the CLI=completed/medium; " +
			"Review=completed/medium; Finalize=completed/medium",
		"update 2 completed|",
		"message loopai run run-1 succeeded.",
	}, sent.take(), "the finish closes the tool call before the final message, and no tick follows it")
}

func TestACPRendererStart(t *testing.T) {
	t.Run("an empty journal shows the run starting", func(t *testing.T) {
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", time.Now)
		r.start(nil)
		assert.Equal(t, []string{"open 1 loopai run run-1: starting"}, sent.take())
		r.apply(acpRenderEvents()[0])
		r.apply(acpRenderEvents()[1])
		assert.Equal(t, []string{
			"plan Task 1: Add the parser=pending/medium; Task 2: Wire the CLI=pending/medium; " +
				"Review=pending/medium; Finalize=pending/medium",
			"update 1 |Tasks",
		}, sent.take(), "the starting tool call becomes the first phase's")
	})
	t.Run("a finished attempt opens no tool call", func(t *testing.T) {
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", time.Now)
		events := append(acpRenderEvents(), runs.Event{Seq: 4, Attempt: 1, Type: runs.EventFinish, Status: runs.StatusFailed})
		r.start(events)
		r.finish(acp.ToolFailed, "loopai run run-1 failed.")
		lines := sent.take()
		require.Len(t, lines, 2)
		assert.True(t, strings.HasPrefix(lines[0], "plan "))
		assert.Equal(t, "message loopai run run-1 failed.", lines[1])
	})
	t.Run("an external review's evaluation keeps its tool call", func(t *testing.T) {
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", time.Now)
		r.start([]runs.Event{{Seq: 1, Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseExternalReview}})
		r.apply(runs.Event{Seq: 2, Attempt: 1, Type: runs.EventReview, Kind: runs.ReviewExternal, Label: "codex iteration 1"})
		r.apply(runs.Event{Seq: 3, Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseExternalEval})
		r.apply(runs.Event{Seq: 4, Attempt: 1, Type: runs.EventReview, Kind: runs.ReviewEvaluation, Label: "claude evaluates"})
		r.apply(runs.Event{Seq: 5, Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseExternalReview})
		assert.Equal(t, []string{
			"open 1 External review",
			"update 1 |External review: codex iteration 1",
			"update 1 |External review: claude evaluates",
		}, sent.take())
	})
	t.Run("an elapsed time counts from the step's journal event", func(t *testing.T) {
		now := acpRenderEpoch.Add(45 * time.Minute)
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", func() time.Time { return now })
		r.start(acpRenderEvents())
		sent.take()
		now = now.Add(acpLivenessInterval)
		r.tick()
		assert.Equal(t, []string{"update 1 |Task 1/2: Add the parser (1h05m elapsed)"}, sent.take())
	})
}

// TestACPRendererEndings covers how a renderer leaves the thread when its prompt ends without a
// finish event to show: a canceled prompt closes its open tool call as completed, and an attempt that
// ended without a finish event shows no plan entry in progress, while one that recorded its finish
// keeps the entries the finish left.
func TestACPRendererEndings(t *testing.T) {
	t.Run("abandon closes the open tool call", func(t *testing.T) {
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", time.Now)
		r.start(acpRenderEvents())
		sent.take()
		r.abandon()
		assert.Equal(t, []string{"update 1 completed|"}, sent.take())
		r.abandon()
		assert.Empty(t, sent.take(), "no tool call is left to close")
	})
	t.Run("an attempt without a finish event", func(t *testing.T) {
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", time.Now)
		r.start(append(acpRenderEvents(), runs.Event{Seq: 4, Attempt: 1, Type: runs.EventPhase, Phase: status.PhaseReview}))
		assert.Contains(t, sent.take()[0], "Review=in_progress/medium")
		r.endedWithoutFinish()
		r.finish(acp.ToolFailed, "loopai run run-1 crashed.")
		assert.Equal(t, []string{
			"plan Task 1: Add the parser=completed/medium; Task 2: Wire the CLI=completed/medium; " +
				"Review=pending/medium; Finalize=pending/medium",
			"update 1 failed|",
			"message loopai run run-1 crashed.",
		}, sent.take(), "the review the run crashed in is pending again")
	})
	t.Run("an attempt with a finish event", func(t *testing.T) {
		sent := &acpSentUpdates{}
		r := newACPRenderer(sent, "run-1", time.Now)
		r.start(append(acpRenderEvents(), runs.Event{Seq: 4, Attempt: 1, Type: runs.EventFinish, Status: runs.StatusFailed}))
		sent.take()
		r.endedWithoutFinish()
		r.finish(acp.ToolFailed, "loopai run run-1 failed.")
		assert.Equal(t, []string{"message loopai run run-1 failed."}, sent.take(), "the plan the finish left is kept")
	})
}

func TestACPRendererTitles(t *testing.T) {
	tests := []struct {
		name   string
		events []runs.Event
		want   string
	}{
		{"task without a total", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseTask},
			{Type: runs.EventTask, N: 3, Title: "Docs"}}, "Task 3: Docs"},
		{"task past the last", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseTask},
			{Type: runs.EventTask, N: 4, M: 3}}, "Task 3/3"},
		{"external review section", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseExternalReview},
			{Type: runs.EventReview, Kind: runs.ReviewExternal, Label: "codex iteration 2"}}, "External review: codex iteration 2"},
		{"evaluation section", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseExternalEval},
			{Type: runs.EventReview, Kind: runs.ReviewEvaluation, Label: "claude evaluates codex"}},
			"External review: claude evaluates codex"},
		{"an evaluation retitles the external review", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseExternalReview},
			{Type: runs.EventReview, Kind: runs.ReviewExternal, Label: "codex iteration 1"},
			{Type: runs.EventPhase, Phase: status.PhaseExternalEval}}, "External review: codex iteration 1"},
		{"another phase's section is not shown", []runs.Event{{Type: runs.EventReview, Kind: runs.ReviewInternal,
			Label: "claude review iteration 4"}, {Type: runs.EventPhase, Phase: status.PhaseExternalReview}}, "External review"},
		{"finalize", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseFinalize}}, "Finalize"},
		{"report", []runs.Event{{Type: runs.EventPhase, Phase: status.PhaseReport}}, "Report"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newACPRenderer(&acpSentUpdates{}, "run-1", time.Now)
			for i, ev := range tc.events {
				ev.Seq, ev.Attempt = i+1, 1
				r.fold(ev)
			}
			assert.Equal(t, tc.want, r.stepTitle)
		})
	}
}

func TestACPElapsed(t *testing.T) {
	assert.Equal(t, "0m", acpElapsed(59*time.Second))
	assert.Equal(t, "20m", acpElapsed(20*time.Minute+30*time.Second))
	assert.Equal(t, "1h00m", acpElapsed(time.Hour))
	assert.Equal(t, "26h07m", acpElapsed(26*time.Hour+7*time.Minute))
}

// acpRenderTurn reads the next n messages, which must all be session/update notifications, and
// returns their updates.
func acpRenderTurn(t *testing.T, w *acpWire, n int) []map[string]any {
	t.Helper()
	updates := make([]map[string]any, 0, n)
	for range n {
		m := w.next()
		require.Equal(t, "session/update", m["method"], "got %v", m)
		params, _ := m["params"].(map[string]any)
		u, _ := params["update"].(map[string]any)
		updates = append(updates, u)
	}
	return updates
}

// TestACPAttachSendsTransitionsAndTheFinalMessage follows a live run over the wire: the plan entries
// carry a priority and the tool calls a title and kind other, only the run's transitions send updates,
// and the final message comes after the closed tool call and before the end_turn answer.
func TestACPAttachSendsTransitionsAndTheFinalMessage(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	events := acpRenderEvents()
	for i := range events {
		events[i].Seq = 0 // the journal stamps its own
	}
	appendRunsTestEvents(t, run.dir, 1, events...)
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	s.send(acpPrompt(3, sid, acp.ContinuationText))
	first := acpRenderTurn(t, s.acpWire, 2)
	assert.Equal(t, "plan", first[0]["sessionUpdate"])
	entries, _ := first[0]["entries"].([]any)
	require.Len(t, entries, 4)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		assert.Equal(t, "medium", entry["priority"], "every plan entry carries a priority: %v", entry)
		assert.NotEmpty(t, entry["content"])
	}
	assert.Equal(t, map[string]any{"sessionUpdate": "tool_call", "toolCallId": first[1]["toolCallId"],
		"title": "Task 1/2: Add the parser", "kind": "other", "status": "in_progress"}, first[1])

	appendRunsTestEvents(t, run.dir, 1, runs.Event{Type: runs.EventPhase, Phase: status.PhaseReview})
	next := acpRenderTurn(t, s.acpWire, 3)
	assert.Equal(t, "plan", next[0]["sessionUpdate"])
	assert.Equal(t, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": first[1]["toolCallId"],
		"status": "completed"}, next[1])
	assert.Equal(t, "tool_call", next[2]["sessionUpdate"])
	assert.Equal(t, "Review", next[2]["title"])
	assert.NotEqual(t, first[1]["toolCallId"], next[2]["toolCallId"], "tool call ids are unique")

	require.NoError(t, os.WriteFile(filepath.Join(run.dir, runs.ReportFile), []byte("# Report\n"), 0o600))
	run.finish(runs.Outcome{Status: runs.StatusSucceeded, Branch: "feature", ReportSummary: "Added the parser.",
		Risk: "low"})
	last := acpRenderTurn(t, s.acpWire, 2)
	assert.Equal(t, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": next[2]["toolCallId"],
		"status": "completed"}, last[0])
	assert.Equal(t, "agent_message_chunk", last[1]["sessionUpdate"])
	content, _ := last[1]["content"].(map[string]any)
	assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.\n\nAdded the parser.\n\nRisk: low\nBranch: feature\n"+
		"Full report: "+filepath.Join(run.dir, runs.ReportFile), content["text"])
	assert.Equal(t, map[string]any{"stopReason": "end_turn"}, s.result(3), "the end_turn answer follows the final message")
}

// TestACPCrashedRunEndsTheTurn ends a followed run without an outcome, as a killed run process does:
// the turn ends with end_turn and a final message naming the crash, and the tool call fails.
func TestACPCrashedRunEndsTheTurn(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	s.send(acpPrompt(3, sid, "status"))
	opened := acpRenderTurn(t, s.acpWire, 1)
	assert.Equal(t, "loopai run "+acpRouteRunID+": starting", opened[0]["title"])
	run.release()
	closed := acpRenderTurn(t, s.acpWire, 2)
	assert.Equal(t, "failed", closed[0]["status"])
	content, _ := closed[1]["content"].(map[string]any)
	assert.Equal(t, "loopai run "+acpRouteRunID+" crashed: the run process ended without recording an outcome.\n\n"+
		"Branch: feature\n\nThe worktree is kept; type `resume` in this thread to continue the run.", content["text"])
	assert.Equal(t, map[string]any{"stopReason": "end_turn"}, s.result(3))
}

// TestACPCrashedRunWithAPlanShowsNoEntryInProgress ends a followed run in its first task without a
// finish event: the final plan update shows the task pending again before the failed tool call and the
// final message.
func TestACPCrashedRunWithAPlanShowsNoEntryInProgress(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	events := acpRenderEvents()
	for i := range events {
		events[i].Seq = 0 // the journal stamps its own
	}
	appendRunsTestEvents(t, run.dir, 1, events...)
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	s.send(acpPrompt(3, sid, "status"))
	opened := acpRenderTurn(t, s.acpWire, 2)
	assert.Equal(t, "in_progress", acpPlanStatuses(opened[0])[0], "the first task is in progress")
	run.release()
	closed := acpRenderTurn(t, s.acpWire, 3)
	assert.Equal(t, "plan", closed[0]["sessionUpdate"])
	assert.Equal(t, []string{"pending", "pending", "pending", "pending"}, acpPlanStatuses(closed[0]))
	assert.Equal(t, "failed", closed[1]["status"])
	assert.Equal(t, "agent_message_chunk", closed[2]["sessionUpdate"])
	assert.Equal(t, map[string]any{"stopReason": "end_turn"}, s.result(3))
}

// TestACPCanceledFollowClosesItsToolCall cancels a prompt following a live run: the open tool call is
// closed as completed before the cancelled answer, so no card keeps spinning in the thread.
func TestACPCanceledFollowClosesItsToolCall(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	s.send(acpPrompt(3, sid, acp.ContinuationText))
	opened := acpRenderTurn(t, s.acpWire, 1)
	require.Equal(t, "tool_call", opened[0]["sessionUpdate"])
	s.send(acpCancel(sid))
	closed := acpRenderTurn(t, s.acpWire, 1)
	assert.Equal(t, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": opened[0]["toolCallId"],
		"status": "completed"}, closed[0])
	assert.Equal(t, map[string]any{"stopReason": string(acp.StopCancelled)}, s.result(3),
		"the answer follows the closed tool call")
	st, err := runs.Classify(cwd, run.dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusRunning, st, "the run goes on")
}

// acpPlanStatuses returns the status of every entry of a plan update.
func acpPlanStatuses(update map[string]any) []string {
	entries, _ := update["entries"].([]any)
	statuses := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		st, _ := entry["status"].(string)
		statuses = append(statuses, st)
	}
	return statuses
}

func TestACPFinalMessage(t *testing.T) {
	t.Run("failed with its finish event's reason and no report", func(t *testing.T) {
		cwd := t.TempDir()
		dir := saveRunsTestRun(t, cwd, runs.Descriptor{ID: "a-20261022-120000-aaaa", Branch: "feat-a"})
		appendRunsTestEvents(t, dir, 1, runs.Event{Type: runs.EventAttemptStart},
			runs.Event{Type: runs.EventFinish, Status: runs.StatusFailed, Reason: "task 2 failed"})
		assert.Equal(t, "loopai run a-20261022-120000-aaaa failed: task 2 failed.\n\nBranch: feat-a\n\n"+acpTestResumeHint,
			acpFinalMessage(dir, "a-20261022-120000-aaaa", runs.StatusFailed))
	})
	t.Run("an earlier attempt's outcome is not shown", func(t *testing.T) {
		cwd := t.TempDir()
		dir := saveRunsTestRun(t, cwd, runs.Descriptor{ID: "b-20261022-120000-bbbb", Branch: "feat-b", Attempt: 2})
		outcome := runs.Outcome{RunID: "b-20261022-120000-bbbb", Attempt: 1, Status: runs.StatusSucceeded,
			ReportSummary: "old summary", Risk: "high"}
		require.NoError(t, runs.SaveOutcome(dir, &outcome))
		assert.Equal(t, "loopai run b-20261022-120000-bbbb stopped: stopped before the run recorded an outcome.\n\n"+
			"Branch: feat-b\n\n"+acpTestResumeHint, acpFinalMessage(dir, "b-20261022-120000-bbbb", runs.StatusStopped))
	})
	t.Run("a truncated message without a report points at the run directory", func(t *testing.T) {
		cwd := t.TempDir()
		dir := saveRunsTestRun(t, cwd, runs.Descriptor{ID: "c-20261022-120000-cccc", Branch: "feat-c"})
		appendRunsTestEvents(t, dir, 1, runs.Event{Type: runs.EventAttemptStart},
			runs.Event{Type: runs.EventFinish, Status: runs.StatusFailed, Reason: strings.Repeat("failure ", 4000)})
		got := acpFinalMessage(dir, "c-20261022-120000-cccc", runs.StatusFailed)
		assert.LessOrEqual(t, acpJSONLen(got), acpFinalMessageLimit)
		assert.NotContains(t, got, "Full report:")
		assert.NotContains(t, got, runs.ReportFile)
		assert.True(t, strings.HasSuffix(got, "[truncated; the run's records are in "+dir+"]\n\n"+acpTestResumeHint),
			"the resume hint survives the cut")
	})
	t.Run("a succeeded run has no resume hint", func(t *testing.T) {
		cwd := t.TempDir()
		dir := saveRunsTestRun(t, cwd, runs.Descriptor{ID: "d-20261022-120000-dddd", Branch: "feat-d"})
		saveRunsTestOutcome(t, dir, runs.Outcome{Status: runs.StatusSucceeded, Branch: "feat-d"})
		assert.Equal(t, "loopai run d-20261022-120000-dddd succeeded.\n\nBranch: feat-d",
			acpFinalMessage(dir, "d-20261022-120000-dddd", runs.StatusSucceeded))
	})
}

// acpTestResumeHint is the line a run that ended without success ends its final message with.
const acpTestResumeHint = "The worktree is kept; type `resume` in this thread to continue the run."

func TestACPCapMessage(t *testing.T) {
	pointer := "\n\n[truncated; the full report is in /repo/.loopai/progress/runs/r/report.md]"
	tests := []struct {
		name string
		unit string
	}{
		{"ASCII", "word "},
		{"Cyrillic", "слово "},
		{"escaped characters", "<a & \"b\">\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := "loopai run r succeeded.\n\n" + strings.Repeat(tc.unit, 9000)
			got := acpCapMessage(text, pointer)
			assert.LessOrEqual(t, acpJSONLen(got), acpFinalMessageLimit)
			assert.Greater(t, acpJSONLen(got), acpFinalMessageLimit-len(tc.unit)*6-8, "the cut keeps as much as fits")
			assert.True(t, utf8.ValidString(got), "the cut falls on a rune boundary")
			require.True(t, strings.HasSuffix(got, pointer), "the report path is kept")
			assert.True(t, strings.HasPrefix(text, strings.TrimSuffix(got, pointer)))
		})
	}
	assert.Equal(t, "short", acpCapMessage("short", pointer), "a message that fits is unchanged")
}

// TestACPLongFinalMessageIsTruncated ends a run whose reason alone exceeds the limit: the final
// message on the wire is cut to fit, with the report path kept.
func TestACPLongFinalMessageIsTruncated(t *testing.T) {
	for _, unit := range []string{"failure ", "ошибка "} {
		t.Run(strings.TrimSpace(unit), func(t *testing.T) {
			cwd := t.TempDir()
			run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
				Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
			require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))
			report := filepath.Join(run.dir, runs.ReportFile)
			require.NoError(t, os.WriteFile(report, []byte("# Report\n"), 0o600))

			s, sid := openACPSession(t, cwd, "thread=thread-1", "")
			s.send(acpPrompt(3, sid, acp.ContinuationText))
			run.waitAttached()
			run.finish(runs.Outcome{Status: runs.StatusFailed, Reason: strings.Repeat(unit, 4000)})
			text := s.turnReply(3, sid)
			assert.LessOrEqual(t, acpJSONLen(text), acpFinalMessageLimit)
			assert.True(t, utf8.ValidString(text))
			assert.True(t, strings.HasPrefix(text, "loopai run "+acpRouteRunID+" failed: "+unit))
			assert.True(t, strings.HasSuffix(text, fmt.Sprintf("[truncated; the full report is in %s]\n\n%s", report,
				acpTestResumeHint)))
		})
	}
}
