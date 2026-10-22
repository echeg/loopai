package t3

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/t3/t3test"
)

func TestHandshakeError(t *testing.T) {
	tests := []struct {
		name        string
		err         *HandshakeError
		text        string
		unsupported bool
	}{
		{name: "426", err: &HandshakeError{StatusCode: 426, Code: "orchestration_protocol_incompatible",
			Message: "Update this client."},
			text: "t3: server answered HTTP 426 orchestration_protocol_incompatible: Update this client.", unsupported: true},
		{name: "426 without a body", err: &HandshakeError{StatusCode: 426},
			text: "t3: server answered HTTP 426", unsupported: true},
		{name: "incompatible code on another status", err: &HandshakeError{StatusCode: 400, Code: "orchestration_protocol_incompatible"},
			text: "t3: server answered HTTP 400 orchestration_protocol_incompatible", unsupported: true},
		{name: "401", err: &HandshakeError{StatusCode: 401, Code: "auth_invalid", Reason: "missing_credential"},
			text: "t3: server answered HTTP 401 auth_invalid (missing_credential)"},
		{name: "404", err: &HandshakeError{StatusCode: 404}, text: "t3: server answered HTTP 404"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.text, tc.err.Error())
			assert.Equal(t, tc.unsupported, errors.Is(tc.err, ErrProtocolUnsupported))
			assert.NotErrorIs(t, tc.err, ErrNotRunning)
		})
	}
}

func TestHandshakeErrorFromResponse(t *testing.T) {
	resp := &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(
		`{"_tag":"EnvironmentAuthInvalidError","code":"auth_invalid","reason":"invalid_credential","traceId":"x"}`))}
	assert.Equal(t, &HandshakeError{StatusCode: 401, Code: "auth_invalid", Reason: "invalid_credential"},
		handshakeError(resp))
	resp = &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(`<html>bad gateway</html>`))}
	assert.Equal(t, &HandshakeError{StatusCode: 502}, handshakeError(resp))
	assert.Equal(t, &HandshakeError{StatusCode: 500}, handshakeError(&http.Response{StatusCode: 500}))
}

func TestDecodeExit(t *testing.T) {
	tests := []struct {
		name      string
		exit      string
		value     string
		remote    *RemoteError
		transport string
		protocol  bool
	}{
		{name: "success with value", exit: `{"_tag":"Success","value":{"sequence":3}}`, value: `{"sequence":3}`},
		{name: "void success", exit: `{"_tag":"Success"}`},
		{name: "fail with message and detail",
			exit: `{"_tag":"Failure","cause":[{"_tag":"Fail","error":{"_tag":"OrchestrationV2DispatchCommandError",` +
				`"commandId":"c","commandType":"thread.pin","message":"Nope","detail":"Archived"}}]}`,
			remote: &RemoteError{Method: "m", Tag: "OrchestrationV2DispatchCommandError", Message: "Nope", Detail: "Archived"}},
		{name: "fail wins over a die",
			exit:   `{"_tag":"Failure","cause":[{"_tag":"Die","defect":"x"},{"_tag":"Fail","error":{"_tag":"E"}}]}`,
			remote: &RemoteError{Method: "m", Tag: "E"}},
		{name: "fail without a tag", exit: `{"_tag":"Failure","cause":[{"_tag":"Fail","error":"text"}]}`,
			remote: &RemoteError{Method: "m", Tag: "UnknownError"}},
		{name: "die", exit: `{"_tag":"Failure","cause":[{"_tag":"Die","defect":"Unknown request tag: x"}]}`,
			transport: "server died: Unknown request tag: x"},
		{name: "die with an object defect",
			exit:      `{"_tag":"Failure","cause":[{"_tag":"Die","defect":{"name":"Error","message":"kaput"}}]}`,
			transport: "server died: kaput"},
		{name: "interrupt", exit: `{"_tag":"Failure","cause":[{"_tag":"Interrupt","fiberId":null}]}`,
			transport: "interrupted by the server"},
		{name: "empty cause", exit: `{"_tag":"Failure","cause":[]}`, transport: "failure without a known cause"},
		{name: "unknown exit tag", exit: `{"_tag":"Maybe"}`, protocol: true},
		{name: "no exit", exit: ``, protocol: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var exit *wireExit
			if tc.exit != "" {
				require.NoError(t, json.Unmarshal([]byte(tc.exit), &exit))
			}
			value, err := decodeExit("m", exit)
			switch {
			case tc.remote != nil:
				var remote *RemoteError
				require.ErrorAs(t, err, &remote)
				remote.Raw = nil
				assert.Equal(t, tc.remote, remote)
			case tc.transport != "":
				var terr *TransportError
				require.ErrorAs(t, err, &terr)
				assert.Equal(t, tc.transport, terr.Reason)
				require.NoError(t, terr.Unwrap())
			case tc.protocol:
				require.ErrorIs(t, err, ErrProtocolUnsupported)
			default:
				require.NoError(t, err)
				if tc.value == "" {
					assert.Empty(t, value)
				} else {
					assert.JSONEq(t, tc.value, string(value))
				}
			}
		})
	}
}

