package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

// fakeClock fires timers synchronously from Advance, in deadline order.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clk   *fakeClock
	at    time.Time
	f     func()
	done  bool
	order int
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clk: c, at: c.now.Add(d), f: f, order: len(c.timers)}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	active := !t.done
	t.done = true
	return active
}

// Advance moves time forward by d, firing every timer due on the way at its own deadline.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.done && !t.at.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			c.now = target
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool {
			if due[i].at.Equal(due[j].at) {
				return due[i].order < due[j].order
			}
			return due[i].at.Before(due[j].at)
		})
		next := due[0]
		next.done = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

// recorder captures the notifications a sink writes.
type recorder struct {
	buf  syncBuffer
	seen int
}

// take returns the session/update payloads written since the previous call.
func (r *recorder) take(t *testing.T) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(r.buf.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	out := make([]map[string]any, 0, len(lines)-r.seen)
	for _, line := range lines[r.seen:] {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				SessionID string         `json:"sessionId"`
				Update    map[string]any `json:"update"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &msg), "line %q", line)
		require.Equal(t, "session/update", msg.Method)
		require.Equal(t, "loopai-1", msg.Params.SessionID)
		out = append(out, msg.Params.Update)
	}
	r.seen = len(lines)
	return out
}

func newTestSink(t *testing.T) (*Sink, *recorder, *fakeClock) {
	t.Helper()
	rec := &recorder{}
	clk := newFakeClock()
	s := newSinkWithClock(NewConn(strings.NewReader(""), &rec.buf), "loopai-1", clk)
	return s, rec, clk
}

func toolCallMsg(id, title string) map[string]any {
	return map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "title": title, "kind": "other", "status": "in_progress"}
}

func toolUpdateMsg(id, title, st string) map[string]any {
	m := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id}
	if title != "" {
		m["title"] = title
	}
	if st != "" {
		m["status"] = st
	}
	return m
}

func thoughtMsg(text string) map[string]any {
	return map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": text}}
}

// only keeps updates of the given kinds.
func only(updates []map[string]any, kinds ...string) []map[string]any {
	var out []map[string]any
	for _, u := range updates {
		for _, k := range kinds {
			if u["sessionUpdate"] == k {
				out = append(out, u)
			}
		}
	}
	return out
}

// planStatuses flattens the latest plan update into "content=status" strings.
func planStatuses(t *testing.T, updates []map[string]any) []string {
	t.Helper()
	plans := only(updates, "plan")
	require.NotEmpty(t, plans, "no plan update in %v", updates)
	entries, ok := plans[len(plans)-1]["entries"].([]any)
	require.True(t, ok)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		assert.Equal(t, "medium", m["priority"])
		out = append(out, fmt.Sprintf("%s=%s", m["content"], m["status"]))
	}
	return out
}

func writePlan(t *testing.T, path string, done ...bool) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Plan\n\n## Implementation Steps\n\n")
	for i, d := range done {
		mark := " "
		if d {
			mark = "x"
		}
		fmt.Fprintf(&b, "### Task %d: step %d\n- [%s] do it\n\n", i+1, i+1, mark)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
}

func TestSinkMessage(t *testing.T) {
	var out bytes.Buffer
	s := NewSink(NewConn(strings.NewReader(""), &out), "loopai-1")
	defer s.Finish(true)
	s.Message("")
	assert.Empty(t, out.String(), "empty text sends nothing")

	s.Message("report")
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"loopai-1",`+
		`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"report"}}}}`, out.String())
}

func TestSinkNilIsNoop(t *testing.T) {
	var s *Sink
	inner := &recordingLogger{}
	assert.NotPanics(t, func() {
		s.Message("text")
		s.SetPlan("plan.md", StageReview)
		s.OnPhase("", status.PhaseTask)
		s.OnSection(status.NewTaskIterationSection(1))
		s.Output("text")
		s.Finish(true)
	})
	assert.Same(t, inner, s.WrapLogger(inner), "a nil sink returns the logger unchanged")
}

