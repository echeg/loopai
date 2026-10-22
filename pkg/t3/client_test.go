package t3

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/t3/t3test"
)

// newTestClient points the T3 home at a temp directory holding s's runtime file and connects.
func newTestClient(t *testing.T, s *t3test.Server, opts ...Option) *Client {
	t.Helper()
	home := useHome(t)
	require.NoError(t, s.WriteRuntime(home))
	c := NewClient(s.Token(), opts...)
	_, err := c.Connect(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// keys returns the sorted keys of a recorded payload.
func keys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

func TestConnectHandshakeCarriesQueryAndBearer(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithToken("secret-token"))
	home := useHome(t)
	require.NoError(t, s.WriteRuntime(home))
	c := NewClient("secret-token")
	defer c.Close()

	rt, err := c.Connect(t.Context())
	require.NoError(t, err)
	assert.Equal(t, s.URL(), rt.Origin)
	assert.NotZero(t, rt.PID)
	assert.NotEmpty(t, rt.StartedAt)
	assert.Equal(t, "0.1.1", rt.ServerVersion)

	handshakes := s.Handshakes()
	require.Len(t, handshakes, 1)
	assert.Equal(t, http.StatusSwitchingProtocols, handshakes[0].Status)
	assert.Equal(t, "2", handshakes[0].Query.Get("orchestrationProtocol"))
	assert.Equal(t, "Bearer secret-token", handshakes[0].Authorization)
}

func TestConnectRereadsRuntimeOnReconnect(t *testing.T) {
	first := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "First"}))
	second := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "Second"}))
	home := useHome(t)
	require.NoError(t, first.WriteRuntime(home))
	c := NewClient(t3test.DefaultToken)
	defer c.Close()

	rt, err := c.Connect(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first.URL(), rt.Origin)
	sub, err := c.SubscribeTerminalMetadata(t.Context())
	require.NoError(t, err)
	_, err = sub.Next()
	require.NoError(t, err)

	require.NoError(t, second.WriteRuntime(home)) // the server restarted on another port
	assert.NotEqual(t, first.URL(), second.URL())
	rt, err = c.Connect(t.Context())
	require.NoError(t, err)
	assert.Equal(t, second.URL(), rt.Origin)
	assert.Len(t, second.Handshakes(), 1)

	thread, err := c.GetThreadProjection(t.Context(), "th")
	require.NoError(t, err)
	assert.Equal(t, "Second", thread.Title, "calls go to the server the fresh runtime file names")
	_, err = sub.Next()
	require.ErrorIs(t, err, ErrClosed, "a reconnect closes the previous connection")
}

func TestConnectLOOPAIURLWins(t *testing.T) {
	runtimeServer := t3test.NewServer(t)
	urlServer := t3test.NewServer(t)
	home := useHome(t)
	require.NoError(t, runtimeServer.WriteRuntime(home))
	t.Setenv(EnvURL, urlServer.URL())
	c := NewClient(t3test.DefaultToken)
	defer c.Close()

	rt, err := c.Connect(t.Context())
	require.NoError(t, err)
	assert.Equal(t, urlServer.URL(), rt.Origin)
	assert.Len(t, urlServer.Handshakes(), 1)
	assert.Empty(t, runtimeServer.Handshakes())
}

