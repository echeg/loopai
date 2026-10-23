package t3test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawConn is a protocol-2 client written against the wire format alone, so the fake is tested
// without pkg/t3. A reader goroutine queues incoming messages, because a gorilla read that hits its
// deadline leaves the connection unusable.
type rawConn struct {
	t        *testing.T
	ws       *websocket.Conn
	messages chan map[string]any
	readErr  chan error
}

func wsURL(s *Server, query string) string {
	return "ws" + strings.TrimPrefix(s.URL(), "http") + "/ws" + query
}

func dialRaw(t *testing.T, s *Server, query string, header http.Header) (*rawConn, *http.Response, error) {
	t.Helper()
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL(s, query), header)
	if err != nil {
		return nil, resp, fmt.Errorf("dial: %w", err)
	}
	c := &rawConn{t: t, ws: ws, messages: make(chan map[string]any, 100), readErr: make(chan error, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg map[string]any
			if err := ws.ReadJSON(&msg); err != nil {
				c.readErr <- err
				return
			}
			c.messages <- msg
		}
	}()
	t.Cleanup(func() {
		_ = ws.Close()
		<-done
	})
	return c, resp, nil
}

func connectRaw(t *testing.T, s *Server) *rawConn {
	t.Helper()
	conn, resp, err := dialRaw(t, s, "?orchestrationProtocol=2", http.Header{"Authorization": {"Bearer " + s.Token()}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.NoError(t, err)
	return conn
}

func (c *rawConn) send(msg any) {
	c.t.Helper()
	require.NoError(c.t, c.ws.WriteJSON(msg))
}

func (c *rawConn) request(id any, method string, payload any) {
	c.t.Helper()
	c.send(map[string]any{"_tag": "Request", "id": id, "tag": method, "payload": payload, "headers": []any{}})
}

// recv reads the next message, failing the test after wait.
func (c *rawConn) recv() map[string]any {
	c.t.Helper()
	msg, err := c.tryRecv(wait)
	require.NoError(c.t, err)
	return msg
}

// errNoMessage reports that no message arrived in time.
var errNoMessage = errors.New("no message")

func (c *rawConn) tryRecv(timeout time.Duration) (map[string]any, error) {
	select {
	case msg := <-c.messages:
		return msg, nil
	case err := <-c.readErr:
		return nil, err
	case <-time.After(timeout):
		return nil, errNoMessage
	}
}

// recvTagged reads two messages and indexes them by _tag, for a reply and a chunk whose order is not
// defined.
func (c *rawConn) recvTagged() map[string]map[string]any {
	c.t.Helper()
	out := map[string]map[string]any{}
	for range 2 {
		msg := c.recv()
		out[msg["_tag"].(string)] = msg
	}
	return out
}

const wait = 10 * time.Second

func TestServerRejectsMissingProtocolBeforeAuth(t *testing.T) {
	s := NewServer(t)
	for _, query := range []string{"", "?orchestrationProtocol=1"} {
		_, resp, err := dialRaw(t, s, query, nil)
		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		require.NotNil(t, resp)
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, readErr)
		assert.Equal(t, http.StatusUpgradeRequired, resp.StatusCode)
		assert.JSONEq(t, IncompatibleProtocolBody, string(body))
		assert.Len(t, body, len(IncompatibleProtocolBody), "the body is the exact T3 Code text")
	}
	handshakes := s.Handshakes()
	require.Len(t, handshakes, 2)
	assert.Equal(t, http.StatusUpgradeRequired, handshakes[0].Status)
	assert.Empty(t, handshakes[0].Authorization)
}

func TestServerProtocolVersionOption(t *testing.T) {
	s := NewServer(t, WithProtocolVersion("3"))
	_, resp, err := dialRaw(t, s, "?orchestrationProtocol=2", http.Header{"Authorization": {"Bearer " + s.Token()}})
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, readErr)
	assert.Equal(t, http.StatusUpgradeRequired, resp.StatusCode)
	assert.JSONEq(t, `{"code":"orchestration_protocol_incompatible",
		"message":"Update this client to one that supports orchestration protocol 3.","orchestrationProtocolVersion":3}`,
		string(body))
}

