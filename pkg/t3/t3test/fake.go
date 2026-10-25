// Package t3test is a fake T3 Code server speaking orchestration protocol 2, for tests of pkg/t3
// and of the code built on it. It serves /ws and GET /api/projects from an httptest server, enforces
// the parts of the contract a client can get wrong (the protocol query parameter before
// authentication, the Bearer token, Request headers, and ack-gated streams), keeps a small mutable
// model of threads, projects, and terminals, and records what clients sent.
//
// It never talks to a real T3 Code server, and it deliberately models only what loopai uses:
// terminal.open, terminal.write, subscribeTerminalMetadata, orchestration.getThreadProjection, and
// orchestration.dispatchCommand with thread.metadata.update, thread.pin, thread.unpin, and
// thread.mark-unread.
package t3test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultToken is the Bearer token a Server accepts unless WithToken sets another.
const DefaultToken = "t3test-token"

// IncompatibleProtocolBody is the exact 426 body T3 Code answers a /ws request without
// orchestrationProtocol=2, before authentication.
const IncompatibleProtocolBody = `{"code":"orchestration_protocol_incompatible",` +
	`"message":"Update this client to one that supports orchestration protocol 2.",` +
	`"orchestrationProtocolVersion":2}`

// forbiddenFields are payload keys loopai must never send; the fake fails a request carrying one so
// the mistake is loud.
var forbiddenFields = []string{"providerInstanceId", "createdBy", "creationSource", "createdAt"}

// Thread is the thread state the fake serves through orchestration.getThreadProjection and changes
// on dispatched commands. Empty WorktreePath and ArchivedAt are null, an empty PinnedAt is absent,
// and TitleRegenerating sets titleRegeneration. LatestRunCompletedAt is the completion time of the
// thread's latest run, empty while that run is still going; thread.mark-unread sets LastVisitedAt
// one millisecond before it, as T3 Code does.
type Thread struct {
	ID                   string
	ProjectID            string
	Title                string
	WorktreePath         string
	PinnedAt             string
	ArchivedAt           string
	TitleRegenerating    bool
	LatestRunCompletedAt string
	LastVisitedAt        string
}

// Project is a project GET /api/projects lists.
type Project struct {
	ID            string
	Title         string
	WorkspaceRoot string
}

// Terminal is a terminal session. An empty Status is running and an empty WorktreePath is null;
// Env is the launch context terminal.open compares to decide between a resize and a reset.
type Terminal struct {
	ThreadID             string
	TerminalID           string
	Cwd                  string
	WorktreePath         string
	Env                  map[string]string
	Status               string
	HasRunningSubprocess bool
	Label                string
}

// Handshake is one /ws connection attempt. Status is 101 for an accepted upgrade.
type Handshake struct {
	Query         url.Values
	Authorization string
	Status        int
}

// Request is one Request message as received, its fields kept raw.
type Request struct {
	ID      json.RawMessage
	Method  string
	Payload json.RawMessage
	Headers json.RawMessage
}

// Option configures a Server.
type Option func(*Server)

// WithProtocolVersion makes the server require another orchestration protocol version, as a newer
// T3 Code would; a protocol-2 client then gets a 426.
func WithProtocolVersion(version string) Option { return func(s *Server) { s.protocol = version } }

// WithToken sets the Bearer token the server accepts.
func WithToken(token string) Option { return func(s *Server) { s.token = token } }

// WithThread adds a thread.
func WithThread(th Thread) Option { return func(s *Server) { s.threads[th.ID] = th } }

// WithProject adds a project.
func WithProject(p Project) Option { return func(s *Server) { s.projects = append(s.projects, p) } }

// WithTerminal adds a running terminal session.
func WithTerminal(term Terminal) Option {
	return func(s *Server) { s.terminals = append(s.terminals, normalizeTerminal(term)) }
}

// Server is a fake protocol-2 T3 Code server. Its methods are safe for concurrent use.
type Server struct {
	http     *httptest.Server
	token    string
	protocol string
	wg       sync.WaitGroup

	mu          sync.Mutex
	threads     map[string]Thread
	projects    []Project
	terminals   []Terminal
	conns       map[*fakeConn]struct{}
	closed      bool
	pid         int
	startedAt   time.Time
	runtimeHome string
	reject401   bool
	ignorePings bool
	// openStarting makes the next terminal.open that starts a session leave it starting
	openStarting bool
	// breakWrite makes the next terminal.write into a running session fail with TerminalWriteError
	breakWrite bool
	stalled    map[string]bool
	overrides  map[string][]func(id json.RawMessage) map[string]any
	receipts   map[string]int
	sequence   int

	handshakes []Handshake
	requests   []Request
	dispatches []map[string]any
	opens      []map[string]any
	writes     []map[string]any
	acks       int
	pings      int
	interrupts int
}

