package t3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// defaultPingInterval paces the client's Ping; a Ping still unanswered at the next tick closes
	// the connection, so a half-open socket (a sleeping laptop, a killed server) is noticed.
	defaultPingInterval = 10 * time.Second
	// handshakeTimeout bounds the WebSocket upgrade.
	handshakeTimeout = 10 * time.Second
	// writeTimeout bounds one frame write.
	writeTimeout = 10 * time.Second
	// maxFrameSize caps one incoming frame; a thread projection is bounded by T3 but can be large.
	maxFrameSize = 64 << 20
	// maxDetailLen caps the defect text quoted in an error.
	maxDetailLen = 300
)

// codeProtocolIncompatible is the 426 body code of a server that requires another protocol.
const codeProtocolIncompatible = "orchestration_protocol_incompatible"

// ErrProtocolUnsupported reports a server that does not speak orchestration protocol 2: an HTTP 426
// at connect, or a reply loopai cannot decode. Callers disable their T3 integration on it.
var ErrProtocolUnsupported = errors.New("t3: server does not speak orchestration protocol 2")

// ErrClosed reports a connection that was closed or lost, or a call made without one.
var ErrClosed = errors.New("t3: connection closed")

// ErrReplyTooLarge reports a server message above the frame limit, such as the projection of a very
// long thread. It closes the shared connection, and a reconnect would read the same message again,
// so callers disable their T3 integration on it rather than retry.
var ErrReplyTooLarge = errors.New("t3: server message exceeds the frame limit")

// HandshakeError is an HTTP-level rejection by the server: a failed WebSocket upgrade, or a failed
// HTTP API request. Code and Reason come from the JSON body, for example 426
// orchestration_protocol_incompatible, or 401 auth_invalid with reason missing_credential or
// invalid_credential.
type HandshakeError struct {
	StatusCode int
	Code       string
	Reason     string
	Message    string
}

// Error describes the rejection; it never carries the token.
func (e *HandshakeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "t3: server answered HTTP %d", e.StatusCode)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Reason != "" {
		b.WriteString(" (" + e.Reason + ")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// Is matches ErrProtocolUnsupported for a 426 or an orchestration_protocol_incompatible body.
func (e *HandshakeError) Is(target error) bool {
	return target == ErrProtocolUnsupported &&
		(e.StatusCode == http.StatusUpgradeRequired || e.Code == codeProtocolIncompatible)
}

// RemoteError is the typed error the server failed one call with: the Fail cause of a Failure exit.
// Tag is the error's _tag, such as TerminalNotRunningError; Message and Detail are empty when the
// error carries none. Raw is the encoded error.
type RemoteError struct {
	Method  string
	Tag     string
	Message string
	Detail  string
	Raw     json.RawMessage
}

// Error names the method, the error tag, and its message and detail when present.
func (e *RemoteError) Error() string {
	msg := fmt.Sprintf("t3 %s: %s", e.Method, e.Tag)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Detail != "" && e.Detail != e.Message {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

// TransportError reports a call that ended without a typed answer: a Die or Interrupt exit, a
// Defect, or a lost connection. Whether a command it carried took effect is unknown. Err is the
// connection's failure when the connection was lost, and nil otherwise.
type TransportError struct {
	Method string
	Reason string
	Err    error
}

// Error names the method and the reason.
func (e *TransportError) Error() string {
	msg := fmt.Sprintf("t3 %s: %s", e.Method, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the connection's failure.
func (e *TransportError) Unwrap() error { return e.Err }

// wireRequest is a client Request. Headers must be present and empty: the server reads them.
type wireRequest struct {
	Tag     string      `json:"_tag"`
	ID      int64       `json:"id"`
	Method  string      `json:"tag"`
	Payload any         `json:"payload"`
	Headers [][2]string `json:"headers"`
}

// wireRequestControl is an Ack or an Interrupt for one request.
type wireRequestControl struct {
	Tag       string `json:"_tag"`
	RequestID int64  `json:"requestId"`
}

// pingFrame is the client's keepalive, answered with a Pong.
var pingFrame = []byte(`{"_tag":"Ping"}`)

// wireMessage is any server message: Chunk, Exit, Defect, or Pong.
type wireMessage struct {
	Tag       string            `json:"_tag"`
	RequestID json.RawMessage   `json:"requestId"`
	Values    []json.RawMessage `json:"values"`
	Exit      *wireExit         `json:"exit"`
	Defect    json.RawMessage   `json:"defect"`
}

// wireExit is a Success with an optional value, or a Failure with its causes.
type wireExit struct {
	Tag   string          `json:"_tag"`
	Value json.RawMessage `json:"value"`
	Cause []wireCause     `json:"cause"`
}

// wireCause is one Fail (a typed error), Die (a defect), or Interrupt cause.
type wireCause struct {
	Tag    string          `json:"_tag"`
	Error  json.RawMessage `json:"error"`
	Defect json.RawMessage `json:"defect"`
}

// callResult is the outcome of one unary call.
type callResult struct {
	value json.RawMessage
	err   error
}

// rpcConn is one protocol-2 WebSocket connection: one JSON message per text frame, responses
// routed by requestId, numeric request ids from a counter.
type rpcConn struct {
	ws     *websocket.Conn
	wmu    sync.Mutex
	lastID atomic.Int64
	pong   chan struct{}
	done   chan struct{}
	loops  sync.WaitGroup

	mu      sync.Mutex
	calls   map[string]pendingCall
	streams map[string]*rpcStream
	err     error
}

// pendingCall is a unary call waiting for its Exit.
type pendingCall struct {
	method string
	result chan callResult
}

// dialRPC opens a connection with the Bearer token. A rejected upgrade is a *HandshakeError carrying
// the JSON body's code, reason, and message.
func dialRPC(ctx context.Context, wsURL, token string, pingInterval time.Duration, maxFrame int64) (*rpcConn, error) {
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: handshakeTimeout}
	ws, resp, err := dialer.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + token}})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		if errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
			return nil, handshakeError(resp)
		}
		return nil, fmt.Errorf("t3: connect %s: %w", wsURL, err)
	}
	ws.SetReadLimit(maxFrame)
	c := &rpcConn{
		ws:      ws,
		pong:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		calls:   map[string]pendingCall{},
		streams: map[string]*rpcStream{},
	}
	c.loops.Add(2)
	go c.readLoop()
	go c.pingLoop(pingInterval)
	return c, nil
}

