// Package t3 is a client of the local T3 Code server's orchestration protocol 2: the WebSocket RPC
// at /ws?orchestrationProtocol=2 (terminals, the thread projection, and thread commands) and the
// HTTP project list. It locates the server through T3 Code's server-runtime.json, which it only
// ever reads, and authenticates with a Bearer token it never logs.
package t3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Method tags of the protocol-2 WebSocket RPC.
const (
	methodTerminalOpen              = "terminal.open"
	methodTerminalWrite             = "terminal.write"
	methodSubscribeTerminalMetadata = "subscribeTerminalMetadata"
	methodGetThreadProjection       = "orchestration.getThreadProjection"
	methodDispatchCommand           = "orchestration.dispatchCommand"
)

// Thread command types loopai dispatches.
const (
	commandMetadataUpdate = "thread.metadata.update"
	commandPin            = "thread.pin"
	commandUnpin          = "thread.unpin"
)

const (
	// defaultTerminalID is the UI's own first terminal; loopai never opens or writes it.
	defaultTerminalID = "term-1"
	// maxTerminalIDLen and maxTerminalWrite are the server's limits.
	maxTerminalIDLen = 128
	maxTerminalWrite = 65536
	// maxProjectsBody caps the /api/projects reply.
	maxProjectsBody = 32 << 20
	// httpTimeout bounds one HTTP API request.
	httpTimeout = 30 * time.Second
)

// Client talks to the local T3 Code server with one Bearer token. Connect opens a WebSocket
// connection, re-reading server-runtime.json every time, and replaces the previous one; every other
// method uses the current connection. A Client also remembers the command id of a thread command
// whose outcome is unknown, so a retry of the same update on a later connection reuses it.
// A Client is safe for concurrent use.
type Client struct {
	token        string
	pingInterval time.Duration
	maxFrame     int64
	httpClient   *http.Client

	mu      sync.Mutex
	conn    *rpcConn
	runtime Runtime
	unknown map[string]pendingCommand
}

// pendingCommand is a thread's last dispatched command whose outcome is unknown.
type pendingCommand struct {
	fingerprint string
	commandID   string
}

// Option configures a Client.
type Option func(*Client)

// WithPingInterval sets how often the client pings; a ping unanswered by the next one closes the
// connection. The default is 10 seconds.
func WithPingInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.pingInterval = d
		}
	}
}

// withMaxFrameSize lowers the frame limit, so a test can exceed it without a 64 MiB reply.
func withMaxFrameSize(n int64) Option {
	return func(c *Client) { c.maxFrame = n }
}

// NewClient returns a Client that authenticates with token. It does not connect.
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		token:        token,
		pingInterval: defaultPingInterval,
		maxFrame:     maxFrameSize,
		httpClient:   &http.Client{Timeout: httpTimeout},
		unknown:      map[string]pendingCommand{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect re-reads the server runtime file (LOOPAI_T3_URL overriding its origin), opens a new
// WebSocket connection, and closes the previous one; a failed Connect keeps the previous one. It
// returns the runtime the connection was made against, so a caller can tell a server restart by its
// PID or StartedAt. A missing runtime file is ErrNotRunning; a rejected upgrade is a
// *HandshakeError, which matches ErrProtocolUnsupported for a 426.
func (c *Client) Connect(ctx context.Context) (Runtime, error) {
	rt, err := ResolveRuntime()
	if err != nil {
		return Runtime{}, err
	}
	wsURL, err := WebSocketURL(rt.Origin)
	if err != nil {
		return Runtime{}, err
	}
	conn, err := dialRPC(ctx, wsURL, c.token, c.pingInterval, c.maxFrame)
	if err != nil {
		return Runtime{}, err
	}
	c.mu.Lock()
	old := c.conn
	c.conn, c.runtime = conn, rt
	c.mu.Unlock()
	if old != nil {
		old.close()
	}
	return rt, nil
}

// Close closes the current connection; pending calls and subscriptions fail with ErrClosed.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		conn.close()
	}
	return nil
}

// current returns the current connection and the runtime it was made against.
func (c *Client) current(method string) (*rpcConn, Runtime, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, Runtime{}, &TransportError{Method: method, Reason: "not connected", Err: ErrClosed}
	}
	return c.conn, c.runtime, nil
}

// call performs a unary call on the current connection.
func (c *Client) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	conn, _, err := c.current(method)
	if err != nil {
		return nil, err
	}
	return conn.call(ctx, method, payload)
}