func TestServerRejectsBadBearer(t *testing.T) {
	s := NewServer(t, WithToken("right"))
	tests := []struct {
		name   string
		header http.Header
		reason string
	}{
		{name: "missing", header: nil, reason: "missing_credential"},
		{name: "wrong", header: http.Header{"Authorization": {"Bearer wrong"}}, reason: "invalid_credential"},
		{name: "not bearer", header: http.Header{"Authorization": {"right"}}, reason: "invalid_credential"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, resp, err := dialRaw(t, s, "?orchestrationProtocol=2", tc.header)
			require.ErrorIs(t, err, websocket.ErrBadHandshake)
			defer resp.Body.Close()
			var body map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.Equal(t, "auth_invalid", body["code"])
			assert.Equal(t, tc.reason, body["reason"])
		})
	}
}

func TestServerRejectNextConnection(t *testing.T) {
	s := NewServer(t)
	s.RejectNextConnection()
	_, resp, err := dialRaw(t, s, "?orchestrationProtocol=2", http.Header{"Authorization": {"Bearer " + s.Token()}})
	require.ErrorIs(t, err, websocket.ErrBadHandshake)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	connectRaw(t, s) // only the next connection is rejected
	handshakes := s.Handshakes()
	require.Len(t, handshakes, 2)
	assert.Equal(t, http.StatusUnauthorized, handshakes[0].Status)
	assert.Equal(t, http.StatusSwitchingProtocols, handshakes[1].Status)
	assert.Equal(t, "Bearer "+DefaultToken, handshakes[1].Authorization)
	assert.Equal(t, "2", handshakes[1].Query.Get("orchestrationProtocol"))
}

