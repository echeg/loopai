package t3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// mcpTestAuthz is the bearer value the fake accepts; no error or formatted value may contain it.
	mcpTestAuthz = "mcp-0f9e8d7c6b5a-a1b2c3"
	// mcpTestThread is an opaque, non-UUID thread id of the kind T3 Code gives subagent threads.
	mcpTestThread  = "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3Aabc%3Aagent%3A3"
	mcpTestSession = "mcp-session-7"
)

// mcpCall is one HTTP request the fake received.
type mcpCall struct {
	httpMethod string
	rpcMethod  string
	header     http.Header
	body       string
}

// fakeMCP is a streamable-HTTP MCP endpoint with T3 Code's request checks: 403 with an Origin, 415
// without a JSON body type, 406 without both Accept types, 401 with the wrong authorization, and 400
// without the session id or protocol version after initialize.
type fakeMCP struct {
	t *testing.T

	sse         bool   // answer requests with server-sent events
	sseNoise    bool   // send a notification event before the response, with CRLF line ends
	result      string // the tools/call result
	omitSession bool   // answer initialize without a session id
	initError   bool   // answer initialize with a JSON-RPC error
	badBody     bool   // answer tools/call with an undecodable body
	wrongID     bool   // answer tools/call with another request's id
	block       bool   // hold every request until the client gives up
	status      map[string]int

	mu      sync.Mutex
	calls   []mcpCall
	deleted bool
}

// threadListResult returns a t3_thread_list result naming threadID (null when nil).
func threadListResult(t *testing.T, threadID any, structured bool) string {
	t.Helper()
	content := map[string]any{"projectId": "project-1", "currentThreadId": threadID, "threads": []any{},
		"nextCursor": nil, "total": 0}
	text, err := json.Marshal(content)
	require.NoError(t, err)
	result := map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": string(text)}}}
	if structured {
		result["structuredContent"] = content
	}
	data, err := json.Marshal(result)
	require.NoError(t, err)
	return string(data)
}

func newFakeMCP(t *testing.T) *fakeMCP {
	t.Helper()
	return &fakeMCP{t: t, result: threadListResult(t, mcpTestThread, true), status: map[string]int{}}
}

// start serves the fake and returns its credential.
func (f *fakeMCP) start() MCPCredential {
	f.t.Helper()
	srv := httptest.NewServer(f)
	f.t.Cleanup(srv.Close)
	return MCPCredential{Endpoint: srv.URL + "/mcp", Authorization: "Bearer " + mcpTestAuthz}
}

func (f *fakeMCP) snapshot() (calls []mcpCall, deleted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcpCall(nil), f.calls...), f.deleted
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(data, &msg)
	f.mu.Lock()
	f.calls = append(f.calls, mcpCall{httpMethod: r.Method, rpcMethod: msg.Method, header: r.Header.Clone(), body: string(data)})
	f.mu.Unlock()

	if f.block {
		<-r.Context().Done()
		return
	}
	if code := f.status[msg.Method]; code != 0 {
		w.WriteHeader(code)
		return
	}
	if code := f.reject(r, msg.Method); code != 0 {
		if code == http.StatusUnauthorized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"invalid_mcp_credential","message":"A valid T3 Code MCP credential is required."}`)
			return
		}
		w.WriteHeader(code)
		return
	}
	switch {
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		f.deleted = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case msg.Method == "initialize":
		if !f.omitSession {
			w.Header().Set(headerMCPSession, mcpTestSession)
		}
		if f.initError {
			f.respond(w, msg.ID, "", `{"code":-32602,"message":"unsupported"}`)
			return
		}
		f.respond(w, msg.ID, `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"T3 Code","version":"0.1.1"}}`, "")
	case msg.Method == "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case msg.Method == "tools/call":
		if f.badBody {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":`)
			return
		}
		id := msg.ID
		if f.wrongID {
			id = json.RawMessage(`"99"`)
		}
		f.respond(w, id, f.result, "")
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

