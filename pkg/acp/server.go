package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
)

// protocolVersion is the ACP protocol version this agent speaks.
const protocolVersion = 1

// ACP stop reasons answered to session/prompt.
const (
	stopEndTurn  = "end_turn"
	stopCanceled = "cancelled" //nolint:misspell // ACP wire value
)

// PromptRequest is one session/prompt handed to the run function.
type PromptRequest struct {
	SessionID string
	Cwd       string // absolute working directory recorded by session/new
	Text      string // first text block of the prompt; later blocks are client runtime instructions
}

// Result is the outcome of a successful or failed run.
type Result struct {
	Message string // final assistant message, such as the completion report; sent before the reply
}

// RunFunc executes one prompt. It must return promptly once ctx is canceled. A non-nil error
// fails the turn with a JSON-RPC error carrying its message, unless the client canceled the prompt.
type RunFunc func(ctx context.Context, req PromptRequest, sink *Sink) (Result, error)

// Options configures a Server.
type Options struct {
	Version string    // reported as agentInfo.version
	Run     RunFunc   // executes prompts
	Debug   io.Writer // optional protocol trace; never carries MCP server header values
}

// Server is the agent side of an ACP connection. It runs at most one prompt at a time across
// all of its sessions, because a run changes the process working directory.
type Server struct {
	conn    *Conn
	run     RunFunc
	version string
	debug   io.Writer
	dmu     sync.Mutex // serializes debug writes

	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]string // session id -> cwd
	active   *activePrompt
}

// activePrompt is the prompt currently running.
type activePrompt struct {
	sessionID       string
	cancel          context.CancelFunc
	cancelRequested bool // guarded by Server.mu
}

// NewServer returns a server reading client messages from r and writing to w.
func NewServer(r io.Reader, w io.Writer, opts Options) *Server {
	ctx, stop := context.WithCancel(context.Background())
	s := &Server{
		conn:     NewConn(r, w),
		run:      opts.Run,
		version:  opts.Version,
		debug:    opts.Debug,
		ctx:      ctx,
		stop:     stop,
		sessions: map[string]string{},
	}
	s.conn.HandleRequest("initialize", s.handleInitialize)
	s.conn.HandleRequest("authenticate", s.replyEmpty("authenticate"))
	s.conn.HandleRequest("session/new", s.handleNewSession)
	s.conn.HandleRequest("session/load", s.handleLoadSession)
	s.conn.HandleRequest("session/set_config_option", s.replyEmpty("session/set_config_option"))
	s.conn.HandleRequest("session/set_mode", s.replyEmpty("session/set_mode"))
	s.conn.HandleRequest("session/set_model", s.replyEmpty("session/set_model"))
	s.conn.HandleRequest("session/prompt", s.handlePrompt)
	s.conn.HandleNotification("session/cancel", s.handleCancel)
	// session/cancel is a notification in ACP; a client sending it as a request still gets an answer
	s.conn.HandleRequest("session/cancel", func(id, params json.RawMessage) {
		s.handleCancel(params)
		_ = s.conn.Reply(id, struct{}{})
	})
	return s
}

// Serve handles client messages until the input ends, then cancels any running prompt and waits
// for it to finish, so its reply is written and its cleanup has run before Serve returns.
func (s *Server) Serve() error {
	err := s.conn.Serve()
	s.stop()
	s.wg.Wait()
	return err
}

type authMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []authMethod      `json:"authMethods"`
	AgentInfo         agentInfo         `json:"agentInfo"`
}

type agentCapabilities struct {
	LoadSession bool `json:"loadSession"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleInitialize(id, _ json.RawMessage) {
	s.logf("initialize")
	// both auth methods the Grok driver may request are accepted; loopai's providers use their own credentials
	_ = s.conn.Reply(id, initializeResult{
		ProtocolVersion:   protocolVersion,
		AgentCapabilities: agentCapabilities{LoadSession: false},
		AuthMethods:       []authMethod{{ID: "cached_token", Name: "cached token"}, {ID: "xai.api_key", Name: "API key"}},
		AgentInfo:         agentInfo{Name: "loopai", Version: s.version},
	})
}

// replyEmpty returns a handler answering the method with an empty object.
func (s *Server) replyEmpty(method string) RequestHandler {
	return func(id, _ json.RawMessage) {
		s.logf("%s", method)
		_ = s.conn.Reply(id, struct{}{})
	}
}

// mcpServerNames decodes only the names of MCP servers and their headers, so header values,
// which carry the client's bearer credential, are never held by the agent.
type mcpServerNames struct {
	Name    string `json:"name"`
	Headers []struct {
		Name string `json:"name"`
	} `json:"headers"`
}

type newSessionParams struct {
	Cwd        string          `json:"cwd"`
	MCPServers json.RawMessage `json:"mcpServers"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

func (s *Server) handleNewSession(id, params json.RawMessage) {
	var p newSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		_ = s.conn.ReplyError(id, CodeInvalidParams, "session/new: invalid params")
		return
	}
	if p.Cwd == "" || !filepath.IsAbs(p.Cwd) {
		_ = s.conn.ReplyError(id, CodeInvalidParams, "session/new: cwd must be an absolute path")
		return
	}
	sid, err := newSessionID()
	if err != nil {
		_ = s.conn.ReplyError(id, CodeInternalError, "session/new: "+err.Error())
		return
	}
	s.mu.Lock()
	s.sessions[sid] = p.Cwd
	s.mu.Unlock()
	s.logf("session/new %s cwd=%s mcpServers=%s", sid, p.Cwd, describeMCPServers(p.MCPServers))
	_ = s.conn.Reply(id, newSessionResult{SessionID: sid})
}