// handshakeError decodes a rejected request's JSON body; a body that is not JSON leaves the status
// alone.
func handshakeError(resp *http.Response) *HandshakeError {
	herr := &HandshakeError{StatusCode: resp.StatusCode}
	if resp.Body == nil {
		return herr
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return herr
	}
	var body struct {
		Code    string `json:"code"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		herr.Code, herr.Reason, herr.Message = body.Code, body.Reason, body.Message
	}
	return herr
}

// close closes the connection and waits for its goroutines; pending calls fail with ErrClosed.
func (c *rpcConn) close() {
	c.wmu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	c.wmu.Unlock()
	c.fail(ErrClosed)
	c.loops.Wait()
}

// fail closes the connection once with err and fails every pending call and stream.
func (c *rpcConn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	calls, streams := c.calls, c.streams
	c.calls, c.streams = map[string]pendingCall{}, map[string]*rpcStream{}
	c.mu.Unlock()
	close(c.done)
	_ = c.ws.Close()
	for _, call := range calls {
		call.result <- callResult{err: &TransportError{Method: call.method, Reason: "connection lost", Err: err}}
	}
	for _, s := range streams {
		s.end(&TransportError{Method: s.method, Reason: "connection lost", Err: err})
	}
}

// readLoop routes every incoming frame until the connection fails. A frame that is not JSON means
// the server speaks something else, so it closes the connection with ErrProtocolUnsupported; a
// frame above the read limit closes it with ErrReplyTooLarge.
func (c *rpcConn) readLoop() {
	defer c.loops.Done()
	for {
		_, data, err := c.ws.ReadMessage()
		if errors.Is(err, websocket.ErrReadLimit) {
			c.fail(fmt.Errorf("%w: %w: %w", ErrClosed, ErrReplyTooLarge, err))
			return
		}
		if err != nil {
			c.fail(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		msgs, err := decodeFrame(data)
		if err != nil {
			c.fail(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		for _, m := range msgs {
			c.route(m)
		}
	}
}

// decodeFrame decodes one text frame: a single message, or an array of them.
func decodeFrame(data []byte) ([]wireMessage, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var msgs []wireMessage
		if err := json.Unmarshal(data, &msgs); err != nil {
			return nil, fmt.Errorf("%w: undecodable frame: %w", ErrProtocolUnsupported, err)
		}
		return msgs, nil
	}
	var m wireMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: undecodable frame: %w", ErrProtocolUnsupported, err)
	}
	return []wireMessage{m}, nil
}

// route delivers one server message. Unknown tags and messages for requests no longer pending are
// ignored.
func (c *rpcConn) route(m wireMessage) {
	switch m.Tag {
	case "Exit":
		c.exit(requestKey(m.RequestID), m.Exit)
	case "Chunk":
		c.mu.Lock()
		s := c.streams[requestKey(m.RequestID)]
		c.mu.Unlock()
		if s != nil {
			s.push(m.Values)
		}
	case "Defect":
		c.defect(m.Defect)
	case "Pong":
		select {
		case c.pong <- struct{}{}:
		default:
		}
	}
}

// exit completes the call or stream an Exit names.
func (c *rpcConn) exit(key string, exit *wireExit) {
	c.mu.Lock()
	call, isCall := c.calls[key]
	s, isStream := c.streams[key]
	delete(c.calls, key)
	delete(c.streams, key)
	c.mu.Unlock()
	switch {
	case isCall:
		value, err := decodeExit(call.method, exit)
		call.result <- callResult{value: value, err: err}
	case isStream:
		_, err := decodeExit(s.method, exit)
		if err == nil {
			err = io.EOF
		}
		s.end(err)
	}
}

// defect fails every pending call and stream: a Defect names no request.
func (c *rpcConn) defect(raw json.RawMessage) {
	c.mu.Lock()
	calls, streams := c.calls, c.streams
	c.calls, c.streams = map[string]pendingCall{}, map[string]*rpcStream{}
	c.mu.Unlock()
	reason := "server defect: " + defectText(raw)
	for _, call := range calls {
		call.result <- callResult{err: &TransportError{Method: call.method, Reason: reason}}
	}
	for _, s := range streams {
		s.end(&TransportError{Method: s.method, Reason: reason})
	}
}

// decodeExit returns a Success value, or the error a Failure carries: a *RemoteError for a Fail
// cause, a *TransportError for Die and Interrupt.
func decodeExit(method string, exit *wireExit) (json.RawMessage, error) {
	if exit == nil {
		return nil, fmt.Errorf("%w: %s exit without a body", ErrProtocolUnsupported, method)
	}
	switch exit.Tag {
	case "Success":
		return exit.Value, nil
	case "Failure":
	default:
		return nil, fmt.Errorf("%w: %s exit tagged %q", ErrProtocolUnsupported, method, exit.Tag)
	}
	for _, cause := range exit.Cause {
		if cause.Tag == "Fail" {
			return nil, remoteError(method, cause.Error)
		}
	}
	for _, cause := range exit.Cause {
		switch cause.Tag {
		case "Die":
			return nil, &TransportError{Method: method, Reason: "server died: " + defectText(cause.Defect)}
		case "Interrupt":
			return nil, &TransportError{Method: method, Reason: "interrupted by the server"}
		}
	}
	return nil, &TransportError{Method: method, Reason: "failure without a known cause"}
}

// remoteError decodes a Fail cause's error, keeping the raw encoding.
func remoteError(method string, raw json.RawMessage) *RemoteError {
	var body struct {
		Tag     string          `json:"_tag"`
		Message json.RawMessage `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	_ = json.Unmarshal(raw, &body)
	tag := body.Tag
	if tag == "" {
		tag = "UnknownError"
	}
	return &RemoteError{Method: method, Tag: tag, Message: jsonString(body.Message), Detail: jsonString(body.Detail), Raw: raw}
}

// defectText renders a defect: a string as is, an object by its message, anything else as bounded
// JSON.
func defectText(raw json.RawMessage) string {
	if s := jsonString(raw); s != "" {
		return truncate(s)
	}
	var obj struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if s := jsonString(obj.Message); s != "" {
			return truncate(s)
		}
	}
	if len(raw) == 0 {
		return "no detail"
	}
	return truncate(string(raw))
}