func TestConnectErrors(t *testing.T) {
	t.Run("missing runtime file", func(t *testing.T) {
		useHome(t)
		_, err := NewClient("tok").Connect(t.Context())
		require.ErrorIs(t, err, ErrNotRunning)
	})
	t.Run("426 incompatible protocol", func(t *testing.T) {
		s := t3test.NewServer(t, t3test.WithProtocolVersion("3"))
		require.NoError(t, s.WriteRuntime(useHome(t)))
		_, err := NewClient(s.Token()).Connect(t.Context())
		require.ErrorIs(t, err, ErrProtocolUnsupported)
		var herr *HandshakeError
		require.ErrorAs(t, err, &herr)
		assert.Equal(t, http.StatusUpgradeRequired, herr.StatusCode)
		assert.Equal(t, "orchestration_protocol_incompatible", herr.Code)
		assert.Equal(t, "Update this client to one that supports orchestration protocol 3.", herr.Message)
		assert.Contains(t, err.Error(), "426")
	})
	t.Run("401 wrong token", func(t *testing.T) {
		s := t3test.NewServer(t)
		require.NoError(t, s.WriteRuntime(useHome(t)))
		_, err := NewClient("wrong-secret").Connect(t.Context())
		var herr *HandshakeError
		require.ErrorAs(t, err, &herr)
		assert.Equal(t, http.StatusUnauthorized, herr.StatusCode)
		assert.Equal(t, "auth_invalid", herr.Code)
		assert.Equal(t, "invalid_credential", herr.Reason)
		require.NotErrorIs(t, err, ErrProtocolUnsupported)
		assert.NotContains(t, err.Error(), "wrong-secret", "an error never carries the token")
	})
	t.Run("401 forced on the next connection only", func(t *testing.T) {
		s := t3test.NewServer(t)
		c := newTestClient(t, s)
		s.RejectNextConnection()
		_, err := c.Connect(t.Context())
		var herr *HandshakeError
		require.ErrorAs(t, err, &herr)
		assert.Equal(t, "auth_invalid", herr.Code)
		_, err = c.Connect(t.Context())
		require.NoError(t, err)
	})
	t.Run("connection refused", func(t *testing.T) {
		home := useHome(t)
		writeRuntimeFile(t, home, `{"version":1,"pid":1,"port":9,"origin":"http://127.0.0.1:9","startedAt":"s"}`)
		_, err := NewClient("tok").Connect(t.Context())
		require.Error(t, err)
		var herr *HandshakeError
		assert.NotErrorAs(t, err, &herr)
	})
}

func TestCallsWithoutConnection(t *testing.T) {
	useHome(t)
	c := NewClient("tok")
	_, err := c.GetThreadProjection(t.Context(), "th")
	require.ErrorIs(t, err, ErrClosed)
	require.ErrorIs(t, c.Pin(t.Context(), "th"), ErrClosed)
	_, err = c.SubscribeTerminalMetadata(t.Context())
	require.ErrorIs(t, err, ErrClosed)
	_, err = c.Projects(t.Context())
	require.ErrorIs(t, err, ErrClosed)
	require.NoError(t, c.Close())
}

func TestTerminalOpenPayload(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)

	session, err := c.TerminalOpen(t.Context(), TerminalOpenInput{
		ThreadID: "th", TerminalID: "loopai-run-1", Cwd: "/repo",
		Env: map[string]string{"T3CODE_PROJECT_ROOT": "/repo"},
	})
	require.NoError(t, err)
	assert.Equal(t, TerminalRunning, session.Status)
	assert.Equal(t, "loopai-run-1", session.TerminalID)
	assert.Nil(t, session.WorktreePath)
	require.NotNil(t, session.PID)

	_, err = c.TerminalOpen(t.Context(), TerminalOpenInput{
		ThreadID: "th", TerminalID: "loopai-run-2", Cwd: "/wt", WorktreePath: "/wt", Cols: 100, Rows: 40,
		Env: map[string]string{"T3CODE_PROJECT_ROOT": "/repo", "T3CODE_WORKTREE_PATH": "/wt"},
	})
	require.NoError(t, err)

	opens := s.Opens()
	require.Len(t, opens, 2)
	assert.Equal(t, []string{"cwd", "env", "terminalId", "threadId"}, keys(opens[0]),
		"worktreePath, cols, and rows are omitted when unset, and providerInstanceId is never sent")
	assert.Equal(t, map[string]any{"threadId": "th", "terminalId": "loopai-run-1", "cwd": "/repo",
		"env": map[string]any{"T3CODE_PROJECT_ROOT": "/repo"}}, opens[0])
	assert.Equal(t, []string{"cols", "cwd", "env", "rows", "terminalId", "threadId", "worktreePath"}, keys(opens[1]))
	assert.Equal(t, "/wt", opens[1]["worktreePath"])
	assert.InDelta(t, 100, opens[1]["cols"], 0)
}

func TestTerminalOpenUndecodableReply(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)
	for _, reply := range []any{map[string]any{"status": 1}, map[string]any{"cwd": "/r"}, nil} {
		s.ReplyNext(methodTerminalOpen, reply)
		_, err := c.TerminalOpen(t.Context(), TerminalOpenInput{ThreadID: "th", TerminalID: "t", Cwd: "/r"})
		require.ErrorIs(t, err, ErrProtocolUnsupported, "reply %v", reply)
	}
}

