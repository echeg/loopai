package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ACP methods the provider serves or sends.
const (
	methodInitialize    = "initialize"
	methodSessionNew    = "session/new"
	methodSessionLoad   = "session/load"
	methodSessionPrompt = "session/prompt"
	methodSessionCancel = "session/cancel"
	methodSessionUpdate = "session/update"
)

const (
	// protocolVersion is the ACP version the provider answers. Answering 1 without an info key keeps
	// T3 Code on the v1 method set; an info key would switch it to v2 methods.
	protocolVersion = 1
	// agentName is the agentInfo.name the provider reports.
	agentName = "loopai"
)

// Fixed error texts. Errors never echo client-supplied strings: T3 Code marks a provider
// unauthenticated when an error text mentions authentication, credentials, or logging in, and a
// method name or session id from the client could.
const (
	errTextMethodNotFound = "method not found"
	errTextInvalidParams  = "invalid params"
	errTextUnknownSession = "unknown session"
)

// StopReason answers a session/prompt.
type StopReason string

// Stop reasons. T3 Code records a turn answered StopCancelled as interrupted.
const (
	StopEndTurn   StopReason = "end_turn"
	StopCancelled StopReason = "cancelled" //nolint:misspell // ACP wire value
)

// ErrProviderStopped is returned for a message the provider did not send because it stopped.
var ErrProviderStopped = errors.New("acp: provider stopped")

// Session describes one session/new or session/load call.
type Session struct {
	ID  string
	Cwd string
	// MCPServers is the raw mcpServers parameter. T3 Code's entry carries a bearer token in its env,
	// so it is never logged.
	MCPServers json.RawMessage
	// ClientName is clientInfo.name from initialize, empty when the client sent none.
	ClientName string
}

// PromptRequest is one session/prompt.
type PromptRequest struct {
	SessionID string
	Cwd       string            // recorded by the session's session/new or session/load
	Blocks    []json.RawMessage // the prompt's content blocks as sent; ParsePrompt reads the message
}

// Handler serves the provider's sessions. NewSession, LoadSession, and Prompt run on goroutines of
// their own, with a context canceled when the provider stops; Cancel runs on the read loop, in
// message order, and must not block.
type Handler interface {
	// NewSession is called for session/new with a fresh session id, which is answered once it
	// returns. session/new always succeeds, for any cwd.
	NewSession(ctx context.Context, s Session)
	// LoadSession is called for session/load with whatever id the client sent. {} is answered once
	// it returns, so it must return promptly; session/load never fails.
	LoadSession(ctx context.Context, s Session)
	// Prompt runs one session/prompt and returns its stop reason, which is answered unless the
	// provider stopped meanwhile. Updates sends the session's session/update notifications.
	Prompt(ctx context.Context, req PromptRequest, updates *Updates) StopReason
	// Cancel reports a session/cancel from the client. The provider stopping never calls it.
	Cancel(sessionID string)
}

// Provider is the agent side of one ACP connection from T3 Code's acpRegistry driver. It answers
// initialize with a fixed ACP v1 response, hands session/new, session/load, session/prompt, and
// session/cancel to its Handler, and answers every other request with a method-not-found error.
// It never sends requests of its own, so it never asks the client for permission.
//
// A Provider stops when its input ends or Shutdown is called. A stopped provider cancels the
// context its handlers run with and sends nothing more: an open prompt is never answered, and
// Cancel is not called.
type Provider struct {
	conn    *Conn
	handler Handler
	version string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	stopped    bool
	clientName string
	sessions   map[string]string // session id -> cwd
}

// NewProvider returns a provider reading client messages from r and writing to w. version is
// reported as agentInfo.version.
func NewProvider(r io.Reader, w io.Writer, version string, h Handler) *Provider {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Provider{
		conn:     NewConn(r, w),
		handler:  h,
		version:  version,
		ctx:      ctx,
		cancel:   cancel,
		sessions: map[string]string{},
	}
	p.conn.HandleRequest(methodInitialize, p.handleInitialize)
	p.conn.HandleRequest(methodSessionNew, p.handleNewSession)
	p.conn.HandleRequest(methodSessionLoad, p.handleLoadSession)
	p.conn.HandleRequest(methodSessionPrompt, p.handlePrompt)
	p.conn.HandleNotification(methodSessionCancel, p.handleCancel)
	// session/cancel is a notification in ACP; a client sending it as a request still gets an answer
	p.conn.HandleRequest(methodSessionCancel, func(id, params json.RawMessage) {
		p.handleCancel(params)
		_ = p.reply(id, struct{}{})
	})
	p.conn.HandleUnknownRequest(func(id json.RawMessage, _ string) {
		_ = p.replyError(id, CodeMethodNotFound, errTextMethodNotFound)
	})
	return p
}

// Serve handles client messages until the input ends, then stops the provider without waiting for
// its handlers; Shutdown waits for them. It returns nil on EOF and the read error otherwise.
func (p *Provider) Serve() error {
	err := p.conn.Serve()
	p.stop()
	return err
}

// Shutdown stops the provider and waits until its handlers have returned or ctx is done. It never
// answers an open prompt and never calls Cancel. Serve keeps blocking on its input, so a caller
// stopping on a signal exits without waiting for Serve.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.stop()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("acp: wait for handlers: %w", ctx.Err())
	}
}

// stop marks the provider stopped and cancels its handlers' context.
func (p *Provider) stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.cancel()
}

