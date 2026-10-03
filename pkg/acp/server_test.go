package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

const testTimeout = 5 * time.Second

// scriptedClient drives a Server over pipes the way an ACP client does.
type scriptedClient struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	served chan error
	nextID int64
	// pending holds messages read while waiting for a different one
	pending []map[string]any
}

func startServer(t *testing.T, opts Options) *scriptedClient {
	t.Helper()
	clientToAgent, agentIn := io.Pipe()
	agentOut, agentToClient := io.Pipe()
	srv := NewServer(clientToAgent, agentToClient, opts)
	c := &scriptedClient{t: t, in: agentIn, lines: make(chan map[string]any, 64), served: make(chan error, 1), nextID: 1 << 40}
	go func() {
		c.served <- srv.Serve()
		_ = agentToClient.Close()
	}()
	go func() {
		defer close(c.lines)
		r := bufio.NewReader(agentOut)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if !assert.NoError(t, json.Unmarshal(line, &m), "agent wrote a non-JSON line %q", line) {
					return
				}
				c.lines <- m
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = agentIn.Close() })
	return c
}

func (c *scriptedClient) send(msg map[string]any) {
	c.t.Helper()
	msg["jsonrpc"] = "2.0"
	data, err := json.Marshal(msg)
	require.NoError(c.t, err)
	_, err = c.in.Write(append(data, '\n'))
	require.NoError(c.t, err)
}

// request sends a request with a large numeric id, the way T3 Code does, and returns the id.
func (c *scriptedClient) request(method string, params any) float64 {
	c.t.Helper()
	c.nextID++
	c.send(map[string]any{"id": c.nextID, "method": method, "params": params, "traceId": "t", "spanId": "s"})
	return float64(c.nextID)
}

// cancel sends session/cancel as a notification, the way ACP defines it.
func (c *scriptedClient) cancel(params any) {
	c.t.Helper()
	c.send(map[string]any{"method": "session/cancel", "params": params})
}

func (c *scriptedClient) next() map[string]any {
	c.t.Helper()
	select {
	case m, ok := <-c.lines:
		require.True(c.t, ok, "agent output closed")
		return m
	case <-time.After(testTimeout):
		c.t.Fatal("timed out waiting for an agent message")
		return nil
	}
}

// response waits for the response to id, keeping notifications seen meanwhile for updates.
func (c *scriptedClient) response(id float64) map[string]any {
	c.t.Helper()
	for i, m := range c.pending {
		if m["id"] == id {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return m
		}
	}
	for {
		m := c.next()
		if m["id"] == id {
			return m
		}
		c.pending = append(c.pending, m)
	}
}

func (c *scriptedClient) call(method string, params any) map[string]any {
	c.t.Helper()
	return c.response(c.request(method, params))
}

// updates returns the session/update payloads collected so far.
func (c *scriptedClient) updates() []map[string]any {
	var out []map[string]any
	for _, m := range c.pending {
		if m["method"] == "session/update" {
			params, _ := m["params"].(map[string]any)
			out = append(out, params)
		}
	}
	return out
}

func (c *scriptedClient) close() error {
	c.t.Helper()
	require.NoError(c.t, c.in.Close())
	select {
	case err := <-c.served:
		return err
	case <-time.After(testTimeout):
		c.t.Fatal("Serve did not return after EOF")
		return nil
	}
}

func (c *scriptedClient) newSession(cwd string) string {
	c.t.Helper()
	resp := c.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	result, ok := resp["result"].(map[string]any)
	require.True(c.t, ok, "session/new result: %v", resp)
	sid, _ := result["sessionId"].(string)
	require.NotEmpty(c.t, sid)
	return sid
}

func textPrompt(sid string, blocks ...string) map[string]any {
	prompt := make([]any, 0, len(blocks))
	for _, b := range blocks {
		prompt = append(prompt, map[string]any{"type": "text", "text": b})
	}
	return map[string]any{"sessionId": sid, "prompt": prompt, "_meta": map[string]any{"promptId": "p1"}}
}