func TestTerminalIDValidation(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)
	for _, id := range []string{"", "term-1", strings.Repeat("x", 129)} {
		_, err := c.TerminalOpen(t.Context(), TerminalOpenInput{ThreadID: "th", TerminalID: id, Cwd: "/r"})
		require.Error(t, err, "id %q", id)
		require.Error(t, c.TerminalWrite(t.Context(), "th", id, "x"), "id %q", id)
	}
	_, err := c.TerminalOpen(t.Context(), TerminalOpenInput{ThreadID: "th", TerminalID: "t"})
	require.ErrorContains(t, err, "cwd")
	require.Error(t, c.TerminalWrite(t.Context(), "th", "t", ""))
	require.Error(t, c.TerminalWrite(t.Context(), "th", "t", strings.Repeat("x", 65537)))
	assert.Empty(t, s.Requests(), "invalid input never reaches the server")
}

func TestTerminalWrite(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithTerminal(t3test.Terminal{ThreadID: "th", TerminalID: "t", Cwd: "/r"}))
	c := newTestClient(t, s)
	require.NoError(t, c.TerminalWrite(t.Context(), "th", "t", " loopai --attach run-1\r"))
	writes := s.Writes()
	require.Len(t, writes, 1)
	assert.Equal(t, map[string]any{"threadId": "th", "terminalId": "t", "data": " loopai --attach run-1\r"}, writes[0])
}

func TestTerminalWriteFailExit(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithTerminal(t3test.Terminal{ThreadID: "th", TerminalID: "t", Cwd: "/r",
		Status: TerminalStarting}))
	c := newTestClient(t, s)

	err := c.TerminalWrite(t.Context(), "th", "t", "x")
	var remote *RemoteError
	require.ErrorAs(t, err, &remote)
	assert.Equal(t, methodTerminalWrite, remote.Method)
	assert.Equal(t, "TerminalNotRunningError", remote.Tag)
	assert.JSONEq(t, `{"_tag":"TerminalNotRunningError","threadId":"th","terminalId":"t"}`, string(remote.Raw))
	assert.Equal(t, "t3 terminal.write: TerminalNotRunningError", err.Error())

	err = c.TerminalWrite(t.Context(), "th", "gone", "x")
	require.ErrorAs(t, err, &remote)
	assert.Equal(t, "TerminalSessionLookupError", remote.Tag)
}

func TestSubscribeTerminalMetadata(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithTerminal(t3test.Terminal{ThreadID: "th", TerminalID: "old", Cwd: "/r"}))
	c := newTestClient(t, s)
	sub, err := c.SubscribeTerminalMetadata(t.Context())
	require.NoError(t, err)
	defer sub.Close()

	event, err := sub.Next()
	require.NoError(t, err)
	assert.Equal(t, MetadataSnapshot, event.Type)
	require.Len(t, event.Terminals, 1)
	assert.Equal(t, "old", event.Terminals[0].TerminalID)
	assert.Equal(t, TerminalRunning, event.Terminals[0].Status)

	s.Upsert(t3test.Terminal{ThreadID: "th", TerminalID: "new", Cwd: "/r", WorktreePath: "/wt", HasRunningSubprocess: true})
	s.Upsert(t3test.Terminal{ThreadID: "th", TerminalID: "new", Cwd: "/r", WorktreePath: "/wt"})
	s.Remove("th", "old")

	event, err = sub.Next()
	require.NoError(t, err)
	assert.Equal(t, MetadataUpsert, event.Type)
	assert.Equal(t, "new", event.Terminal.TerminalID)
	assert.True(t, event.Terminal.HasRunningSubprocess)
	require.NotNil(t, event.Terminal.WorktreePath)
	assert.Equal(t, "/wt", *event.Terminal.WorktreePath)

	event, err = sub.Next()
	require.NoError(t, err)
	assert.Equal(t, MetadataUpsert, event.Type)
	assert.False(t, event.Terminal.HasRunningSubprocess)

	event, err = sub.Next()
	require.NoError(t, err)
	assert.Equal(t, TerminalMetadataEvent{Type: MetadataRemove, ThreadID: "th", TerminalID: "old"}, event)
	require.Eventually(t, func() bool { return s.Acks() == 4 }, 10*time.Second, 10*time.Millisecond,
		"the client acknowledges every chunk, the snapshot included")
}

