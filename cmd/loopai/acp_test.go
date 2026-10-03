package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/awake"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/t3"
)

func TestACPRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, false},
		{"acp flag", []string{"--acp"}, true},
		{"acp with other flags", []string{"--debug", "--acp", "--no-color"}, true},
		{"plain run", []string{"docs/plans/x.md"}, false},
		{"after terminator", []string{"--", "--acp"}, false},
		{"prefix only", []string{"--acp-x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, acpRequested(tc.args))
		})
	}
}

func TestVersionBannerWriter(t *testing.T) {
	assert.Same(t, os.Stderr, versionBannerWriter([]string{"--acp"}))
	assert.Same(t, os.Stdout, versionBannerWriter([]string{"plan.md"}))
}

func TestValidateACPFlags(t *testing.T) {
	tests := []struct {
		name    string
		o       opts
		wantErr string
	}{
		{"not acp", opts{PlanFile: "plan.md"}, ""},
		{"bare", opts{ACP: true}, ""},
		{"process options allowed", opts{ACP: true, Debug: true, NoColor: true, ConfigDir: "/cfg", T3: true, Orca: true}, ""},
		{"plan file", opts{ACP: true, PlanFile: "plan.md"}, "--acp cannot be combined with a plan file argument"},
		{"t3 launch", opts{ACP: true, T3Launch: true}, "--acp cannot be combined with --t3-launch"},
		{"cmux workspace", opts{ACP: true, CmuxWorkspace: "auto"}, "--acp cannot be combined with --cmux-workspace"},
		{"clear", opts{ACP: true, Clear: true}, "--acp cannot be combined with --clear"},
		{"merge", opts{ACP: true, mergeSet: true}, "--acp cannot be combined with --merge, --pr, or --report"},
		{"task model", opts{ACP: true, TaskModel: "claude:opus"}, "--acp cannot be combined with execution flags"},
		{"worktree", opts{ACP: true, Worktree: true}, "--acp cannot be combined with execution flags"},
		{"serve", opts{ACP: true, Serve: true}, "--acp cannot be combined with execution flags"},
		{"init", opts{ACP: true, Init: true}, "--acp cannot be combined with execution flags"},
		{"reset", opts{ACP: true, Reset: true}, "--acp cannot be combined with execution flags"},
		{"dump defaults", opts{ACP: true, DumpDefaults: "/tmp/d"}, "--acp cannot be combined with execution flags"},
		{"gen agents", opts{ACP: true, GenAgents: true}, "--acp cannot be combined with execution flags"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateACPFlags(tc.o)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("validateFlags runs the check", func(t *testing.T) {
		err := validateFlags(opts{ACP: true, Worktree: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--acp cannot be combined with execution flags")
	})
}

func TestACPIsStandaloneCommand(t *testing.T) {
	assert.True(t, isStandaloneCommand(opts{ACP: true}))
}

func TestParseACPPrompt(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    opts
		wantErr string
	}{
		{name: "plan only", text: "docs/plans/x.md",
			want: opts{PlanFile: "docs/plans/x.md", PlanFiles: []string{"docs/plans/x.md"}}},
		{name: "surrounding whitespace", text: "\n  docs/plans/x.md  \n",
			want: opts{PlanFile: "docs/plans/x.md", PlanFiles: []string{"docs/plans/x.md"}}},
		{name: "all flags separated", text: "docs/plans/x.md --task-model codex:gpt-6-astra:high --review-model claude:opus " +
			"--external-reviewers codex,claude:sonnet",
			want: opts{PlanFile: "docs/plans/x.md", PlanFiles: []string{"docs/plans/x.md"}, TaskModel: "codex:gpt-6-astra:high",
				ReviewModel: "claude:opus", ExternalReviewers: "codex,claude:sonnet", externalReviewersSet: true}},
		{name: "inline values and flags first", text: "--task-model=claude::high --external-reviewers=custom /abs/plan.md",
			want: opts{PlanFile: "/abs/plan.md", PlanFiles: []string{"/abs/plan.md"}, TaskModel: "claude::high",
				ExternalReviewers: "custom", externalReviewersSet: true}},
		{name: "empty", text: "  ", wantErr: "the prompt must name a plan file"},
		{name: "flags without plan", text: "--task-model claude:opus", wantErr: "the prompt must name a plan file"},
		{name: "two plans", text: "a.md b.md", wantErr: `unexpected argument "b.md"`},
		{name: "chain", text: "a.md,b.md", wantErr: "plan chains are not supported"},
		{name: "unknown option", text: "a.md --worktree", wantErr: `unsupported option "--worktree"`},
		{name: "missing value at end", text: "a.md --task-model", wantErr: "--task-model requires a value"},
		{name: "missing value before flag", text: "a.md --task-model --review-model claude:opus", wantErr: "--task-model requires a value"},
		{name: "empty inline value", text: "a.md --external-reviewers=", wantErr: "--external-reviewers requires a value"},
		{name: "duplicate", text: "a.md --task-model claude:opus --task-model codex:x", wantErr: "--task-model given more than once"},
		{name: "unprefixed model", text: "a.md --task-model opus", wantErr: `prompt: --task-model "opus" needs a claude or codex provider prefix`},
		{name: "custom model", text: "a.md --review-model custom", wantErr: "needs a claude or codex provider prefix"},
		{name: "invalid characters", text: "a.md --external-reviewers codex;rm", wantErr: `prompt: invalid --external-reviewers value "codex;rm"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseACPPrompt(tc.text)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestACPStages(t *testing.T) {
	assert.Equal(t, []acp.Stage{acp.StageReview}, acpStages(&config.Config{}, externalReviewSelection{}))
	assert.Equal(t, []acp.Stage{acp.StageReview, acp.StageExternalReview, acp.StageFinalize, acp.StageReport},
		acpStages(&config.Config{FinalizeEnabled: true, ReportEnabled: true},
			externalReviewSelection{Reviewers: []resolvedReviewer{{}}}))
}

func TestACPRunResult(t *testing.T) {
	failure := errors.New("task failed")
	tests := []struct {
		name    string
		outcome *planExecutionOutcome
		runErr  error
		wantMsg string
		wantErr string
	}{
		{"success with report", &planExecutionOutcome{succeeded: true, report: "# Report"}, nil, "# Report", ""},
		{"success without report", &planExecutionOutcome{succeeded: true}, nil, "loopai completed plan.md", ""},
		{"run error keeps partial report", &planExecutionOutcome{report: "partial"}, failure, "partial", "task failed"},
		{"abort returned nil", &planExecutionOutcome{failure: failure}, nil, "", "task failed"},
		{"no success and no reason", &planExecutionOutcome{}, nil, "", "loopai run did not complete"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := acpRunResult("plan.md", tc.outcome, tc.runErr)
			assert.Equal(t, tc.wantMsg, res.Message)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRedirectStdioForACP(t *testing.T) {
	origIn, origOut, origColor := os.Stdin, os.Stdout, color.Output

	in, out, restore, err := redirectStdioForACP()
	require.NoError(t, err)
	assert.Same(t, origIn, in, "the real stdin is reserved for the protocol")
	assert.Same(t, origOut, out, "the real stdout is reserved for the protocol")
	assert.Same(t, os.Stderr, os.Stdout, "stray stdout writes go to stderr")
	assert.Same(t, color.Error, color.Output, "colored output goes to stderr")
	buf := make([]byte, 1)
	n, readErr := os.Stdin.Read(buf)
	assert.Zero(t, n)
	require.ErrorIs(t, readErr, io.EOF, "stray stdin reads see end of input")

	restore()
	assert.Same(t, origIn, os.Stdin)
	assert.Same(t, origOut, os.Stdout)
	assert.Equal(t, origColor, color.Output)
}

// TestRunACPServesBeforeLoadingConfig drives --acp through run() on swapped process stdio. Config
// is loaded per prompt in the session cwd, so an invalid config in the directory the client started
// loopai in must not keep the agent from answering initialize, and nothing but protocol lines may
// reach stdout.
func TestRunACPServesBeforeLoadingConfig(t *testing.T) {
	start := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(start, ".loopai"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(start, ".loopai", "config"), []byte("executor = codex\n"), 0o600))
	t.Chdir(start)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	_, err = inW.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, inW.Close())
	outFile, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	require.NoError(t, err)
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outFile
	t.Cleanup(func() {
		os.Stdin, os.Stdout = origIn, origOut
		_ = inR.Close()
		_ = outFile.Close()
	})

	require.NoError(t, run(t.Context(), opts{ACP: true, ConfigDir: t.TempDir()}))
	assert.Same(t, inR, os.Stdin, "the process stdin is restored after serving")
	assert.Same(t, outFile, os.Stdout, "the process stdout is restored after serving")

	data, err := os.ReadFile(outFile.Name())
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1, "stdout carries only the initialize response: %q", data)
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &resp))
	assert.Equal(t, 1, resp.ID)
	assert.Contains(t, string(resp.Result), `"protocolVersion":1`)
	assert.Empty(t, resp.Error)
}

func TestLoadACPSessionConfigForcesT3OrcaAndWorktreeOff(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "config"),
		[]byte("use_worktree = true\nt3 = true\norca = true\n"), 0o600))
	t.Chdir(dir)
	o := opts{ConfigDir: t.TempDir()}

	plain, err := loadRunConfig(o)
	require.NoError(t, err)
	require.True(t, plain.T3 && plain.Orca && plain.WorktreeEnabled, "the fixture enables all three")

	cfg, err := loadACPSessionConfig(o)
	require.NoError(t, err)
	assert.False(t, cfg.T3, "the session reports through ACP, not the T3 thread API")
	assert.False(t, cfg.Orca, "the session has no terminal for titles")
	assert.False(t, cfg.WorktreeEnabled, "T3 Code owns the thread's worktree")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "config"), []byte("executor = codex\n"), 0o600))
	_, err = loadACPSessionConfig(o)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task_model = codex:")
}

func TestEnterDir(t *testing.T) {
	start := t.TempDir()
	t.Chdir(start)
	target := t.TempDir()

	leave, err := enterDir(target, io.Discard)
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, canonicalPlanPath(target), canonicalPlanPath(wd))

	leave()
	wd, err = os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, canonicalPlanPath(start), canonicalPlanPath(wd))

	_, err = enterDir(filepath.Join(target, "missing"), io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enter session directory")
}

// acpTestClient drives serveACP over pipes the way T3 Code does and keeps every raw line the
// agent wrote, so tests can assert that nothing but JSON-RPC reached the protocol writer.
type acpTestClient struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	served chan error
	nextID int64

	mu      sync.Mutex
	raw     []string
	pending []map[string]any
}

func startACPServer(ctx context.Context, t *testing.T, o opts, stderr io.Writer) *acpTestClient {
	t.Helper()
	clientToAgent, agentIn := io.Pipe()
	agentOut, agentToClient := io.Pipe()
	c := &acpTestClient{t: t, in: agentIn, lines: make(chan map[string]any, 1024), served: make(chan error, 1), nextID: 1 << 33}
	go func() {
		c.served <- serveACP(ctx, o, clientToAgent, agentToClient, stderr)
		_ = agentToClient.Close()
	}()
	go func() {
		defer close(c.lines)
		r := bufio.NewReader(agentOut)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				c.mu.Lock()
				c.raw = append(c.raw, string(line))
				c.mu.Unlock()
				var m map[string]any
				if json.Unmarshal(line, &m) == nil {
					c.lines <- m
				}
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = agentIn.Close() })
	return c
}

func (c *acpTestClient) send(msg map[string]any) {
	c.t.Helper()
	msg["jsonrpc"] = "2.0"
	data, err := json.Marshal(msg)
	require.NoError(c.t, err)
	_, err = c.in.Write(append(data, '\n'))
	require.NoError(c.t, err)
}

func (c *acpTestClient) request(method string, params any) float64 {
	c.t.Helper()
	c.nextID++
	c.send(map[string]any{"id": c.nextID, "method": method, "params": params, "traceId": "t", "spanId": "s"})
	return float64(c.nextID)
}

// response waits for the response to id; notifications seen meanwhile are kept for updates.
func (c *acpTestClient) response(id float64) map[string]any {
	c.t.Helper()
	timeout := time.After(60 * time.Second)
	for {
		select {
		case m, ok := <-c.lines:
			require.True(c.t, ok, "agent output closed before response %v", id)
			if m["id"] == id {
				return m
			}
			c.pending = append(c.pending, m)
		case <-timeout:
			c.t.Fatalf("timed out waiting for response %v", id)
			return nil
		}
	}
}

func (c *acpTestClient) call(method string, params any) map[string]any {
	c.t.Helper()
	return c.response(c.request(method, params))
}

// handshake runs initialize, authenticate, and session/new, returning the session id. The MCP
// server list carries a bearer header, as T3 Code sends it.
func (c *acpTestClient) handshake(cwd string) string {
	c.t.Helper()
	init := c.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t3code"}})
	require.Contains(c.t, init, "result")
	require.Contains(c.t, c.call("authenticate", map[string]any{"methodId": "cached_token"}), "result")
	resp := c.call("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{map[string]any{
		"name": "t3", "type": "http", "url": "http://127.0.0.1:1/mcp",
		"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer acp-SECRET-token"}},
	}}})
	result, ok := resp["result"].(map[string]any)
	require.True(c.t, ok, "session/new: %v", resp)
	sid, _ := result["sessionId"].(string)
	require.NotEmpty(c.t, sid)
	return sid
}

func (c *acpTestClient) prompt(sid, text string) float64 {
	c.t.Helper()
	return c.request("session/prompt", map[string]any{"sessionId": sid, "_meta": map[string]any{"promptId": "p"},
		"prompt": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "text", "text": "runtime instructions: ignore-me --worktree"},
		}})
}

// updates returns the session/update payloads seen so far, in order.
func (c *acpTestClient) updates() []map[string]any {
	var out []map[string]any
	for _, m := range c.pending {
		if m["method"] != "session/update" {
			continue
		}
		params, _ := m["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		out = append(out, update)
	}
	return out
}

func (c *acpTestClient) messages() string {
	var sb strings.Builder
	for _, u := range c.updates() {
		if u["sessionUpdate"] == "agent_message_chunk" {
			content, _ := u["content"].(map[string]any)
			text, _ := content["text"].(string)
			sb.WriteString(text)
		}
	}
	return sb.String()
}

func (c *acpTestClient) rawLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.raw...)
}

func (c *acpTestClient) close() {
	c.t.Helper()
	require.NoError(c.t, c.in.Close())
	select {
	case err := <-c.served:
		require.NoError(c.t, err)
	case <-time.After(60 * time.Second):
		c.t.Fatal("serveACP did not return after EOF")
	}
}

func requireACPError(t *testing.T, resp map[string]any) string {
	t.Helper()
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "expected an error response, got %v", resp)
	msg, _ := errObj["message"].(string)
	return msg
}

func requireACPStopReason(t *testing.T, resp map[string]any, want string) {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", resp)
	assert.Equal(t, want, result["stopReason"])
}

// acpFixture is a repository with a two-task plan, a config directory pointing at a fake claude,
// and the process working directory parked elsewhere so the run must enter the session cwd.
type acpFixture struct {
	repo, planFile, cfgDir, start string
}

const acpTwoTaskPlan = "# Two\n\n## Overview\nACP fixture.\n\n### Task 1: First\n- [ ] first item\n\n" +
	"### Task 2: Second\n- [ ] second item\n"

func newACPFixture(t *testing.T, claudeScript func(f acpFixture) string, extraConfig ...string) acpFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	f := acpFixture{repo: setupTestRepo(t), cfgDir: t.TempDir(), start: t.TempDir()}
	f.planFile = filepath.Join(f.repo, "docs", "plans", "two.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(f.planFile), 0o750))
	require.NoError(t, os.WriteFile(f.planFile, []byte(acpTwoTaskPlan), 0o600))
	runGit(t, f.repo, "add", ".")
	runGit(t, f.repo, "commit", "-m", "add plan")

	fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
	writeExecutable(t, fakeClaude, claudeScript(f))
	require.NoError(t, os.WriteFile(filepath.Join(f.cfgDir, "config"), []byte("claude_command = "+fakeClaude+"\n"+
		"claude_swap_enabled = false\ncodex_enabled = false\nfinalize_enabled = false\nreport_enabled = true\n"+
		"iteration_delay_ms = 0\ntask_retry_count = 0\nkeep_awake = false\n"+strings.Join(extraConfig, "")), 0o600))
	t.Chdir(f.start)
	return f
}

// acpTaskClaude completes one task per task session by ticking the plan's first open checkbox
// and committing it, signals completion once none are left, and ends every other session with
// REVIEW_DONE.
func acpTaskClaude(f acpFixture) string {
	return `#!/bin/sh
prompt=$(cat)
plan='` + f.planFile + `'
case "$prompt" in
*"Complete ONE Task section"*)
  awk 'BEGIN{d=0} !d && /- \[ \]/ {sub(/- \[ \]/, "- [x]"); d=1} {print}' "$plan" > "$plan.tmp" && mv "$plan.tmp" "$plan"
  git add "$plan" >/dev/null && git commit -q -m "complete task" >/dev/null
  printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"ticked a task\n"}}'
  if ! grep -q -- '- \[ \]' "$plan"; then
    printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:ALL_TASKS_DONE>>>"}}'
  fi
  ;;
*)
  printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:REVIEW_DONE>>>"}}'
  ;;
esac
printf '%s\n' '{"type":"result","result":""}'
`
}

func TestServeACPRunsPlanInProcess(t *testing.T) {
	f := newACPFixture(t, acpTaskClaude)
	stderr := &lockedBuffer{}

	var c *acpTestClient
	var resp map[string]any
	stdout := captureStdout(t, func() {
		c = startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir, Debug: true}, stderr)
		sid := c.handshake(f.repo)
		resp = c.response(c.prompt(sid, "docs/plans/two.md --task-model=claude:sonnet --review-model claude:opus"))
		c.close()
	})

	requireACPStopReason(t, resp, "end_turn")
	assert.Empty(t, stdout, "nothing may reach the process stdout in ACP mode")
	for _, line := range c.rawLines() {
		assert.True(t, json.Valid([]byte(line)), "the protocol writer received a non-JSON line %q", line)
	}

	updates := c.updates()
	var planUpdates [][]any
	calls := map[string]string{}
	var titles []string
	for _, u := range updates {
		switch u["sessionUpdate"] {
		case "plan":
			entries, _ := u["entries"].([]any)
			planUpdates = append(planUpdates, entries)
		case "tool_call":
			id, _ := u["toolCallId"].(string)
			title, _ := u["title"].(string)
			calls[id] = "in_progress"
			titles = append(titles, title)
		case "tool_call_update":
			id, _ := u["toolCallId"].(string)
			if st, ok := u["status"].(string); ok {
				calls[id] = st
			}
			if title, ok := u["title"].(string); ok {
				titles = append(titles, title)
			}
		}
	}

	require.NotEmpty(t, planUpdates)
	entryStatus := func(entries []any, content string) string {
		for _, e := range entries {
			m, _ := e.(map[string]any)
			if m["content"] == content {
				s, _ := m["status"].(string)
				return s
			}
		}
		return ""
	}
	first := planUpdates[0]
	assert.Equal(t, "pending", entryStatus(first, "Task 1: First"))
	assert.Equal(t, "pending", entryStatus(first, "Task 2: Second"))
	assert.Equal(t, "pending", entryStatus(first, "Review"))
	sawPartial := false
	for _, entries := range planUpdates {
		if entryStatus(entries, "Task 1: First") == "completed" && entryStatus(entries, "Task 2: Second") != "completed" {
			sawPartial = true
		}
	}
	assert.True(t, sawPartial, "plan entries progress task by task: %v", planUpdates)
	last := planUpdates[len(planUpdates)-1]
	for _, content := range []string{"Task 1: First", "Task 2: Second", "Review", "Report"} {
		assert.Equal(t, "completed", entryStatus(last, content), "final status of %q in %v", content, last)
	}

	require.NotEmpty(t, calls)
	for id, st := range calls {
		assert.Equal(t, "completed", st, "tool call %s must be closed", id)
	}
	assert.Contains(t, titles, "task 1/2")
	assert.Contains(t, titles, "task 2/2")
	assert.True(t, containsPrefix(titles, "review"), "a review tool call is opened: %v", titles)

	assert.Contains(t, c.messages(), "| internal review |", "the final message carries the completion report")
	assert.FileExists(t, filepath.Join(f.repo, "docs", "plans", "completed", "two.md"), "the completed plan is archived")
	branch := strings.TrimSpace(gitOutput(t, f.repo, "branch", "--show-current"))
	assert.Equal(t, "two", branch, "the plan branch is created in place in the session cwd")

	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, canonicalPlanPath(f.start), canonicalPlanPath(wd), "the working directory is restored")
	assert.Contains(t, stderr.String(), "starting loopai loop", "human-readable output goes to stderr")
	assert.Regexp(t, `(?m)^task:\s+claude sonnet$`, stderr.String(), "the prompt's --task-model reaches the run")
	assert.Regexp(t, `(?m)^review:\s+claude opus$`, stderr.String(), "the prompt's --review-model reaches the run")
	assert.Contains(t, stderr.String(), "Authorization=[redacted]")
	assert.NotContains(t, stderr.String(), "acp-SECRET-token")
}

func TestServeACPPromptErrors(t *testing.T) {
	failingClaude := func(acpFixture) string {
		return `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:TASK_FAILED>>>"}}'
printf '%s\n' '{"type":"result","result":""}'
`
	}
	tests := []struct {
		name     string
		prompt   string
		wantErr  string
		wantMsg  string
		planKept bool
		// sessionDir prepares and returns the session cwd; nil uses the repository
		sessionDir func(t *testing.T, f acpFixture) string
	}{
		{name: "malformed prompt", prompt: "docs/plans/two.md --worktree", wantErr: `unsupported option "--worktree"`,
			wantMsg: "usage: <plan-file>", planKept: true},
		{name: "missing plan", prompt: "docs/plans/missing.md", wantErr: "plan file not found: docs/plans/missing.md", planKept: true},
		{name: "run failure", prompt: "docs/plans/two.md", wantErr: "FAILED signal received", planKept: true},
		{name: "invalid session config", prompt: "docs/plans/two.md", wantErr: "task_model = codex:", planKept: true,
			sessionDir: func(t *testing.T, f acpFixture) string {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(f.repo, ".loopai"), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(f.repo, ".loopai", "config"), []byte("executor = codex\n"), 0o600))
				return f.repo
			}},
		{name: "session directory removed", prompt: "docs/plans/two.md", wantErr: "enter session directory", planKept: true,
			sessionDir: func(t *testing.T, _ acpFixture) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "removed")
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newACPFixture(t, failingClaude)
			cwd := f.repo
			if tc.sessionDir != nil {
				cwd = tc.sessionDir(t, f)
			}
			c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
			sid := c.handshake(cwd)

			msg := requireACPError(t, c.response(c.prompt(sid, tc.prompt)))
			c.close()

			assert.Contains(t, msg, tc.wantErr)
			if tc.wantMsg != "" {
				assert.Contains(t, c.messages(), tc.wantMsg)
			}
			if tc.planKept {
				data, err := os.ReadFile(f.planFile)
				require.NoError(t, err)
				assert.Equal(t, acpTwoTaskPlan, string(data), "a failed run leaves the plan in place")
			}
			wd, err := os.Getwd()
			require.NoError(t, err)
			assert.Equal(t, canonicalPlanPath(f.start), canonicalPlanPath(wd))
		})
	}
}

// TestServeACPForcesWorktreeT3AndOrcaOff runs a prompt in a project whose config enables a
// worktree, T3 thread reporting, and Orca titles: T3 Code owns the thread's worktree and the
// session reports through ACP, so the plan must run in place with none of them.
func TestServeACPForcesWorktreeT3AndOrcaOff(t *testing.T) {
	f := newACPFixture(t, acpTaskClaude, "use_worktree = true\nt3 = true\norca = true\nkeep_awake = true\n")
	originalT3, originalAwake := newT3Reporter, newAwakeHolder
	t.Cleanup(func() { newT3Reporter, newAwakeHolder = originalT3, originalAwake })
	var mu sync.Mutex
	t3Calls := 0
	var awakeCalls []bool
	newT3Reporter = func(t3.Options, func(string) string) (*t3.Reporter, error) {
		mu.Lock()
		t3Calls++
		mu.Unlock()
		return nil, errors.New("t3 reporting is disabled in tests")
	}
	newAwakeHolder = func(enabled bool) *awake.Holder {
		mu.Lock()
		awakeCalls = append(awakeCalls, enabled)
		mu.Unlock()
		return nil
	}

	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)
	requireACPStopReason(t, c.response(c.prompt(sid, "docs/plans/two.md")), "end_turn")
	c.close()

	assert.Equal(t, "two", strings.TrimSpace(gitOutput(t, f.repo, "branch", "--show-current")),
		"the plan branch is checked out in the session cwd, not in a loopai worktree")
	assert.NoDirExists(t, filepath.Join(f.repo, ".loopai", "worktrees"))
	assert.FileExists(t, filepath.Join(f.repo, "docs", "plans", "completed", "two.md"))
	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, t3Calls, "no T3 thread reporter is built for an ACP run")
	assert.Equal(t, []bool{true}, awakeCalls, "each prompt takes a keep-awake hold honoring keep_awake")
}

// TestServeACPSecondPromptAfterFailedRun sends a second prompt on the same connection after a
// real run failed, with a config directory given relative to the directory loopai started in.
func TestServeACPSecondPromptAfterFailedRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "failed-once")
	f := newACPFixture(t, func(f acpFixture) string {
		return strings.Replace(acpTaskClaude(f), "prompt=$(cat)\n", "prompt=$(cat)\n"+
			"if [ ! -e '"+marker+"' ]; then\n  touch '"+marker+"'\n"+
			`  printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:TASK_FAILED>>>"}}'`+"\n"+
			`  printf '%s\n' '{"type":"result","result":""}'`+"\n  exit 0\nfi\n", 1)
	})
	// start two levels below the fixture's start directory, so the relative path to the config
	// directory resolves somewhere else from the session cwd
	start := filepath.Join(f.start, "a", "b")
	require.NoError(t, os.MkdirAll(start, 0o750))
	t.Chdir(start)
	relCfg, err := filepath.Rel(start, f.cfgDir)
	require.NoError(t, err)

	c := startACPServer(t.Context(), t, opts{ConfigDir: relCfg}, io.Discard)
	sid := c.handshake(f.repo)
	msg := requireACPError(t, c.response(c.prompt(sid, "docs/plans/two.md")))
	assert.Contains(t, msg, "FAILED signal received", "the first run uses the configured fake claude and fails")
	requireACPStopReason(t, c.response(c.prompt(sid, "docs/plans/two.md")), "end_turn")
	c.close()

	assert.FileExists(t, filepath.Join(f.repo, "docs", "plans", "completed", "two.md"))
	assert.NoFileExists(t, filepath.Join(f.repo, relCfg, "config"), "no config is installed relative to the session cwd")
	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, canonicalPlanPath(start), canonicalPlanPath(wd))
}

func TestServeACPCancelMidRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	f := newACPFixture(t, func(acpFixture) string {
		return "#!/bin/sh\ncat >/dev/null\ntouch '" + marker + "'\nexec sleep 30\n"
	})
	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)

	id := c.prompt(sid, "docs/plans/two.md")
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 30*time.Second, 10*time.Millisecond)
	c.send(map[string]any{"method": "session/cancel", "params": map[string]any{"sessionId": sid}})

	requireACPStopReason(t, c.response(id), "cancelled") //nolint:misspell // ACP wire value
	c.close()
	data, err := os.ReadFile(f.planFile)
	require.NoError(t, err)
	assert.Equal(t, acpTwoTaskPlan, string(data), "a canceled run leaves the plan in place")
	assert.NoFileExists(t, filepath.Join(f.repo, "docs", "plans", "completed", "two.md"))
}

func TestServeACPContextCancelShutsDown(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	f := newACPFixture(t, func(acpFixture) string {
		return "#!/bin/sh\ncat >/dev/null\ntouch '" + marker + "'\nexec sleep 30\n"
	})
	ctx, cancel := context.WithCancel(t.Context())
	c := startACPServer(ctx, t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)

	id := c.prompt(sid, "docs/plans/two.md")
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 30*time.Second, 10*time.Millisecond)
	cancel()

	requireACPError(t, c.response(id))
	select {
	case err := <-c.served:
		require.NoError(t, err, "serveACP returns once the running prompt is answered, while stdin stays open")
	case <-time.After(60 * time.Second):
		t.Fatal("serveACP did not return after cancellation")
	}
}

func containsPrefix(values []string, prefix string) bool {
	for _, v := range values {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// lockedBuffer is a goroutine-safe buffer standing in for stderr.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // test helper
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
