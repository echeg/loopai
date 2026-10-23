package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// t3Initialize is T3 Code's initialize request, verbatim.
const t3Initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,` +
	`"info":{"name":"t3-code","version":"0.0.0"},"capabilities":{"elicitation":{"form":{},"url":{}}},` +
	`"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,` +
	`"elicitation":{"form":{},"url":{}}},"clientInfo":{"name":"t3-code","version":"0.0.0"}}}`

// initializeAs is T3 Code's initialize request with another clientInfo name, as its probes send it.
func initializeAs(id int, clientName string) string {
	return strings.Replace(strings.Replace(t3Initialize, `"id":1`, fmt.Sprintf(`"id":%d`, id), 1),
		`"clientInfo":{"name":"t3-code"`, `"clientInfo":{"name":"`+clientName+`"`, 1)
}

// authRE is the Go form of the pattern T3 Code uses to mark a provider unauthenticated.
var authRE = regexp.MustCompile(`(?i)\b(authenticat(e|ed|es|ing|ion)|credentials?|log([\s-]+)?in)\b`)

// fakeHandler records every call and delegates to optional hooks.
type fakeHandler struct {
	mu      sync.Mutex
	news    []Session
	loads   []Session
	prompts []PromptRequest
	cancels []string

	onNew    func(ctx context.Context, s Session)
	onLoad   func(ctx context.Context, s Session)
	onPrompt func(ctx context.Context, req PromptRequest, u *Updates) StopReason
	onCancel func(sessionID string)
}

func (h *fakeHandler) NewSession(ctx context.Context, s Session) {
	h.mu.Lock()
	h.news = append(h.news, s)
	h.mu.Unlock()
	if h.onNew != nil {
		h.onNew(ctx, s)
	}
}

func (h *fakeHandler) LoadSession(ctx context.Context, s Session) {
	h.mu.Lock()
	h.loads = append(h.loads, s)
	h.mu.Unlock()
	if h.onLoad != nil {
		h.onLoad(ctx, s)
	}
}

func (h *fakeHandler) Prompt(ctx context.Context, req PromptRequest, u *Updates) StopReason {
	h.mu.Lock()
	h.prompts = append(h.prompts, req)
	h.mu.Unlock()
	if h.onPrompt != nil {
		return h.onPrompt(ctx, req, u)
	}
	return StopEndTurn
}

func (h *fakeHandler) Cancel(sessionID string) {
	h.mu.Lock()
	h.cancels = append(h.cancels, sessionID)
	h.mu.Unlock()
	if h.onCancel != nil {
		h.onCancel(sessionID)
	}
}

func (h *fakeHandler) snapshot() (news, loads []Session, prompts []PromptRequest, cancels []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Session(nil), h.news...), append([]Session(nil), h.loads...),
		append([]PromptRequest(nil), h.prompts...), append([]string(nil), h.cancels...)
}

// script drives a provider over pipes the way T3 Code does.
type script struct {
	t      *testing.T
	p      *Provider
	in     *io.PipeWriter
	lines  chan string
	served chan error
}

func startProvider(t *testing.T, h Handler) *script {
	t.Helper()
	clientToAgent, agentIn := io.Pipe()
	agentOut, agentToClient := io.Pipe()
	s := &script{
		t:      t,
		p:      NewProvider(clientToAgent, agentToClient, "1.2.3", h),
		in:     agentIn,
		lines:  make(chan string, 256),
		served: make(chan error, 1),
	}
	go func() { s.served <- s.p.Serve() }()
	go func() {
		sc := bufio.NewScanner(agentOut)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
		close(s.lines)
	}()
	t.Cleanup(func() {
		_ = agentIn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, s.p.Shutdown(ctx), "every handler returns once the provider stops")
		_ = agentToClient.Close()
	})
	return s
}

func (s *script) send(line string) {
	s.t.Helper()
	_, err := io.WriteString(s.in, line+"\n")
	require.NoError(s.t, err)
}