func TestSinkIgnoresWriteErrors(t *testing.T) {
	s := NewSink(NewConn(strings.NewReader(""), failingWriter{}), "loopai-1")
	assert.NotPanics(t, func() {
		s.Message("text")
		s.OnPhase("", status.PhaseTask)
		s.Output("out")
		s.Finish(false)
	})
}

func TestSinkPhaseTransitions(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	writePlan(t, planFile, false, false)
	s, rec, _ := newTestSink(t)
	s.SetPlan(planFile)
	rec.take(t)

	s.OnPhase("", status.PhaseTask)
	assert.Equal(t, []map[string]any{toolCallMsg("loopai-1", "task")}, only(rec.take(t), "tool_call", "tool_call_update"))

	s.OnSection(status.NewTaskIterationSection(1))
	assert.Equal(t, []map[string]any{toolUpdateMsg("loopai-1", "task 1/2", "")},
		only(rec.take(t), "tool_call", "tool_call_update"), "the bare phase call gains its task in place")

	s.OnSection(status.NewTaskIterationSection(1))
	assert.Empty(t, only(rec.take(t), "tool_call", "tool_call_update"), "a retried task is not a new activity")

	s.OnSection(status.NewTaskIterationSection(2))
	assert.Equal(t, []map[string]any{
		toolUpdateMsg("loopai-1", "", "completed"),
		toolCallMsg("loopai-2", "task 2/2"),
	}, only(rec.take(t), "tool_call", "tool_call_update"))

	s.OnPhase(status.PhaseTask, status.PhaseReview)
	assert.Equal(t, []map[string]any{
		toolUpdateMsg("loopai-2", "", "completed"),
		toolCallMsg("loopai-3", "review"),
	}, only(rec.take(t), "tool_call", "tool_call_update"))

	s.OnSection(status.NewInternalReviewSection(0, ""))
	assert.Empty(t, only(rec.take(t), "tool_call", "tool_call_update"), "the first review pass keeps the bare title")

	s.OnSection(status.NewInternalReviewSection(1, ": critical/major"))
	assert.Equal(t, []map[string]any{toolUpdateMsg("loopai-3", "review · iteration 1", "")},
		only(rec.take(t), "tool_call", "tool_call_update"))

	s.OnSection(status.NewInternalReviewSection(2, ""))
	assert.Equal(t, []map[string]any{
		toolUpdateMsg("loopai-3", "", "completed"),
		toolCallMsg("loopai-4", "review · iteration 2"),
	}, only(rec.take(t), "tool_call", "tool_call_update"))

	s.OnPhase(status.PhaseReview, status.PhaseExternalReview)
	s.OnSection(status.NewExternalReviewIterationSection("codex", 1))
	s.OnPhase(status.PhaseExternalReview, status.PhaseExternalEval)
	s.OnSection(status.NewExternalEvaluationSection("claude", "codex"))
	s.OnPhase(status.PhaseExternalEval, status.PhaseFinalize)
	assert.Equal(t, []map[string]any{
		toolUpdateMsg("loopai-4", "", "completed"),
		toolCallMsg("loopai-5", "external review"),
		toolUpdateMsg("loopai-5", "external review · iteration 1", ""),
		toolUpdateMsg("loopai-5", "", "completed"),
		toolCallMsg("loopai-6", "external review evaluation"),
		toolUpdateMsg("loopai-6", "", "completed"),
		toolCallMsg("loopai-7", "finalize"),
	}, only(rec.take(t), "tool_call", "tool_call_update"))

	s.Finish(true)
	assert.Equal(t, []map[string]any{toolUpdateMsg("loopai-7", "", "completed")},
		only(rec.take(t), "tool_call", "tool_call_update"))

	s.Finish(true)
	s.OnPhase(status.PhaseFinalize, status.PhaseReport)
	s.OnSection(status.NewTaskIterationSection(1))
	s.Output("late")
	assert.Empty(t, rec.take(t), "events after Finish are dropped")
}