// TerminalOpenInput is the terminal.open payload. WorktreePath is sent only when non-empty, and
// Cols, Rows, and Env only when set. It has no providerInstanceId field: loopai never sends one.
type TerminalOpenInput struct {
	ThreadID     string            `json:"threadId"`
	TerminalID   string            `json:"terminalId"`
	Cwd          string            `json:"cwd"`
	WorktreePath string            `json:"worktreePath,omitempty"`
	Cols         int               `json:"cols,omitempty"`
	Rows         int               `json:"rows,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// Terminal statuses.
const (
	TerminalStarting = "starting"
	TerminalRunning  = "running"
	TerminalExited   = "exited"
	TerminalError    = "error"
)

// TerminalSummary describes one terminal session, as terminal.open and the metadata stream report
// it. Nullable fields are nil when null; terminal.open replies carry no HasRunningSubprocess.
type TerminalSummary struct {
	ThreadID             string  `json:"threadId"`
	TerminalID           string  `json:"terminalId"`
	Cwd                  string  `json:"cwd"`
	WorktreePath         *string `json:"worktreePath"`
	Status               string  `json:"status"`
	PID                  *int    `json:"pid"`
	ExitCode             *int    `json:"exitCode"`
	ExitSignal           *int    `json:"exitSignal"`
	HasRunningSubprocess bool    `json:"hasRunningSubprocess"`
	Label                string  `json:"label"`
	UpdatedAt            string  `json:"updatedAt"`
}

// TerminalOpen opens the terminal in.TerminalID of a thread, creating a fresh shell for an unknown
// id. Reopening a live terminal with a different cwd, env, or worktree path resets it, so a caller
// reopens only with its original launch context.
func (c *Client) TerminalOpen(ctx context.Context, in TerminalOpenInput) (TerminalSummary, error) {
	if err := validateTerminalID(in.TerminalID); err != nil {
		return TerminalSummary{}, err
	}
	if in.ThreadID == "" || in.Cwd == "" {
		return TerminalSummary{}, errors.New("t3: terminal.open needs a thread id and a cwd")
	}
	value, err := c.call(ctx, methodTerminalOpen, in)
	if err != nil {
		return TerminalSummary{}, err
	}
	var session TerminalSummary
	if err := decodeValue(methodTerminalOpen, value, &session); err != nil {
		return TerminalSummary{}, err
	}
	if session.TerminalID == "" || session.Status == "" {
		return TerminalSummary{}, fmt.Errorf("%w: terminal.open reply lacks terminalId or status", ErrProtocolUnsupported)
	}
	return session, nil
}

// TerminalWrite types data into a terminal. The server fails it with TerminalNotRunningError while
// the terminal is starting or errored, and drops it once the terminal exited.
func (c *Client) TerminalWrite(ctx context.Context, threadID, terminalID, data string) error {
	if err := validateTerminalID(terminalID); err != nil {
		return err
	}
	if data == "" || len(data) > maxTerminalWrite {
		return fmt.Errorf("t3: terminal.write data must be 1 to %d bytes", maxTerminalWrite)
	}
	payload := struct {
		ThreadID   string `json:"threadId"`
		TerminalID string `json:"terminalId"`
		Data       string `json:"data"`
	}{threadID, terminalID, data}
	_, err := c.call(ctx, methodTerminalWrite, payload)
	return err
}

// validateTerminalID enforces the server's id limits and keeps loopai off the UI's own terminal.
func validateTerminalID(id string) error {
	switch {
	case id == "" || len(id) > maxTerminalIDLen:
		return fmt.Errorf("t3: terminal id must be 1 to %d characters", maxTerminalIDLen)
	case id == defaultTerminalID:
		return fmt.Errorf("t3: terminal id %q belongs to the T3 Code UI", defaultTerminalID)
	}
	return nil
}

// MetadataEventType names a terminal metadata event.
type MetadataEventType string

// Terminal metadata event types; a snapshot always comes first.
const (
	MetadataSnapshot MetadataEventType = "snapshot"
	MetadataUpsert   MetadataEventType = "upsert"
	MetadataRemove   MetadataEventType = "remove"
)

// TerminalMetadataEvent is one item of the terminal metadata stream. A snapshot carries Terminals,
// an upsert Terminal, and a remove ThreadID and TerminalID. Events cover every thread's terminals.
type TerminalMetadataEvent struct {
	Type       MetadataEventType
	Terminals  []TerminalSummary
	Terminal   TerminalSummary
	ThreadID   string
	TerminalID string
}

// TerminalMetadataSubscription is a live subscribeTerminalMetadata stream. It acknowledges every
// chunk it receives, which the server waits for before sending the next.
type TerminalMetadataSubscription struct {
	ctx     context.Context
	stream  *rpcStream
	stop    func() bool
	pending []TerminalMetadataEvent
}

// SubscribeTerminalMetadata subscribes to terminal metadata. Canceling ctx, or Close, unsubscribes
// with an Interrupt.
func (c *Client) SubscribeTerminalMetadata(ctx context.Context) (*TerminalMetadataSubscription, error) {
	conn, _, err := c.current(methodSubscribeTerminalMetadata)
	if err != nil {
		return nil, err
	}
	stream, err := conn.stream(methodSubscribeTerminalMetadata, struct{}{})
	if err != nil {
		return nil, err
	}
	sub := &TerminalMetadataSubscription{ctx: ctx, stream: stream}
	sub.stop = context.AfterFunc(ctx, stream.interrupt)
	return sub, nil
}

// Next returns the next event. It blocks until one arrives, and returns io.EOF when the server ends
// the stream, the context's error after cancellation, and a *TransportError when the connection is
// lost. Next is not safe for concurrent use.
func (s *TerminalMetadataSubscription) Next() (TerminalMetadataEvent, error) {
	for len(s.pending) == 0 {
		values, err := s.stream.next(s.ctx)
		if err != nil {
			if ctxErr := s.ctx.Err(); ctxErr != nil {
				return TerminalMetadataEvent{}, fmt.Errorf("t3 %s: %w", methodSubscribeTerminalMetadata, ctxErr)
			}
			return TerminalMetadataEvent{}, err
		}
		for _, raw := range values {
			event, known, err := decodeMetadataEvent(raw)
			if err != nil {
				return TerminalMetadataEvent{}, err
			}
			if known {
				s.pending = append(s.pending, event)
			}
		}
	}
	event := s.pending[0]
	s.pending = s.pending[1:]
	return event, nil
}

// Close unsubscribes with an Interrupt; Next then returns an error.
func (s *TerminalMetadataSubscription) Close() error {
	s.stop()
	s.stream.interrupt()
	return nil
}

// decodeMetadataEvent decodes one stream item. An unknown event type is skipped (known is false); a
// known one missing its fields is ErrProtocolUnsupported.
func decodeMetadataEvent(raw json.RawMessage) (event TerminalMetadataEvent, known bool, err error) {
	var wire struct {
		Type       MetadataEventType  `json:"type"`
		Terminals  *[]TerminalSummary `json:"terminals"`
		Terminal   *TerminalSummary   `json:"terminal"`
		ThreadID   string             `json:"threadId"`
		TerminalID string             `json:"terminalId"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return event, false, fmt.Errorf("%w: undecodable terminal metadata: %w", ErrProtocolUnsupported, err)
	}
	event.Type = wire.Type
	switch wire.Type {
	case MetadataSnapshot:
		if wire.Terminals == nil {
			return event, false, fmt.Errorf("%w: terminal metadata snapshot lacks terminals", ErrProtocolUnsupported)
		}
		event.Terminals = *wire.Terminals
	case MetadataUpsert:
		if wire.Terminal == nil || wire.Terminal.TerminalID == "" {
			return event, false, fmt.Errorf("%w: terminal metadata upsert lacks a terminal", ErrProtocolUnsupported)
		}
		event.Terminal = *wire.Terminal
	case MetadataRemove:
		if wire.ThreadID == "" || wire.TerminalID == "" {
			return event, false, fmt.Errorf("%w: terminal metadata remove lacks ids", ErrProtocolUnsupported)
		}
		event.ThreadID, event.TerminalID = wire.ThreadID, wire.TerminalID
	default:
		return event, false, nil
	}
	return event, true, nil
}