// next returns the next message the provider wrote.
func (s *script) next() map[string]any {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		require.True(s.t, ok, "provider output ended")
		var m map[string]any
		require.NoError(s.t, json.Unmarshal([]byte(line), &m), "line %q", line)
		return m
	case <-time.After(10 * time.Second):
		s.t.Fatal("no message from the provider")
		return nil
	}
}

// nextRaw returns the next line the provider wrote.
func (s *script) nextRaw() string {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		require.True(s.t, ok, "provider output ended")
		return line
	case <-time.After(10 * time.Second):
		s.t.Fatal("no message from the provider")
		return ""
	}
}

// expectSilence asserts the provider writes nothing for a while.
func (s *script) expectSilence() {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		if ok {
			s.t.Fatalf("unexpected message: %s", line)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// sync sends an unknown request and waits for its answer, proving every earlier message was handled
// and answered first when it was answered on the read loop.
func (s *script) sync(id int) {
	s.t.Helper()
	s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"sync/marker"}`, id))
	m := s.next()
	require.InDelta(s.t, float64(id), m["id"], 0, "got %v", m)
}

// newSession opens a session through session/new and returns its id.
func (s *script) newSession(id int, cwd string) string {
	s.t.Helper()
	s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":%q,"mcpServers":[]}}`, id, cwd))
	m := s.next()
	require.InDelta(s.t, float64(id), m["id"], 0)
	result, ok := m["result"].(map[string]any)
	require.True(s.t, ok, "session/new answered %v", m)
	sid, ok := result["sessionId"].(string)
	require.True(s.t, ok)
	return sid
}

func errorOf(t *testing.T, m map[string]any) (code float64, message string) {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	require.True(t, ok, "expected an error, got %v", m)
	code, _ = e["code"].(float64)
	message, _ = e["message"].(string)
	return code, message
}

func TestProviderInitialize(t *testing.T) {
	s := startProvider(t, &fakeHandler{})
	s.send(t3Initialize)
	line := s.nextRaw()
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,`+
		`"promptCapabilities":{"image":false,"audio":false,"embeddedContext":false},"mcpCapabilities":{"http":false,"sse":false}},`+
		`"authMethods":[],"agentInfo":{"name":"loopai","version":"1.2.3"}}}`, line)

	var m struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(line), &m))
	assert.NotContains(t, m.Result, "info", "an info key would switch T3 Code to ACP v2")
	assert.NotContains(t, string(m.Result["agentCapabilities"]), "acp", "mcpCapabilities.acp would drop the MCP bridge")
	assert.NotContains(t, m.Result, "sessionCapabilities")
	assert.JSONEq(t, `[]`, string(m.Result["authMethods"]))
}

func TestProviderInitializeOnlyThenEOF(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	s.send(initializeAs(1, "t3-code-provider-auth"))
	m := s.next()
	assert.InDelta(t, 1, m["id"], 0)
	assert.Contains(t, m, "result")
	require.NoError(t, s.in.Close())
	select {
	case err := <-s.served:
		require.NoError(t, err, "EOF ends Serve cleanly")
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
	s.expectSilence()
	news, loads, prompts, cancels := h.snapshot()
	assert.Empty(t, news)
	assert.Empty(t, loads)
	assert.Empty(t, prompts)
	assert.Empty(t, cancels)
}

func TestProviderNewSession(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	s.send(initializeAs(1, "t3-code-provider-test"))
	s.next()

	probe := s.newSession(2, "/")
	assert.Regexp(t, `^loopai-[0-9a-f]{32}$`, probe)
	other := s.newSession(3, "/does/not/exist")
	assert.NotEqual(t, probe, other, "every session/new gets a fresh id")

	news, _, _, _ := h.snapshot()
	require.Len(t, news, 2)
	assert.Equal(t, Session{ID: probe, Cwd: "/", MCPServers: json.RawMessage(`[]`), ClientName: "t3-code-provider-test"}, news[0])
	assert.Equal(t, "/does/not/exist", news[1].Cwd)
}

func TestProviderNewSessionPassesMCPServers(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	servers := `[{"name":"t3-code","command":"/Applications/T3.app/t3","args":["acp-mcp-bridge"],"env":[` +
		`{"name":"T3_ACP_MCP_ENDPOINT","value":"http://127.0.0.1:3773/mcp"},` +
		`{"name":"T3_ACP_MCP_AUTHORIZATION","value":"Bearer secret-token"}]}]`
	s.send(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/repo","mcpServers":` + servers + `}}`)
	m := s.next()
	assert.Contains(t, m, "result")
	news, _, _, _ := h.snapshot()
	require.Len(t, news, 1)
	assert.JSONEq(t, servers, string(news[0].MCPServers))
	assert.Empty(t, news[0].ClientName, "no initialize, no client name")
}