func TestSubscribeTerminalMetadataUnsubscribe(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)

	sub, err := c.SubscribeTerminalMetadata(t.Context())
	require.NoError(t, err)
	_, err = sub.Next()
	require.NoError(t, err)
	require.NoError(t, sub.Close())
	_, err = sub.Next()
	require.Error(t, err)
	require.Eventually(t, func() bool { return s.Interrupts() == 1 }, 10*time.Second, 10*time.Millisecond,
		"Close unsubscribes with an Interrupt")
	require.NoError(t, sub.Close(), "a second Close sends nothing")

	ctx, cancel := context.WithCancel(t.Context())
	sub, err = c.SubscribeTerminalMetadata(ctx)
	require.NoError(t, err)
	_, err = sub.Next()
	require.NoError(t, err)
	cancel()
	_, err = sub.Next()
	require.ErrorIs(t, err, context.Canceled)
	require.Eventually(t, func() bool { return s.Interrupts() == 2 }, 10*time.Second, 10*time.Millisecond,
		"canceling the context unsubscribes with an Interrupt")
}

func TestSubscribeTerminalMetadataConnectionLost(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)
	sub, err := c.SubscribeTerminalMetadata(t.Context())
	require.NoError(t, err)
	_, err = sub.Next()
	require.NoError(t, err)

	require.NoError(t, s.Restart())
	_, err = sub.Next()
	require.ErrorIs(t, err, ErrClosed)
	var terr *TransportError
	require.ErrorAs(t, err, &terr)
}

func TestDecodeMetadataEvent(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    TerminalMetadataEvent
		known   bool
		wantErr bool
	}{
		{name: "empty snapshot", raw: `{"type":"snapshot","terminals":[]}`,
			want: TerminalMetadataEvent{Type: MetadataSnapshot, Terminals: []TerminalSummary{}}, known: true},
		{name: "remove", raw: `{"type":"remove","threadId":"a:b%3A","terminalId":"t"}`,
			want: TerminalMetadataEvent{Type: MetadataRemove, ThreadID: "a:b%3A", TerminalID: "t"}, known: true},
		{name: "unknown type is skipped", raw: `{"type":"renamed","terminalId":"t"}`},
		{name: "snapshot without terminals", raw: `{"type":"snapshot"}`, wantErr: true},
		{name: "upsert without terminal", raw: `{"type":"upsert"}`, wantErr: true},
		{name: "remove without ids", raw: `{"type":"remove","threadId":"a"}`, wantErr: true},
		{name: "wrong shape", raw: `{"type":"upsert","terminal":"x"}`, wantErr: true},
		{name: "not an object", raw: `[1]`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, known, err := decodeMetadataEvent([]byte(tc.raw))
			if tc.wantErr {
				require.ErrorIs(t, err, ErrProtocolUnsupported)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.known, known)
			if tc.known {
				assert.Equal(t, tc.want, event)
			}
		})
	}
}

var commandIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestDispatchPayloads(t *testing.T) {
	const threadID = "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3Aagent%3A3"
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: threadID, ProjectID: "p", Title: "My thread",
		TitleRegenerating: true}))
	c := newTestClient(t, s)

	require.NoError(t, c.SetTitle(t.Context(), threadID, "[loopai: task 1/2] My thread"))
	require.NoError(t, c.Pin(t.Context(), threadID))
	th, _ := s.Thread(threadID)
	assert.Equal(t, "[loopai: task 1/2] My thread", th.Title)
	assert.NotEmpty(t, th.PinnedAt)
	require.NoError(t, c.Unpin(t.Context(), threadID))
	require.NoError(t, c.SetTitle(t.Context(), threadID, "[loopai: task 1/2] My thread"))
	th, _ = s.Thread(threadID)
	assert.Empty(t, th.PinnedAt)

	dispatches := s.Dispatches()
	require.Len(t, dispatches, 4)
	assert.Equal(t, []string{"commandId", "threadId", "title", "type"}, keys(dispatches[0]), "title only")
	assert.Equal(t, "thread.metadata.update", dispatches[0]["type"])
	assert.Equal(t, threadID, dispatches[0]["threadId"], "the thread id reaches the payload unchanged")
	assert.Equal(t, []string{"commandId", "threadId", "type"}, keys(dispatches[1]))
	assert.Equal(t, "thread.pin", dispatches[1]["type"])
	assert.Equal(t, []string{"commandId", "threadId", "type"}, keys(dispatches[2]))
	assert.Equal(t, "thread.unpin", dispatches[2]["type"])

	seen := map[any]bool{}
	for _, d := range dispatches {
		id, ok := d["commandId"].(string)
		require.True(t, ok)
		assert.Regexp(t, commandIDPattern, id)
		assert.False(t, seen[id], "every logical update gets a fresh command id, a repeated title included")
		seen[id] = true
	}
}