// NewServer starts a fake server, closed when the test ends.
func NewServer(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{
		token:     DefaultToken,
		protocol:  "2",
		threads:   map[string]Thread{},
		conns:     map[*fakeConn]struct{}{},
		pid:       40000,
		startedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
		stalled:   map[string]bool{},
		overrides: map[string][]func(json.RawMessage) map[string]any{},
		receipts:  map[string]int{},
	}
	for _, opt := range opts {
		opt(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.serveWS)
	mux.HandleFunc("/api/projects", s.serveProjects)
	s.http = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// URL returns the server's HTTP origin, as server-runtime.json records it.
func (s *Server) URL() string { return s.http.URL }

// Token returns the accepted Bearer token.
func (s *Server) Token() string { return s.token }

// Close drops every connection and stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.dropConnections()
	s.http.Close()
	s.wg.Wait()
}

// WriteRuntime writes <home>/userdata/server-runtime.json pointing at this server; Restart rewrites
// it with the new pid and start time.
func (s *Server) WriteRuntime(home string) error {
	s.mu.Lock()
	s.runtimeHome = home
	s.mu.Unlock()
	return s.writeRuntime()
}

func (s *Server) writeRuntime() error {
	s.mu.Lock()
	home, pid, startedAt := s.runtimeHome, s.pid, s.startedAt
	s.mu.Unlock()
	if home == "" {
		return nil
	}
	u, err := url.Parse(s.http.URL)
	if err != nil {
		return fmt.Errorf("parse server url: %w", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return fmt.Errorf("parse server port: %w", err)
	}
	data, err := json.Marshal(map[string]any{
		"version": 1, "pid": pid, "host": u.Hostname(), "port": port, "origin": s.http.URL,
		"startedAt": startedAt.Format(time.RFC3339Nano), "serverVersion": "0.1.1",
	})
	if err != nil {
		return fmt.Errorf("encode runtime: %w", err)
	}
	dir := filepath.Join(home, "userdata")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server-runtime.json"), data, 0o600); err != nil {
		return fmt.Errorf("write runtime: %w", err)
	}
	return nil
}

// Restart simulates a server restart: every connection drops, terminal sessions (which live only in
// server memory) are cleared, and the pid and start time change, rewritten into the runtime file
// when WriteRuntime was called. Threads, projects, and command receipts survive.
func (s *Server) Restart() error {
	s.mu.Lock()
	s.terminals = nil
	s.pid++
	s.startedAt = s.startedAt.Add(time.Minute)
	s.mu.Unlock()
	s.dropConnections()
	return s.writeRuntime()
}

// RejectNextConnection makes the next /ws upgrade fail with 401 auth_invalid, as an expired or
// revoked token does.
func (s *Server) RejectNextConnection() {
	s.mu.Lock()
	s.reject401 = true
	s.mu.Unlock()
}

// SetThread adds or replaces a thread, as a user rename does.
func (s *Server) SetThread(th Thread) {
	s.mu.Lock()
	s.threads[th.ID] = th
	s.mu.Unlock()
}

// Thread returns a thread's current state.
func (s *Server) Thread(id string) (Thread, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.threads[id]
	return th, ok
}

// Terminals returns the current terminal sessions.
func (s *Server) Terminals() []Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.terminals)
}

// Upsert adds or replaces a terminal session and streams an upsert to every subscriber.
func (s *Server) Upsert(term Terminal) {
	term = normalizeTerminal(term)
	s.mu.Lock()
	s.putTerminal(term)
	s.mu.Unlock()
	s.broadcast(map[string]any{"type": "upsert", "terminal": terminalSummary(term)})
}

// OpenNextStarting makes the next terminal.open that starts a session leave it starting, as a shell
// still launching is, so writes fail with TerminalNotRunningError until SetTerminalState marks it
// running. An open that only resizes a live session does not use it up.
func (s *Server) OpenNextStarting() {
	s.mu.Lock()
	s.openStarting = true
	s.mu.Unlock()
}

// BreakNextWrite makes the next terminal.write into a running session fail with TerminalWriteError,
// as T3 Code answers when the pty write throws because the shell died between its status check and
// the write. The session is left as it is. A write refused for another reason does not use it up.
func (s *Server) BreakNextWrite() {
	s.mu.Lock()
	s.breakWrite = true
	s.mu.Unlock()
}

