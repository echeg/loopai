package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// requestTimeout bounds every HTTP call so the run never waits on T3 Code.
const requestTimeout = 2 * time.Second

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 4096

// built-in provider instance ids used for thread model selections.
const (
	InstanceClaude = "claudeAgent"
	InstanceCodex  = "codex"
)

// APIError is a non-2xx response from the T3 Code server.
type APIError struct {
	Status int
	Code   string
	Reason string
}

func (e *APIError) Error() string {
	var parts []string
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("t3: HTTP %d", e.Status)
	}
	return fmt.Sprintf("t3: HTTP %d: %s", e.Status, strings.Join(parts, ": "))
}

// IsAuthError reports whether err is a rejected or insufficient credential.
func IsAuthError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden
}

// Client talks to the orchestration HTTP API of one T3 Code server.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	newID   func() string
	now     func() time.Time
}

// NewClient returns a client for the endpoint.
func NewClient(ep Endpoint) *Client {
	return &Client{
		baseURL: strings.TrimRight(ep.BaseURL, "/"),
		token:   ep.Token,
		http:    &http.Client{Timeout: requestTimeout},
		newID:   NewID,
		now:     time.Now,
	}
}

// NewID returns a random RFC 4122 version 4 identifier. T3 Code ids are client-generated.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Command is one client orchestration command. Dispatch fills in its command id.
type Command interface {
	header() *commandHeader
}

type commandHeader struct {
	Type      string `json:"type"`
	CommandID string `json:"commandId"`
}

func (h *commandHeader) header() *commandHeader { return h }

// ModelSelection names the provider instance and model recorded on a thread.
type ModelSelection struct {
	InstanceID string `json:"instanceId"`
	Model      string `json:"model"`
}

// ThreadCreate creates a thread. It starts no provider session.
type ThreadCreate struct {
	commandHeader
	ThreadID        string         `json:"threadId"`
	ProjectID       string         `json:"projectId"`
	Title           string         `json:"title"`
	ModelSelection  ModelSelection `json:"modelSelection"`
	RuntimeMode     string         `json:"runtimeMode"`
	InteractionMode string         `json:"interactionMode"`
	Branch          *string        `json:"branch"`
	WorktreePath    *string        `json:"worktreePath"`
	CreatedAt       string         `json:"createdAt"`
}

// ThreadTitleUpdate changes only a thread's title. Branch and worktree changes are deliberately
// not expressible here because they re-trigger a server-side pull-request lookup.
type ThreadTitleUpdate struct {
	commandHeader
	ThreadID string `json:"threadId"`
	Title    string `json:"title"`
}

// ThreadPullRequestLink links a pull request to a thread.
type ThreadPullRequestLink struct {
	commandHeader
	ThreadID   string `json:"threadId"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	URL        string `json:"url"`
	Source     string `json:"source"`
}

// NewThreadCreate builds a thread.create command. Empty branch or worktree path are sent as null.
func NewThreadCreate(threadID, projectID, title string, model ModelSelection, branch, worktreePath string) *ThreadCreate {
	return &ThreadCreate{
		commandHeader:   commandHeader{Type: "thread.create"},
		ThreadID:        threadID,
		ProjectID:       projectID,
		Title:           title,
		ModelSelection:  model,
		RuntimeMode:     "full-access",
		InteractionMode: "default",
		Branch:          optionalString(branch),
		WorktreePath:    optionalString(worktreePath),
	}
}

// NewThreadTitleUpdate builds a title-only thread.meta.update command.
func NewThreadTitleUpdate(threadID, title string) *ThreadTitleUpdate {
	return &ThreadTitleUpdate{commandHeader: commandHeader{Type: "thread.meta.update"}, ThreadID: threadID, Title: title}
}

// NewThreadPullRequestLink builds a manual thread.pull-request.link command.
func NewThreadPullRequestLink(threadID string, pr PullRequest) *ThreadPullRequestLink {
	return &ThreadPullRequestLink{
		commandHeader: commandHeader{Type: "thread.pull-request.link"},
		ThreadID:      threadID,
		Host:          pr.Host,
		Repository:    pr.Repository,
		Number:        pr.Number,
		URL:           pr.URL,
		Source:        "manual",
	}
}

func optionalString(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// Dispatch sends one command and returns the event sequence the server assigned.
func (c *Client) Dispatch(ctx context.Context, cmd Command) (int64, error) {
	h := cmd.header()
	if h.CommandID == "" {
		h.CommandID = c.newID()
	}
	if create, ok := cmd.(*ThreadCreate); ok && create.CreatedAt == "" {
		create.CreatedAt = c.now().UTC().Format(time.RFC3339Nano)
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("t3: encode %s: %w", h.Type, err)
	}
	var result struct {
		Sequence int64 `json:"sequence"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/orchestration/dispatch", body, &result); err != nil {
		return 0, fmt.Errorf("t3: dispatch %s: %w", h.Type, err)
	}
	return result.Sequence, nil
}

// Shell is the subset of the orchestration shell snapshot loopai uses.
type Shell struct {
	Projects []Project `json:"projects"`
	Threads  []Thread  `json:"threads"`
}

// Project is a T3 Code project rooted at a workspace directory.
type Project struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspaceRoot"`
}

// Thread is a T3 Code thread as seen in the shell snapshot.
type Thread struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"projectId"`
	Title        string            `json:"title"`
	Branch       *string           `json:"branch"`
	WorktreePath *string           `json:"worktreePath"`
	ArchivedAt   *string           `json:"archivedAt"`
	PullRequests []PullRequestLink `json:"pullRequests"`
}

// PullRequestLink is a pull request already linked to a thread.
type PullRequestLink struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
}

// Shell fetches the orchestration shell snapshot.
func (c *Client) Shell(ctx context.Context) (Shell, error) {
	var shell Shell
	if err := c.do(ctx, http.MethodGet, "/api/orchestration/shell", nil, &shell); err != nil {
		return Shell{}, fmt.Errorf("t3: shell snapshot: %w", err)
	}
	return shell, nil
}

// FindProject returns the project whose workspace root matches root under T3 Code's path
// comparison rules.
func (s Shell) FindProject(root string) (Project, bool) {
	want := NormalizePath(root)
	for _, p := range s.Projects {
		if NormalizePath(p.WorkspaceRoot) == want {
			return p, true
		}
	}
	return Project{}, false
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func decodeAPIError(resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var payload struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(data, &payload) == nil {
		apiErr.Code = payload.Code
		apiErr.Reason = payload.Reason
	}
	return apiErr
}
