package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
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
	errTextInternal       = "internal error"
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
	// so it is never logged, and String and GoString report only its size.
	MCPServers json.RawMessage
	// ClientName is clientInfo.name from initialize, empty when the client sent none.
	ClientName string
}

// String describes the session with its MCP servers redacted to their size, so formatting a session
// with %v, %+v, or %s never prints the bearer token T3 Code's entry carries.
func (s Session) String() string {
	return fmt.Sprintf("{ID:%s Cwd:%s MCPServers:[%d bytes redacted] ClientName:%s}", s.ID, s.Cwd, len(s.MCPServers),
		s.ClientName)
}

// GoString is String, so %#v redacts too.
func (s Session) GoString() string { return "acp.Session" + s.String() }

// PromptRequest is one session/prompt.
type PromptRequest struct {
	SessionID string
	Cwd       string            // recorded by the session's session/new or session/load
	Blocks    []json.RawMessage // the prompt's content blocks as sent; ParsePrompt reads the message
	// Canceled is closed by the first session/cancel for this session that the provider reads after
	// this prompt while the prompt is open, which lasts until its answer is sent. The provider records
	// the prompt before it reads the next message, so such a cancel closes it even when it is handled
	// before Prompt is called. A cancel read before this prompt never closes it, and neither does the
	// provider stopping.
	Canceled <-chan struct{}
}

// Handler serves the provider's sessions. NewSession, LoadSession, and Prompt run on goroutines of
// their own, with a context canceled when the provider stops. Cancel runs on the read loop, in
// message order, and must not block. A Prompt of a session is called only once the NewSession or
// LoadSession call that opened the session has returned, so it sees what that call recorded even when
// the client sent the prompt before the session's answer.
//
// Because Prompt starts on its own goroutine, a session/cancel read right after a session/prompt
// can reach Cancel before that prompt reaches Prompt. A handler therefore learns that a prompt was
// canceled from PromptRequest.Canceled, which is already closed when Cancel runs, and not from
// whether Prompt was called before Cancel.
//
// A panic in any of the four methods is recovered and logged, and the provider keeps serving: a
// session/new or session/load whose hook panicked is answered with an internal error, a prompt whose
// Prompt panicked with StopEndTurn.
type Handler interface {
	// NewSession is called for session/new with a fresh session id, which is answered once it
	// returns. session/new always succeeds, for any cwd, unless NewSession panics.
	NewSession(ctx context.Context, s Session)
	// LoadSession is called for session/load with whatever id the client sent. {} is answered once
	// it returns, so it must return promptly; session/load never fails unless LoadSession panics.
	LoadSession(ctx context.Context, s Session)
	// Prompt runs one session/prompt and returns its stop reason, which is answered unless the
	// provider stopped meanwhile. Updates sends the session's session/update notifications.
	Prompt(ctx context.Context, req PromptRequest, updates *Updates) StopReason
	// Cancel reports a session/cancel from the client, after the Canceled channels of the session's
	// open prompts are closed. It is called whether or not a prompt is open, and it may run before
	// the Prompt call of a prompt received just before the cancel. The provider stopping never calls
	// it.
	Cancel(sessionID string)
}

// Provider is the agent side of one ACP connection from T3 Code's acpRegistry driver. It answers
// initialize with a fixed ACP v1 response, hands session/new, session/load, session/prompt, and
// session/cancel to its Handler, and answers every other request with a method-not-found error.
// It never sends requests of its own, so it never asks the client for permission.
//
// A Provider stops when its input ends or Shutdown is called. A stopped provider cancels the
// context its handlers run with and sends nothing more: an open prompt is never answered, its
// Canceled channel is not closed, and Cancel is not called.
type Provider struct {
	conn    *Conn
	handler Handler
	version string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	stopped    bool
	errLog     io.Writer // where recovered handler panics are reported
	clientName string
	sessions   map[string]session         // session id -> its cwd and readiness
	open       map[string][]chan struct{} // session id -> Canceled channels of prompts not yet canceled or ended
}

// session is what the provider records for a session/new or session/load: the session's cwd, and a
// channel closed once the handler call that opened it returned.
type session struct {
	cwd   string
	ready chan struct{}
}

// NewProvider returns a provider reading client messages from r and writing to w. version is
// reported as agentInfo.version. Recovered handler panics are reported on os.Stderr until
// SetErrorLog names another writer.
func NewProvider(r io.Reader, w io.Writer, version string, h Handler) *Provider {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Provider{
		conn:     NewConn(r, w),
		handler:  h,
		version:  version,
		ctx:      ctx,
		cancel:   cancel,
		errLog:   os.Stderr,
		sessions: map[string]session{},
		open:     map[string][]chan struct{}{},
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

// SetErrorLog names the writer recovered handler panics are reported on. It never receives the
// protocol's own traffic, so it must not be the provider's output.
func (p *Provider) SetErrorLog(w io.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errLog = w
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

// spawn runs fn on a goroutine counted by Shutdown, unless the provider stopped. It reports whether
// fn was started. A panic in fn is recovered and reported under name, then onPanic answers the
// request fn was serving, so a handler bug never ends the process or leaves a request unanswered.
func (p *Provider) spawn(name string, fn, onPanic func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return false
	}
	p.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				p.logPanic(name, r)
				onPanic()
			}
		}()
		fn()
	})
	return true
}