func TestServerRequestValidation(t *testing.T) {
	s := NewServer(t, WithThread(Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := connectRaw(t, s)

	c.send(map[string]any{"_tag": "Request", "id": 1, "tag": "orchestration.getThreadProjection",
		"payload": map[string]any{"threadId": "th"}})
	msg := c.recv()
	assert.Equal(t, "Exit", msg["_tag"])
	assert.InDelta(t, 1, msg["requestId"], 0)
	exit := msg["exit"].(map[string]any)
	assert.Equal(t, "Failure", exit["_tag"])
	cause := exit["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "Die", cause["_tag"])
	assert.Contains(t, cause["defect"], "without headers")

	c.request(map[string]any{"bad": true}, "orchestration.getThreadProjection", map[string]any{"threadId": "th"})
	msg = c.recv()
	assert.Equal(t, "Defect", msg["_tag"])

	c.request(2, "no.such.method", map[string]any{})
	msg = c.recv()
	cause = msg["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "Die", cause["_tag"])
	assert.Equal(t, "Unknown request tag: no.such.method", cause["defect"])

	requests := s.Requests()
	require.Len(t, requests, 3)
	assert.Empty(t, requests[0].Headers)
	assert.JSONEq(t, `[]`, string(requests[2].Headers))
}

func TestServerThreadProjectionAndDispatch(t *testing.T) {
	s := NewServer(t,
		WithThread(Thread{ID: "th", ProjectID: "p", Title: "Old", WorktreePath: "/wt", TitleRegenerating: true}),
		WithThread(Thread{ID: "archived", ProjectID: "p", Title: "A", ArchivedAt: "2026-10-01T00:00:00.000Z"}))
	c := connectRaw(t, s)

	c.request("s1", "orchestration.getThreadProjection", map[string]any{"threadId": "th"})
	msg := c.recv()
	assert.Equal(t, "s1", msg["requestId"], "the requestId echoes a string id")
	exit := msg["exit"].(map[string]any)
	assert.Equal(t, "Success", exit["_tag"])
	thread := exit["value"].(map[string]any)["thread"].(map[string]any)
	assert.Equal(t, "Old", thread["title"])
	assert.Equal(t, "/wt", thread["worktreePath"])
	assert.Nil(t, thread["archivedAt"])
	assert.NotContains(t, thread, "pinnedAt")
	assert.NotNil(t, thread["titleRegeneration"])

	c.request(2, "orchestration.dispatchCommand",
		map[string]any{"type": "thread.metadata.update", "commandId": "c1", "threadId": "th", "title": "New"})
	exit = c.recv()["exit"].(map[string]any)
	assert.Equal(t, map[string]any{"sequence": float64(1)}, exit["value"])
	th, ok := s.Thread("th")
	require.True(t, ok)
	assert.Equal(t, "New", th.Title)
	assert.False(t, th.TitleRegenerating, "a title write clears a pending regeneration")

	s.SetThread(Thread{ID: "th", ProjectID: "p", Title: "Renamed"})
	c.request(3, "orchestration.dispatchCommand",
		map[string]any{"type": "thread.metadata.update", "commandId": "c1", "threadId": "th", "title": "Again"})
	exit = c.recv()["exit"].(map[string]any)
	assert.Equal(t, map[string]any{"sequence": float64(1)}, exit["value"], "a reused command id replays its receipt")
	th, _ = s.Thread("th")
	assert.Equal(t, "Renamed", th.Title)

	c.request(4, "orchestration.dispatchCommand", map[string]any{"type": "thread.pin", "commandId": "c2", "threadId": "th"})
	c.recv()
	th, _ = s.Thread("th")
	assert.NotEmpty(t, th.PinnedAt)
	c.request(5, "orchestration.dispatchCommand", map[string]any{"type": "thread.unpin", "commandId": "c3", "threadId": "th"})
	c.recv()
	th, _ = s.Thread("th")
	assert.Empty(t, th.PinnedAt)

	c.request(6, "orchestration.dispatchCommand",
		map[string]any{"type": "thread.pin", "commandId": "c4", "threadId": "archived"})
	cause := c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "Fail", cause["_tag"])
	failure := cause["error"].(map[string]any)
	assert.Equal(t, "OrchestrationV2DispatchCommandError", failure["_tag"])
	assert.Equal(t, "c4", failure["commandId"])
	assert.NotEmpty(t, failure["message"])

	c.request(7, "orchestration.dispatchCommand",
		map[string]any{"type": "thread.pin", "commandId": "c5", "threadId": "th", "createdBy": "user"})
	cause = c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "T3TestForbiddenFieldError", cause["error"].(map[string]any)["_tag"])

	c.request(8, "orchestration.getThreadProjection", map[string]any{"threadId": "missing"})
	cause = c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "OrchestrationV2GetThreadProjectionError", cause["error"].(map[string]any)["_tag"])

	assert.Len(t, s.Dispatches(), 6, "a rejected request is recorded too")
	assert.Equal(t, "New", s.Dispatches()[0]["title"])
}

func TestServerTerminals(t *testing.T) {
	s := NewServer(t, WithTerminal(Terminal{ThreadID: "th", TerminalID: "starting", Cwd: "/r", Status: "starting"}),
		WithTerminal(Terminal{ThreadID: "th", TerminalID: "done", Cwd: "/r", Status: "exited"}))
	c := connectRaw(t, s)

	open := map[string]any{"threadId": "th", "terminalId": "loopai-1", "cwd": "/r",
		"env": map[string]any{"T3CODE_PROJECT_ROOT": "/r"}}
	c.request(1, "terminal.open", open)
	exit := c.recv()["exit"].(map[string]any)
	session := exit["value"].(map[string]any)
	assert.Equal(t, "running", session["status"])
	assert.Nil(t, session["worktreePath"])
	assert.Contains(t, session, "history")

	c.request(2, "terminal.write", map[string]any{"threadId": "th", "terminalId": "loopai-1", "data": " ls\r"})
	exit = c.recv()["exit"].(map[string]any)
	assert.Equal(t, map[string]any{"_tag": "Success"}, exit, "a Void success carries no value")

	for i, tc := range []struct {
		terminal string
		tag      string
	}{{"starting", "TerminalNotRunningError"}, {"missing", "TerminalSessionLookupError"}} {
		c.request(3+i, "terminal.write", map[string]any{"threadId": "th", "terminalId": tc.terminal, "data": "x"})
		cause := c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
		assert.Equal(t, "Fail", cause["_tag"])
		assert.Equal(t, tc.tag, cause["error"].(map[string]any)["_tag"])
	}
	c.request(5, "terminal.write", map[string]any{"threadId": "th", "terminalId": "done", "data": "x"})
	assert.Equal(t, "Success", c.recv()["exit"].(map[string]any)["_tag"], "data for an exited terminal is dropped")

	c.request(6, "terminal.open", map[string]any{"threadId": "th", "terminalId": "x", "cwd": "/r", "providerInstanceId": "p"})
	cause := c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "T3TestForbiddenFieldError", cause["error"].(map[string]any)["_tag"])

	assert.Len(t, s.Opens(), 2, "a rejected request is recorded too")
	assert.Len(t, s.Writes(), 4)
	assert.Equal(t, " ls\r", s.Writes()[0]["data"])
	assert.Len(t, s.Terminals(), 3, "a rejected open creates no terminal")
}

// readChunk reads the next Chunk of a stream and returns its single value.
func readChunk(t *testing.T, c *rawConn) map[string]any {
	t.Helper()
	msg := c.recv()
	require.Equal(t, "Chunk", msg["_tag"], "message %v", msg)
	values := msg["values"].([]any)
	require.Len(t, values, 1)
	return values[0].(map[string]any)
}