func TestErrorText(t *testing.T) {
	assert.Equal(t, "t3 m: E", (&RemoteError{Method: "m", Tag: "E"}).Error())
	assert.Equal(t, "t3 m: E: msg (detail)", (&RemoteError{Method: "m", Tag: "E", Message: "msg", Detail: "detail"}).Error())
	assert.Equal(t, "t3 m: E: same", (&RemoteError{Method: "m", Tag: "E", Message: "same", Detail: "same"}).Error())

	cause := errors.New("eof")
	terr := &TransportError{Method: "m", Reason: "connection lost", Err: cause}
	assert.Equal(t, "t3 m: connection lost: eof", terr.Error())
	require.ErrorIs(t, terr, cause)
	assert.Equal(t, "t3 m: interrupted", (&TransportError{Method: "m", Reason: "interrupted"}).Error())
}

func TestDefectText(t *testing.T) {
	assert.Equal(t, "plain", defectText(json.RawMessage(`"plain"`)))
	assert.Equal(t, "inner", defectText(json.RawMessage(`{"message":"inner","stack":"..."}`)))
	assert.Equal(t, `{"code":7}`, defectText(json.RawMessage(`{"code":7}`)))
	assert.Equal(t, "no detail", defectText(nil))
	long := defectText(json.RawMessage(`"` + strings.Repeat("x", 500) + `"`))
	assert.Equal(t, strings.Repeat("x", maxDetailLen)+"…", long)
}

func TestRequestKey(t *testing.T) {
	assert.Equal(t, "7", requestKey(json.RawMessage(`7`)))
	assert.Equal(t, "7", requestKey(json.RawMessage(`"7"`)), "a string id and its number are the same request")
	assert.Equal(t, "abc", requestKey(json.RawMessage(` "abc" `)))
	assert.Equal(t, "12", requestKey(json.RawMessage(" 12 ")))
}

func TestDecodeFrame(t *testing.T) {
	msgs, err := decodeFrame([]byte(`{"_tag":"Pong"}`))
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Pong", msgs[0].Tag)

	msgs, err = decodeFrame([]byte(` [{"_tag":"Pong"},{"_tag":"Chunk","requestId":1,"values":[1,2]}]`))
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Len(t, msgs[1].Values, 2)

	for _, frame := range []string{`not json`, `[{"_tag":1}]`, ``} {
		_, err = decodeFrame([]byte(frame))
		require.ErrorIs(t, err, ErrProtocolUnsupported, "frame %q", frame)
	}
}

func TestRequestsCarryHeadersAndNumericIDs(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := newTestClient(t, s)
	for range 3 {
		_, err := c.GetThreadProjection(t.Context(), "th")
		require.NoError(t, err)
	}
	requests := s.Requests()
	require.Len(t, requests, 3)
	for i, req := range requests {
		assert.JSONEq(t, `[]`, string(req.Headers), "request %d", i)
		var id int
		require.NoError(t, json.Unmarshal(req.ID, &id), "the id is a JSON number: %s", req.ID)
		assert.Equal(t, i+1, id, "ids come from a counter")
		assert.Equal(t, methodGetThreadProjection, req.Method)
	}
}

func TestDefectFailsAllPendingCalls(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := newTestClient(t, s)
	sub, err := c.SubscribeTerminalMetadata(t.Context())
	require.NoError(t, err)
	_, err = sub.Next()
	require.NoError(t, err)

	s.Stall(methodGetThreadProjection)
	s.Stall(methodDispatchCommand)
	errs := make(chan error, 2)
	go func() {
		_, callErr := c.GetThreadProjection(t.Context(), "th")
		errs <- callErr
	}()
	go func() { errs <- c.SetTitle(t.Context(), "th", "x") }()
	require.Eventually(t, func() bool { return len(s.Requests()) == 3 }, 10*time.Second, 10*time.Millisecond)

	s.SendDefect("the server fell over")
	for range 2 {
		select {
		case callErr := <-errs:
			var terr *TransportError
			require.ErrorAs(t, callErr, &terr)
			assert.Equal(t, "server defect: the server fell over", terr.Reason)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "a pending call survived the defect")
		}
	}
	_, err = sub.Next()
	var terr *TransportError
	require.ErrorAs(t, err, &terr, "a pending stream fails too")
	assert.Contains(t, terr.Reason, "server defect")

	var remote *RemoteError
	require.ErrorAs(t, c.TerminalWrite(t.Context(), "th", "gone", "x"), &remote,
		"the connection stays usable: the next call gets its typed answer")
}

