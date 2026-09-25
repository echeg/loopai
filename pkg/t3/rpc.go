package t3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// dialTimeout bounds the WebSocket handshake.
const dialTimeout = 5 * time.Second

// RPC method tags from packages/contracts/src/rpc.ts.
const (
	methodCreateWorktree = "vcs.createWorktree"
	methodTerminalOpen   = "terminal.open"
	methodTerminalWrite  = "terminal.write"
)

// RPCClient speaks T3 Code's Effect RPC protocol over one WebSocket connection. Each frame is a
// JSON message (or a JSON array of messages); requests are answered by an Exit with the same id.
// Calls are serialized; loopai issues a handful of sequential calls per launch.
type RPCClient struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	nextID int64
}

// RPCError is a Failure exit returned by the server.
type RPCError struct {
	Method  string
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("t3: %s failed: %s", e.Method, e.Message)
}

// DialRPC opens the /ws endpoint with the bearer token in the upgrade request.
func DialRPC(ctx context.Context, ep Endpoint) (*RPCClient, error) {
	wsURL, err := websocketURL(ep.BaseURL)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout, Proxy: http.ProxyFromEnvironment}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+ep.Token)
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("t3: websocket handshake: %w", &APIError{Status: resp.StatusCode})
		}
		return nil, fmt.Errorf("t3: websocket dial: %w", err)
	}
	return &RPCClient{conn: conn}, nil
}

func websocketURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("t3: parse server url: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("t3: unsupported server url scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	return u.String(), nil
}

// Close closes the connection.
func (c *RPCClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("t3: close websocket: %w", err)
	}
	return nil
}

type rpcRequest struct {
	Tag     string      `json:"_tag"`
	ID      string      `json:"id"`
	Method  string      `json:"tag"`
	Payload any         `json:"payload"`
	Headers [][2]string `json:"headers"`
}

type rpcMessage struct {
	Tag       string          `json:"_tag"`
	RequestID string          `json:"requestId"`
	Exit      *rpcExit        `json:"exit"`
	Defect    json.RawMessage `json:"defect"`
}

type rpcExit struct {
	Tag   string          `json:"_tag"`
	Value json.RawMessage `json:"value"`
	Cause json.RawMessage `json:"cause"`
}

// Call sends one request and waits for its Exit, decoding a Success value into out when out is
// not nil. The context deadline bounds the whole exchange.
func (c *RPCClient) Call(ctx context.Context, method string, payload, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Minute)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetReadDeadline(time.Now()) })
	defer stop()
	_ = c.conn.SetWriteDeadline(deadline)
	_ = c.conn.SetReadDeadline(deadline)

	req := rpcRequest{Tag: "Request", ID: id, Method: method, Payload: payload, Headers: [][2]string{}}
	if err := c.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("t3: send %s: %w", method, err)
	}

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("t3: %s: %w", method, ctxErr)
			}
			return fmt.Errorf("t3: read %s response: %w", method, err)
		}
		messages, err := decodeFrame(data)
		if err != nil {
			return fmt.Errorf("t3: decode %s response: %w", method, err)
		}
		for _, msg := range messages {
			done, err := c.handle(method, id, msg, out)
			if done {
				return err
			}
		}
	}
}

// handle processes one server message and reports whether the call is complete.
func (c *RPCClient) handle(method, id string, msg rpcMessage, out any) (bool, error) {
	switch msg.Tag {
	case "Ping":
		if err := c.conn.WriteJSON(map[string]string{"_tag": "Pong"}); err != nil {
			return true, fmt.Errorf("t3: send pong: %w", err)
		}
		return false, nil
	case "Defect":
		return true, &RPCError{Method: method, Message: "server defect: " + compactJSON(msg.Defect)}
	case "Exit":
		if msg.RequestID != id || msg.Exit == nil {
			return false, nil
		}
		if msg.Exit.Tag != "Success" {
			return true, &RPCError{Method: method, Message: causeMessage(msg.Exit.Cause)}
		}
		if out == nil || len(msg.Exit.Value) == 0 {
			return true, nil
		}
		if err := json.Unmarshal(msg.Exit.Value, out); err != nil {
			return true, fmt.Errorf("t3: decode %s result: %w", method, err)
		}
		return true, nil
	default:
		// chunks of server-initiated streams, acks and pongs are not ours to answer
		return false, nil
	}
}