func requireError(t *testing.T, resp map[string]any, code int) string {
	t.Helper()
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "expected an error response, got %v", resp)
	assert.InDelta(t, float64(code), errObj["code"], 0)
	assert.NotContains(t, resp, "result")
	msg, _ := errObj["message"].(string)
	return msg
}

func requireStopReason(t *testing.T, resp map[string]any, want string) {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", resp)
	assert.Equal(t, want, result["stopReason"])
}

func TestServerHandshake(t *testing.T) {
	c := startServer(t, Options{Version: "1.2.3"})

	resp := c.call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": true}},
		"clientInfo":         map[string]any{"name": "t3code", "version": "x"},
	})
	assert.Equal(t, map[string]any{
		"protocolVersion":   float64(1),
		"agentCapabilities": map[string]any{"loadSession": true},
		"authMethods": []any{
			map[string]any{"id": "cached_token", "name": "cached token"},
			map[string]any{"id": "xai.api_key", "name": "API key"},
		},
		"agentInfo": map[string]any{"name": "loopai", "version": "1.2.3"},
	}, resp["result"])

	for _, method := range []string{"cached_token", "xai.api_key", "something-else"} {
		resp = c.call("authenticate", map[string]any{"methodId": method})
		assert.Equal(t, map[string]any{}, resp["result"], method)
	}

	sid := c.newSession(t.TempDir())
	assert.Regexp(t, `^loopai-[0-9a-f]{16}$`, sid)
	assert.NotEqual(t, sid, c.newSession(t.TempDir()), "session ids are unique")

	for _, method := range []string{"session/set_config_option", "session/set_mode", "session/set_model"} {
		resp = c.call(method, map[string]any{"sessionId": sid, "configId": "model", "value": "x"})
		assert.Equal(t, map[string]any{}, resp["result"], method)
	}

	require.NoError(t, c.close())
}

// TestServerLoadSession covers the path T3 Code takes once a thread's earlier agent process is
// gone: it sends session/load with the saved id instead of session/new, then prompts that id.
func TestServerLoadSession(t *testing.T) {
	var got PromptRequest
	c := startServer(t, Options{Run: func(_ context.Context, req PromptRequest, _ *Sink) (Result, error) {
		got = req
		return Result{}, nil
	}})
	c.call("initialize", map[string]any{"protocolVersion": 1})

	cwd := t.TempDir()
	resp := c.call("session/load", map[string]any{"sessionId": "loopai-0123456789abcdef", "cwd": cwd,
		"mcpServers": []any{}})
	assert.Equal(t, map[string]any{}, resp["result"], "a saved session id is accepted without replaying history")

	requireStopReason(t, c.call("session/prompt", textPrompt("loopai-0123456789abcdef", "plan.md")), stopEndTurn)
	assert.Equal(t, PromptRequest{SessionID: "loopai-0123456789abcdef", Cwd: cwd, Text: "plan.md"}, got)

	moved := t.TempDir()
	c.call("session/load", map[string]any{"sessionId": "loopai-0123456789abcdef", "cwd": moved})
	requireStopReason(t, c.call("session/prompt", textPrompt("loopai-0123456789abcdef", "plan.md")), stopEndTurn)
	assert.Equal(t, moved, got.Cwd, "loading a known session again records its new cwd")

	tests := []struct {
		name   string
		params any
	}{
		{name: "missing session id", params: map[string]any{"cwd": cwd}},
		{name: "blank session id", params: map[string]any{"sessionId": "  ", "cwd": cwd}},
		{name: "relative cwd", params: map[string]any{"sessionId": "loopai-1", "cwd": "relative/dir"}},
		{name: "not an object", params: []any{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireError(t, c.call("session/load", tt.params), CodeInvalidParams)
		})
	}
	require.NoError(t, c.close())
}