// logPanic reports a recovered handler panic and its stack on the error log.
func (p *Provider) logPanic(name string, r any) {
	p.mu.Lock()
	w := p.errLog
	p.mu.Unlock()
	if w != nil {
		_, _ = fmt.Fprintf(w, "acp: recovered from a panic in the %s handler: %v\n%s", name, r, debug.Stack())
	}
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

// register records a session's cwd for its prompts and returns the channel the caller closes once
// the handler call opening the session returned. A session loaded again gets a fresh channel.
func (p *Provider) register(sessionID, cwd string) chan struct{} {
	ready := make(chan struct{})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[sessionID] = session{cwd: cwd, ready: ready}
	return ready
}

// lookupSession returns what the provider recorded for a session.
func (p *Provider) lookupSession(sessionID string) (session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	return s, ok
}

// openSession runs hook, the handler's NewSession or LoadSession, on its own goroutine, marks the
// session ready once hook returned, and then answers the request with result. A panicking hook is
// answered with an internal error instead.
func (p *Provider) openSession(name string, id json.RawMessage, s Session, hook func(context.Context, Session),
	result any) {
	ready := p.register(s.ID, s.Cwd)
	p.spawn(name, func() {
		func() {
			defer close(ready)
			hook(p.ctx, s)
		}()
		_ = p.reply(id, result)
	}, func() { _ = p.replyError(id, CodeInternalError, errTextInternal) })
}

// handleNewSession answers a fresh session id once the handler's NewSession returns. It reads no
// config, repository, or file, so it succeeds for any cwd, the background probe's included.
func (p *Provider) handleNewSession(id, params json.RawMessage) {
	s := p.decodeSession(params)
	s.ID = newSessionID()
	p.openSession(methodSessionNew, id, s, p.handler.NewSession, struct {
		SessionID string `json:"sessionId"`
	}{s.ID})
}

// handleLoadSession registers any session id and answers {} once the handler's LoadSession
// returns. T3 Code falls back to a new session when a load fails, so a load never fails.
func (p *Provider) handleLoadSession(id, params json.RawMessage) {
	s := p.decodeSession(params)
	if s.ID == "" {
		_ = p.reply(id, struct{}{})
		return
	}
	p.openSession(methodSessionLoad, id, s, p.handler.LoadSession, struct{}{})
}

// handlePrompt runs a prompt of a known session on its own goroutine, once the session is ready, and
// answers its stop reason. The prompt is recorded as open before the read loop moves on, so a
// session/cancel read next closes its Canceled channel even when Prompt has not been called yet.
func (p *Provider) handlePrompt(id, params json.RawMessage) {
	var req struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(params, &req); err != nil || req.SessionID == "" {
		_ = p.replyError(id, CodeInvalidParams, errTextInvalidParams)
		return
	}
	s, ok := p.lookupSession(req.SessionID)
	if !ok {
		_ = p.replyError(id, CodeInvalidParams, errTextUnknownSession)
		return
	}
	canceled := p.openPrompt(req.SessionID)
	pr := PromptRequest{SessionID: req.SessionID, Cwd: s.cwd, Blocks: req.Prompt, Canceled: canceled}
	updates := &Updates{p: p, sessionID: req.SessionID, messageID: newMessageID()}
	answer := func(reason StopReason) {
		_ = p.reply(id, struct {
			StopReason StopReason `json:"stopReason"`
		}{reason})
	}
	started := p.spawn(methodSessionPrompt, func() {
		defer p.endPrompt(req.SessionID, canceled)
		select {
		case <-s.ready:
		case <-p.ctx.Done():
		}
		if p.ctx.Err() != nil { // stopped while waiting, even if the session became ready meanwhile
			return
		}
		reason := p.handler.Prompt(p.ctx, pr, updates)
		if reason == "" {
			reason = StopEndTurn
		}
		answer(reason)
	}, func() { answer(StopEndTurn) })
	if !started {
		p.endPrompt(req.SessionID, canceled)
	}
}

// openPrompt records a prompt of a session as open and returns its Canceled channel.
func (p *Provider) openPrompt(sessionID string) chan struct{} {
	ch := make(chan struct{})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open[sessionID] = append(p.open[sessionID], ch)
	return ch
}

// endPrompt forgets a prompt whose handler returned. A prompt a cancel already closed was forgotten
// then, so a channel is never closed twice.
func (p *Provider) endPrompt(sessionID string, ch chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rest := slices.DeleteFunc(p.open[sessionID], func(c chan struct{}) bool { return c == ch })
	if len(rest) == 0 {
		delete(p.open, sessionID)
		return
	}
	p.open[sessionID] = rest
}

// cancelPrompts closes the Canceled channels of a session's open prompts and forgets them.
func (p *Provider) cancelPrompts(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ch := range p.open[sessionID] {
		close(ch)
	}
	delete(p.open, sessionID)
}

// handleCancel closes the Canceled channels of the session's open prompts, then reports the
// session/cancel to the handler, unless the provider stopped. Both happen on the read loop, before
// any later message is read. A panic in the handler's Cancel is recovered, so the read loop keeps
// serving.
func (p *Provider) handleCancel(params json.RawMessage) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &req) != nil || req.SessionID == "" || p.isStopped() {
		return
	}
	p.cancelPrompts(req.SessionID)
	defer func() {
		if r := recover(); r != nil {
			p.logPanic(methodSessionCancel, r)
		}
	}()
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