// reject applies T3 Code's transport checks and returns the status of a rejected request, else 0.
func (f *fakeMCP) reject(r *http.Request, rpcMethod string) int {
	if r.Header.Get("Origin") != "" {
		return http.StatusForbidden
	}
	if r.Header.Get("Authorization") != "Bearer "+mcpTestAuthz {
		return http.StatusUnauthorized
	}
	if r.Method == http.MethodPost {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			return http.StatusUnsupportedMediaType
		}
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			return http.StatusNotAcceptable
		}
	}
	if rpcMethod == "initialize" {
		if r.Header.Get(headerMCPSession) != "" {
			return http.StatusBadRequest
		}
		return 0
	}
	if r.Header.Get(headerMCPSession) != mcpTestSession || r.Header.Get(headerMCPProtocolVersion) != mcpProtocolVersion {
		return http.StatusBadRequest
	}
	return 0
}

// respond writes a JSON-RPC response as JSON or as server-sent events.
func (f *fakeMCP) respond(w http.ResponseWriter, id json.RawMessage, result, rpcErr string) {
	body := `{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + result + `}`
	if rpcErr != "" {
		body = `{"jsonrpc":"2.0","id":` + string(id) + `,"error":` + rpcErr + `}`
	}
	if !f.sse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if f.sseNoise {
		_, _ = io.WriteString(w, "event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\r\n"+
			"data: \"params\":{\"level\":\"info\"}}\r\n\r\n: keep-alive\r\n\r\n")
		_, _ = io.WriteString(w, "event: message\r\ndata: "+body+"\r\n\r\n")
		return
	}
	_, _ = io.WriteString(w, "event: message\ndata: "+body+"\n\n")
}

func TestMCPClientCurrentThreadID(t *testing.T) {
	tests := []struct {
		name     string
		sse      bool
		sseNoise bool
	}{
		{name: "json replies"},
		{name: "sse replies", sse: true},
		{name: "sse with a notification first", sse: true, sseNoise: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeMCP(t)
			f.sse, f.sseNoise = tt.sse, tt.sseNoise
			cred := f.start()

			id, err := NewMCPClient(cred, "1.2.3").CurrentThreadID(context.Background())
			require.NoError(t, err)
			assert.Equal(t, mcpTestThread, id)

			calls, deleted := f.snapshot()
			assert.True(t, deleted, "the session is ended")
			require.Len(t, calls, 4)
			assert.Equal(t, "initialize", calls[0].rpcMethod)
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":"2025-06-18",`+
				`"capabilities":{},"clientInfo":{"name":"loopai","version":"1.2.3"}}}`, calls[0].body)
			assert.Empty(t, calls[0].header.Get(headerMCPSession), "initialize carries no session id")
			assert.JSONEq(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, calls[1].body)
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t3_thread_list","arguments":{"limit":1}}}`,
				calls[2].body)
			assert.Equal(t, http.MethodDelete, calls[3].httpMethod)
			for i, c := range calls {
				assert.Equal(t, cred.Authorization, c.header.Get("Authorization"), "call %d", i)
				assert.Empty(t, c.header.Get("Origin"), "call %d", i)
				if i > 0 {
					assert.Equal(t, mcpTestSession, c.header.Get(headerMCPSession), "call %d", i)
					assert.Equal(t, mcpProtocolVersion, c.header.Get(headerMCPProtocolVersion), "call %d", i)
				}
				if c.httpMethod == http.MethodPost {
					assert.Equal(t, "application/json, text/event-stream", c.header.Get("Accept"), "call %d", i)
					assert.Equal(t, "application/json", c.header.Get("Content-Type"), "call %d", i)
				}
			}
		})
	}
}

func TestMCPClientReadsContentTextWithoutStructuredContent(t *testing.T) {
	f := newFakeMCP(t)
	f.result = threadListResult(t, "thread-from-text", false)
	id, err := NewMCPClient(f.start(), "v").CurrentThreadID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "thread-from-text", id)
}

func TestMCPClientNoCurrentThread(t *testing.T) {
	tests := []struct {
		name   string
		result func(t *testing.T) string
	}{
		{name: "null thread", result: func(t *testing.T) string { return threadListResult(t, nil, true) }},
		{name: "null thread in text only", result: func(t *testing.T) string { return threadListResult(t, nil, false) }},
		{name: "empty thread", result: func(t *testing.T) string { return threadListResult(t, "", true) }},
		{name: "no content", result: func(*testing.T) string { return `{"isError":false,"content":[]}` }},
		{name: "text that is not json", result: func(*testing.T) string {
			return `{"isError":false,"content":[{"type":"text","text":"threads: 0"}]}`
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeMCP(t)
			f.result = tt.result(t)
			id, err := NewMCPClient(f.start(), "v").CurrentThreadID(context.Background())
			require.ErrorIs(t, err, ErrNoMCPThread)
			assert.Empty(t, id)
		})
	}
}