func TestSinkLimitWaitResumesActivity(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	writePlan(t, planFile, true, false)
	s, rec, _ := newTestSink(t)
	s.SetPlan(planFile)
	s.OnPhase("", status.PhaseTask)
	s.OnSection(status.NewTaskIterationSection(2))
	rec.take(t)

	s.OnPhase(status.PhaseTask, status.PhaseLimitWait)
	s.OnPhase(status.PhaseLimitWait, status.PhaseTask)
	assert.Equal(t, []map[string]any{
		toolUpdateMsg("loopai-1", "", "completed"),
		toolCallMsg("loopai-2", "waiting for provider limit"),
		toolUpdateMsg("loopai-2", "", "completed"),
		toolCallMsg("loopai-3", "task 2/2"),
	}, only(rec.take(t), "tool_call", "tool_call_update"))

	s.OnSection(status.NewTaskIterationSection(2))
	assert.Empty(t, only(rec.take(t), "tool_call", "tool_call_update"), "the re-emitted section is the resumed task")
}

func TestSinkFinishFailedClosesCallAsFailed(t *testing.T) {
	s, rec, clk := newTestSink(t)
	s.OnPhase("", status.PhaseTask)
	s.Output("first")
	s.Output("pending")
	rec.take(t)

	s.Finish(false)
	assert.Equal(t, []map[string]any{
		thoughtMsg("pending\n"),
		toolUpdateMsg("loopai-1", "", "failed"),
	}, rec.take(t), "pending reasoning is flushed before the call closes")

	clk.Advance(time.Hour)
	assert.Empty(t, rec.take(t), "no flush or heartbeat after Finish")

	s.Message("report")
	assert.Equal(t, []map[string]any{{"sessionUpdate": "agent_message_chunk",
		"content": map[string]any{"type": "text", "text": "report"}}}, rec.take(t), "the final message still goes out")
}

func TestSinkPlanEntries(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "docs", "plans", "plan.md")
	writePlan(t, planFile, false, false)
	s, rec, _ := newTestSink(t)

	s.SetPlan(planFile, StageReview, StageFinalize, StageReview)
	assert.Equal(t, []string{"Task 1: step 1=pending", "Task 2: step 2=pending", "Review=pending", "Finalize=pending"},
		planStatuses(t, rec.take(t)), "declared stages follow the tasks once each")

	s.OnPhase("", status.PhaseTask)
	assert.Empty(t, only(rec.take(t), "plan"), "an unchanged plan is not resent")

	s.OnSection(status.NewTaskIterationSection(1))
	assert.Equal(t, []string{"Task 1: step 1=in_progress", "Task 2: step 2=pending", "Review=pending", "Finalize=pending"},
		planStatuses(t, rec.take(t)))

	writePlan(t, planFile, true, false)
	s.OnSection(status.NewTaskIterationSection(2))
	assert.Equal(t, []string{"Task 1: step 1=completed", "Task 2: step 2=in_progress", "Review=pending", "Finalize=pending"},
		planStatuses(t, rec.take(t)))

	writePlan(t, planFile, true, true)
	s.OnPhase(status.PhaseTask, status.PhaseReview)
	assert.Equal(t, []string{"Task 1: step 1=completed", "Task 2: step 2=completed", "Review=in_progress", "Finalize=pending"},
		planStatuses(t, rec.take(t)))

	s.OnPhase(status.PhaseReview, status.PhaseLimitWait)
	assert.Empty(t, only(rec.take(t), "plan"), "a limit wait keeps the stage")

	s.OnPhase(status.PhaseLimitWait, status.PhaseExternalReview)
	assert.Equal(t, []string{"Task 1: step 1=completed", "Task 2: step 2=completed", "Review=completed", "Finalize=pending",
		"External review=in_progress"}, planStatuses(t, rec.take(t)), "an undeclared stage is appended when reached")

	s.OnPhase(status.PhaseExternalReview, status.PhaseExternalEval)
	assert.Empty(t, only(rec.take(t), "plan"), "evaluation belongs to the external review stage")

	s.OnPhase(status.PhaseExternalEval, status.PhaseReview)
	assert.Empty(t, only(rec.take(t), "plan"), "the review after external findings does not reopen the completed review stage")

	s.OnPhase(status.PhaseReview, status.PhaseFinalize)
	assert.Equal(t, []string{"Task 1: step 1=completed", "Task 2: step 2=completed", "Review=completed", "Finalize=in_progress",
		"External review=completed"}, planStatuses(t, rec.take(t)))

	// the plan is archived before the run ends; its tasks are read from completed/
	archived := filepath.Join(filepath.Dir(planFile), "completed", "plan.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(archived), 0o750))
	require.NoError(t, os.Rename(planFile, archived))
	s.Finish(true)
	assert.Equal(t, []string{"Task 1: step 1=completed", "Task 2: step 2=completed", "Review=completed", "Finalize=completed",
		"External review=completed"}, planStatuses(t, rec.take(t)))
}