// SetTerminalState changes an existing session's status and hasRunningSubprocess in place, keeping
// its launch context, and streams an upsert to every subscriber. It reports whether the session
// exists.
func (s *Server) SetTerminalState(threadID, terminalID, status string, hasRunningSubprocess bool) bool {
	s.mu.Lock()
	term, found := s.terminal(threadID, terminalID)
	if found {
		term.Status, term.HasRunningSubprocess = status, hasRunningSubprocess
		s.putTerminal(term)
	}
	s.mu.Unlock()
	if found {
		s.broadcast(map[string]any{"type": "upsert", "terminal": terminalSummary(term)})
	}
	return found
}

// Remove deletes a terminal session and streams a remove to every subscriber, as closing its tab
// does.
func (s *Server) Remove(threadID, terminalID string) {
	s.mu.Lock()
	s.terminals = slices.DeleteFunc(s.terminals, func(t Terminal) bool {
		return t.ThreadID == threadID && t.TerminalID == terminalID
	})
	s.mu.Unlock()
	s.broadcast(map[string]any{"type": "remove", "threadId": threadID, "terminalId": terminalID})
}

// Stall makes the server leave every later call of method unanswered.
func (s *Server) Stall(method string) {
	s.mu.Lock()
	s.stalled[method] = true
	s.mu.Unlock()
}

// DieNext makes the next call of method end with a Die cause carrying defect, an outcome the
// client cannot interpret; the call has no effect.
func (s *Server) DieNext(method, defect string) {
	s.pushOverride(method, func(id json.RawMessage) map[string]any { return exitDie(id, defect) })
}

// FailNext makes the next call of method fail with a typed error tagged tag, as T3 Code answers a
// request it refuses; the call has no effect.
func (s *Server) FailNext(method, tag string) {
	s.pushOverride(method, func(id json.RawMessage) map[string]any {
		return exitFail(id, map[string]any{"_tag": tag, "message": "t3test: " + tag})
	})
}

// ReplyNext makes the next call of method succeed with value instead of its normal reply.
func (s *Server) ReplyNext(method string, value any) {
	s.pushOverride(method, func(id json.RawMessage) map[string]any { return exitSuccess(id, value) })
}

func (s *Server) pushOverride(method string, reply func(json.RawMessage) map[string]any) {
	s.mu.Lock()
	s.overrides[method] = append(s.overrides[method], reply)
	s.mu.Unlock()
}

// SendDefect sends a Defect message, which names no request, on every connection.
func (s *Server) SendDefect(defect string) {
	for _, fc := range s.connections() {
		fc.send(map[string]any{"_tag": "Defect", "defect": defect})
	}
}

// IgnorePings stops answering Ping with Pong.
func (s *Server) IgnorePings() {
	s.mu.Lock()
	s.ignorePings = true
	s.mu.Unlock()
}

// Handshakes returns every /ws connection attempt.
func (s *Server) Handshakes() []Handshake {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.handshakes)
}

// Requests returns every Request message received.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Dispatches returns the payload of every orchestration.dispatchCommand call, answered or not.
func (s *Server) Dispatches() []map[string]any { return s.recorded(&s.dispatches) }

// Opens returns the payload of every terminal.open call, answered or not.
func (s *Server) Opens() []map[string]any { return s.recorded(&s.opens) }

// Writes returns the payload of every terminal.write call, answered or not.
func (s *Server) Writes() []map[string]any { return s.recorded(&s.writes) }

func (s *Server) recorded(list *[]map[string]any) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(*list)
}

// Acks returns how many stream Acks clients sent.
func (s *Server) Acks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks
}

// Interrupts returns how many stream Interrupts clients sent.
func (s *Server) Interrupts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupts
}