// describeMCPServers summarizes the MCP server list for the debug trace with header values redacted.
// loopai runs no MCP client, so the list is otherwise discarded.
func describeMCPServers(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "[]"
	}
	var servers []mcpServerNames
	if err := json.Unmarshal(raw, &servers); err != nil {
		return "(unparsed)"
	}
	parts := make([]string, 0, len(servers))
	for _, srv := range servers {
		headers := make([]string, 0, len(srv.Headers))
		for _, h := range srv.Headers {
			headers = append(headers, h.Name+"=[redacted]")
		}
		parts = append(parts, fmt.Sprintf("%s{%s}", srv.Name, strings.Join(headers, ",")))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return "loopai-" + hex.EncodeToString(b[:]), nil
}

func (s *Server) handleLoadSession(id, _ json.RawMessage) {
	s.logf("session/load rejected")
	_ = s.conn.ReplyError(id, CodeMethodNotFound, "session/load is not supported")
}

type promptParams struct {
	SessionID string        `json:"sessionId"`
	Prompt    []textContent `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

func (s *Server) handlePrompt(id, params json.RawMessage) {
	var p promptParams
	if err := json.Unmarshal(params, &p); err != nil {
		_ = s.conn.ReplyError(id, CodeInvalidParams, "session/prompt: invalid params")
		return
	}
	// only the first block is the user's message; the client appends runtime instructions after it
	if len(p.Prompt) == 0 || p.Prompt[0].Type != "text" {
		_ = s.conn.ReplyError(id, CodeInvalidParams, "session/prompt: the prompt must start with a text block")
		return
	}

	s.mu.Lock()
	cwd, ok := s.sessions[p.SessionID]
	switch {
	case !ok:
		s.mu.Unlock()
		_ = s.conn.ReplyError(id, CodeInvalidParams, "session/prompt: unknown session "+p.SessionID)
		return
	case s.active != nil:
		s.mu.Unlock()
		_ = s.conn.ReplyError(id, CodeInvalidRequest, "session/prompt: a loopai run is already in progress")
		return
	case s.ctx.Err() != nil:
		s.mu.Unlock()
		_ = s.conn.ReplyError(id, CodeInternalError, "session/prompt: agent is shutting down")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	active := &activePrompt{sessionID: p.SessionID, cancel: cancel}
	s.active = active
	s.wg.Add(1)
	s.mu.Unlock()

	s.logf("session/prompt %s started", p.SessionID)
	req := PromptRequest{SessionID: p.SessionID, Cwd: cwd, Text: p.Prompt[0].Text}
	go s.runPrompt(ctx, id, req, active)
}

// runPrompt executes one prompt and answers its request exactly once.
func (s *Server) runPrompt(ctx context.Context, id json.RawMessage, req PromptRequest, active *activePrompt) {
	defer s.wg.Done()
	sink := NewSink(s.conn, req.SessionID)
	res, err := s.invokeRun(ctx, req, sink)

	s.mu.Lock()
	canceled := active.cancelRequested
	s.active = nil
	s.mu.Unlock()
	active.cancel()

	switch {
	case canceled:
		s.logf("session/prompt %s canceled", req.SessionID)
		_ = s.conn.Reply(id, promptResult{StopReason: stopCanceled})
	case err != nil:
		s.logf("session/prompt %s failed", req.SessionID)
		sink.Message(res.Message)
		_ = s.conn.ReplyError(id, CodeInternalError, err.Error())
	default:
		s.logf("session/prompt %s completed", req.SessionID)
		sink.Message(res.Message)
		_ = s.conn.Reply(id, promptResult{StopReason: stopEndTurn})
	}
}

// invokeRun calls the run function, turning a missing function or a panic into an error so the
// prompt is still answered.
func (s *Server) invokeRun(ctx context.Context, req PromptRequest, sink *Sink) (res Result, err error) {
	if s.run == nil {
		return Result{}, errors.New("no run function configured")
	}
	defer func() {
		if r := recover(); r != nil {
			res, err = Result{}, fmt.Errorf("loopai run panicked: %v", r)
		}
	}()
	return s.run(ctx, req, sink)
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

func (s *Server) handleCancel(params json.RawMessage) {
	var p cancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	s.mu.Lock()
	active := s.active
	if active == nil || active.sessionID != p.SessionID {
		s.mu.Unlock()
		return
	}
	active.cancelRequested = true
	s.mu.Unlock()
	s.logf("session/cancel %s", p.SessionID)
	active.cancel()
}

// logf writes one debug trace line. Callers never pass MCP server header values.
func (s *Server) logf(format string, args ...any) {
	if s.debug == nil {
		return
	}
	s.dmu.Lock()
	defer s.dmu.Unlock()
	_, _ = fmt.Fprintf(s.debug, "acp: "+format+"\n", args...)
}