func TestSinkPlanEntriesKeepLastParse(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	writePlan(t, planFile, false)
	s, rec, _ := newTestSink(t)
	s.SetPlan(planFile)
	rec.take(t)
	require.NoError(t, os.Remove(planFile))

	s.OnPhase("", status.PhaseTask)
	s.OnSection(status.NewTaskIterationSection(1))
	assert.Equal(t, []string{"Task 1: step 1=in_progress"}, planStatuses(t, rec.take(t)))

	s.Finish(false)
	assert.Equal(t, []string{"Task 1: step 1=pending"}, planStatuses(t, rec.take(t)),
		"a failed run leaves the unfinished task pending")
}

func TestSinkFailedRunLeavesOpenStagePending(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	writePlan(t, planFile, true)
	s, rec, _ := newTestSink(t)
	s.SetPlan(planFile, StageReview, StageFinalize)
	rec.take(t)

	s.OnPhase("", status.PhaseReview)
	assert.Equal(t, []string{"Task 1: step 1=completed", "Review=in_progress", "Finalize=pending"}, planStatuses(t, rec.take(t)))

	s.Finish(false)
	assert.Equal(t, []string{"Task 1: step 1=completed", "Review=pending", "Finalize=pending"}, planStatuses(t, rec.take(t)),
		"a failed run does not leave its stage in progress")
}

func TestSinkCompletesCheckpointSkippedStages(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	writePlan(t, planFile, true)

	t.Run("a later stage completes the skipped ones", func(t *testing.T) {
		s, rec, _ := newTestSink(t)
		s.SetPlan(planFile, StageReview, StageExternalReview, StageFinalize)
		rec.take(t)

		s.OnPhase("", status.PhaseExternalReview)
		assert.Equal(t, []string{"Task 1: step 1=completed", "Review=completed", "External review=in_progress", "Finalize=pending"},
			planStatuses(t, rec.take(t)), "a resumed run skipping internal review completes it")

		s.OnPhase(status.PhaseExternalEval, status.PhaseReview)
		assert.Empty(t, only(rec.take(t), "plan"), "the review after external findings does not reopen the skipped stage")

		s.Finish(true)
		assert.Equal(t, []string{"Task 1: step 1=completed", "Review=completed", "External review=completed", "Finalize=completed"},
			planStatuses(t, rec.take(t)))
	})

	t.Run("a successful run with every stage skipped completes them all", func(t *testing.T) {
		s, rec, _ := newTestSink(t)
		s.SetPlan(planFile, StageReview, StageExternalReview)
		rec.take(t)

		s.OnPhase("", status.PhaseTask)
		s.Finish(true)
		assert.Equal(t, []string{"Task 1: step 1=completed", "Review=completed", "External review=completed"},
			planStatuses(t, rec.take(t)))
	})
}