func TestServerNewSessionValidation(t *testing.T) {
	c := startServer(t, Options{})
	tests := []struct {
		name   string
		params any
	}{
		{name: "missing cwd", params: map[string]any{"mcpServers": []any{}}},
		{name: "relative cwd", params: map[string]any{"cwd": "relative/dir", "mcpServers": []any{}}},
		{name: "not an object", params: []any{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireError(t, c.call("session/new", tt.params), CodeInvalidParams)
		})
	}
	require.NoError(t, c.close())
}

func TestServerPromptSuccess(t *testing.T) {
	cwd := t.TempDir()
	var got PromptRequest
	c := startServer(t, Options{Run: func(ctx context.Context, req PromptRequest, sink *Sink) (Result, error) {
		got = req
		sink.Message("progress")
		return Result{Message: "# Report\nall done"}, nil
	}})
	c.call("initialize", map[string]any{"protocolVersion": 1})
	sid := c.newSession(cwd)

	resp := c.call("session/prompt", textPrompt(sid, "docs/plans/x.md --task-model codex", "<runtime instructions>"))
	requireStopReason(t, resp, stopEndTurn)

	assert.Equal(t, PromptRequest{SessionID: sid, Cwd: cwd, Text: "docs/plans/x.md --task-model codex"}, got,
		"only the first text block reaches the run; the runtime-instructions block is ignored")
	updates := c.updates()
	require.Len(t, updates, 2, "the run's update and the final message precede the reply")
	assert.Equal(t, sid, updates[0]["sessionId"])
	assert.Equal(t, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": "progress"},
	}, updates[0]["update"])
	assert.Equal(t, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": "# Report\nall done"},
	}, updates[1]["update"])

	require.NoError(t, c.close())
}

func TestServerPromptFailure(t *testing.T) {
	c := startServer(t, Options{Run: func(context.Context, PromptRequest, *Sink) (Result, error) {
		return Result{Message: "partial report"}, errors.New("task 2 failed: validation")
	}})
	sid := c.newSession(t.TempDir())

	resp := c.call("session/prompt", textPrompt(sid, "plan.md"))
	assert.Equal(t, "task 2 failed: validation", requireError(t, resp, CodeInternalError))
	updates := c.updates()
	require.Len(t, updates, 1)
	assert.Equal(t, "agent_message_chunk", updates[0]["update"].(map[string]any)["sessionUpdate"])

	require.NoError(t, c.close())
}

func TestServerFinishesSinkBeforeReply(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"success", nil, []string{"tool_call/in_progress", "agent_thought_chunk/", "agent_thought_chunk/",
			"tool_call_update/completed", "agent_message_chunk/"}},
		{"failure", errors.New("boom"), []string{"tool_call/in_progress", "agent_thought_chunk/", "agent_thought_chunk/",
			"tool_call_update/failed", "agent_message_chunk/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := startServer(t, Options{Run: func(_ context.Context, _ PromptRequest, sink *Sink) (Result, error) {
				sink.OnPhase("", status.PhaseFinalize)
				sink.Output("first")
				sink.Output("still pending when the run returns")
				return Result{Message: "report"}, tt.err
			}})
			sid := c.newSession(t.TempDir())
			c.call("session/prompt", textPrompt(sid, "plan.md"))

			var got []string
			for _, u := range c.updates() {
				upd, _ := u["update"].(map[string]any)
				if upd["sessionUpdate"] == "plan" {
					continue
				}
				st, _ := upd["status"].(string)
				got = append(got, fmt.Sprintf("%v/%s", upd["sessionUpdate"], st))
			}
			assert.Equal(t, tt.want, got, "pending output is flushed and the call closed before the final message and reply")
			require.NoError(t, c.close())
		})
	}
}