func TestProviderNewSessionAnswersAfterTheHook(t *testing.T) {
	release := make(chan struct{})
	h := &fakeHandler{onNew: func(ctx context.Context, _ Session) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	s := startProvider(t, h)
	s.send(`{"jsonrpc":"2.0","id":5,"method":"session/new","params":{"cwd":"/repo","mcpServers":[]}}`)
	s.sync(6) // the read loop is not blocked by the hook
	s.expectSilence()
	close(release)
	m := s.next()
	assert.InDelta(t, 5, m["id"], 0)
	assert.Contains(t, m, "result")
}

func TestProviderNewSessionMalformedParamsSucceeds(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	for i, params := range []string{`"oops"`, `{"cwd":7,"mcpServers":"x"}`, `null`} {
		s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":%s}`, i+1, params))
		m := s.next()
		result, ok := m["result"].(map[string]any)
		require.True(t, ok, "session/new never fails: %v", m)
		assert.NotEmpty(t, result["sessionId"])
	}
}

func TestProviderLoadSession(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"never-seen-42","cwd":"/repo","mcpServers":[]}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{}}`, s.nextRaw())

	_, loads, _, _ := h.snapshot()
	require.Len(t, loads, 1)
	assert.Equal(t, "never-seen-42", loads[0].ID)
	assert.Equal(t, "/repo", loads[0].Cwd)

	// the loaded id serves prompts with the cwd it was loaded with
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"never-seen-42","prompt":[{"type":"text","text":"resume"}]}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}`, s.nextRaw())
	_, _, prompts, _ := h.snapshot()
	require.Len(t, prompts, 1)
	assert.Equal(t, "/repo", prompts[0].Cwd)
}

func TestProviderLoadSessionAnswersAfterTheHook(t *testing.T) {
	release := make(chan struct{})
	h := &fakeHandler{onLoad: func(ctx context.Context, _ Session) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	s := startProvider(t, h)
	s.send(`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"old","cwd":"/repo","mcpServers":[]}}`)
	s.sync(6) // the read loop is not blocked by the hook
	s.expectSilence()
	close(release)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":5,"result":{}}`, s.nextRaw(), "a binding restored by the hook precedes the answer")
}

func TestProviderLoadSessionNeverFails(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	for i, params := range []string{`"oops"`, `{"cwd":"/repo"}`, `{"sessionId":""}`, `null`, `{"sessionId":5}`} {
		s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/load","params":%s}`, i+1, params))
		assert.JSONEq(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, i+1), s.nextRaw(), "params %s", params)
	}
	_, loads, _, _ := h.snapshot()
	assert.Empty(t, loads, "a load without a session id reaches no handler")
}