func TestServerMetadataStreamWaitsForAck(t *testing.T) {
	s := NewServer(t, WithTerminal(Terminal{ThreadID: "th", TerminalID: "a", Cwd: "/r"}))
	c := connectRaw(t, s)
	c.request(1, "subscribeTerminalMetadata", map[string]any{})

	snapshot := readChunk(t, c)
	assert.Equal(t, "snapshot", snapshot["type"])
	terminals := snapshot["terminals"].([]any)
	require.Len(t, terminals, 1)
	assert.Equal(t, false, terminals[0].(map[string]any)["hasRunningSubprocess"])

	s.Upsert(Terminal{ThreadID: "th", TerminalID: "b", Cwd: "/r", HasRunningSubprocess: true})
	s.Remove("th", "a")
	_, err := c.tryRecv(300 * time.Millisecond)
	require.ErrorIs(t, err, errNoMessage, "without an Ack the stream stalls after its snapshot")

	c.send(map[string]any{"_tag": "Ack", "requestId": 1})
	upsert := readChunk(t, c)
	assert.Equal(t, "upsert", upsert["type"])
	assert.Equal(t, "b", upsert["terminal"].(map[string]any)["terminalId"])
	assert.Equal(t, true, upsert["terminal"].(map[string]any)["hasRunningSubprocess"])

	c.send(map[string]any{"_tag": "Ack", "requestId": 1})
	remove := readChunk(t, c)
	assert.Equal(t, map[string]any{"type": "remove", "threadId": "th", "terminalId": "a"}, remove)
	assert.Equal(t, 2, s.Acks())

	c.send(map[string]any{"_tag": "Interrupt", "requestId": 1})
	exit := c.recv()
	assert.Equal(t, "Exit", exit["_tag"])
	assert.Equal(t, "Interrupt", exit["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)["_tag"])
	assert.Equal(t, 1, s.Interrupts())
}

func TestServerMetadataStreamKeepsARemoveRacingTheSubscription(t *testing.T) {
	for i := range 40 {
		s := NewServer(t, WithTerminal(Terminal{ThreadID: "th", TerminalID: "a", Cwd: "/r"}))
		c := connectRaw(t, s)
		removed := make(chan struct{})
		go func() {
			defer close(removed)
			s.Remove("th", "a")
		}()
		c.request(1, "subscribeTerminalMetadata", map[string]any{})
		snapshot := readChunk(t, c)
		<-removed
		if len(snapshot["terminals"].([]any)) == 0 {
			continue // the remove came first; the snapshot already lacks the terminal
		}
		c.send(map[string]any{"_tag": "Ack", "requestId": 1})
		event := readChunk(t, c)
		assert.Equal(t, map[string]any{"type": "remove", "threadId": "th", "terminalId": "a"}, event,
			"iteration %d: a snapshot holding the terminal is followed by its remove", i)
	}
}

func TestServerOpenNextStarting(t *testing.T) {
	s := NewServer(t)
	c := connectRaw(t, s)
	c.request(1, "subscribeTerminalMetadata", map[string]any{})
	readChunk(t, c)
	c.send(map[string]any{"_tag": "Ack", "requestId": 1})

	s.OpenNextStarting()
	open := map[string]any{"threadId": "th", "terminalId": "t", "cwd": "/r", "env": map[string]any{"K": "v"}}
	c.request(2, "terminal.open", open)
	replies := c.recvTagged()
	assert.Equal(t, "starting", replies["Exit"]["exit"].(map[string]any)["value"].(map[string]any)["status"])
	upsert := replies["Chunk"]["values"].([]any)[0].(map[string]any)
	assert.Equal(t, "starting", upsert["terminal"].(map[string]any)["status"])
	c.send(map[string]any{"_tag": "Ack", "requestId": 1})

	c.request(3, "terminal.write", map[string]any{"threadId": "th", "terminalId": "t", "data": "x"})
	cause := c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "TerminalNotRunningError", cause["error"].(map[string]any)["_tag"])

	require.True(t, s.SetTerminalState("th", "t", "running", true))
	upsert = readChunk(t, c)
	assert.Equal(t, "upsert", upsert["type"])
	summary := upsert["terminal"].(map[string]any)
	assert.Equal(t, "running", summary["status"])
	assert.Equal(t, true, summary["hasRunningSubprocess"])
	c.send(map[string]any{"_tag": "Ack", "requestId": 1})
	c.request(4, "terminal.write", map[string]any{"threadId": "th", "terminalId": "t", "data": "x"})
	assert.Equal(t, "Success", c.recv()["exit"].(map[string]any)["_tag"])

	terminals := s.Terminals()
	require.Len(t, terminals, 1)
	assert.Equal(t, map[string]string{"K": "v"}, terminals[0].Env, "the launch context is kept")
	assert.False(t, s.SetTerminalState("th", "missing", "running", false))

	c.request(5, "terminal.open", map[string]any{"threadId": "th", "terminalId": "u", "cwd": "/r"})
	replies = c.recvTagged()
	assert.Equal(t, "running", replies["Exit"]["exit"].(map[string]any)["value"].(map[string]any)["status"],
		"the hook applies to one open only")
}

func TestServerBreakNextWrite(t *testing.T) {
	s := NewServer(t, WithTerminal(Terminal{ThreadID: "th", TerminalID: "starting", Cwd: "/r", Status: "starting"}),
		WithTerminal(Terminal{ThreadID: "th", TerminalID: "t", Cwd: "/r"}))
	c := connectRaw(t, s)
	writeTag := func(id int, terminal string) string {
		t.Helper()
		c.request(id, "terminal.write", map[string]any{"threadId": "th", "terminalId": terminal, "data": "x"})
		exit := c.recv()["exit"].(map[string]any)
		if exit["_tag"] == "Success" {
			return "Success"
		}
		return exit["cause"].([]any)[0].(map[string]any)["error"].(map[string]any)["_tag"].(string)
	}

	s.BreakNextWrite()
	assert.Equal(t, "TerminalNotRunningError", writeTag(1, "starting"), "a write refused otherwise keeps the hook")
	assert.Equal(t, "TerminalSessionLookupError", writeTag(2, "missing"))

	c.request(3, "terminal.write", map[string]any{"threadId": "th", "terminalId": "t", "data": "x"})
	cause := c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, "Fail", cause["_tag"])
	assert.Equal(t, map[string]any{"_tag": "TerminalWriteError", "threadId": "th", "terminalId": "t",
		"terminalPid": float64(50000), "cause": map[string]any{"name": "Error", "message": "write EIO"}}, cause["error"])

	assert.Equal(t, "Success", writeTag(4, "t"), "the hook applies to one write only")
	terminals := s.Terminals()
	require.Len(t, terminals, 2)
	assert.Equal(t, "running", terminals[1].Status, "the session is left as it is")
}