func TestPing(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := newTestClient(t, s, WithPingInterval(20*time.Millisecond))
	require.Eventually(t, func() bool { return s.Pings() >= 3 }, 10*time.Second, 10*time.Millisecond)
	_, err := c.GetThreadProjection(t.Context(), "th")
	require.NoError(t, err, "answered pings keep the connection open")

	s.IgnorePings()
	require.Eventually(t, func() bool {
		_, err = c.GetThreadProjection(t.Context(), "th")
		return err != nil
	}, 10*time.Second, 10*time.Millisecond)
	require.ErrorIs(t, err, ErrClosed)
	assert.Contains(t, err.Error(), "no pong")
}

func TestCallContextCanceled(t *testing.T) {
	s := t3test.NewServer(t, t3test.WithThread(t3test.Thread{ID: "th", ProjectID: "p", Title: "T"}))
	c := newTestClient(t, s)
	s.Stall(methodTerminalWrite)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := c.TerminalWrite(ctx, "th", "t", "x")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = c.GetThreadProjection(t.Context(), "th")
	require.NoError(t, err, "an abandoned call leaves the connection usable")
}

func TestCloseFailsPendingCalls(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)
	s.Stall(methodGetThreadProjection)
	errs := make(chan error, 1)
	go func() {
		_, err := c.GetThreadProjection(t.Context(), "th")
		errs <- err
	}()
	require.Eventually(t, func() bool { return len(s.Requests()) == 1 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, c.Close())
	select {
	case err := <-errs:
		require.ErrorIs(t, err, ErrClosed)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "Close left a call pending")
	}
}

func TestStreamAcksOnlyWhatItConsumes(t *testing.T) {
	s := t3test.NewServer(t)
	c := newTestClient(t, s)
	sub, err := c.SubscribeTerminalMetadata(t.Context())
	require.NoError(t, err)
	event, err := sub.Next()
	require.NoError(t, err)
	assert.Equal(t, MetadataSnapshot, event.Type)
	require.Eventually(t, func() bool { return s.Acks() == 1 }, 10*time.Second, 10*time.Millisecond)

	for _, id := range []string{"a", "b", "c"} {
		s.Upsert(t3test.Terminal{ThreadID: "th", TerminalID: id, Cwd: "/r"})
	}
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, 1, s.Acks(), "an unread chunk is not acknowledged, so the server holds the rest")

	for _, want := range []string{"a", "b", "c"} {
		event, err := sub.Next()
		require.NoError(t, err)
		assert.Equal(t, want, event.Terminal.TerminalID)
	}
	require.Eventually(t, func() bool { return s.Acks() == 4 }, 10*time.Second, 10*time.Millisecond)
}

func TestStreamEnd(t *testing.T) {
	c := &rpcConn{calls: map[string]pendingCall{}, streams: map[string]*rpcStream{}}
	newStream := func(key string) *rpcStream {
		s := &rpcStream{conn: c, method: "m", key: key, wake: make(chan struct{}, 1)}
		c.streams[key] = s
		return s
	}
	done := newStream("1")
	c.exit("1", &wireExit{Tag: "Success"})
	_, err := done.next(t.Context())
	require.ErrorIs(t, err, io.EOF, "a stream the server finished ends with io.EOF")

	failed := newStream("2")
	c.exit("2", &wireExit{Tag: "Failure", Cause: []wireCause{{Tag: "Fail", Error: json.RawMessage(`{"_tag":"E"}`)}}})
	_, err = failed.next(t.Context())
	var remote *RemoteError
	require.ErrorAs(t, err, &remote)
	assert.Empty(t, c.streams)

	c.exit("unknown", &wireExit{Tag: "Success"}) // a late exit for a forgotten request is ignored
}

// garbageServer upgrades and answers the first message with a frame that is not JSON.
func garbageServer(t *testing.T) *httptest.Server {
	t.Helper()
	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		wg.Add(1)
		defer wg.Done()
		defer ws.Close()
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
		_ = ws.WriteMessage(websocket.TextMessage, []byte("<<<not json>>>"))
		_, _, _ = ws.ReadMessage()
	}))
	t.Cleanup(func() {
		srv.Close()
		wg.Wait()
	})
	return srv
}

func TestUndecodableFrameIsProtocolUnsupported(t *testing.T) {
	srv := garbageServer(t)
	useHome(t)
	t.Setenv(EnvURL, srv.URL)
	c := NewClient("tok")
	defer c.Close()
	_, err := c.Connect(t.Context())
	require.NoError(t, err)
	_, err = c.GetThreadProjection(t.Context(), "th")
	require.ErrorIs(t, err, ErrProtocolUnsupported)
	_, err = c.GetThreadProjection(t.Context(), "th")
	require.ErrorIs(t, err, ErrProtocolUnsupported, "the connection stays failed with the cause")
	require.ErrorIs(t, err, ErrClosed)
}