func TestProviderPrompt(t *testing.T) {
	h := &fakeHandler{onPrompt: func(_ context.Context, req PromptRequest, u *Updates) StopReason {
		assert.NoError(t, u.Plan([]PlanEntry{{Content: "Task 1: add the provider", Status: PlanInProgress}}))
		assert.NoError(t, u.AgentMessageChunk("done"))
		return StopEndTurn
	}}
	s := startProvider(t, h)
	sid := s.newSession(1, "/repo")

	first := `{"type":"text","text":"<t3_code_instructions>\nx\n</t3_code_instructions>\n\n<user_request>\ndocs/plans/a.md\n</user_request>"}`
	second := `{"type":"text","text":"<runtime_info>T3</runtime_info>"}`
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[` + first + `,` + second + `]}}`)

	plan := s.next()
	assert.Equal(t, "session/update", plan["method"])
	assert.NotContains(t, plan, "id")
	chunk := s.next()
	assert.Equal(t, "session/update", chunk["method"])
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}`, s.nextRaw(),
		"the answer follows the updates the handler sent")

	_, _, prompts, _ := h.snapshot()
	require.Len(t, prompts, 1)
	assert.Equal(t, sid, prompts[0].SessionID)
	assert.Equal(t, "/repo", prompts[0].Cwd)
	require.Len(t, prompts[0].Blocks, 2)
	assert.JSONEq(t, first, string(prompts[0].Blocks[0]))
	assert.JSONEq(t, second, string(prompts[0].Blocks[1]))
	assert.Equal(t, "docs/plans/a.md", ParsePrompt(prompts[0].Blocks))
}

func TestProviderPromptStopReasons(t *testing.T) {
	tests := []struct {
		name   string
		reason StopReason
		want   string
	}{
		{name: "end turn", reason: StopEndTurn, want: "end_turn"},
		{name: "cancelled", reason: StopCancelled, want: "cancelled"}, //nolint:misspell // ACP wire value
		{name: "empty means end turn", reason: "", want: "end_turn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := startProvider(t, &fakeHandler{onPrompt: func(context.Context, PromptRequest, *Updates) StopReason {
				return tt.reason
			}})
			sid := s.newSession(1, "/repo")
			s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[]}}`)
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"`+tt.want+`"}}`, s.nextRaw())
		})
	}
}

func TestProviderPromptRejected(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	tests := []struct {
		params string
		want   string
	}{
		{params: `"oops"`, want: errTextInvalidParams},
		{params: `{"prompt":[]}`, want: errTextInvalidParams},
		{params: `{"sessionId":"login-to-continue","prompt":[]}`, want: errTextUnknownSession},
	}
	for i, tt := range tests {
		s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":%s}`, i+1, tt.params))
		code, msg := errorOf(t, s.next())
		assert.InDelta(t, CodeInvalidParams, code, 0)
		assert.Equal(t, tt.want, msg, "the text never echoes the client's session id")
	}
	_, _, prompts, _ := h.snapshot()
	assert.Empty(t, prompts)
}

func TestProviderCancel(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)

	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)
	s.sync(1) // a notification is never answered
	_, _, _, cancels := h.snapshot()
	assert.Equal(t, []string{"s1"}, cancels)

	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/cancel","params":{"sessionId":"s2"}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{}}`, s.nextRaw(), "a cancel sent as a request is answered")
	_, _, _, cancels = h.snapshot()
	assert.Equal(t, []string{"s1", "s2"}, cancels)

	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}`)
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":"oops"}`)
	s.sync(3)
	_, _, _, cancels = h.snapshot()
	assert.Equal(t, []string{"s1", "s2"}, cancels, "a cancel without a session id is ignored")
}

// untilCanceled is a prompt handler that answers StopCancelled once its prompt is canceled.
func untilCanceled(ctx context.Context, req PromptRequest, _ *Updates) StopReason {
	select {
	case <-req.Canceled:
		return StopCancelled
	case <-ctx.Done():
		return StopEndTurn
	}
}

// isClosed reports whether a Canceled channel is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// openPromptCount returns how many prompts the provider still records as open.
func (p *Provider) openPromptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, chs := range p.open {
		n += len(chs)
	}
	return n
}

func TestProviderCancelEndsAnOpenPrompt(t *testing.T) {
	h := &fakeHandler{onPrompt: untilCanceled}
	s := startProvider(t, h)
	sid := s.newSession(1, "/repo")
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[]}}`)
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sid + `"}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"cancelled"}}`, s.nextRaw()) //nolint:misspell // ACP wire value
	s.sync(3)
	_, _, _, cancels := h.snapshot()
	assert.Equal(t, []string{sid}, cancels, "the handler hears the cancel too")
	require.Eventually(t, func() bool { return s.p.openPromptCount() == 0 }, 10*time.Second, 10*time.Millisecond)
}