func TestServerPromptPanicAndMissingRun(t *testing.T) {
	t.Run("panic", func(t *testing.T) {
		c := startServer(t, Options{Run: func(context.Context, PromptRequest, *Sink) (Result, error) {
			panic("boom")
		}})
		sid := c.newSession(t.TempDir())
		msg := requireError(t, c.call("session/prompt", textPrompt(sid, "plan.md")), CodeInternalError)
		assert.Contains(t, msg, "panicked: boom")
		// the server recovers and accepts the next prompt
		requireError(t, c.call("session/prompt", textPrompt(sid, "plan.md")), CodeInternalError)
		require.NoError(t, c.close())
	})

	t.Run("no run function", func(t *testing.T) {
		c := startServer(t, Options{})
		sid := c.newSession(t.TempDir())
		msg := requireError(t, c.call("session/prompt", textPrompt(sid, "plan.md")), CodeInternalError)
		assert.Contains(t, msg, "panicked", "a missing run function still answers the prompt")
		require.NoError(t, c.close())
	})
}

func TestServerPromptValidation(t *testing.T) {
	var runs atomic.Int32
	c := startServer(t, Options{Run: func(context.Context, PromptRequest, *Sink) (Result, error) {
		runs.Add(1)
		return Result{}, nil
	}})
	sid := c.newSession(t.TempDir())

	tests := []struct {
		name   string
		params any
		code   int
	}{
		{name: "unknown session", params: textPrompt("loopai-nope", "plan.md"), code: CodeInvalidParams},
		{name: "empty prompt", params: map[string]any{"sessionId": sid, "prompt": []any{}}, code: CodeInvalidParams},
		{name: "image first", params: map[string]any{"sessionId": sid, "prompt": []any{
			map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
			map[string]any{"type": "text", "text": "<runtime instructions>"},
		}}, code: CodeInvalidParams},
		{name: "malformed params", params: "nope", code: CodeInvalidParams},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireError(t, c.call("session/prompt", tt.params), tt.code)
		})
	}
	assert.Zero(t, runs.Load(), "invalid prompts never start a run")
	require.NoError(t, c.close())
}

// blockingRun returns a run function that blocks until ctx is canceled or release is closed.
func blockingRun(started chan<- PromptRequest, release <-chan struct{}) RunFunc {
	return func(ctx context.Context, req PromptRequest, _ *Sink) (Result, error) {
		started <- req
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-release:
			return Result{Message: "done"}, nil
		}
	}
}

func waitStarted(t *testing.T, started <-chan PromptRequest) PromptRequest {
	t.Helper()
	select {
	case req := <-started:
		return req
	case <-time.After(testTimeout):
		t.Fatal("run did not start")
		return PromptRequest{}
	}
}

func TestServerCancel(t *testing.T) {
	started := make(chan PromptRequest, 1)
	c := startServer(t, Options{Run: blockingRun(started, make(chan struct{}))})
	sid := c.newSession(t.TempDir())
	other := c.newSession(t.TempDir())

	id := c.request("session/prompt", textPrompt(sid, "plan.md"))
	waitStarted(t, started)

	// a cancel for a different session or a malformed one leaves the run alone
	c.cancel(map[string]any{"sessionId": other})
	c.cancel("garbage")
	resp := c.call("session/set_mode", map[string]any{"sessionId": sid, "modeId": "x"})
	assert.Equal(t, map[string]any{}, resp["result"])
	assert.Empty(t, c.pending, "the prompt is still running")

	c.cancel(map[string]any{"sessionId": sid})
	requireStopReason(t, c.response(id), stopCanceled)

	// the prompt is answered exactly once
	resp = c.call("session/set_mode", map[string]any{"sessionId": sid, "modeId": "x"})
	assert.Equal(t, map[string]any{}, resp["result"])
	for _, m := range c.pending {
		assert.NotEqual(t, id, m["id"], "duplicate reply to the prompt")
	}
	require.NoError(t, c.close())
}