// Pings returns how many Pings clients sent.
func (s *Server) Pings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// serveWS checks the protocol query parameter before authentication, as T3 Code does, then the
// Bearer token, then upgrades.
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	attempt := Handshake{Query: r.URL.Query(), Authorization: r.Header.Get("Authorization")}
	if attempt.Query.Get("orchestrationProtocol") != s.protocol {
		s.recordHandshake(attempt, http.StatusUpgradeRequired)
		writeJSON(w, http.StatusUpgradeRequired, incompatibleProtocolBody(s.protocol))
		return
	}
	s.mu.Lock()
	forced := s.reject401
	s.reject401 = false
	s.mu.Unlock()
	if reason := s.authFailure(attempt.Authorization, forced); reason != "" {
		s.recordHandshake(attempt, http.StatusUnauthorized)
		writeAuthInvalid(w, reason)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.recordHandshake(attempt, http.StatusBadRequest)
		return
	}
	s.recordHandshake(attempt, http.StatusSwitchingProtocols)
	fc := &fakeConn{server: s, ws: ws, subs: map[string]*fakeSub{}, done: make(chan struct{})}
	// registering under the lock Close takes first keeps an upgrade racing Close from outliving the
	// server, and the Add from racing Close's Wait
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.conns[fc] = struct{}{}
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if closed {
		_ = ws.Close()
		return
	}
	defer s.wg.Done()
	fc.readLoop()
}

func (s *Server) recordHandshake(h Handshake, status int) {
	h.Status = status
	s.mu.Lock()
	s.handshakes = append(s.handshakes, h)
	s.mu.Unlock()
}

// authFailure returns the auth_invalid reason for a request, or "" when it is authenticated.
func (s *Server) authFailure(authorization string, forced bool) string {
	switch {
	case authorization == "":
		return "missing_credential"
	case forced || authorization != "Bearer "+s.token:
		return "invalid_credential"
	}
	return ""
}

// serveProjects answers GET /api/projects for a valid Bearer token.
func (s *Server) serveProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if reason := s.authFailure(r.Header.Get("Authorization"), false); reason != "" {
		writeAuthInvalid(w, reason)
		return
	}
	s.mu.Lock()
	projects := make([]map[string]any, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, map[string]any{
			"id": p.ID, "title": p.Title, "workspaceRoot": p.WorkspaceRoot, "defaultModelSelection": nil,
			"scripts": []any{}, "createdAt": timestamp, "updatedAt": timestamp, "deletedAt": nil,
		})
	}
	s.mu.Unlock()
	data, err := json.Marshal(map[string]any{"projects": projects, "updatedAt": timestamp})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// incompatibleProtocolBody is the 426 body for a server requiring version.
func incompatibleProtocolBody(version string) []byte {
	if version == "2" {
		return []byte(IncompatibleProtocolBody)
	}
	var number any = version
	if n, err := strconv.Atoi(version); err == nil {
		number = n
	}
	body, _ := json.Marshal(map[string]any{
		"code":                         "orchestration_protocol_incompatible",
		"message":                      "Update this client to one that supports orchestration protocol " + version + ".",
		"orchestrationProtocolVersion": number,
	})
	return body
}

// timestamp is the fixed time the fake reports for creation and update fields.
const timestamp = "2026-10-22T12:00:00.000Z"

// timestampLayout is the millisecond ISO 8601 layout T3 Code writes timestamps in.
const timestampLayout = "2006-01-02T15:04:05.000Z"

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeAuthInvalid(w http.ResponseWriter, reason string) {
	body, _ := json.Marshal(map[string]any{
		"_tag": "EnvironmentAuthInvalidError", "code": "auth_invalid", "reason": reason, "traceId": "t3test",
	})
	writeJSON(w, http.StatusUnauthorized, body)
}

func (s *Server) connections() []*fakeConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	conns := make([]*fakeConn, 0, len(s.conns))
	for fc := range s.conns {
		conns = append(conns, fc)
	}
	return conns
}

// dropConnections closes every connection, as a server shutdown does.
func (s *Server) dropConnections() {
	s.mu.Lock()
	conns := s.conns
	s.conns = map[*fakeConn]struct{}{}
	s.mu.Unlock()
	for fc := range conns {
		fc.close()
	}
}

// broadcast streams one metadata event to every subscriber of every connection.
func (s *Server) broadcast(event map[string]any) {
	for _, fc := range s.connections() {
		fc.mu.Lock()
		subs := make([]*fakeSub, 0, len(fc.subs))
		for _, sub := range fc.subs {
			subs = append(subs, sub)
		}
		fc.mu.Unlock()
		for _, sub := range subs {
			sub.enqueue(event)
		}
	}
}

// putTerminal adds or replaces a session; the caller holds s.mu.
func (s *Server) putTerminal(term Terminal) {
	for i, existing := range s.terminals {
		if existing.ThreadID == term.ThreadID && existing.TerminalID == term.TerminalID {
			s.terminals[i] = term
			return
		}
	}
	s.terminals = append(s.terminals, term)
}