// TestProviderCancelBeforePromptCall pins the order a handler cannot see: Prompt starts on its own
// goroutine, so the cancel that follows a prompt can reach Cancel before the prompt reaches Prompt.
// The handler here starts its prompt only after Cancel returned, and still finds it canceled.
func TestProviderCancelBeforePromptCall(t *testing.T) {
	cancelSeen := make(chan struct{})
	h := &fakeHandler{
		onCancel: func(string) { close(cancelSeen) },
		onPrompt: func(ctx context.Context, req PromptRequest, u *Updates) StopReason {
			select {
			case <-cancelSeen:
			case <-ctx.Done():
				return StopEndTurn
			}
			assert.True(t, isClosed(req.Canceled), "Canceled is closed before Cancel runs")
			return untilCanceled(ctx, req, u)
		},
	}
	s := startProvider(t, h)
	sid := s.newSession(1, "/repo")
	// one write, so the read loop handles the cancel before the prompt's goroutine is likely to run
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[]}}` + "\n" +
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sid + `"}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"cancelled"}}`, s.nextRaw()) //nolint:misspell // ACP wire value
}

// TestProviderCancelBackToBack sends each prompt and its cancel in one write, the way a Stop pressed
// right after sending reaches the provider, and expects every prompt answered StopCancelled.
func TestProviderCancelBackToBack(t *testing.T) {
	s := startProvider(t, &fakeHandler{onPrompt: untilCanceled})
	sid := s.newSession(1, "/repo")
	for i := range 100 {
		id := i + 2
		s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[]}}`+"\n"+
			`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, id, sid, sid))
		assert.JSONEq(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"cancelled"}}`, id), //nolint:misspell // ACP wire value
			s.nextRaw(), "iteration %d", i)
	}
}

// TestProviderCancelClosesOnlyEarlierPrompts walks a steer: a cancel closes the prompts of its own
// session received before it, never another session's, never a prompt received after it, and a
// repeated cancel closes nothing twice.
func TestProviderCancelClosesOnlyEarlierPrompts(t *testing.T) {
	reqs := make(chan PromptRequest, 4)
	h := &fakeHandler{onPrompt: func(ctx context.Context, req PromptRequest, u *Updates) StopReason {
		reqs <- req
		return untilCanceled(ctx, req, u)
	}}
	s := startProvider(t, h)
	a := s.newSession(1, "/repo")
	b := s.newSession(2, "/repo")
	nextReq := func() PromptRequest {
		t.Helper()
		select {
		case r := <-reqs:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("prompt did not start")
			return PromptRequest{}
		}
	}

	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + a + `","prompt":[]}}`)
	first := nextReq()
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + b + `"}}`)
	s.sync(4)
	assert.False(t, isClosed(first.Canceled), "another session's cancel leaves the prompt open")

	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + a + `"}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`, s.nextRaw()) //nolint:misspell // ACP wire value
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + a + `"}}`)
	s.sync(5)

	// the steered prompt arrives after the cancel and stays open until a cancel of its own
	s.send(`{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{"sessionId":"` + a + `","prompt":[]}}`)
	steered := nextReq()
	s.sync(7)
	assert.False(t, isClosed(steered.Canceled), "a cancel read before the prompt never closes it")
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + a + `"}}` + "\n" +
		`{"jsonrpc":"2.0","id":8,"method":"session/cancel","params":{"sessionId":"` + a + `"}}`)
	got := map[float64]bool{}
	for range 2 {
		m := s.next()
		id, _ := m["id"].(float64)
		got[id] = true
	}
	assert.Equal(t, map[float64]bool{6: true, 8: true}, got, "the steered prompt and the cancel request are answered")
	s.sync(9)

	_, _, _, cancels := h.snapshot()
	assert.Equal(t, []string{b, a, a, a, a}, cancels, "every cancel reaches the handler, open prompt or not")
	require.Eventually(t, func() bool { return s.p.openPromptCount() == 0 }, 10*time.Second, 10*time.Millisecond)
}

