package t3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// mcpProtocolVersion is the MCP version loopai offers; T3 Code's endpoint speaks only this one.
	mcpProtocolVersion = "2025-06-18"
	// mcpServerName names T3 Code's entry in ACP mcpServers: a stdio acp-mcp-bridge whose env holds
	// the thread-scoped endpoint and authorization.
	mcpServerName = "t3-code"
	// headerMCPSession and headerMCPProtocolVersion are the streamable-HTTP session headers. T3
	// Code's endpoint rejects a request after initialize that lacks either.
	headerMCPSession         = "Mcp-Session-Id"
	headerMCPProtocolVersion = "Mcp-Protocol-Version"
	// mcpAccept is the Accept value streamable HTTP requires: both JSON and SSE replies.
	mcpAccept = "application/json, text/event-stream"
	// maxMCPBody caps one MCP reply.
	maxMCPBody = 4 << 20
	// toolThreadList is the tool whose reply names the caller's thread.
	toolThreadList = "t3_thread_list"
)

// Environment variables carrying the MCP endpoint and authorization in an ACP provider's spawn
// environment, frozen at spawn time; the "t3-code" mcpServers entry carries the same pair.
const (
	EnvMCPEndpoint      = "T3_ACP_MCP_ENDPOINT"
	EnvMCPAuthorization = "T3_ACP_MCP_AUTHORIZATION"
)

// JSON-RPC ids of the two MCP requests loopai sends per session.
const (
	mcpInitializeID = "1"
	mcpToolCallID   = "2"
)

// ErrNoMCPThread reports a thread-list reply that names no current thread.
var ErrNoMCPThread = errors.New("t3 mcp: no current thread")

// MCPCredential addresses T3 Code's thread-scoped MCP endpoint. Authorization is the complete
// Authorization header value ("Bearer <token>"); String and GoString redact it, and it is never
// logged.
type MCPCredential struct {
	Endpoint      string
	Authorization string
}

// String describes the credential with its authorization redacted.
func (c MCPCredential) String() string {
	auth := ""
	if c.Authorization != "" {
		auth = "[redacted]"
	}
	return fmt.Sprintf("{Endpoint:%s Authorization:%s}", c.Endpoint, auth)
}

// GoString is String, so %#v redacts too.
func (c MCPCredential) GoString() string { return c.String() }

// complete reports whether both values are set.
func (c MCPCredential) complete() bool { return c.Endpoint != "" && c.Authorization != "" }

// MCPCredentialFromServers returns the MCP endpoint and authorization for an ACP session: the env
// array of the "t3-code" entry in mcpServers (the raw session/new or session/load parameter), else
// T3_ACP_MCP_ENDPOINT and T3_ACP_MCP_AUTHORIZATION read through getenv. A source counts only with
// both values set, so a pair is never mixed. It reports false when neither source has a pair.
func MCPCredentialFromServers(mcpServers json.RawMessage, getenv func(string) string) (MCPCredential, bool) {
	if cred, ok := credentialFromServers(mcpServers); ok {
		return cred, true
	}
	if getenv == nil {
		return MCPCredential{}, false
	}
	cred := MCPCredential{Endpoint: getenv(EnvMCPEndpoint), Authorization: getenv(EnvMCPAuthorization)}
	if !cred.complete() {
		return MCPCredential{}, false
	}
	return cred, true
}

// credentialFromServers reads the "t3-code" entry's env array; entries that do not decode are
// skipped.
func credentialFromServers(raw json.RawMessage) (MCPCredential, bool) {
	var entries []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return MCPCredential{}, false
	}
	for _, entry := range entries {
		var server struct {
			Name string `json:"name"`
			Env  []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		}
		if json.Unmarshal(entry, &server) != nil || server.Name != mcpServerName {
			continue
		}
		var cred MCPCredential
		for _, v := range server.Env {
			switch v.Name {
			case EnvMCPEndpoint:
				cred.Endpoint = v.Value
			case EnvMCPAuthorization:
				cred.Authorization = v.Value
			}
		}
		if cred.complete() {
			return cred, true
		}
	}
	return MCPCredential{}, false
}