func TestServerCancelAsRequest(t *testing.T) {
	started := make(chan PromptRequest, 1)
	c := startServer(t, Options{Run: blockingRun(started, make(chan struct{}))})
	sid := c.newSession(t.TempDir())

	id := c.request("session/prompt", textPrompt(sid, "plan.md"))
	waitStarted(t, started)
	resp := c.call("session/cancel", map[string]any{"sessionId": sid})
	assert.Equal(t, map[string]any{}, resp["result"])
	requireStopReason(t, c.response(id), stopCanceled)
	require.NoError(t, c.close())
}

func TestServerCancelWinsOverLateSuccess(t *testing.T) {
	started := make(chan PromptRequest, 1)
	c := startServer(t, Options{Run: func(ctx context.Context, req PromptRequest, sink *Sink) (Result, error) {
		sink.OnPhase("", status.PhaseTask)
		started <- req
		<-ctx.Done()
		// the run finishes successfully anyway, as a run past its last cancellation point does
		return Result{Message: "late report"}, nil
	}})
	sid := c.newSession(t.TempDir())

	id := c.request("session/prompt", textPrompt(sid, "plan.md"))
	waitStarted(t, started)
	c.cancel(map[string]any{"sessionId": sid})
	requireStopReason(t, c.response(id), stopCanceled)

	updates := c.updates()
	got := make([]string, 0, len(updates))
	for _, u := range updates {
		upd, _ := u["update"].(map[string]any)
		st, _ := upd["status"].(string)
		got = append(got, fmt.Sprintf("%v/%s", upd["sessionUpdate"], st))
	}
	assert.Contains(t, got, "tool_call_update/failed", "a canceled turn closes its open tool call as failed")
	assert.NotContains(t, got, "agent_message_chunk/", "a canceled turn sends no final message")
	require.NoError(t, c.close())
}

func TestServerRejectsConcurrentPrompt(t *testing.T) {
	started := make(chan PromptRequest, 1)
	release := make(chan struct{})
	c := startServer(t, Options{Run: blockingRun(started, release)})
	sid := c.newSession(t.TempDir())
	other := c.newSession(t.TempDir())

	id := c.request("session/prompt", textPrompt(sid, "first.md"))
	assert.Equal(t, "first.md", waitStarted(t, started).Text)

	msg := requireError(t, c.call("session/prompt", textPrompt(sid, "second.md")), CodeInvalidRequest)
	assert.Contains(t, msg, "already in progress")
	requireError(t, c.call("session/prompt", textPrompt(other, "third.md")), CodeInvalidRequest)

	close(release)
	requireStopReason(t, c.response(id), stopEndTurn)

	// once the first run ends, the next prompt is accepted
	id = c.request("session/prompt", textPrompt(other, "fourth.md"))
	assert.Equal(t, "fourth.md", waitStarted(t, started).Text)
	requireStopReason(t, c.response(id), stopEndTurn)
	require.NoError(t, c.close())
}

// steeringRun is blockingRun whose canceled run returns only once exit is closed, as a run
// cleaning up after cancellation does. running counts runs in progress and overlap records any
// moment two runs were in progress at once.
func steeringRun(started chan<- PromptRequest, release, exit <-chan struct{}, running *atomic.Int32,
	overlap *atomic.Bool) RunFunc {
	return func(ctx context.Context, req PromptRequest, _ *Sink) (Result, error) {
		if running.Add(1) > 1 {
			overlap.Store(true)
		}
		defer running.Add(-1)
		started <- req
		select {
		case <-ctx.Done():
			<-exit
			return Result{}, ctx.Err()
		case <-release:
			return Result{Message: "done"}, nil
		}
	}
}

