package t3

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []recordedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			assert.NoError(t, json.Unmarshal(data, &rec.body))
		}
		mu.Lock()
		requests = append(requests, rec)
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), requests...)
	}
}

func testClient(url string) *Client {
	c := NewClient(Endpoint{BaseURL: url, Token: "secret"})
	c.newID = func() string { return "cmd-1" }
	c.now = func() time.Time { return time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC) }
	return c
}

func okSequence(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"sequence":7}`))
}

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, b := NewID(), NewID()
	assert.Regexp(t, re, a)
	assert.NotEqual(t, a, b)
}

func TestDispatchThreadCreate(t *testing.T) {
	srv, reqs := newTestServer(t, okSequence)
	c := testClient(srv.URL)

	seq, err := c.Dispatch(context.Background(), NewThreadCreate("th-1", "pr-1", "plan · task",
		ModelSelection{InstanceID: InstanceCodex, Model: "gpt-5"}, "loopai/plan", ""))
	require.NoError(t, err)
	assert.Equal(t, int64(7), seq)

	require.Len(t, reqs(), 1)
	req := reqs()[0]
	assert.Equal(t, http.MethodPost, req.method)
	assert.Equal(t, "/api/orchestration/dispatch", req.path)
	assert.Equal(t, "Bearer secret", req.auth)
	assert.Equal(t, map[string]any{
		"type":            "thread.create",
		"commandId":       "cmd-1",
		"threadId":        "th-1",
		"projectId":       "pr-1",
		"title":           "plan · task",
		"modelSelection":  map[string]any{"instanceId": "codex", "model": "gpt-5"},
		"runtimeMode":     "full-access",
		"interactionMode": "default",
		"branch":          "loopai/plan",
		"worktreePath":    nil,
		"createdAt":       "2026-09-25T10:00:00Z",
	}, req.body)
}

func TestDispatchTitleUpdateAndPullRequestLink(t *testing.T) {
	srv, reqs := newTestServer(t, okSequence)
	c := testClient(srv.URL)

	_, err := c.Dispatch(context.Background(), NewThreadTitleUpdate("th-1", "plan · done"))
	require.NoError(t, err)
	_, err = c.Dispatch(context.Background(), NewThreadPullRequestLink("th-1", PullRequest{
		Host: "github.com", Repository: "o/r", Number: 12, URL: "https://github.com/o/r/pull/12",
	}))
	require.NoError(t, err)

	require.Len(t, reqs(), 2)
	assert.Equal(t, map[string]any{
		"type": "thread.meta.update", "commandId": "cmd-1", "threadId": "th-1", "title": "plan · done",
	}, reqs()[0].body)
	assert.Equal(t, map[string]any{
		"type": "thread.pull-request.link", "commandId": "cmd-1", "threadId": "th-1",
		"host": "github.com", "repository": "o/r", "number": float64(12),
		"url": "https://github.com/o/r/pull/12", "source": "manual",
	}, reqs()[1].body)
}

func TestDispatchKeepsExplicitCommandID(t *testing.T) {
	srv, reqs := newTestServer(t, okSequence)
	cmd := NewThreadTitleUpdate("th-1", "x")
	cmd.CommandID = "fixed"
	_, err := testClient(srv.URL).Dispatch(context.Background(), cmd)
	require.NoError(t, err)
	assert.Equal(t, "fixed", reqs()[0].body["commandId"])
}

func TestDispatchErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantAuth bool
		wantMsg  string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"code":"auth_invalid","reason":"invalid_credential"}`, true, "HTTP 401: auth_invalid: invalid_credential"},
		{"forbidden", http.StatusForbidden, `{"code":"insufficient_scope"}`, true, "HTTP 403: insufficient_scope"},
		{"rule violation", http.StatusInternalServerError, `{"code":"internal_error","reason":"orchestration_dispatch_failed"}`, false, "orchestration_dispatch_failed"},
		{"non-json", http.StatusBadGateway, `oops`, false, "HTTP 502"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := testClient(srv.URL).Dispatch(context.Background(), NewThreadTitleUpdate("th", "x"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.Equal(t, tc.wantAuth, IsAuthError(err))
		})
	}
}

func TestDispatchBadResponseAndUnreachable(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) })
	_, err := testClient(srv.URL).Dispatch(context.Background(), NewThreadTitleUpdate("th", "x"))
	require.ErrorContains(t, err, "decode response")

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err = testClient(closed.URL).Dispatch(context.Background(), NewThreadTitleUpdate("th", "x"))
	require.Error(t, err)
	assert.False(t, IsAuthError(err))
}

func TestDispatchHonorsContext(t *testing.T) {
	block := make(chan struct{})
	srv, _ := newTestServer(t, func(_ http.ResponseWriter, _ *http.Request) { <-block })
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := testClient(srv.URL).Dispatch(ctx, NewThreadTitleUpdate("th", "x"))
	require.Error(t, err)
}

func TestShellAndFindProject(t *testing.T) {
	srv, reqs := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"snapshotSequence":3,"updatedAt":"x","projects":[
			{"id":"p1","title":"Other","workspaceRoot":"C:\\Other"},
			{"id":"p2","title":"Loopai","workspaceRoot":"C:\\Projects\\AI\\loopai","scripts":[]}],
			"threads":[{"id":"t1","projectId":"p2","title":"x","branch":"feat","worktreePath":null,
			"archivedAt":null,"pullRequests":[{"host":"github.com","repository":"o/r","number":3}]}]}`))
	})
	shell, err := testClient(srv.URL).Shell(context.Background())
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, reqs()[0].method)
	assert.Equal(t, "/api/orchestration/shell", reqs()[0].path)

	project, ok := shell.FindProject("c:/projects/ai/loopai/")
	require.True(t, ok)
	assert.Equal(t, "p2", project.ID)
	_, ok = shell.FindProject(`C:\Missing`)
	assert.False(t, ok)

	require.Len(t, shell.Threads, 1)
	thread := shell.Threads[0]
	require.NotNil(t, thread.Branch)
	assert.Equal(t, "feat", *thread.Branch)
	assert.Nil(t, thread.WorktreePath)
	assert.True(t, thread.HasPullRequest(PullRequest{Host: "GitHub.com", Repository: "O/R", Number: 3}))
	assert.False(t, thread.HasPullRequest(PullRequest{Host: "github.com", Repository: "o/r", Number: 4}))
}

func TestShellError(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	_, err := testClient(srv.URL).Shell(context.Background())
	require.ErrorContains(t, err, "shell snapshot")
	assert.True(t, IsAuthError(err))
}

func TestAPIErrorMessage(t *testing.T) {
	assert.Equal(t, "t3: HTTP 500", (&APIError{Status: 500}).Error())
	assert.False(t, IsAuthError(nil))
}