// MCPClient calls tools on T3 Code's thread-scoped MCP endpoint over streamable HTTP, the way T3
// Code's own bridge client does. Every call opens its own MCP session: initialize without a session
// id, notifications/initialized, the tool call, then a DELETE ending the session. Replies may be
// JSON or server-sent events. It sends no Origin header, never follows a redirect, never logs, and
// its errors never carry the authorization value.
type MCPClient struct {
	cred       MCPCredential
	version    string
	httpClient *http.Client
}

// NewMCPClient returns a client for cred that reports version as clientInfo.version.
func NewMCPClient(cred MCPCredential, version string) *MCPClient {
	return &MCPClient{
		cred:    cred,
		version: version,
		httpClient: &http.Client{
			Timeout:       httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// CurrentThreadID returns the id of the thread the endpoint's credential belongs to: the
// currentThreadId of a t3_thread_list call with limit 1. Any failure, a rejected credential
// included, returns "" and an error; a reply without a current thread is ErrNoMCPThread.
func (c *MCPClient) CurrentThreadID(ctx context.Context) (string, error) {
	result, err := c.callTool(ctx, toolThreadList, struct {
		Limit int `json:"limit"`
	}{1})
	if err != nil {
		return "", err
	}
	return currentThreadID(result)
}

// mcpEnvelope is a JSON-RPC response from the endpoint.
type mcpEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mcpSession is one MCP session on the endpoint.
type mcpSession struct {
	c               *MCPClient
	sessionID       string
	protocolVersion string
}

// callTool runs one tool call in a fresh MCP session and returns its result.
func (c *MCPClient) callTool(ctx context.Context, name string, args any) (json.RawMessage, error) {
	if !c.cred.complete() {
		return nil, errors.New("t3 mcp: no endpoint configured")
	}
	s := &mcpSession{c: c}
	if err := s.initialize(ctx); err != nil {
		return nil, err
	}
	defer s.end(ctx)
	if err := s.notify(ctx, "notifications/initialized"); err != nil {
		return nil, err
	}
	env, _, err := s.post(ctx, mcpToolCallID, "tools/call", struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	}{name, args})
	if err != nil {
		return nil, err
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, fmt.Errorf("t3 mcp: undecodable %s result: %w", name, err)
	}
	if result.IsError {
		return nil, fmt.Errorf("t3 mcp: %s reported an error", name)
	}
	return env.Result, nil
}

// initialize opens the session and records its id and protocol version.
func (s *mcpSession) initialize(ctx context.Context) error {
	env, header, err := s.post(ctx, mcpInitializeID, "initialize", struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    struct{}       `json:"capabilities"`
		ClientInfo      implementation `json:"clientInfo"`
	}{mcpProtocolVersion, struct{}{}, implementation{Name: "loopai", Version: s.c.version}})
	if err != nil {
		return err
	}
	s.sessionID = header.Get(headerMCPSession)
	if s.sessionID == "" {
		return errors.New("t3 mcp: initialize reply has no session id")
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	s.protocolVersion = mcpProtocolVersion
	if json.Unmarshal(env.Result, &result) == nil && result.ProtocolVersion != "" {
		s.protocolVersion = result.ProtocolVersion
	}
	return nil
}

// implementation is an MCP clientInfo.
type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// post sends one JSON-RPC request and returns the response carrying its id and the HTTP headers.
func (s *mcpSession) post(ctx context.Context, id, method string, params any) (mcpEnvelope, http.Header, error) {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return mcpEnvelope{}, nil, fmt.Errorf("t3 mcp: encode %s: %w", method, err)
	}
	resp, err := s.send(ctx, http.MethodPost, method, body)
	if err != nil {
		return mcpEnvelope{}, nil, err
	}
	defer resp.Body.Close()
	env, err := readMCPResponse(resp, id)
	if err != nil {
		return mcpEnvelope{}, nil, fmt.Errorf("t3 mcp %s: %w", method, err)
	}
	if env.Error != nil {
		return mcpEnvelope{}, nil, fmt.Errorf("t3 mcp %s: error %d: %s", method, env.Error.Code, truncate(env.Error.Message))
	}
	return env, resp.Header, nil
}

// notify sends a notification in the session.
func (s *mcpSession) notify(ctx context.Context, method string) error {
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}{"2.0", method})
	if err != nil {
		return fmt.Errorf("t3 mcp: encode %s: %w", method, err)
	}
	resp, err := s.send(ctx, http.MethodPost, method, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// end asks the endpoint to drop the session. It is best-effort: the session expires on its own.
func (s *mcpSession) end(ctx context.Context) {
	resp, err := s.send(ctx, http.MethodDelete, "session end", nil)
	if err == nil {
		resp.Body.Close()
	}
}

// send performs one HTTP request with the session headers and returns a 2xx response, whose body
// the caller closes. Any other status is an error naming only the method and the status.
func (s *mcpSession) send(ctx context.Context, httpMethod, method string, body []byte) (*http.Response, error) {
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, httpMethod, s.c.cred.Endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("t3 mcp: build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", s.c.cred.Authorization)
	req.Header.Set("Accept", mcpAccept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.sessionID != "" {
		req.Header.Set(headerMCPSession, s.sessionID)
		req.Header.Set(headerMCPProtocolVersion, s.protocolVersion)
	}
	resp, err := s.c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t3 mcp %s: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMCPBody))
		resp.Body.Close()
		return nil, fmt.Errorf("t3 mcp %s: HTTP %d", method, resp.StatusCode)
	}
	return resp, nil
}