// terminal finds a session; the caller holds s.mu.
func (s *Server) terminal(threadID, terminalID string) (Terminal, bool) {
	for _, term := range s.terminals {
		if term.ThreadID == threadID && term.TerminalID == terminalID {
			return term, true
		}
	}
	return Terminal{}, false
}

func normalizeTerminal(term Terminal) Terminal {
	if term.Status == "" {
		term.Status = "running"
	}
	if term.Label == "" {
		term.Label = "Terminal"
	}
	return term
}

// terminalSummary is the wire TerminalSummary of a session.
func terminalSummary(term Terminal) map[string]any {
	summary := terminalSession(term)
	delete(summary, "history")
	summary["hasRunningSubprocess"] = term.HasRunningSubprocess
	return summary
}

// terminalSession is the wire TerminalSessionSnapshot terminal.open returns.
func terminalSession(term Terminal) map[string]any {
	var pid any
	if term.Status == "running" {
		pid = 50000
	}
	return map[string]any{
		"threadId": term.ThreadID, "terminalId": term.TerminalID, "cwd": term.Cwd,
		"worktreePath": nullable(term.WorktreePath), "status": term.Status, "pid": pid, "history": "",
		"exitCode": nil, "exitSignal": nil, "label": term.Label, "updatedAt": timestamp,
	}
}

// nullable maps "" to JSON null.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fakeConn is one accepted WebSocket connection.
type fakeConn struct {
	server *Server
	ws     *websocket.Conn
	wmu    sync.Mutex
	once   sync.Once
	done   chan struct{}

	mu   sync.Mutex
	subs map[string]*fakeSub
}

// clientMessage is any client message.
type clientMessage struct {
	Tag       string          `json:"_tag"`
	ID        json.RawMessage `json:"id"`
	Method    string          `json:"tag"`
	Payload   json.RawMessage `json:"payload"`
	Headers   json.RawMessage `json:"headers"`
	RequestID json.RawMessage `json:"requestId"`
}

func (fc *fakeConn) readLoop() {
	defer fc.close()
	for {
		_, data, err := fc.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			fc.send(map[string]any{"_tag": "Defect", "defect": "undecodable message: " + err.Error()})
			continue
		}
		fc.handle(msg)
	}
}

func (fc *fakeConn) handle(msg clientMessage) {
	s := fc.server
	switch msg.Tag {
	case "Request":
		fc.request(msg)
	case "Ack":
		s.mu.Lock()
		s.acks++
		s.mu.Unlock()
		if sub := fc.sub(msg.RequestID); sub != nil {
			sub.ack()
		}
	case "Interrupt":
		s.mu.Lock()
		s.interrupts++
		s.mu.Unlock()
		if sub := fc.sub(msg.RequestID); sub != nil {
			fc.mu.Lock()
			delete(fc.subs, requestKey(msg.RequestID))
			fc.mu.Unlock()
			sub.stop()
			fc.send(exitInterrupt(msg.RequestID))
		}
	case "Ping":
		s.mu.Lock()
		s.pings++
		ignore := s.ignorePings
		s.mu.Unlock()
		if !ignore {
			fc.send(map[string]any{"_tag": "Pong"})
		}
	case "Eof":
	default:
		fc.send(map[string]any{"_tag": "Defect", "defect": "Unknown request tag: " + msg.Tag})
	}
}

// request validates and answers one Request. A request id that is neither a number nor a string is
// a Defect, and a Request without a headers array is rejected with a Die exit.
func (fc *fakeConn) request(msg clientMessage) {
	s := fc.server
	s.mu.Lock()
	s.requests = append(s.requests, Request{ID: msg.ID, Method: msg.Method, Payload: msg.Payload, Headers: msg.Headers})
	s.mu.Unlock()
	if len(msg.ID) == 0 || (msg.ID[0] != '"' && (msg.ID[0] < '0' || msg.ID[0] > '9')) {
		fc.send(map[string]any{"_tag": "Defect", "defect": "Invalid request id: " + string(msg.ID)})
		return
	}
	if len(bytes.TrimSpace(msg.Headers)) == 0 || bytes.TrimSpace(msg.Headers)[0] != '[' {
		fc.send(exitDie(msg.ID, "t3test: Request without headers"))
		return
	}
	s.record(msg.Method, msg.Payload)
	s.mu.Lock()
	stalled := s.stalled[msg.Method]
	var override func(json.RawMessage) map[string]any
	if queue := s.overrides[msg.Method]; len(queue) > 0 && !stalled {
		override, s.overrides[msg.Method] = queue[0], queue[1:]
	}
	s.mu.Unlock()
	switch {
	case stalled:
		return
	case override != nil:
		fc.send(override(msg.ID))
		return
	}
	if field := forbiddenField(msg.Payload); field != "" {
		fc.send(exitFail(msg.ID, map[string]any{
			"_tag": "T3TestForbiddenFieldError", "message": "t3test: " + field + " must never be sent",
		}))
		return
	}
	switch msg.Method {
	case "terminal.open":
		fc.send(s.terminalOpen(msg.ID, msg.Payload))
	case "terminal.write":
		fc.send(s.terminalWrite(msg.ID, msg.Payload))
	case "subscribeTerminalMetadata":
		fc.subscribe(msg.ID)
	case "orchestration.getThreadProjection":
		fc.send(s.threadProjection(msg.ID, msg.Payload))
	case "orchestration.dispatchCommand":
		fc.send(s.dispatch(msg.ID, msg.Payload))
	default:
		fc.send(exitDie(msg.ID, "Unknown request tag: "+msg.Method))
	}
}