// ThreadProjection holds the thread fields loopai reads from orchestration.getThreadProjection.
// Nullable fields are nil when null or absent. A non-nil TitleRegeneration means T3 is generating a
// title, which any title write cancels.
type ThreadProjection struct {
	ID                string
	ProjectID         string
	Title             string
	WorktreePath      *string
	PinnedAt          *time.Time
	ArchivedAt        *time.Time
	TitleRegeneration *TitleRegeneration
}

// TitleRegeneration is a pending title generation.
type TitleRegeneration struct {
	RequestID string    `json:"requestId"`
	StartedAt time.Time `json:"startedAt"`
}

// GetThreadProjection reads a thread. A reply without the thread, its title, or its project id is
// ErrProtocolUnsupported.
func (c *Client) GetThreadProjection(ctx context.Context, threadID string) (ThreadProjection, error) {
	value, err := c.call(ctx, methodGetThreadProjection, struct {
		ThreadID string `json:"threadId"`
	}{threadID})
	if err != nil {
		return ThreadProjection{}, err
	}
	var wire struct {
		Thread *struct {
			ID                string             `json:"id"`
			ProjectID         *string            `json:"projectId"`
			Title             *string            `json:"title"`
			WorktreePath      *string            `json:"worktreePath"`
			PinnedAt          *time.Time         `json:"pinnedAt"`
			ArchivedAt        *time.Time         `json:"archivedAt"`
			TitleRegeneration *TitleRegeneration `json:"titleRegeneration"`
		} `json:"thread"`
	}
	if err := decodeValue(methodGetThreadProjection, value, &wire); err != nil {
		return ThreadProjection{}, err
	}
	if wire.Thread == nil || wire.Thread.Title == nil || wire.Thread.ProjectID == nil {
		return ThreadProjection{}, fmt.Errorf("%w: thread projection lacks thread.title or thread.projectId",
			ErrProtocolUnsupported)
	}
	t := wire.Thread
	return ThreadProjection{
		ID: t.ID, ProjectID: *t.ProjectID, Title: *t.Title, WorktreePath: t.WorktreePath,
		PinnedAt: t.PinnedAt, ArchivedAt: t.ArchivedAt, TitleRegeneration: t.TitleRegeneration,
	}, nil
}