func decodeFrame(data []byte) ([]rpcMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var msgs []rpcMessage
		if err := json.Unmarshal(trimmed, &msgs); err != nil {
			return nil, fmt.Errorf("decode message batch: %w", err)
		}
		return msgs, nil
	}
	var msg rpcMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return []rpcMessage{msg}, nil
}

// causeMessage extracts a readable reason from an encoded Effect Cause, which is an array of
// {_tag:"Fail", error} or {_tag:"Die", defect} entries.
func causeMessage(raw json.RawMessage) string {
	var entries []struct {
		Tag    string          `json:"_tag"`
		Error  json.RawMessage `json:"error"`
		Defect json.RawMessage `json:"defect"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return compactJSON(raw)
	}
	var parts []string
	for _, e := range entries {
		switch {
		case len(e.Error) > 0:
			parts = append(parts, errorMessage(e.Error))
		case len(e.Defect) > 0:
			parts = append(parts, compactJSON(e.Defect))
		default:
			parts = append(parts, e.Tag)
		}
	}
	return strings.Join(parts, "; ")
}

func errorMessage(raw json.RawMessage) string {
	var e struct {
		Tag     string `json:"_tag"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return compactJSON(raw)
	}
	msg := e.Message
	if msg == "" {
		msg = e.Detail
	}
	switch {
	case e.Tag != "" && msg != "":
		return e.Tag + ": " + msg
	case msg != "":
		return msg
	case e.Tag != "":
		return e.Tag
	default:
		return compactJSON(raw)
	}
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	const limit = 300
	if buf.Len() > limit {
		return buf.String()[:limit] + "…"
	}
	return buf.String()
}

// CreateWorktreeInput mirrors VcsCreateWorktreeInput: T3 runs
// `git worktree add -b <NewRefName> <path> <RefName>`; a null path selects T3's managed location.
type CreateWorktreeInput struct {
	Cwd        string  `json:"cwd"`
	RefName    string  `json:"refName"`
	NewRefName string  `json:"newRefName,omitempty"`
	Path       *string `json:"path"`
}

// Worktree is the worktree T3 Code created.
type Worktree struct {
	Path    string `json:"path"`
	RefName string `json:"refName"`
}

// CreateWorktree asks T3 Code to create a worktree it manages.
func (c *RPCClient) CreateWorktree(ctx context.Context, in CreateWorktreeInput) (Worktree, error) {
	var result struct {
		Worktree Worktree `json:"worktree"`
	}
	if err := c.Call(ctx, methodCreateWorktree, in, &result); err != nil {
		return Worktree{}, err
	}
	if result.Worktree.Path == "" {
		return Worktree{}, errors.New("t3: vcs.createWorktree returned no path")
	}
	return result.Worktree, nil
}

// TerminalOpenInput mirrors TerminalOpenInput.
type TerminalOpenInput struct {
	ThreadID     string            `json:"threadId"`
	TerminalID   string            `json:"terminalId"`
	Cwd          string            `json:"cwd"`
	WorktreePath string            `json:"worktreePath,omitempty"`
	Cols         int               `json:"cols,omitempty"`
	Rows         int               `json:"rows,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// OpenTerminal opens (or reattaches) a terminal inside a thread.
func (c *RPCClient) OpenTerminal(ctx context.Context, in TerminalOpenInput) error {
	return c.Call(ctx, methodTerminalOpen, in, nil)
}

// WriteTerminal writes input to a thread terminal.
func (c *RPCClient) WriteTerminal(ctx context.Context, threadID, terminalID, data string) error {
	payload := map[string]string{"threadId": threadID, "terminalId": terminalID, "data": data}
	return c.Call(ctx, methodTerminalWrite, payload, nil)
}