// forbiddenField returns the first key of payload loopai must never send.
func forbiddenField(payload json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return ""
	}
	for _, name := range forbiddenFields {
		if _, ok := fields[name]; ok {
			return name
		}
	}
	return ""
}

// record appends the payload of a recorded method to its list, before the call is answered, so
// stalled, overridden, and rejected calls are recorded too.
func (s *Server) record(method string, payload json.RawMessage) {
	var list *[]map[string]any
	switch method {
	case "terminal.open":
		list = &s.opens
	case "terminal.write":
		list = &s.writes
	case "orchestration.dispatchCommand":
		list = &s.dispatches
	default:
		return
	}
	var fields map[string]any
	_ = json.Unmarshal(payload, &fields)
	s.mu.Lock()
	*list = append(*list, fields)
	s.mu.Unlock()
}

// terminalOpen creates a session for an unknown id, only resizes one opened with the same launch
// context, and resets one opened with another.
func (s *Server) terminalOpen(id, payload json.RawMessage) map[string]any {
	var in struct {
		ThreadID     string            `json:"threadId"`
		TerminalID   string            `json:"terminalId"`
		Cwd          string            `json:"cwd"`
		WorktreePath *string           `json:"worktreePath"`
		Env          map[string]string `json:"env"`
	}
	err := json.Unmarshal(payload, &in)
	if err != nil || in.ThreadID == "" || in.TerminalID == "" || len(in.TerminalID) > 128 || in.Cwd == "" {
		return exitDie(id, "t3test: invalid terminal.open payload")
	}
	term := Terminal{ThreadID: in.ThreadID, TerminalID: in.TerminalID, Cwd: in.Cwd, Env: in.Env}
	if in.WorktreePath != nil {
		term.WorktreePath = *in.WorktreePath
	}
	s.mu.Lock()
	existing, found := s.terminal(in.ThreadID, in.TerminalID)
	sameContext := found && existing.Cwd == term.Cwd && existing.WorktreePath == term.WorktreePath &&
		maps.Equal(existing.Env, term.Env)
	if sameContext {
		term = existing
	} else {
		if s.openStarting {
			term.Status, s.openStarting = "starting", false
		}
		term = normalizeTerminal(term)
		s.putTerminal(term)
	}
	s.mu.Unlock()
	if !sameContext {
		s.broadcast(map[string]any{"type": "upsert", "terminal": terminalSummary(term)})
	}
	return exitSuccess(id, terminalSession(term))
}

// terminalWrite accepts data for a running session, drops it for an exited one, and fails while the
// session is starting or errored, when it does not exist, or once after BreakNextWrite.
func (s *Server) terminalWrite(id, payload json.RawMessage) map[string]any {
	var in struct {
		ThreadID   string `json:"threadId"`
		TerminalID string `json:"terminalId"`
		Data       string `json:"data"`
	}
	if err := json.Unmarshal(payload, &in); err != nil || in.Data == "" || len(in.Data) > 65536 {
		return exitDie(id, "t3test: invalid terminal.write payload")
	}
	s.mu.Lock()
	term, found := s.terminal(in.ThreadID, in.TerminalID)
	broken := found && term.Status == "running" && s.breakWrite
	if broken {
		s.breakWrite = false
	}
	s.mu.Unlock()
	ids := map[string]any{"threadId": in.ThreadID, "terminalId": in.TerminalID}
	switch {
	case !found:
		return exitFail(id, withTag("TerminalSessionLookupError", ids))
	case term.Status == "starting" || term.Status == "error":
		return exitFail(id, withTag("TerminalNotRunningError", ids))
	case broken:
		fields := map[string]any{"terminalPid": 50000, "cause": map[string]any{"name": "Error", "message": "write EIO"}}
		maps.Copy(fields, ids)
		return exitFail(id, withTag("TerminalWriteError", fields))
	}
	return exitSuccess(id, nil)
}