// threadCommand is a thread command loopai dispatches. It never carries createdBy,
// creationSource, or providerInstanceId.
type threadCommand struct {
	Type      string `json:"type"`
	CommandID string `json:"commandId"`
	ThreadID  string `json:"threadId"`
	Title     string `json:"title,omitempty"`
}

// SetTitle sets a thread's title with a title-only thread.metadata.update.
func (c *Client) SetTitle(ctx context.Context, threadID, title string) error {
	if title == "" {
		return errors.New("t3: a thread title cannot be empty")
	}
	return c.dispatch(ctx, threadCommand{Type: commandMetadataUpdate, ThreadID: threadID, Title: title})
}

// Pin pins a thread, which keeps T3 from auto-settling it.
func (c *Client) Pin(ctx context.Context, threadID string) error {
	return c.dispatch(ctx, threadCommand{Type: commandPin, ThreadID: threadID})
}

// Unpin unpins a thread.
func (c *Client) Unpin(ctx context.Context, threadID string) error {
	return c.dispatch(ctx, threadCommand{Type: commandUnpin, ThreadID: threadID})
}

// dispatch sends one command through orchestration.dispatchCommand. Every logical update gets a
// fresh command id, the server's idempotency key; only when the previous command for the thread
// ended with an unknown outcome (a lost connection, a Die, a canceled call) and this one is the
// same update is its id reused, so a retry can never apply twice. A typed failure (*RemoteError)
// is a definite answer and forgets the id.
func (c *Client) dispatch(ctx context.Context, cmd threadCommand) error {
	fingerprint := cmd.Type + "\x00" + cmd.Title
	c.mu.Lock()
	if prev, ok := c.unknown[cmd.ThreadID]; ok && prev.fingerprint == fingerprint {
		cmd.CommandID = prev.commandID
	}
	c.mu.Unlock()
	if cmd.CommandID == "" {
		id, err := newCommandID()
		if err != nil {
			return err
		}
		cmd.CommandID = id
	}
	c.mu.Lock()
	c.unknown[cmd.ThreadID] = pendingCommand{fingerprint: fingerprint, commandID: cmd.CommandID}
	c.mu.Unlock()

	_, err := c.call(ctx, methodDispatchCommand, cmd)
	var remote *RemoteError
	if err == nil || errors.As(err, &remote) {
		c.mu.Lock()
		if c.unknown[cmd.ThreadID].commandID == cmd.CommandID {
			delete(c.unknown, cmd.ThreadID)
		}
		c.mu.Unlock()
	}
	return err
}

// newCommandID returns a fresh 32-hex command id.
func newCommandID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("t3: generate command id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Project is a T3 Code project from GET /api/projects.
type Project struct {
	ID            string `json:"id"`
	WorkspaceRoot string `json:"workspaceRoot"`
}

// Projects lists the server's projects through GET /api/projects on the current connection's
// origin. A rejected request is a *HandshakeError; an undecodable reply is ErrProtocolUnsupported.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	_, rt, err := c.current("GET /api/projects")
	if err != nil {
		return nil, err
	}
	origin, err := httpOrigin(rt.Origin)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/projects", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("t3: build projects request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t3: GET /api/projects: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, handshakeError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProjectsBody))
	if err != nil {
		return nil, fmt.Errorf("t3: read /api/projects: %w", err)
	}
	var body struct {
		Projects *[]Project `json:"projects"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Projects == nil {
		return nil, fmt.Errorf("%w: undecodable /api/projects reply", ErrProtocolUnsupported)
	}
	return *body.Projects, nil
}

// decodeValue decodes a Success value; a value that does not decode is ErrProtocolUnsupported.
func decodeValue(method string, value json.RawMessage, v any) error {
	if len(value) == 0 {
		return fmt.Errorf("%w: %s reply has no value", ErrProtocolUnsupported, method)
	}
	if err := json.Unmarshal(value, v); err != nil {
		return fmt.Errorf("%w: undecodable %s reply: %w", ErrProtocolUnsupported, method, err)
	}
	return nil
}