func TestServerTerminalOpenResizesOrResets(t *testing.T) {
	s := NewServer(t)
	c := connectRaw(t, s)
	c.request(1, "subscribeTerminalMetadata", map[string]any{})
	readChunk(t, c)
	c.send(map[string]any{"_tag": "Ack", "requestId": 1})

	open := map[string]any{"threadId": "th", "terminalId": "t", "cwd": "/r"}
	c.request(2, "terminal.open", open)
	replies := c.recvTagged() // a new session streams an upsert beside the reply
	require.Contains(t, replies, "Exit")
	upsert := replies["Chunk"]["values"].([]any)[0].(map[string]any)
	assert.Equal(t, "upsert", upsert["type"])
	c.send(map[string]any{"_tag": "Ack", "requestId": 1})

	c.request(3, "terminal.open", open)
	assert.Equal(t, "Exit", c.recv()["_tag"])
	_, err := c.tryRecv(300 * time.Millisecond)
	require.ErrorIs(t, err, errNoMessage, "an identical launch context only resizes, without an upsert")

	c.request(4, "terminal.open", map[string]any{"threadId": "th", "terminalId": "t", "cwd": "/other"})
	replies = c.recvTagged()
	upsert = replies["Chunk"]["values"].([]any)[0].(map[string]any)
	assert.Equal(t, "/other", upsert["terminal"].(map[string]any)["cwd"], "another launch context resets the session")
}

