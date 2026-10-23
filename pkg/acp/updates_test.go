package acp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestUpdates returns updates for session s1 of a provider writing to the returned buffer.
func newTestUpdates(t *testing.T) (*Updates, *bytes.Buffer, *Provider) {
	t.Helper()
	var out bytes.Buffer
	p := NewProvider(strings.NewReader(""), &out, "v", nil)
	return &Updates{p: p, sessionID: "s1", messageID: newMessageID()}, &out, p
}

// sentLines returns the lines written to out.
func sentLines(out *bytes.Buffer) []string {
	s := strings.TrimSuffix(out.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestUpdatesPlan(t *testing.T) {
	u, out, _ := newTestUpdates(t)
	entries := []PlanEntry{
		{Content: "Task 1: add the registry", Priority: PriorityMedium, Status: PlanCompleted},
		{Content: "Task 2: add the journal", Status: PlanInProgress},
		{Content: "Review"},
	}
	require.NoError(t, u.Plan(entries))
	require.NoError(t, u.Plan(nil))

	lines := sentLines(out)
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{
		"sessionUpdate":"plan","entries":[
			{"content":"Task 1: add the registry","priority":"medium","status":"completed"},
			{"content":"Task 2: add the journal","priority":"medium","status":"in_progress"},
			{"content":"Review","priority":"medium","status":"pending"}]}}}`, lines[0])
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{
		"sessionUpdate":"plan","entries":[]}}}`, lines[1], "an empty plan is an empty array, never null")
	assert.Empty(t, entries[2].Priority, "the caller's entries are not modified")
}

func TestUpdatesToolCall(t *testing.T) {
	u, out, _ := newTestUpdates(t)
	require.NoError(t, u.ToolCall("phase-1", "Task 3/7: add the journal"))
	require.NoError(t, u.ToolCall("phase-2", ""))

	lines := sentLines(out)
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{
		"sessionUpdate":"tool_call","toolCallId":"phase-1","title":"Task 3/7: add the journal",
		"kind":"other","status":"in_progress"}}}`, lines[0])
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{
		"sessionUpdate":"tool_call","toolCallId":"phase-2","title":"","kind":"other","status":"in_progress"}}}`,
		lines[1], "title is required, even when empty")
}

func TestUpdatesToolCallUpdate(t *testing.T) {
	tests := []struct {
		name   string
		status string
		title  string
		want   string
	}{
		{name: "status and title", status: ToolCompleted, title: "Task 3/7: done",
			want: `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","title":"Task 3/7: done"}`},
		{name: "status only", status: ToolFailed,
			want: `{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"failed"}`},
		{name: "retitle only", title: "Review (20m)",
			want: `{"sessionUpdate":"tool_call_update","toolCallId":"c1","title":"Review (20m)"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, out, _ := newTestUpdates(t)
			require.NoError(t, u.ToolCallUpdate("c1", tt.status, tt.title))
			lines := sentLines(out)
			require.Len(t, lines, 1)
			assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":`+tt.want+`}}`, lines[0])
		})
	}
}

func TestUpdatesAgentMessageChunk(t *testing.T) {
	u, out, p := newTestUpdates(t)
	require.Regexp(t, `^loopai-final-[0-9a-f]{16}$`, u.MessageID())
	require.NoError(t, u.AgentMessageChunk("Run succeeded.\n"))
	require.NoError(t, u.AgentMessageChunk("Report: .loopai/progress/runs/r1/report.md"))

	lines := sentLines(out)
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{
		"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Run succeeded.\n"},
		"messageId":"`+u.MessageID()+`"}}}`, lines[0])
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{
		"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Report: .loopai/progress/runs/r1/report.md"},
		"messageId":"`+u.MessageID()+`"}}}`, lines[1], "every chunk of one prompt shares its messageId")

	other := &Updates{p: p, sessionID: "s1", messageID: newMessageID()}
	assert.NotEqual(t, u.MessageID(), other.MessageID(), "another prompt starts another message")
}

func TestUpdatesAfterStop(t *testing.T) {
	u, out, p := newTestUpdates(t)
	p.stop()
	require.ErrorIs(t, u.Plan([]PlanEntry{{Content: "Task 1"}}), ErrProviderStopped)
	require.ErrorIs(t, u.ToolCall("c1", "Task 1"), ErrProviderStopped)
	require.ErrorIs(t, u.ToolCallUpdate("c1", ToolCompleted, ""), ErrProviderStopped)
	require.ErrorIs(t, u.AgentMessageChunk("late"), ErrProviderStopped)
	assert.Empty(t, out.String(), "a stopped provider sends nothing")
}