// TestServerPromptAfterCancelWaitsForRun covers T3 Code's steering, which sends a message typed
// while a turn runs as session/cancel followed at once by session/prompt: the new prompt starts
// once the canceled run has ended instead of being rejected.
func TestServerPromptAfterCancelWaitsForRun(t *testing.T) {
	started := make(chan PromptRequest, 2)
	release, exit := make(chan struct{}), make(chan struct{})
	var running atomic.Int32
	var overlap atomic.Bool
	c := startServer(t, Options{Run: steeringRun(started, release, exit, &running, &overlap)})
	sid := c.newSession(t.TempDir())
	other := c.newSession(t.TempDir())

	first := c.request("session/prompt", textPrompt(sid, "first.md"))
	assert.Equal(t, "first.md", waitStarted(t, started).Text)
	c.cancel(map[string]any{"sessionId": sid})
	second := c.request("session/prompt", textPrompt(sid, "second.md"))

	// another session still cannot start while the canceled run cleans up
	msg := requireError(t, c.call("session/prompt", textPrompt(other, "third.md")), CodeInvalidRequest)
	assert.Contains(t, msg, "already in progress")
	select {
	case req := <-started:
		t.Fatalf("%s started before the canceled run ended", req.Text)
	case <-time.After(50 * time.Millisecond):
	}

	close(exit)
	requireStopReason(t, c.response(first), stopCanceled)
	assert.Equal(t, "second.md", waitStarted(t, started).Text)
	close(release)
	requireStopReason(t, c.response(second), stopEndTurn)
	assert.False(t, overlap.Load(), "two runs were in progress at once")
	require.NoError(t, c.close())
}

// TestServerCancelWhileWaitingForCanceledRun cancels a steered prompt before its predecessor has
// ended: it is answered as canceled without running.
func TestServerCancelWhileWaitingForCanceledRun(t *testing.T) {
	started := make(chan PromptRequest, 2)
	exit := make(chan struct{})
	var running atomic.Int32
	var overlap atomic.Bool
	c := startServer(t, Options{Run: steeringRun(started, make(chan struct{}), exit, &running, &overlap)})
	sid := c.newSession(t.TempDir())

	first := c.request("session/prompt", textPrompt(sid, "first.md"))
	waitStarted(t, started)
	c.cancel(map[string]any{"sessionId": sid})
	second := c.request("session/prompt", textPrompt(sid, "second.md"))
	c.cancel(map[string]any{"sessionId": sid})
	// messages are dispatched in order, so this reply proves the cancel was handled before the
	// predecessor ends; otherwise second.md would legitimately start
	c.call("session/set_mode", map[string]any{"sessionId": sid, "modeId": "x"})

	close(exit)
	requireStopReason(t, c.response(first), stopCanceled)
	requireStopReason(t, c.response(second), stopCanceled)
	select {
	case req := <-started:
		t.Fatalf("%s ran after being canceled", req.Text)
	default:
	}

	// the server accepts a new prompt afterwards
	third := c.request("session/prompt", textPrompt(sid, "third.md"))
	assert.Equal(t, "third.md", waitStarted(t, started).Text)
	c.cancel(map[string]any{"sessionId": sid})
	requireStopReason(t, c.response(third), stopCanceled)
	require.NoError(t, c.close())
}

func TestServerShutdownCancelsRunningPrompt(t *testing.T) {
	started := make(chan PromptRequest, 1)
	var finished atomic.Bool
	c := startServer(t, Options{Run: func(ctx context.Context, req PromptRequest, sink *Sink) (Result, error) {
		res, err := blockingRun(started, nil)(ctx, req, sink)
		time.Sleep(20 * time.Millisecond) // cleanup that Serve must wait for
		finished.Store(true)
		return res, err
	}})
	sid := c.newSession(t.TempDir())
	c.request("session/prompt", textPrompt(sid, "plan.md"))
	waitStarted(t, started)

	require.NoError(t, c.close())
	assert.True(t, finished.Load(), "Serve waits for the running prompt to finish")
}