func TestServerRestart(t *testing.T) {
	s := NewServer(t, WithTerminal(Terminal{ThreadID: "th", TerminalID: "a", Cwd: "/r"}),
		WithThread(Thread{ID: "th", ProjectID: "p", Title: "T"}))
	home := t.TempDir()
	require.NoError(t, s.WriteRuntime(home))
	before := readRuntime(t, home)
	assert.Equal(t, s.URL(), before["origin"])
	c := connectRaw(t, s)

	require.NoError(t, s.Restart())
	_, err := c.tryRecv(wait)
	require.Error(t, err, "a restart drops the connection")
	require.NotErrorIs(t, err, errNoMessage)
	assert.Empty(t, s.Terminals(), "terminal sessions live only in server memory")
	_, ok := s.Thread("th")
	assert.True(t, ok, "threads survive a restart")
	after := readRuntime(t, home)
	assert.NotEqual(t, before["pid"], after["pid"])
	assert.NotEqual(t, before["startedAt"], after["startedAt"])
	assert.Equal(t, before["origin"], after["origin"])

	c = connectRaw(t, s)
	c.request(1, "subscribeTerminalMetadata", map[string]any{})
	assert.Empty(t, readChunk(t, c)["terminals"])
}

func readRuntime(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "userdata", "server-runtime.json")) //nolint:gosec // test temp path
	require.NoError(t, err)
	var rt map[string]any
	require.NoError(t, json.Unmarshal(data, &rt))
	return rt
}

func TestServerOverridesStallAndDefect(t *testing.T) {
	s := NewServer(t, WithThread(Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := connectRaw(t, s)

	s.ReplyNext("orchestration.getThreadProjection", map[string]any{"thread": 42})
	c.request(1, "orchestration.getThreadProjection", map[string]any{"threadId": "th"})
	assert.Equal(t, map[string]any{"thread": float64(42)}, c.recv()["exit"].(map[string]any)["value"])

	s.DieNext("orchestration.dispatchCommand", "boom")
	c.request(2, "orchestration.dispatchCommand", map[string]any{"type": "thread.pin", "commandId": "c", "threadId": "th"})
	cause := c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"_tag": "Die", "defect": "boom"}, cause)
	th, _ := s.Thread("th")
	assert.Empty(t, th.PinnedAt, "a Die override does not apply the command")

	s.FailNext("orchestration.dispatchCommand", "SomeRefusalError")
	c.request(4, "orchestration.dispatchCommand", map[string]any{"type": "thread.pin", "commandId": "c2", "threadId": "th"})
	cause = c.recv()["exit"].(map[string]any)["cause"].([]any)[0].(map[string]any)
	assert.Equal(t, map[string]any{"_tag": "Fail", "error": map[string]any{"_tag": "SomeRefusalError",
		"message": "t3test: SomeRefusalError"}}, cause)
	th, _ = s.Thread("th")
	assert.Empty(t, th.PinnedAt, "a Fail override does not apply the command")

	s.Stall("orchestration.getThreadProjection")
	c.request(3, "orchestration.getThreadProjection", map[string]any{"threadId": "th"})
	_, err := c.tryRecv(300 * time.Millisecond)
	require.ErrorIs(t, err, errNoMessage, "a stalled call is never answered")

	c = connectRaw(t, s)
	s.SendDefect("bad things")
	assert.Equal(t, map[string]any{"_tag": "Defect", "defect": "bad things"}, c.recv())
}

func TestServerPing(t *testing.T) {
	s := NewServer(t)
	c := connectRaw(t, s)
	c.send(map[string]any{"_tag": "Ping"})
	assert.Equal(t, map[string]any{"_tag": "Pong"}, c.recv())
	s.IgnorePings()
	c.send(map[string]any{"_tag": "Ping"})
	_, err := c.tryRecv(300 * time.Millisecond)
	require.ErrorIs(t, err, errNoMessage)
	assert.Equal(t, 2, s.Pings())
}

func TestServerProjects(t *testing.T) {
	s := NewServer(t, WithProject(Project{ID: "p", Title: "Repo", WorkspaceRoot: "/repo"}))
	get := func(auth string) (status int, body map[string]any) {
		req, err := http.NewRequest(http.MethodGet, s.URL()+"/api/projects", http.NoBody)
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		return resp.StatusCode, body
	}
	status, body := get("Bearer " + s.Token())
	assert.Equal(t, http.StatusOK, status)
	projects := body["projects"].([]any)
	require.Len(t, projects, 1)
	assert.Equal(t, "/repo", projects[0].(map[string]any)["workspaceRoot"])
	assert.Equal(t, "p", projects[0].(map[string]any)["id"])

	status, body = get("")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "missing_credential", body["reason"])
	status, _ = get("Bearer nope")
	assert.Equal(t, http.StatusUnauthorized, status)
}

func TestServerCloseIsIdempotent(t *testing.T) {
	s := NewServer(t)
	connectRaw(t, s)
	s.Close()
	s.Close()
	_, resp, err := dialRaw(t, s, "?orchestrationProtocol=2", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
}