func TestNewSinkCallIDsUniqueAcrossPrompts(t *testing.T) {
	firstCall := func() string {
		var out bytes.Buffer
		s := NewSink(NewConn(strings.NewReader(""), &out), "loopai-1")
		s.OnPhase("", status.PhaseTask)
		s.Finish(true)
		var msg struct {
			Params struct {
				Update struct {
					ToolCallID string `json:"toolCallId"`
				} `json:"update"`
			} `json:"params"`
		}
		line, _, _ := strings.Cut(out.String(), "\n")
		require.NoError(t, json.Unmarshal([]byte(line), &msg))
		return msg.Params.Update.ToolCallID
	}
	first, second := firstCall(), firstCall()
	assert.Regexp(t, `^loopai-[0-9a-f]{8}-1$`, first)
	assert.NotEqual(t, first, second, "two prompts of one session never share a tool call id")
}

func TestSinkWithoutPlanSendsNoPlanEntries(t *testing.T) {
	s, rec, _ := newTestSink(t)
	s.OnPhase("", status.PhaseTask)
	s.OnSection(status.NewTaskIterationSection(1))
	s.Finish(true)
	assert.Empty(t, only(rec.take(t), "plan"))
}

func TestSinkOutputCoalescing(t *testing.T) {
	s, rec, clk := newTestSink(t)

	s.Output("a")
	assert.Equal(t, []map[string]any{thoughtMsg("a\n")}, rec.take(t), "the first output goes out at once")

	s.Output("b\n")
	s.Output("c")
	clk.Advance(thoughtInterval - time.Millisecond)
	assert.Empty(t, rec.take(t), "output within the interval waits")

	clk.Advance(time.Millisecond)
	assert.Equal(t, []map[string]any{thoughtMsg("b\nc\n")}, rec.take(t), "waiting output is coalesced into one chunk")

	clk.Advance(time.Second)
	s.Output("d")
	assert.Equal(t, []map[string]any{thoughtMsg("d\n")}, rec.take(t), "output after a quiet interval goes out at once")

	s.Output("")
	clk.Advance(time.Second)
	assert.Empty(t, rec.take(t), "empty output sends nothing")

	s.Output("e")
	s.Output("f")
	s.OnPhase("", status.PhaseReview)
	assert.Equal(t, []map[string]any{thoughtMsg("e\n"), thoughtMsg("f\n"), toolCallMsg("loopai-1", "review")},
		only(rec.take(t), "agent_thought_chunk", "tool_call"), "a phase change flushes pending output first")
	clk.Advance(time.Second)
	assert.Empty(t, rec.take(t), "the canceled flush does not fire")
}

func TestSinkOutputCap(t *testing.T) {
	s, rec, clk := newTestSink(t)
	s.Output("start")
	rec.take(t)

	big := strings.Repeat("x", maxThoughtChunk-10) + strings.Repeat("é", 20)
	s.Output(big)
	s.Output("more output")
	clk.Advance(thoughtInterval)

	updates := rec.take(t)
	require.Len(t, updates, 1)
	content, _ := updates[0]["content"].(map[string]any)
	text, _ := content["text"].(string)
	head, note, found := strings.Cut(text, "[")
	require.True(t, found, "an omission note follows the capped text")
	assert.LessOrEqual(t, len(head), maxThoughtChunk)
	assert.True(t, strings.HasPrefix(head, strings.Repeat("x", maxThoughtChunk-10)))
	assert.True(t, strings.HasSuffix(head, "éééé"), "the cap cuts on a rune boundary: %q", head[len(head)-12:])
	omitted := len(big) + 1 - len(head) + len("more output\n")
	assert.Equal(t, fmt.Sprintf("%d bytes of output omitted]\n", omitted), note)

	clk.Advance(time.Second)
	s.Output("next")
	assert.Equal(t, []map[string]any{thoughtMsg("next\n")}, rec.take(t), "the cap resets with each chunk")
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		text string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "h"},
		{"héllo", 3, "hé"},
		{"é", 1, ""},
		{"abc", 0, ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, truncateUTF8(tt.text, tt.n), "%q/%d", tt.text, tt.n)
	}
}