func TestServerShutdownWithoutEOF(t *testing.T) {
	clientToAgent, agentIn := io.Pipe()
	agentOut, agentToClient := io.Pipe()
	t.Cleanup(func() { _ = agentIn.Close(); _ = agentOut.Close() })
	started := make(chan PromptRequest, 1)
	var finished atomic.Bool
	srv := NewServer(clientToAgent, agentToClient, Options{Run: func(ctx context.Context, req PromptRequest, sink *Sink) (Result, error) {
		res, err := blockingRun(started, nil)(ctx, req, sink)
		finished.Store(true)
		return res, err
	}})
	go func() { _ = srv.Serve() }()
	lines := make(chan map[string]any, 64)
	go func() {
		r := bufio.NewReader(agentOut)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				close(lines)
				return
			}
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				lines <- m
			}
		}
	}()
	send := func(id int, method string, params any) {
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		require.NoError(t, err)
		_, err = agentIn.Write(append(data, '\n'))
		require.NoError(t, err)
	}
	await := func(id int) map[string]any {
		for {
			select {
			case m, ok := <-lines:
				require.True(t, ok, "agent output closed")
				if m["id"] == float64(id) {
					return m
				}
			case <-time.After(testTimeout):
				t.Fatalf("timed out waiting for response %d", id)
			}
		}
	}

	send(1, "session/new", map[string]any{"cwd": t.TempDir()})
	sid, _ := await(1)["result"].(map[string]any)["sessionId"].(string)
	require.NotEmpty(t, sid)
	send(2, "session/prompt", textPrompt(sid, "plan.md"))
	waitStarted(t, started)

	done := make(chan struct{})
	go func() { srv.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Shutdown did not return while input stays open")
	}
	assert.True(t, finished.Load(), "Shutdown waits for the running prompt")
	requireError(t, await(2), CodeInternalError)

	send(3, "session/prompt", textPrompt(sid, "again.md"))
	assert.Contains(t, requireError(t, await(3), CodeInternalError), "shutting down")
}

// syncBuffer is a goroutine-safe buffer standing in for stderr.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // test helper
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServerNeverLogsCredentials(t *testing.T) {
	const secret = "t3-bearer-SECRET-0123456789"
	stderr := &syncBuffer{}
	c := startServer(t, Options{Debug: stderr, Run: func(context.Context, PromptRequest, *Sink) (Result, error) {
		return Result{}, nil
	}})
	c.call("initialize", map[string]any{"protocolVersion": 1})
	resp := c.call("session/new", map[string]any{
		"cwd": filepath.Join(t.TempDir(), "repo"),
		"mcpServers": []any{map[string]any{
			"type":    "http",
			"name":    "t3",
			"url":     "http://127.0.0.1:3773/mcp",
			"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer " + secret}},
		}},
	})
	sid := resp["result"].(map[string]any)["sessionId"].(string)
	requireStopReason(t, c.call("session/prompt", textPrompt(sid, "plan.md")), stopEndTurn)
	require.NoError(t, c.close())

	log := stderr.String()
	assert.Contains(t, log, "session/new "+sid)
	assert.Contains(t, log, "t3{Authorization=[redacted]}")
	assert.Contains(t, log, "session/prompt "+sid+" completed")
	assert.NotContains(t, log, secret)
	assert.NotContains(t, log, "Bearer")
}

func TestDescribeMCPServers(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "absent", raw: ``, want: "[]"},
		{name: "null", raw: `null`, want: "[]"},
		{name: "empty", raw: `[]`, want: "[]"},
		{name: "headers", raw: `[{"name":"a","headers":[{"name":"X","value":"v1"},{"name":"Y","value":"v2"}]},{"name":"b"}]`,
			want: "[a{X=[redacted],Y=[redacted]} b{}]"},
		{name: "unexpected shape", raw: `{"headers":{"Authorization":"Bearer z"}}`, want: "(unparsed)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeMCPServers(json.RawMessage(tt.raw))
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "v1")
			assert.NotContains(t, got, "Bearer")
		})
	}
}

func TestStopReasonWireValues(t *testing.T) {
	assert.Equal(t, "end_turn", stopEndTurn)
	assert.Equal(t, "cancelled", stopCanceled) //nolint:misspell // ACP wire value
}