func withTag(tag string, fields map[string]any) map[string]any {
	out := map[string]any{"_tag": tag}
	maps.Copy(out, fields)
	return out
}

// threadProjection answers orchestration.getThreadProjection with the thread and empty history.
func (s *Server) threadProjection(id, payload json.RawMessage) map[string]any {
	var in struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(payload, &in)
	s.mu.Lock()
	th, ok := s.threads[in.ThreadID]
	s.mu.Unlock()
	if !ok {
		return exitFail(id, map[string]any{
			"_tag": "OrchestrationV2GetThreadProjectionError", "threadId": in.ThreadID,
			"message": "Failed to load orchestration V2 thread " + in.ThreadID,
		})
	}
	thread := map[string]any{
		"id": th.ID, "projectId": th.ProjectID, "title": th.Title, "branch": nil,
		"worktreePath": nullable(th.WorktreePath), "archivedAt": nullable(th.ArchivedAt),
		"createdAt": timestamp, "updatedAt": timestamp, "deletedAt": nil,
	}
	if th.PinnedAt != "" {
		thread["pinnedAt"] = th.PinnedAt
	}
	if th.TitleRegenerating {
		thread["titleRegeneration"] = map[string]any{"requestId": "regen-1", "startedAt": timestamp}
	}
	return exitSuccess(id, map[string]any{
		"thread": thread, "runs": []any{}, "messages": []any{}, "updatedAt": timestamp,
	})
}

// dispatch applies thread.metadata.update, thread.pin, thread.unpin, and thread.mark-unread. A
// command id seen before replays its receipt without applying the command again.
func (s *Server) dispatch(id, payload json.RawMessage) map[string]any {
	var cmd struct {
		Type      string  `json:"type"`
		CommandID string  `json:"commandId"`
		ThreadID  string  `json:"threadId"`
		Title     *string `json:"title"`
	}
	err := json.Unmarshal(payload, &cmd)
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(message string) map[string]any {
		return exitFail(id, map[string]any{
			"_tag": "OrchestrationV2DispatchCommandError", "commandId": cmd.CommandID, "commandType": cmd.Type,
			"message": message, "detail": message,
		})
	}
	if err != nil || cmd.CommandID == "" {
		return exitDie(id, "t3test: invalid orchestration.dispatchCommand payload")
	}
	if seq, ok := s.receipts[cmd.CommandID]; ok {
		return exitSuccess(id, map[string]any{"sequence": seq})
	}
	th, ok := s.threads[cmd.ThreadID]
	if !ok {
		return fail("Thread " + cmd.ThreadID + " was not found.")
	}
	switch cmd.Type {
	case "thread.metadata.update":
		if cmd.Title != nil {
			th.Title, th.TitleRegenerating = *cmd.Title, false
		}
	case "thread.pin", "thread.unpin":
		if th.ArchivedAt != "" {
			return fail("Archived threads cannot be pinned or unpinned.")
		}
		th.PinnedAt = ""
		if cmd.Type == "thread.pin" {
			th.PinnedAt = timestamp
		}
	case "thread.mark-unread":
		if th.LatestRunCompletedAt == "" {
			return fail("Thread " + cmd.ThreadID + " has no completed run to mark unread.")
		}
		completed, perr := time.Parse(time.RFC3339Nano, th.LatestRunCompletedAt)
		if perr != nil {
			return exitDie(id, "t3test: invalid LatestRunCompletedAt "+th.LatestRunCompletedAt)
		}
		th.LastVisitedAt = completed.Add(-time.Millisecond).UTC().Format(timestampLayout)
	default:
		return fail("t3test: unsupported command type " + cmd.Type)
	}
	s.threads[cmd.ThreadID] = th
	s.sequence++
	s.receipts[cmd.CommandID] = s.sequence
	return exitSuccess(id, map[string]any{"sequence": s.sequence})
}