// jsonString returns raw's value when it is a JSON string, else "".
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// truncate bounds s to maxDetailLen bytes.
func truncate(s string) string {
	if len(s) <= maxDetailLen {
		return s
	}
	return s[:maxDetailLen] + "…"
}

// requestKey normalizes a requestId, which echoes the JSON type of the request id: a number and its
// decimal string are the same request.
func requestKey(raw json.RawMessage) string {
	if s := jsonString(raw); s != "" {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// pingLoop sends a Ping every interval and closes the connection when the previous one went
// unanswered.
func (c *rpcConn) pingLoop(interval time.Duration) {
	defer c.loops.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.pingOnTicks(ticker.C, interval)
}

// pingOnTicks is pingLoop's body: it sends a Ping on every tick and fails the connection when the
// previous one went unanswered for an interval.
func (c *rpcConn) pingOnTicks(ticks <-chan time.Time, interval time.Duration) {
	awaiting := false
	for {
		select {
		case <-c.done:
			return
		case <-c.pong:
			awaiting = false
		case <-ticks:
			// a pong and the tick can be ready together after a descheduled interval, and select
			// picks either, so a pong already delivered still counts
			if awaiting && !c.pongArrived() {
				c.fail(fmt.Errorf("%w: no pong within %s", ErrClosed, interval))
				return
			}
			awaiting = true
			if err := c.write(pingFrame); err != nil {
				return
			}
		}
	}
}

// pongArrived takes a pong already delivered, without waiting for one.
func (c *rpcConn) pongArrived() bool {
	select {
	case <-c.pong:
		return true
	default:
		return false
	}
}

// write sends one text frame; a failed write fails the connection.
func (c *rpcConn) write(frame []byte) error {
	c.wmu.Lock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	err := c.ws.WriteMessage(websocket.TextMessage, frame)
	c.wmu.Unlock()
	if err != nil {
		err = fmt.Errorf("%w: write: %w", ErrClosed, err)
		c.fail(err)
		return err
	}
	return nil
}

// writeJSON encodes v and sends it as one frame.
func (c *rpcConn) writeJSON(v any) error {
	frame, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("t3: encode message: %w", err)
	}
	return c.write(frame)
}

// send registers a request with register and writes it; register runs under the connection lock so
// a reply can never arrive before the request is known.
func (c *rpcConn) send(method string, payload any, register func(id int64, key string)) error {
	id := c.lastID.Add(1)
	key := strconv.FormatInt(id, 10)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return &TransportError{Method: method, Reason: "connection lost", Err: err}
	}
	register(id, key)
	c.mu.Unlock()
	req := wireRequest{Tag: "Request", ID: id, Method: method, Payload: payload, Headers: [][2]string{}}
	if err := c.writeJSON(req); err != nil {
		c.mu.Lock()
		delete(c.calls, key)
		delete(c.streams, key)
		c.mu.Unlock()
		return &TransportError{Method: method, Reason: "send failed", Err: err}
	}
	return nil
}