func TestDispatchRetryReusesCommandID(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := newTestClient(t, s)

	s.DieNext(methodDispatchCommand, "lost")
	err := c.SetTitle(t.Context(), "th", "[loopai: review] T")
	var terr *TransportError
	require.ErrorAs(t, err, &terr, "a Die leaves the outcome unknown")

	_, err = c.Connect(t.Context()) // the retry state survives a reconnect
	require.NoError(t, err)
	require.NoError(t, c.SetTitle(t.Context(), "th", "[loopai: review] T"))
	require.NoError(t, c.SetTitle(t.Context(), "th", "[loopai: review] T"))

	s.DieNext(methodDispatchCommand, "lost")
	require.Error(t, c.Pin(t.Context(), "th"))
	require.NoError(t, c.SetTitle(t.Context(), "th", "[loopai: done] T"))
	require.NoError(t, c.Pin(t.Context(), "th"))

	ids := make([]string, 0, 6)
	for _, d := range s.Dispatches() {
		ids = append(ids, d["commandId"].(string))
	}
	require.Len(t, ids, 6)
	assert.Equal(t, ids[0], ids[1], "a retry of the same update reuses the command id")
	assert.NotEqual(t, ids[1], ids[2], "the same title after a known outcome is a new update")
	assert.NotEqual(t, ids[3], ids[4])
	assert.NotEqual(t, ids[3], ids[5], "another update in between makes the pin a new command")
}

func TestDispatchFailExit(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T",
		ArchivedAt: "2026-10-01T00:00:00.000Z"}))
	c := newTestClient(t, s)

	err := c.Pin(t.Context(), "th")
	var remote *RemoteError
	require.ErrorAs(t, err, &remote)
	assert.Equal(t, "OrchestrationV2DispatchCommandError", remote.Tag)
	assert.Equal(t, "Archived threads cannot be pinned or unpinned.", remote.Message)
	assert.Equal(t, "t3 orchestration.dispatchCommand: OrchestrationV2DispatchCommandError: "+
		"Archived threads cannot be pinned or unpinned.", err.Error())

	require.ErrorAs(t, c.Pin(t.Context(), "th"), &remote)
	dispatches := s.Dispatches()
	require.Len(t, dispatches, 2)
	assert.NotEqual(t, dispatches[0]["commandId"], dispatches[1]["commandId"], "a typed failure is a definite answer")

	require.ErrorContains(t, c.SetTitle(t.Context(), "th", ""), "empty")
}

func TestGetThreadProjection(t *testing.T) {
	s := t3test.NewServer(t,
		t3test.WithThread(t3test.Thread{ID: "plain", ProjectID: "p1", Title: "Plain"}),
		t3test.WithThread(t3test.Thread{ID: "busy", ProjectID: "p2", Title: "Busy", WorktreePath: "/wt",
			PinnedAt: "2026-10-22T12:30:00.000Z", ArchivedAt: "2026-10-23T08:00:00Z", TitleRegenerating: true}))
	c := newTestClient(t, s)

	plain, err := c.GetThreadProjection(t.Context(), "plain")
	require.NoError(t, err)
	assert.Equal(t, ThreadProjection{ID: "plain", ProjectID: "p1", Title: "Plain"}, plain)

	busy, err := c.GetThreadProjection(t.Context(), "busy")
	require.NoError(t, err)
	assert.Equal(t, "Busy", busy.Title)
	assert.Equal(t, "p2", busy.ProjectID)
	require.NotNil(t, busy.WorktreePath)
	assert.Equal(t, "/wt", *busy.WorktreePath)
	require.NotNil(t, busy.PinnedAt)
	assert.Equal(t, time.Date(2026, 10, 22, 12, 30, 0, 0, time.UTC), busy.PinnedAt.UTC())
	require.NotNil(t, busy.ArchivedAt)
	require.NotNil(t, busy.TitleRegeneration)
	assert.NotEmpty(t, busy.TitleRegeneration.RequestID)
	assert.False(t, busy.TitleRegeneration.StartedAt.IsZero())

	requests := s.Requests()
	require.Len(t, requests, 2)
	assert.JSONEq(t, `{"threadId":"plain"}`, string(requests[0].Payload))
}