func TestSinkHeartbeat(t *testing.T) {
	s, rec, clk := newTestSink(t)
	s.OnPhase("", status.PhaseLimitWait)
	rec.take(t)

	clk.Advance(heartbeatIdle - time.Second)
	assert.Empty(t, rec.take(t), "no heartbeat before the idle window")

	clk.Advance(time.Second)
	assert.Equal(t, []map[string]any{toolUpdateMsg("loopai-1", "waiting for provider limit · waiting 4m", "")}, rec.take(t))

	clk.Advance(heartbeatIdle)
	assert.Equal(t, []map[string]any{toolUpdateMsg("loopai-1", "waiting for provider limit · waiting 8m", "")}, rec.take(t),
		"the elapsed wait counts from the last real activity")

	s.Output("resumed")
	assert.Equal(t, []map[string]any{thoughtMsg("resumed\n"), toolUpdateMsg("loopai-1", "waiting for provider limit", "")},
		rec.take(t), "output restores the plain title")

	// steady output keeps the heartbeat quiet
	for range 12 {
		clk.Advance(time.Minute)
		s.Output("tick")
	}
	for _, u := range rec.take(t) {
		assert.Equal(t, "agent_thought_chunk", u["sessionUpdate"])
	}

	clk.Advance(heartbeatIdle)
	s.OnPhase(status.PhaseLimitWait, status.PhaseTask)
	assert.Equal(t, []map[string]any{
		toolUpdateMsg("loopai-1", "waiting for provider limit · waiting 4m", ""),
		toolUpdateMsg("loopai-1", "waiting for provider limit", "completed"),
		toolCallMsg("loopai-2", "task"),
	}, rec.take(t), "closing a call drops the heartbeat suffix")

	s.Finish(true)
	rec.take(t)
	clk.Advance(time.Hour)
	assert.Empty(t, rec.take(t), "the heartbeat stops with the run")
}

func TestSinkHeartbeatWithoutOpenCall(t *testing.T) {
	s, rec, clk := newTestSink(t)
	clk.Advance(heartbeatIdle)
	assert.Equal(t, []map[string]any{
		toolCallMsg("loopai-1", "loopai"),
		toolUpdateMsg("loopai-1", "loopai · waiting 4m", ""),
	}, rec.take(t))
	clk.Advance(heartbeatIdle)
	assert.Equal(t, []map[string]any{toolUpdateMsg("loopai-1", "loopai · waiting 8m", "")}, rec.take(t),
		"the heartbeat's own call does not reset the elapsed wait")
	s.Finish(true)
}

func TestFormatWait(t *testing.T) {
	assert.Equal(t, "0m", formatWait(30*time.Second))
	assert.Equal(t, "4m", formatWait(4*time.Minute+59*time.Second))
	assert.Equal(t, "59m", formatWait(59*time.Minute))
	assert.Equal(t, "1h0m", formatWait(time.Hour))
	assert.Equal(t, "2h8m", formatWait(2*time.Hour+8*time.Minute))
}

func TestActivityTitle(t *testing.T) {
	tests := []struct {
		a     activity
		total int
		want  string
	}{
		{activity{phase: status.PhaseTask}, 5, "task"},
		{activity{phase: status.PhaseTask, task: 2}, 5, "task 2/5"},
		{activity{phase: status.PhaseTask, task: 2}, 0, "task 2"},
		{activity{phase: status.PhaseReview}, 0, "review"},
		{activity{phase: status.PhaseReview, iteration: 1}, 0, "review · iteration 1"},
		{activity{phase: status.PhaseExternalReview, iteration: 3}, 0, "external review · iteration 3"},
		{activity{phase: status.PhaseCodex}, 0, "external review"},
		{activity{phase: status.PhaseExternalEval}, 0, "external review evaluation"},
		{activity{phase: status.PhaseClaudeEval}, 0, "external review evaluation"},
		{activity{phase: status.PhasePlan, iteration: 2}, 0, "plan · iteration 2"},
		{activity{phase: status.PhaseFinalize}, 0, "finalize"},
		{activity{phase: status.PhaseReport}, 0, "completion report"},
		{activity{phase: status.PhaseLimitWait}, 0, "waiting for provider limit"},
		{activity{phase: "custom"}, 0, "custom"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, activityTitle(tt.a, tt.total))
	}
}