// call performs one unary request and returns its Success value. A context already done sends
// nothing, so a canceled command can never apply; a context canceled later abandons the call, and
// its late reply is ignored.
func (c *rpcConn) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("t3 %s: %w", method, err)
	}
	result := make(chan callResult, 1)
	var key string
	err := c.send(method, payload, func(_ int64, k string) {
		key = k
		c.calls[k] = pendingCall{method: method, result: result}
	})
	if err != nil {
		return nil, err
	}
	select {
	case res := <-result:
		return res.value, res.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.calls, key)
		c.mu.Unlock()
		return nil, fmt.Errorf("t3 %s: %w", method, ctx.Err())
	}
}

// stream starts a stream request.
func (c *rpcConn) stream(method string, payload any) (*rpcStream, error) {
	s := &rpcStream{conn: c, method: method, wake: make(chan struct{}, 1)}
	err := c.send(method, payload, func(id int64, key string) {
		s.id, s.key = id, key
		c.streams[key] = s
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// errStreamClosed ends a stream the client interrupted.
var errStreamClosed = errors.New("t3: subscription closed")

// rpcStream is one stream request. The server sends the next Chunk only after the client's Ack, so
// next acknowledges every chunk it hands out.
type rpcStream struct {
	conn   *rpcConn
	method string
	id     int64
	key    string
	wake   chan struct{}

	mu     sync.Mutex
	chunks [][]json.RawMessage
	err    error
}

// push queues one chunk's values.
func (s *rpcStream) push(values []json.RawMessage) {
	s.mu.Lock()
	if s.err == nil {
		s.chunks = append(s.chunks, values)
	}
	s.mu.Unlock()
	s.signal()
}

// end records the stream's terminal error once; chunks already queued are still handed out.
func (s *rpcStream) end(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.signal()
}

func (s *rpcStream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next returns the next chunk's values and sends its Ack. It returns io.EOF when the server ended
// the stream, and the stream's error otherwise.
func (s *rpcStream) next(ctx context.Context) ([]json.RawMessage, error) {
	for {
		s.mu.Lock()
		if len(s.chunks) > 0 {
			values := s.chunks[0]
			s.chunks = s.chunks[1:]
			s.mu.Unlock()
			if err := s.conn.writeJSON(wireRequestControl{Tag: "Ack", RequestID: s.id}); err != nil {
				return nil, &TransportError{Method: s.method, Reason: "ack failed", Err: err}
			}
			return values, nil
		}
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, fmt.Errorf("t3 %s: %w", s.method, ctx.Err())
		}
	}
}

// interrupt cancels the stream on the server with an Interrupt, unless it already ended.
func (s *rpcStream) interrupt() {
	c := s.conn
	c.mu.Lock()
	_, live := c.streams[s.key]
	delete(c.streams, s.key)
	c.mu.Unlock()
	s.end(errStreamClosed)
	if live {
		_ = c.writeJSON(wireRequestControl{Tag: "Interrupt", RequestID: s.id})
	}
}