func TestGetThreadProjectionErrors(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := newTestClient(t, s)

	_, err := c.GetThreadProjection(t.Context(), "missing")
	var remote *RemoteError
	require.ErrorAs(t, err, &remote)
	assert.Equal(t, "OrchestrationV2GetThreadProjectionError", remote.Tag)
	assert.Equal(t, "Failed to load orchestration V2 thread missing", remote.Message)

	for _, reply := range []any{
		map[string]any{"thread": 42},
		map[string]any{"thread": map[string]any{"id": "th", "projectId": "p"}},
		map[string]any{"thread": map[string]any{"id": "th", "title": "T"}},
		map[string]any{"thread": map[string]any{"id": "th", "projectId": "p", "title": "T", "pinnedAt": "yesterday"}},
		map[string]any{"runs": []any{}},
		nil,
	} {
		s.ReplyNext(methodGetThreadProjection, reply)
		_, replyErr := c.GetThreadProjection(t.Context(), "th")
		require.ErrorIs(t, replyErr, ErrProtocolUnsupported, "reply %v", reply)
	}
	_, err = c.GetThreadProjection(t.Context(), "th")
	require.NoError(t, err, "a decode failure does not break the connection")
}

func TestProjects(t *testing.T) {
	s := t3test.NewServer(t,
		t3test.WithProject(t3test.Project{ID: "p1", Title: "One", WorkspaceRoot: "/repo/one"}),
		t3test.WithProject(t3test.Project{ID: "p2", Title: "Two", WorkspaceRoot: "/repo/two"}))
	c := newTestClient(t, s)
	projects, err := c.Projects(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []Project{{ID: "p1", WorkspaceRoot: "/repo/one"}, {ID: "p2", WorkspaceRoot: "/repo/two"}}, projects)
}

// projectsServer serves an idle /ws and a fixed /api/projects reply.
func projectsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProjectsErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		check  func(t *testing.T, err error)
	}{
		{name: "401", status: http.StatusUnauthorized,
			body: `{"_tag":"EnvironmentAuthInvalidError","code":"auth_invalid","reason":"invalid_credential"}`,
			check: func(t *testing.T, err error) {
				var herr *HandshakeError
				require.ErrorAs(t, err, &herr)
				assert.Equal(t, http.StatusUnauthorized, herr.StatusCode)
				assert.Equal(t, "invalid_credential", herr.Reason)
			}},
		{name: "not json", status: http.StatusOK, body: `<html>`,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrProtocolUnsupported) }},
		{name: "no projects key", status: http.StatusOK, body: `{"items":[]}`,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrProtocolUnsupported) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := projectsServer(t, tc.status, tc.body)
			useHome(t)
			t.Setenv(EnvURL, srv.URL)
			c := NewClient("tok")
			defer c.Close()
			_, err := c.Connect(t.Context())
			require.NoError(t, err)
			_, err = c.Projects(t.Context())
			tc.check(t, err)
		})
	}
}

// treeSnapshot lists every path below root with its size and modification time.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		out[path] = info.ModTime().String() + "/" + info.Mode().String()
		if !d.IsDir() {
			data, err := os.ReadFile(path) //nolint:gosec // test temp path
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			out[path] += "/" + string(data)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

func TestClientNeverWritesBelowHome(t *testing.T) {
	s := t3test.NewServer(t,
		t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}),
		t3test.WithProject(t3test.Project{ID: "p", Title: "P", WorkspaceRoot: "/r"}))
	home := useHome(t)
	require.NoError(t, s.WriteRuntime(home))
	before := treeSnapshot(t, home)

	c := NewClient(s.Token())
	_, err := c.Connect(t.Context())
	require.NoError(t, err)
	_, err = c.GetThreadProjection(t.Context(), "th")
	require.NoError(t, err)
	_, err = c.Projects(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.SetTitle(t.Context(), "th", "new"))
	_, err = c.TerminalOpen(t.Context(), TerminalOpenInput{ThreadID: "th", TerminalID: "t", Cwd: "/r"})
	require.NoError(t, err)
	_, err = c.Connect(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	assert.Equal(t, before, treeSnapshot(t, home))
}

func TestNewCommandID(t *testing.T) {
	a, err := newCommandID()
	require.NoError(t, err)
	b, err := newCommandID()
	require.NoError(t, err)
	assert.Regexp(t, commandIDPattern, a)
	assert.NotEqual(t, a, b)
}
