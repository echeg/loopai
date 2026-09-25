package t3

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wsServer is a scripted fake of the T3 Code /ws endpoint. respond receives each decoded client
// message and returns the raw frames to send back.
type wsServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	received []map[string]any
	auth     string
	path     string
}

func newWSServer(t *testing.T, respond func(msg map[string]any) []string) *wsServer {
	t.Helper()
	ws := &wsServer{}
	upgrader := websocket.Upgrader{}
	ws.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.mu.Lock()
		ws.auth = r.Header.Get("Authorization")
		ws.path = r.URL.Path
		ws.mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				return
			}
			ws.mu.Lock()
			ws.received = append(ws.received, msg)
			ws.mu.Unlock()
			for _, frame := range respond(msg) {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (ws *wsServer) messages() []map[string]any {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]map[string]any(nil), ws.received...)
}

func dialTest(t *testing.T, ws *wsServer) *RPCClient {
	t.Helper()
	c, err := DialRPC(context.Background(), Endpoint{BaseURL: ws.srv.URL, Token: "secret"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func successFor(msg map[string]any, value string) string {
	return `{"_tag":"Exit","requestId":"` + msg["id"].(string) + `","exit":{"_tag":"Success","value":` + value + `}}`
}

func TestWebsocketURL(t *testing.T) {
	tests := []struct{ in, want, wantErr string }{
		{in: "http://127.0.0.1:3773", want: "ws://127.0.0.1:3773/ws"},
		{in: "https://host/base/", want: "wss://host/base/ws"},
		{in: "ftp://host", wantErr: "unsupported server url scheme"},
		{in: "http://%zz", wantErr: "parse server url"},
	}
	for _, tc := range tests {
		got, err := websocketURL(tc.in)
		if tc.wantErr != "" {
			require.ErrorContains(t, err, tc.wantErr, tc.in)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestRPCCreateWorktree(t *testing.T) {
	ws := newWSServer(t, func(msg map[string]any) []string {
		return []string{
			`{"_tag":"Chunk","requestId":"99","values":[{}]}`,
			successFor(msg, `{"worktree":{"path":"C:\\wt\\feat","refName":"feat"}}`),
		}
	})
	c := dialTest(t, ws)

	wt, err := c.CreateWorktree(context.Background(), CreateWorktreeInput{Cwd: `C:\repo`, RefName: "main", NewRefName: "feat"})
	require.NoError(t, err)
	assert.Equal(t, Worktree{Path: `C:\wt\feat`, RefName: "feat"}, wt)

	ws.mu.Lock()
	assert.Equal(t, "Bearer secret", ws.auth)
	assert.Equal(t, "/ws", ws.path)
	ws.mu.Unlock()

	msgs := ws.messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, map[string]any{
		"_tag": "Request", "id": "1", "tag": "vcs.createWorktree", "headers": []any{},
		"payload": map[string]any{"cwd": `C:\repo`, "refName": "main", "newRefName": "feat", "path": nil},
	}, msgs[0])
}

func TestRPCTerminalOpenAndWrite(t *testing.T) {
	ws := newWSServer(t, func(msg map[string]any) []string {
		// answer as a batched frame to exercise array decoding
		return []string{`[` + successFor(msg, `{"threadId":"th","terminalId":"term-1"}`) + `]`}
	})
	c := dialTest(t, ws)

	require.NoError(t, c.OpenTerminal(context.Background(), TerminalOpenInput{
		ThreadID: "th", TerminalID: "term-1", Cwd: `C:\wt`, WorktreePath: `C:\wt`,
		Env: map[string]string{"LOOPAI_T3": "true"},
	}))
	require.NoError(t, c.WriteTerminal(context.Background(), "th", "term-1", "loopai --t3 plan.md\r"))

	msgs := ws.messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "terminal.open", msgs[0]["tag"])
	assert.Equal(t, map[string]any{
		"threadId": "th", "terminalId": "term-1", "cwd": `C:\wt`, "worktreePath": `C:\wt`,
		"env": map[string]any{"LOOPAI_T3": "true"},
	}, msgs[0]["payload"])
	assert.Equal(t, "2", msgs[1]["id"])
	assert.Equal(t, "terminal.write", msgs[1]["tag"])
	assert.Equal(t, map[string]any{"threadId": "th", "terminalId": "term-1", "data": "loopai --t3 plan.md\r"}, msgs[1]["payload"])
}

func TestRPCPingIsAnswered(t *testing.T) {
	var pending map[string]any
	ws := newWSServer(t, func(msg map[string]any) []string {
		switch msg["_tag"] {
		case "Request":
			pending = msg
			return []string{`{"_tag":"Ping"}`}
		case "Pong":
			return []string{successFor(pending, `null`)}
		}
		return nil
	})
	c := dialTest(t, ws)
	require.NoError(t, c.WriteTerminal(context.Background(), "th", "term-1", "x"))
	msgs := ws.messages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "Pong", msgs[1]["_tag"])
}

func TestRPCFailures(t *testing.T) {
	tests := []struct {
		name    string
		frame   func(msg map[string]any) string
		wantErr string
	}{
		{
			name: "tagged failure",
			frame: func(msg map[string]any) string {
				return `{"_tag":"Exit","requestId":"` + msg["id"].(string) + `","exit":{"_tag":"Failure","cause":[{"_tag":"Fail","error":{"_tag":"GitCommandError","message":"git worktree add failed"}}]}}`
			},
			wantErr: "vcs.createWorktree failed: GitCommandError: git worktree add failed",
		},
		{
			name: "die",
			frame: func(msg map[string]any) string {
				return `{"_tag":"Exit","requestId":"` + msg["id"].(string) + `","exit":{"_tag":"Failure","cause":[{"_tag":"Die","defect":"boom"}]}}`
			},
			wantErr: `"boom"`,
		},
		{
			name:    "defect",
			frame:   func(map[string]any) string { return `{"_tag":"Defect","defect":{"message":"bad"}}` },
			wantErr: "server defect",
		},
		{
			name:    "garbage",
			frame:   func(map[string]any) string { return `{` },
			wantErr: "decode vcs.createWorktree response",
		},
		{
			name:    "empty path",
			frame:   func(msg map[string]any) string { return successFor(msg, `{"worktree":{"path":""}}`) },
			wantErr: "returned no path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := newWSServer(t, func(msg map[string]any) []string { return []string{tc.frame(msg)} })
			c := dialTest(t, ws)
			_, err := c.CreateWorktree(context.Background(), CreateWorktreeInput{Cwd: "/r", RefName: "main"})
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestRPCContextDeadline(t *testing.T) {
	ws := newWSServer(t, func(map[string]any) []string { return nil })
	c := dialTest(t, ws)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.WriteTerminal(ctx, "th", "term-1", "x")
	require.Error(t, err)
}

func TestRPCContextCancel(t *testing.T) {
	ws := newWSServer(t, func(map[string]any) []string { return nil })
	c := dialTest(t, ws)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := c.WriteTerminal(ctx, "th", "term-1", "x")
	require.ErrorIs(t, err, context.Canceled)
}

func TestDialRPCHandshakeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := DialRPC(context.Background(), Endpoint{BaseURL: srv.URL, Token: "bad"})
	require.Error(t, err)
	assert.True(t, IsAuthError(err))

	_, err = DialRPC(context.Background(), Endpoint{BaseURL: "ftp://x", Token: "t"})
	require.ErrorContains(t, err, "unsupported server url scheme")
}

func TestRPCNilClose(t *testing.T) {
	var c *RPCClient
	assert.NoError(t, c.Close())
}

func TestCauseMessage(t *testing.T) {
	assert.Equal(t, "x", causeMessage(json.RawMessage(`[{"_tag":"Fail","error":{"message":"x"}}]`)))
	assert.Equal(t, "Interrupt", causeMessage(json.RawMessage(`[{"_tag":"Interrupt"}]`)))
	assert.Equal(t, `{"a":1}`, causeMessage(json.RawMessage(`{"a": 1}`)))
	assert.Equal(t, "E", errorMessage(json.RawMessage(`{"_tag":"E"}`)))
	assert.Equal(t, "d", errorMessage(json.RawMessage(`{"detail":"d"}`)))
	assert.Equal(t, `"s"`, errorMessage(json.RawMessage(`"s"`)))
	assert.Equal(t, `{}`, errorMessage(json.RawMessage(`{}`)))
	long := make([]byte, 0, 400)
	long = append(long, '"')
	for range 350 {
		long = append(long, 'a')
	}
	long = append(long, '"')
	assert.Len(t, []rune(compactJSON(long)), 301)
}