// isStopped reports whether the provider stopped.
func (p *Provider) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// spawn runs fn on a goroutine counted by Shutdown, unless the provider stopped.
func (p *Provider) spawn(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.wg.Go(fn)
}

// reply answers a request unless the provider stopped. An answer already being written when the
// provider stops still completes.
func (p *Provider) reply(id json.RawMessage, result any) error {
	if p.isStopped() {
		return ErrProviderStopped
	}
	return p.conn.Reply(id, result)
}

// replyError answers a request with an error unless the provider stopped. Callers never pass
// -32000, which T3 Code reads as an authentication failure.
func (p *Provider) replyError(id json.RawMessage, code int, message string) error {
	if p.isStopped() {
		return ErrProviderStopped
	}
	return p.conn.ReplyError(id, code, message)
}

// notify sends a notification unless the provider stopped.
func (p *Provider) notify(method string, params any) error {
	if p.isStopped() {
		return ErrProviderStopped
	}
	return p.conn.Notify(method, params)
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []struct{}        `json:"authMethods"`
	AgentInfo         implementation    `json:"agentInfo"`
}

type agentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
	MCPCapabilities    mcpCapabilities    `json:"mcpCapabilities"`
}

type promptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// mcpCapabilities deliberately has no acp field: advertising it makes T3 Code send its MCP entry
// without the stdio bridge that carries the endpoint.
type mcpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize records the client's name and answers the fixed v1 response, whatever version
// the client asked for.
func (p *Provider) handleInitialize(id, params json.RawMessage) {
	var req struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if json.Unmarshal(params, &req) == nil {
		p.mu.Lock()
		p.clientName = req.ClientInfo.Name
		p.mu.Unlock()
	}
	_ = p.reply(id, initializeResult{
		ProtocolVersion:   protocolVersion,
		AgentCapabilities: agentCapabilities{LoadSession: true},
		AuthMethods:       []struct{}{},
		AgentInfo:         implementation{Name: agentName, Version: p.version},
	})
}

// decodeSession reads session/new and session/load params. A malformed field is left empty: neither
// method ever fails.
func (p *Provider) decodeSession(params json.RawMessage) Session {
	var req struct {
		SessionID  string          `json:"sessionId"`
		Cwd        string          `json:"cwd"`
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	_ = json.Unmarshal(params, &req) // a type mismatch leaves the other fields decoded
	p.mu.Lock()
	defer p.mu.Unlock()
	return Session{ID: req.SessionID, Cwd: req.Cwd, MCPServers: req.MCPServers, ClientName: p.clientName}
}

// register records a session's cwd for its prompts.
func (p *Provider) register(sessionID, cwd string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[sessionID] = cwd
}

// sessionCwd returns the cwd recorded for a session.
func (p *Provider) sessionCwd(sessionID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cwd, ok := p.sessions[sessionID]
	return cwd, ok
}

// handleNewSession answers a fresh session id once the handler's NewSession returns. It reads no
// config, repository, or file, so it succeeds for any cwd, the background probe's included.
func (p *Provider) handleNewSession(id, params json.RawMessage) {
	s := p.decodeSession(params)
	s.ID = newSessionID()
	p.register(s.ID, s.Cwd)
	p.spawn(func() {
		p.handler.NewSession(p.ctx, s)
		_ = p.reply(id, struct {
			SessionID string `json:"sessionId"`
		}{s.ID})
	})
}

// handleLoadSession registers any session id and answers {} once the handler's LoadSession
// returns. T3 Code falls back to a new session when a load fails, so a load never fails.
func (p *Provider) handleLoadSession(id, params json.RawMessage) {
	s := p.decodeSession(params)
	if s.ID == "" {
		_ = p.reply(id, struct{}{})
		return
	}
	p.register(s.ID, s.Cwd)
	p.spawn(func() {
		p.handler.LoadSession(p.ctx, s)
		_ = p.reply(id, struct{}{})
	})
}

// handlePrompt runs a prompt of a known session on its own goroutine and answers its stop reason.
func (p *Provider) handlePrompt(id, params json.RawMessage) {
	var req struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(params, &req); err != nil || req.SessionID == "" {
		_ = p.replyError(id, CodeInvalidParams, errTextInvalidParams)
		return
	}
	cwd, ok := p.sessionCwd(req.SessionID)
	if !ok {
		_ = p.replyError(id, CodeInvalidParams, errTextUnknownSession)
		return
	}
	pr := PromptRequest{SessionID: req.SessionID, Cwd: cwd, Blocks: req.Prompt}
	updates := &Updates{p: p, sessionID: req.SessionID, messageID: newMessageID()}
	p.spawn(func() {
		reason := p.handler.Prompt(p.ctx, pr, updates)
		if reason == "" {
			reason = StopEndTurn
		}
		_ = p.reply(id, struct {
			StopReason StopReason `json:"stopReason"`
		}{reason})
	})
}

// handleCancel reports a session/cancel to the handler, unless the provider stopped.
func (p *Provider) handleCancel(params json.RawMessage) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &req) != nil || req.SessionID == "" || p.isStopped() {
		return
	}
	p.handler.Cancel(req.SessionID)
}

// newSessionID returns a fresh session id: one safe path segment, unique in practice.
func newSessionID() string {
	return "loopai-" + randomHex(16)
}

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return hex.EncodeToString(b)
}
