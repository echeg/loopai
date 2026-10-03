// Package acp serves loopai as an Agent Client Protocol (ACP) agent over stdio, so a client such
// as T3 Code can host a loopai run as a provider session.
package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// maxLineSize caps one incoming JSON-RPC line; a longer line is discarded rather than buffered.
const maxLineSize = 16 << 20

// JSON-RPC 2.0 error codes.
const (
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// errLineTooLong reports an incoming line above maxLineSize; the line has already been consumed.
var errLineTooLong = errors.New("acp: line exceeds size limit")

// RequestHandler handles one incoming request. It runs on the read loop, so it must not block;
// long work belongs on another goroutine, which answers later through Reply or ReplyError.
// The id is kept as raw JSON so large numbers and strings round-trip unchanged.
type RequestHandler func(id, params json.RawMessage)

// NotificationHandler handles one incoming notification. It runs on the read loop and must not block.
type NotificationHandler func(params json.RawMessage)

// Conn is a newline-delimited JSON-RPC 2.0 connection. Each message is one line; writes are
// serialized so concurrent senders never interleave within a line.
type Conn struct {
	r io.Reader
	w io.Writer

	wmu sync.Mutex

	mu            sync.RWMutex
	requests      map[string]RequestHandler
	notifications map[string]NotificationHandler
}

// incoming is any message read from the peer. Unknown fields such as traceId and spanId are ignored.
type incoming struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type resultResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   responseError   `json:"error"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NewConn returns a connection reading requests from r and writing messages to w.
func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{
		r:             r,
		w:             w,
		requests:      map[string]RequestHandler{},
		notifications: map[string]NotificationHandler{},
	}
}

// HandleRequest registers the handler for requests of the given method.
func (c *Conn) HandleRequest(method string, h RequestHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests[method] = h
}

// HandleNotification registers the handler for notifications of the given method.
func (c *Conn) HandleNotification(method string, h NotificationHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifications[method] = h
}

// Serve reads and dispatches messages until the reader is exhausted. It returns nil on EOF and
// the read error otherwise. Malformed and oversized lines are skipped, unknown requests are
// answered with CodeMethodNotFound, and unknown notifications and responses are ignored.
func (c *Conn) Serve() error {
	br := bufio.NewReader(c.r)
	for {
		line, err := readLine(br)
		if errors.Is(err, errLineTooLong) {
			continue
		}
		if len(line) > 0 {
			c.dispatch(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("acp: read: %w", err)
		}
	}
}

// readLine returns the next line without its terminator. At EOF it returns any unterminated
// trailing data together with io.EOF. A line above maxLineSize is consumed and reported as
// errLineTooLong.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxLineSize+1 { // +1 allows the newline itself
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			if err != nil {
				return nil, err //nolint:wrapcheck // EOF or the read error is passed through to Serve
			}
			return nil, errLineTooLong
		}
		return bytes.TrimSpace(buf), err
	}
}

func (c *Conn) dispatch(line []byte) {
	var msg incoming
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if msg.Method == "" {
		// a response to an agent-initiated request, or garbage; the agent sends no requests yet
		return
	}
	if len(msg.ID) == 0 {
		c.mu.RLock()
		h := c.notifications[msg.Method]
		c.mu.RUnlock()
		if h != nil {
			h(msg.Params)
		}
		return
	}
	c.mu.RLock()
	h := c.requests[msg.Method]
	c.mu.RUnlock()
	if h == nil {
		_ = c.ReplyError(msg.ID, CodeMethodNotFound, "method not found: "+msg.Method)
		return
	}
	h(msg.ID, msg.Params)
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	return c.send(notification{JSONRPC: "2.0", Method: method, Params: params})
}

// Reply answers the request with the given id. A nil result is sent as JSON null.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	return c.send(resultResponse{JSONRPC: "2.0", ID: id, Result: result})
}

// ReplyError answers the request with the given id with a JSON-RPC error.
func (c *Conn) ReplyError(id json.RawMessage, code int, message string) error {
	return c.send(errorResponse{JSONRPC: "2.0", ID: id, Error: responseError{Code: code, Message: message}})
}

// send marshals v and writes it as one line in a single Write call.
func (c *Conn) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("acp: marshal: %w", err)
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(data); err != nil {
		return fmt.Errorf("acp: write: %w", err)
	}
	return nil
}