func TestSinkCustomReviewerSection(t *testing.T) {
	s, rec, _ := newTestSink(t)
	s.OnPhase("", status.PhaseExternalReview)
	s.OnSection(status.NewCustomIterationSection(2))
	updates := rec.take(t)
	assert.Equal(t, []map[string]any{
		toolCallMsg("loopai-1", "external review"),
		toolUpdateMsg("loopai-1", "external review · iteration 2", ""),
	}, only(updates, "tool_call", "tool_call_update"))
	assert.Equal(t, []string{"External review=in_progress"}, planStatuses(t, updates), "stages are shown without a plan file")
}

// recordingLogger records the calls it receives.
type recordingLogger struct {
	mu    sync.Mutex
	calls []string
}

func (l *recordingLogger) record(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

func (l *recordingLogger) Print(format string, args ...any) { l.record("print:"+format, args...) }
func (l *recordingLogger) PrintRaw(format string, args ...any) {
	l.record("raw:"+format, args...)
}
func (l *recordingLogger) PrintSection(section status.Section) { l.record("section:%s", section.Label) }
func (l *recordingLogger) PrintAligned(text string)            { l.record("aligned:%s", text) }
func (l *recordingLogger) LogQuestion(question string, options []string) {
	l.record("question:%s:%s", question, strings.Join(options, ","))
}
func (l *recordingLogger) LogAnswer(answer string) { l.record("answer:%s", answer) }
func (l *recordingLogger) LogDraftReview(action, feedback string) {
	l.record("draft:%s:%s", action, feedback)
}
func (l *recordingLogger) Path() string { return "progress.txt" }

func TestSinkWrapLogger(t *testing.T) {
	s, rec, _ := newTestSink(t)
	inner := &recordingLogger{}
	logger := s.WrapLogger(inner)

	s.OnPhase("", status.PhaseTask)
	logger.Print("hello %s", "world")
	logger.PrintRaw("raw %d", 1)
	logger.PrintSection(status.NewTaskIterationSection(3))
	logger.PrintAligned("executor output")
	logger.LogQuestion("q", []string{"a", "b"})
	logger.LogAnswer("a")
	logger.LogDraftReview("accept", "fine")
	assert.Equal(t, "progress.txt", logger.Path())

	assert.Equal(t, []string{
		"print:hello world", "raw:raw 1", "section:task iteration 3", "aligned:executor output",
		"question:q:a,b", "answer:a", "draft:accept:fine",
	}, inner.calls, "every call reaches the inner logger")
	assert.Equal(t, []map[string]any{
		toolCallMsg("loopai-1", "task"),
		toolUpdateMsg("loopai-1", "task 3", ""),
		thoughtMsg("executor output\n"),
	}, rec.take(t), "sections and aligned output reach the session; Print is ignored")
}

func TestSinkConcurrentUse(t *testing.T) {
	var out syncBuffer
	s := NewSink(NewConn(strings.NewReader(""), &out), "loopai-1")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 50 {
				s.Output(fmt.Sprintf("line %d-%d", i, j))
				s.OnSection(status.NewTaskIterationSection(j%3 + 1))
				if j%10 == 0 {
					s.OnPhase(status.PhaseTask, status.PhaseReview)
				}
			}
		})
	}
	wg.Wait()
	s.Finish(true)
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		assert.True(t, json.Valid([]byte(line)), "line %q", line)
	}
}