func TestMCPClientFailures(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fakeMCP, cred *MCPCredential)
		wantErr string
	}{
		{name: "401 for another token", wantErr: "HTTP 401",
			setup: func(_ *fakeMCP, cred *MCPCredential) { cred.Authorization = "Bearer revoked-" + mcpTestAuthz }},
		{name: "406 on initialize", wantErr: "HTTP 406",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.status["initialize"] = http.StatusNotAcceptable }},
		{name: "415 on initialize", wantErr: "HTTP 415",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.status["initialize"] = http.StatusUnsupportedMediaType }},
		{name: "404 for a lost session", wantErr: "HTTP 404",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.status["tools/call"] = http.StatusNotFound }},
		{name: "initialized refused", wantErr: "HTTP 400",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.status["notifications/initialized"] = http.StatusBadRequest }},
		{name: "initialize without a session id", wantErr: "no session id",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.omitSession = true }},
		{name: "initialize answered with an error", wantErr: "error -32602",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.initError = true }},
		{name: "undecodable reply", wantErr: "no response",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.badBody = true }},
		{name: "tool reports an error", wantErr: "t3_thread_list reported an error",
			setup: func(f *fakeMCP, _ *MCPCredential) {
				f.result = `{"isError":true,"content":[{"type":"text","text":"denied"}]}`
			}},
		{name: "json reply for another id", wantErr: "reply has no response",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.wrongID = true }},
		{name: "event stream without the response", wantErr: "event stream has no response",
			setup: func(f *fakeMCP, _ *MCPCredential) { f.sse, f.sseNoise, f.wrongID = true, true, true }},
		{name: "no endpoint", wantErr: "no endpoint configured",
			setup: func(_ *fakeMCP, cred *MCPCredential) { cred.Endpoint = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeMCP(t)
			cred := f.start()
			tt.setup(f, &cred)
			id, err := NewMCPClient(cred, "v").CurrentThreadID(context.Background())
			require.Error(t, err)
			assert.Empty(t, id, "any failure means no thread id")
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), mcpTestAuthz, "errors never carry the authorization")
		})
	}
}

func TestMCPClientTimeout(t *testing.T) {
	f := newFakeMCP(t)
	f.block = true
	cred := f.start()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	id, err := NewMCPClient(cred, "v").CurrentThreadID(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, id)
	assert.Less(t, time.Since(start), 5*time.Second, "the caller's deadline bounds the lookup")
	assert.NotContains(t, err.Error(), mcpTestAuthz)
}

func TestMCPClientUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL + "/mcp"
	srv.Close()
	id, err := NewMCPClient(MCPCredential{Endpoint: endpoint, Authorization: "Bearer " + mcpTestAuthz}, "v").
		CurrentThreadID(context.Background())
	require.Error(t, err)
	assert.Empty(t, id)
	assert.NotContains(t, err.Error(), mcpTestAuthz)
}

func TestMCPClientNeverFollowsRedirects(t *testing.T) {
	var leaked sync.Map
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"), true)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	id, err := NewMCPClient(MCPCredential{Endpoint: redirect.URL + "/mcp", Authorization: "Bearer " + mcpTestAuthz}, "v").
		CurrentThreadID(context.Background())
	leaked.Range(func(key, _ any) bool {
		t.Errorf("redirect target received a request with authorization %q", key)
		return true
	})
	require.ErrorContains(t, err, "HTTP 307")
	assert.Empty(t, id)
}

func TestFindMCPResponse(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantOK  bool
		want    string
	}{
		{name: "matching response", payload: `{"jsonrpc":"2.0","id":"2","result":{"a":1}}`, wantOK: true, want: `{"a":1}`},
		{name: "another id", payload: `{"jsonrpc":"2.0","id":"1","result":{}}`},
		{name: "numeric id is another id", payload: `{"jsonrpc":"2.0","id":2,"result":{}}`},
		{name: "batch with a notification first", wantOK: true, want: `{"b":2}`,
			payload: ` [{"jsonrpc":"2.0","method":"notifications/message","params":{}},{"jsonrpc":"2.0","id":"2","result":{"b":2}}]`},
		{name: "request echoing the id is not a response", payload: `{"jsonrpc":"2.0","id":"2","method":"ping"}`},
		{name: "undecodable batch", payload: `[{"id":`},
		{name: "undecodable message", payload: `{"id":`},
		{name: "empty", payload: ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, ok := findMCPResponse([]byte(tt.payload), "2")
			require.Equal(t, tt.wantOK, ok)
			if ok {
				assert.JSONEq(t, tt.want, string(env.Result))
			}
		})
	}
}