// subscribe starts a metadata stream: the snapshot at once, every later event only after the
// client acknowledged the previous chunk.
func (fc *fakeConn) subscribe(id json.RawMessage) {
	s := fc.server
	// the stream is registered before the snapshot is read, so an event changing the terminals in
	// between is queued behind the snapshot instead of lost: an upsert may then arrive twice, which
	// is harmless, while a lost remove is not. The snapshot counts as unacknowledged before it is
	// sent, so an Ack racing ahead of run is kept.
	sub := &fakeSub{conn: fc, id: id, wake: make(chan struct{}, 1), quit: make(chan struct{}), unacked: true}
	fc.mu.Lock()
	fc.subs[requestKey(id)] = sub
	fc.mu.Unlock()
	s.mu.Lock()
	terminals := make([]map[string]any, 0, len(s.terminals))
	for _, term := range s.terminals {
		terminals = append(terminals, terminalSummary(term))
	}
	s.mu.Unlock()
	fc.send(chunk(id, map[string]any{"type": "snapshot", "terminals": terminals}))
	s.wg.Go(sub.run)
}

func (fc *fakeConn) sub(requestID json.RawMessage) *fakeSub {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.subs[requestKey(requestID)]
}

// send writes one message; a failed write closes the connection.
func (fc *fakeConn) send(msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fc.wmu.Lock()
	_ = fc.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = fc.ws.WriteMessage(websocket.TextMessage, data)
	fc.wmu.Unlock()
	if err != nil {
		fc.close()
	}
}

// close closes the connection once and stops its streams.
func (fc *fakeConn) close() {
	fc.once.Do(func() {
		close(fc.done)
		_ = fc.ws.Close()
		fc.mu.Lock()
		subs := fc.subs
		fc.subs = map[string]*fakeSub{}
		fc.mu.Unlock()
		for _, sub := range subs {
			sub.stop()
		}
		s := fc.server
		s.mu.Lock()
		delete(s.conns, fc)
		s.mu.Unlock()
	})
}

// fakeSub is one metadata stream; it holds events until the previous chunk is acknowledged.
type fakeSub struct {
	conn     *fakeConn
	id       json.RawMessage
	wake     chan struct{}
	quit     chan struct{}
	quitOnce sync.Once

	mu      sync.Mutex
	queue   []map[string]any
	unacked bool
}

func (sub *fakeSub) enqueue(event map[string]any) {
	sub.mu.Lock()
	sub.queue = append(sub.queue, event)
	sub.mu.Unlock()
	sub.signal()
}

func (sub *fakeSub) ack() {
	sub.mu.Lock()
	sub.unacked = false
	sub.mu.Unlock()
	sub.signal()
}

func (sub *fakeSub) signal() {
	select {
	case sub.wake <- struct{}{}:
	default:
	}
}

func (sub *fakeSub) stop() { sub.quitOnce.Do(func() { close(sub.quit) }) }

// run sends queued events one chunk at a time, each only after the previous chunk's Ack.
func (sub *fakeSub) run() {
	for {
		sub.mu.Lock()
		if !sub.unacked && len(sub.queue) > 0 {
			event := sub.queue[0]
			sub.queue = sub.queue[1:]
			sub.unacked = true
			sub.mu.Unlock()
			sub.conn.send(chunk(sub.id, event))
			continue
		}
		sub.mu.Unlock()
		select {
		case <-sub.wake:
		case <-sub.quit:
			return
		case <-sub.conn.done:
			return
		}
	}
}

func chunk(id json.RawMessage, value map[string]any) map[string]any {
	return map[string]any{"_tag": "Chunk", "requestId": id, "values": []any{value}}
}

func exitSuccess(id json.RawMessage, value any) map[string]any {
	exit := map[string]any{"_tag": "Success"}
	if value != nil {
		exit["value"] = value
	}
	return map[string]any{"_tag": "Exit", "requestId": id, "exit": exit}
}

func exitFail(id json.RawMessage, errValue map[string]any) map[string]any {
	return exitFailure(id, map[string]any{"_tag": "Fail", "error": errValue})
}

func exitDie(id json.RawMessage, defect string) map[string]any {
	return exitFailure(id, map[string]any{"_tag": "Die", "defect": defect})
}

func exitInterrupt(id json.RawMessage) map[string]any {
	return exitFailure(id, map[string]any{"_tag": "Interrupt", "fiberId": nil})
}

func exitFailure(id json.RawMessage, cause map[string]any) map[string]any {
	return map[string]any{"_tag": "Exit", "requestId": id, "exit": map[string]any{"_tag": "Failure", "cause": []any{cause}}}
}

// requestKey normalizes a request id: a number and its decimal string are the same request.
func requestKey(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}