// TestProviderAnsweredPromptIsForgotten checks that a prompt answered without a cancel is no longer
// open, so a later cancel of its session closes nothing of it.
func TestProviderAnsweredPromptIsForgotten(t *testing.T) {
	h := &fakeHandler{}
	s := startProvider(t, h)
	sid := s.newSession(1, "/repo")
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[]}}`)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}`, s.nextRaw())
	require.Eventually(t, func() bool { return s.p.openPromptCount() == 0 }, 10*time.Second, 10*time.Millisecond)
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sid + `"}}`)
	s.sync(3)
	_, _, prompts, cancels := h.snapshot()
	require.Len(t, prompts, 1)
	assert.False(t, isClosed(prompts[0].Canceled), "a cancel after the answer does not reach the answered prompt")
	assert.Equal(t, []string{sid}, cancels)
}

// openPrompt starts a prompt whose handler blocks until its context ends and reports that it did.
func openPrompt(t *testing.T) (s *script, h *fakeHandler, ended chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	ended = make(chan struct{})
	h = &fakeHandler{onPrompt: func(ctx context.Context, _ PromptRequest, _ *Updates) StopReason {
		close(started)
		<-ctx.Done()
		close(ended)
		return StopEndTurn
	}}
	s = startProvider(t, h)
	sid := s.newSession(1, "/repo")
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[]}}`)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not start")
	}
	return s, h, ended
}

func TestProviderShutdownNeverCancelsOrAnswers(t *testing.T) {
	s, h, ended := openPrompt(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.p.Shutdown(ctx))
	select {
	case <-ended:
	default:
		t.Fatal("Shutdown returned before the prompt handler")
	}
	s.expectSilence()
	_, _, prompts, cancels := h.snapshot()
	assert.Empty(t, cancels, "shutdown is never reported as a cancel")
	require.Len(t, prompts, 1)
	assert.False(t, isClosed(prompts[0].Canceled), "shutdown never closes Canceled")

	// a stopped provider answers nothing, initialize included
	s.send(t3Initialize)
	s.send(`{"jsonrpc":"2.0","id":9,"method":"session/fork"}`)
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}`)
	s.expectSilence()
	_, _, _, cancels = h.snapshot()
	assert.Empty(t, cancels)

	// a prompt read after the stop starts no handler and is not left open
	assert.Zero(t, s.p.openPromptCount(), "the stopped prompt's handler returned and was forgotten")
	s.p.handlePrompt(json.RawMessage(`99`), json.RawMessage(`{"sessionId":"`+prompts[0].SessionID+`","prompt":[]}`))
	assert.Zero(t, s.p.openPromptCount(), "a prompt the stopped provider never starts is forgotten at once")
	s.expectSilence()
	_, _, prompts, _ = h.snapshot()
	assert.Len(t, prompts, 1)
}

func TestProviderEOFNeverCancelsOrAnswers(t *testing.T) {
	s, h, ended := openPrompt(t)
	require.NoError(t, s.in.Close())
	select {
	case err := <-s.served:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("EOF did not cancel the prompt's context")
	}
	s.expectSilence()
	_, _, prompts, cancels := h.snapshot()
	assert.Empty(t, cancels, "EOF is never reported as a cancel")
	require.Len(t, prompts, 1)
	assert.False(t, isClosed(prompts[0].Canceled), "EOF never closes Canceled")
}