func TestMCPCredentialFromServers(t *testing.T) {
	entry := func(name, endpoint, auth string) string {
		var env []string
		if endpoint != "" {
			env = append(env, fmt.Sprintf(`{"name":"T3_ACP_MCP_ENDPOINT","value":%q}`, endpoint))
		}
		if auth != "" {
			env = append(env, fmt.Sprintf(`{"name":"T3_ACP_MCP_AUTHORIZATION","value":%q}`, auth))
		}
		return fmt.Sprintf(`{"name":%q,"command":"/Applications/T3 Code.app/t3","args":["--x","acp-mcp-bridge"],"env":[`+
			`{"name":"ELECTRON_RUN_AS_NODE","value":"1"},%s]}`, name, strings.Join(env, ","))
	}
	fromParams := MCPCredential{Endpoint: "http://127.0.0.1:3773/mcp", Authorization: "Bearer params-token"}
	fromEnv := MCPCredential{Endpoint: "http://127.0.0.1:3773/mcp", Authorization: "Bearer env-token"}
	envVars := map[string]string{EnvMCPEndpoint: fromEnv.Endpoint, EnvMCPAuthorization: fromEnv.Authorization}

	tests := []struct {
		name    string
		servers string
		env     map[string]string
		want    MCPCredential
		wantOK  bool
	}{
		{name: "params win over env", servers: "[" + entry("t3-code", fromParams.Endpoint, fromParams.Authorization) + "]",
			env: envVars, want: fromParams, wantOK: true},
		{name: "params without env", servers: "[" + entry("other", "http://x", "Bearer other") + "," +
			entry("t3-code", fromParams.Endpoint, fromParams.Authorization) + "]", want: fromParams, wantOK: true},
		{name: "env when no t3-code entry", servers: "[" + entry("other", "http://x", "Bearer other") + "]",
			env: envVars, want: fromEnv, wantOK: true},
		{name: "env when servers are empty", servers: "[]", env: envVars, want: fromEnv, wantOK: true},
		{name: "env when servers are absent", env: envVars, want: fromEnv, wantOK: true},
		{name: "env when the entry lacks the authorization",
			servers: "[" + entry("t3-code", fromParams.Endpoint, "") + "]", env: envVars, want: fromEnv, wantOK: true},
		{name: "env when servers do not decode", servers: `{"name":"t3-code"}`, env: envVars, want: fromEnv, wantOK: true},
		{name: "undecodable entries are skipped", servers: `[42,"x",` + entry("t3-code", fromParams.Endpoint, fromParams.Authorization) + "]",
			want: fromParams, wantOK: true},
		{name: "nothing anywhere", servers: "[]"},
		{name: "env with the endpoint only", env: map[string]string{EnvMCPEndpoint: fromEnv.Endpoint}},
		{name: "incomplete entry and no env", servers: "[" + entry("t3-code", "", fromParams.Authorization) + "]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			got, ok := MCPCredentialFromServers(json.RawMessage(tt.servers), getenv)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}

	got, ok := MCPCredentialFromServers(json.RawMessage("["+entry("t3-code", fromParams.Endpoint, fromParams.Authorization)+"]"), nil)
	assert.True(t, ok, "a nil getenv still reads the params")
	assert.Equal(t, fromParams, got)
	_, ok = MCPCredentialFromServers(nil, nil)
	assert.False(t, ok)
}

func TestMCPCredentialFormattingRedacts(t *testing.T) {
	cred := MCPCredential{Endpoint: "http://127.0.0.1:3773/mcp", Authorization: "Bearer " + mcpTestAuthz}
	for _, verb := range []string{"%v", "%+v", "%s", "%#v", "%q"} {
		out := fmt.Sprintf(verb, cred)
		assert.NotContains(t, out, mcpTestAuthz, verb)
		assert.Contains(t, out, cred.Endpoint, verb)
	}
	assert.Equal(t, "{Endpoint:http://127.0.0.1:3773/mcp Authorization:[redacted]}", cred.String())
	assert.Equal(t, "{Endpoint: Authorization:}", MCPCredential{}.String())
}