// readMCPResponse returns the JSON-RPC response with the given id from a JSON body (one message or
// a batch) or a server-sent event stream, which may carry notifications before the response.
func readMCPResponse(resp *http.Response, id string) (mcpEnvelope, error) {
	body := io.LimitReader(resp.Body, maxMCPBody)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSEResponse(body, id)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return mcpEnvelope{}, fmt.Errorf("read reply: %w", err)
	}
	if env, ok := findMCPResponse(data, id); ok {
		return env, nil
	}
	return mcpEnvelope{}, errors.New("reply has no response")
}

// readSSEResponse scans events until one carries the response with the given id. Each event's data
// lines, joined by newlines, form one JSON message.
func readSSEResponse(r io.Reader, id string) (mcpEnvelope, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxMCPBody)
	var data []string
	flush := func() (mcpEnvelope, bool) {
		if len(data) == 0 {
			return mcpEnvelope{}, false
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return findMCPResponse([]byte(payload), id)
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if env, ok := flush(); ok {
				return env, nil
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return mcpEnvelope{}, fmt.Errorf("read event stream: %w", err)
	}
	if env, ok := flush(); ok {
		return env, nil
	}
	return mcpEnvelope{}, errors.New("event stream has no response")
}

// findMCPResponse returns the response with the given string id from a JSON message or batch.
func findMCPResponse(payload []byte, id string) (mcpEnvelope, bool) {
	payload = bytes.TrimSpace(payload)
	var messages []mcpEnvelope
	if len(payload) > 0 && payload[0] == '[' {
		if json.Unmarshal(payload, &messages) != nil {
			return mcpEnvelope{}, false
		}
	} else {
		var env mcpEnvelope
		if json.Unmarshal(payload, &env) != nil {
			return mcpEnvelope{}, false
		}
		messages = []mcpEnvelope{env}
	}
	for _, env := range messages {
		if jsonString(env.ID) == id && (env.Result != nil || env.Error != nil) {
			return env, true
		}
	}
	return mcpEnvelope{}, false
}

// currentThreadID reads currentThreadId from a t3_thread_list result: structuredContent first, else
// the JSON text of the first content item.
func currentThreadID(result json.RawMessage) (string, error) {
	var wire struct {
		StructuredContent *struct {
			CurrentThreadID *string `json:"currentThreadId"`
		} `json:"structuredContent"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &wire); err != nil {
		return "", fmt.Errorf("t3 mcp: undecodable %s result: %w", toolThreadList, err)
	}
	if wire.StructuredContent != nil && wire.StructuredContent.CurrentThreadID != nil {
		return nonEmptyThread(*wire.StructuredContent.CurrentThreadID)
	}
	if len(wire.Content) > 0 && wire.Content[0].Type == "text" {
		var text struct {
			CurrentThreadID *string `json:"currentThreadId"`
		}
		if json.Unmarshal([]byte(wire.Content[0].Text), &text) == nil && text.CurrentThreadID != nil {
			return nonEmptyThread(*text.CurrentThreadID)
		}
	}
	return "", ErrNoMCPThread
}

// nonEmptyThread returns id, or ErrNoMCPThread when it is empty.
func nonEmptyThread(id string) (string, error) {
	if id == "" {
		return "", ErrNoMCPThread
	}
	return id, nil
}