func TestProviderShutdownBoundedByContext(t *testing.T) {
	release := make(chan struct{})
	h := &fakeHandler{onPrompt: func(context.Context, PromptRequest, *Updates) StopReason {
		<-release // ignores its context
		return StopEndTurn
	}}
	s := startProvider(t, h)
	sid := s.newSession(1, "/repo")
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[]}}`)
	require.Eventually(t, func() bool {
		_, _, prompts, _ := h.snapshot()
		return len(prompts) == 1
	}, 10*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.p.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)
	s.expectSilence()
}

func TestProviderUnknownMethods(t *testing.T) {
	s := startProvider(t, &fakeHandler{})
	methods := []string{"authenticate", "session/set_model", "session/set_mode", "session/set_config_option",
		"session/fork", "session/resume", "_x.ai/login", "logout"}
	for i, method := range methods {
		s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`, i+1, method))
		m := s.next()
		assert.InDelta(t, float64(i+1), m["id"], 0)
		code, msg := errorOf(t, m)
		assert.InDelta(t, CodeMethodNotFound, code, 0, method)
		assert.Equal(t, errTextMethodNotFound, msg, "the text never echoes the method name")
	}
}

func TestProviderErrorsNeverReadAsAuthFailures(t *testing.T) {
	s := startProvider(t, &fakeHandler{})
	requests := []string{
		t3Initialize,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":"oops"}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize"}`,
		initializeAs(4, "t3-code-provider-test"),
		initializeAs(5, "t3-code-session-manager"),
		`{"jsonrpc":"2.0","id":6,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":7,"method":"session/new","params":[1,2]}`,
		`{"jsonrpc":"2.0","id":8,"method":"session/load","params":{"sessionId":"x","cwd":"/","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":9,"method":"session/load","params":42}`,
		`{"jsonrpc":"2.0","id":10,"method":"authenticate","params":{"methodId":"login"}}`,
		`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"credentials","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":12,"method":"session/prompt","params":"log in"}`,
		`{"jsonrpc":"2.0","id":13,"method":"session/login"}`,
	}
	mustSucceed := map[float64]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true}
	for _, r := range requests {
		s.send(r)
	}
	for range requests {
		m := s.next()
		id, _ := m["id"].(float64)
		if mustSucceed[id] {
			assert.Contains(t, m, "result", "initialize, session/new, and session/load never fail: %v", m)
			assert.NotContains(t, m, "error")
			continue
		}
		code, msg := errorOf(t, m)
		assert.NotEqual(t, -32000, int(code), "request %v", id)
		assert.False(t, authRE.MatchString(msg), "request %v answered %q", id, msg)
	}
}

func TestProviderSendsNoRequests(t *testing.T) {
	h := &fakeHandler{onPrompt: func(_ context.Context, _ PromptRequest, u *Updates) StopReason {
		assert.NoError(t, u.Plan([]PlanEntry{{Content: "Task 1"}}))
		assert.NoError(t, u.ToolCall("c1", "Task 1/1"))
		assert.NoError(t, u.ToolCallUpdate("c1", ToolCompleted, ""))
		assert.NoError(t, u.AgentMessageChunk("done"))
		return StopEndTurn
	}}
	s := startProvider(t, h)
	s.send(t3Initialize)
	sid := ""
	s.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/repo","mcpServers":[]}}`)
	for range 2 {
		m := s.next()
		if r, ok := m["result"].(map[string]any); ok && r["sessionId"] != nil {
			sid, _ = r["sessionId"].(string)
		}
	}
	require.NotEmpty(t, sid)
	s.send(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"old","cwd":"/repo","mcpServers":[]}}`)
	s.send(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"` + sid + `","prompt":[{"type":"text","text":"go"}]}}`)
	s.send(`{"jsonrpc":"2.0","id":5,"method":"session/cancel","params":{"sessionId":"` + sid + `"}}`)
	s.send(`{"jsonrpc":"2.0","id":6,"method":"session/request_permission","params":{}}`)

	answered := map[float64]bool{}
	for len(answered) < 4 {
		m := s.next()
		method, hasMethod := m["method"]
		if hasMethod {
			assert.Equal(t, "session/update", method, "the only message the provider initiates")
			assert.NotContains(t, m, "id", "the provider sends notifications, never requests")
			continue
		}
		id, _ := m["id"].(float64)
		answered[id] = true
	}
	assert.Equal(t, map[float64]bool{3: true, 4: true, 5: true, 6: true}, answered)
}
