package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
)

func TestACPRoute(t *testing.T) {
	cwd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "plan.txt"), []byte("x"), 0o600))
	tests := []struct {
		text, want string
	}{
		{"docs/plans/x.md", acpRouteLaunch},
		{"--task-model claude:opus docs/plans/x.md", acpRouteLaunch},
		{"plan.txt", acpRouteLaunch},
		{"plan.txt is stale", acpRouteChat},
		{"README.md опечатка в заголовке", acpRouteChat},
		{"merge", acpCommandMerge},
		{" PR ", acpCommandPR},
		{"мерж", acpCommandMerge},
		{"МЕРЖ\n", acpCommandMerge},
		{"пр", acpCommandPR},
		{"Merge", acpCommandMerge},
		{"test", acpRouteChat},
		{"thanks", acpRouteChat},
		{"давай мержить", acpRouteChat},
		{"merge please", acpRouteChat},
		{"почему риск medium?", acpRouteChat},
		{"", acpRouteChat},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			assert.Equal(t, tc.want, acpRoute(tc.text, cwd))
		})
	}
}

func TestACPChatProvider(t *testing.T) {
	tests := []struct {
		name          string
		cfg           config.Config
		wantProvider  string
		wantResumable bool
	}{
		{"claude default", config.Config{}, "claude", true},
		{"claude wrapper", config.Config{ClaudeCommand: "/opt/pi-as-claude.sh"}, "claude", false},
		{"codex", config.Config{ReviewProvider: config.ExecutorCodex}, "codex", true},
		{"codex wrapper", config.Config{ReviewProvider: config.ExecutorCodex, CodexCommand: "my-codex"}, "codex", false},
		{"codex review ignores a claude wrapper", config.Config{ReviewProvider: config.ExecutorCodex,
			ClaudeCommand: "/opt/pi-as-claude.sh"}, "codex", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, resumable := acpChatProvider(&tc.cfg)
			assert.Equal(t, tc.wantProvider, provider)
			assert.Equal(t, tc.wantResumable, resumable)
		})
	}
}

func TestACPBranchLog(t *testing.T) {
	repo := setupTestRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	for i := range acpChatLogLimit + 2 {
		runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "change "+strconv.Itoa(i))
	}
	gitSvc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)

	log := acpBranchLog(gitSvc, "master")
	lines := strings.Split(log, "\n")
	require.Len(t, lines, acpChatLogLimit+1)
	assert.Regexp(t, `^[0-9a-f]{7} change `+strconv.Itoa(acpChatLogLimit+1)+`$`, lines[0], "newest first, short hashes")
	assert.Equal(t, "... and 2 more", lines[acpChatLogLimit])
	assert.Empty(t, acpBranchLog(gitSvc, ""), "no base, no log")
	assert.Empty(t, acpBranchLog(gitSvc, "no-such-branch"), "an unreadable range is empty")
}

// acpChatStub is a fake claude named claude, so loopai treats it as the real CLI and resumes its
// sessions. Each call records its arguments and prompt under dir, reports session id sess-<n>, and
// answers with the text set through setAnswer; a "fail" file in dir makes every call exit 1.
type acpChatStub struct {
	dir string
}

func newACPChatStub(t *testing.T) acpChatStub {
	t.Helper()
	s := acpChatStub{dir: t.TempDir()}
	writeExecutable(t, filepath.Join(s.dir, "claude"), `#!/bin/sh
dir='`+s.dir+`'
n=$(( $(cat "$dir/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$dir/count"
cat > "$dir/prompt-$n"
printf '%s\n' "$*" > "$dir/args-$n"
if [ -f "$dir/fail" ]; then
  exit 1
fi
if [ -f "$dir/sleep" ]; then
  touch "$dir/started"
  exec sleep 300
fi
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-'"$n"'"}'
printf '{"type":"content_block_delta","delta":{"type":"text_delta","text":%s}}\n' "$(cat "$dir/answer.json")"
printf '%s\n' '{"type":"result","result":""}'
`)
	s.setAnswer(t, "ok")
	return s
}

func (s acpChatStub) setAnswer(t *testing.T, answer string) {
	t.Helper()
	data, err := json.Marshal(answer)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "answer.json"), data, 0o600))
}

func (s acpChatStub) calls() int {
	data, err := os.ReadFile(filepath.Join(s.dir, "count"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

func (s acpChatStub) read(t *testing.T, name string, n int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, name+"-"+strconv.Itoa(n))) //nolint:gosec // test temp dir
	require.NoError(t, err)
	return string(data)
}

// newACPChatFixture is newACPFixture with the chat stub as the claude command.
func newACPChatFixture(t *testing.T) (acpFixture, acpChatStub) {
	t.Helper()
	f := newACPFixture(t, func(acpFixture) string { return "#!/bin/sh\nexit 1\n" })
	stub := newACPChatStub(t)
	cfgPath := filepath.Join(f.cfgDir, "config")
	data, err := os.ReadFile(cfgPath) //nolint:gosec // test temp dir
	require.NoError(t, err)
	data = regexp.MustCompile(`(?m)^claude_command = .*$`).ReplaceAll(data, []byte("claude_command = "+filepath.Join(stub.dir, "claude")))
	require.NoError(t, os.WriteFile(cfgPath, data, 0o600))
	return f, stub
}

// loadSession revives a saved session id in a fresh server, as T3 Code does after a restart.
func (c *acpTestClient) loadSession(sid, cwd string) {
	c.t.Helper()
	require.Contains(c.t, c.call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t3code"}}), "result")
	require.Contains(c.t, c.call("session/load", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{}}), "result")
}

func TestServeACPChatResumesProviderSession(t *testing.T) {
	f, stub := newACPChatFixture(t)
	stub.setAnswer(t, "Risk is medium because of the fixture.\n<<<RALPHEX:REVIEW_DONE>>>")
	stderr := &lockedBuffer{}

	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, stderr)
	sid := c.handshake(f.repo)
	requireACPStopReason(t, c.response(c.prompt(sid, "почему риск medium?")), "end_turn")

	updates := c.updates()
	require.NotEmpty(t, updates)
	assert.Equal(t, "agent_message_chunk", updates[len(updates)-1]["sessionUpdate"], "the answer is the last update")
	msg := c.messages()
	assert.Equal(t, "Risk is medium because of the fixture.", msg, "signals are stripped and no note is added")
	assert.NotContains(t, stub.read(t, "args", 1), "--resume", "the first turn starts a fresh session")
	first := stub.read(t, "prompt", 1)
	assert.Contains(t, first, "почему риск medium?")
	assert.Contains(t, first, "Branch: master")
	assert.Contains(t, first, "(no loopai run recorded in this thread)", "a fresh session receives the context block")

	state, err := loadACPState(f.repo, sid, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, acpSessionState{Provider: "claude", ProviderSessionID: "sess-1"}, state)

	stub.setAnswer(t, "Next, merge it.")
	requireACPStopReason(t, c.response(c.prompt(sid, "  и что дальше?  ")), "end_turn")
	c.close()
	assert.Contains(t, stub.read(t, "args", 2), "--resume sess-1", "the second turn resumes the first turn's session")
	assert.Equal(t, "и что дальше?", strings.TrimSpace(stub.read(t, "prompt", 2)), "a resumed session gets the message alone")
	assert.Contains(t, c.messages(), "Next, merge it.")

	// a restarted agent revives the session through session/load and continues the provider session
	c2 := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, stderr)
	c2.loadSession(sid, f.repo)
	requireACPStopReason(t, c2.response(c2.prompt(sid, "ещё вопрос")), "end_turn")
	c2.close()
	assert.Contains(t, stub.read(t, "args", 3), "--resume sess-2")

	state, err = loadACPState(f.repo, sid, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "sess-3", state.ProviderSessionID)
	assert.FileExists(t, filepath.Join(f.repo, ".loopai", "progress", "progress-acp-chat.txt"))
	assert.Empty(t, strings.TrimSpace(gitOutput(t, f.repo, "status", "--porcelain")), "chat artifacts are ignored")
	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, canonicalPlanPath(f.start), canonicalPlanPath(wd), "the working directory is restored")
}

func TestServeACPChatDropsSessionOfAnotherProvider(t *testing.T) {
	f, stub := newACPChatFixture(t)
	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)
	require.NoError(t, saveACPState(f.repo, sid, acpSessionState{Provider: "codex", ProviderSessionID: "codex-old",
		LastRun: &acpLastRun{Plan: "docs/plans/two.md", Report: "# Report: Two\n\nfixture report"}}))

	requireACPStopReason(t, c.response(c.prompt(sid, "what changed?")), "end_turn")
	c.close()

	assert.NotContains(t, stub.read(t, "args", 1), "--resume")
	assert.Contains(t, stub.read(t, "prompt", 1), "fixture report", "the fresh session receives the last run's report")
	state, err := loadACPState(f.repo, sid, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "claude", state.Provider)
	assert.Equal(t, "sess-1", state.ProviderSessionID)
	require.NotNil(t, state.LastRun, "a chat turn keeps the recorded run")
	assert.Equal(t, "docs/plans/two.md", state.LastRun.Plan)
}

func TestServeACPChatCanceledKeepsSession(t *testing.T) {
	f, stub := newACPChatFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(stub.dir, "sleep"), nil, 0o600))
	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)
	require.NoError(t, saveACPState(f.repo, sid, acpSessionState{Provider: "claude", ProviderSessionID: "keep-me"}))

	id := c.prompt(sid, "long question")
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(stub.dir, "started"))
		return err == nil
	}, 30*time.Second, 10*time.Millisecond)
	c.send(map[string]any{"method": "session/cancel", "params": map[string]any{"sessionId": sid}})
	requireACPStopReason(t, c.response(id), "cancelled") //nolint:misspell // ACP wire value
	c.close()

	assert.Contains(t, stub.read(t, "args", 1), "--resume keep-me")
	state, err := loadACPState(f.repo, sid, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "keep-me", state.ProviderSessionID, "a canceled turn keeps the previous provider session")
}

// acpChatWorktree adds the layout T3 Code gives an agent-mode thread: branch two checked out in a
// linked worktree with one commit beyond master, which stays checked out in the primary checkout.
func acpChatWorktree(t *testing.T, f acpFixture) string {
	t.Helper()
	worktree := filepath.Join(t.TempDir(), "two-wt")
	runGit(t, f.repo, "worktree", "add", "-q", "-b", "two", worktree)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, worktree, "add", "feature.txt")
	runGit(t, worktree, "commit", "-q", "-m", "add feature")
	return worktree
}

func TestServeACPChatMergeSignal(t *testing.T) {
	f, stub := newACPChatFixture(t)
	worktree := acpChatWorktree(t, f)
	stub.setAnswer(t, "Merging the branch now.\n<<<RALPHEX:ACP_MERGE>>>")
	stderr := &lockedBuffer{}

	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, stderr)
	sid := c.handshake(worktree)
	requireACPStopReason(t, c.response(c.prompt(sid, "давай мержить")), "end_turn")
	c.close()

	head := revParse(t, worktree, "HEAD")
	assert.Equal(t, head, revParse(t, f.repo, "master"), "master receives the feature head")
	assert.DirExists(t, worktree, "the thread's worktree survives")
	assert.True(t, branchExists(t, f.repo, "two"), "the branch survives")
	assert.Equal(t, "two", strings.TrimSpace(gitOutput(t, worktree, "branch", "--show-current")), "no checkout switch")
	msg := c.messages()
	assert.True(t, strings.HasPrefix(msg, "Merging the branch now.\n\n## Merge\n\nMerged `two` into `master` (fast-forward"), msg)
	assert.NotContains(t, msg, "RALPHEX")
	assert.Contains(t, stderr.String(), "acp merge: merged two into master")
}

func TestServeACPCommands(t *testing.T) {
	f, stub := newACPChatFixture(t)
	worktree := acpChatWorktree(t, f)
	stderr := &lockedBuffer{}

	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, stderr)
	sid := c.handshake(worktree)
	requireACPStopReason(t, c.response(c.prompt(sid, " Мерж ")), "end_turn")
	assert.Equal(t, revParse(t, worktree, "HEAD"), revParse(t, f.repo, "master"), "the command merges")
	assert.Contains(t, c.messages(), "## Merge\n\nMerged `two` into `master`")
	requireACPStopReason(t, c.response(c.prompt(sid, "merge")), "end_turn")
	assert.Contains(t, c.messages(), "## Merge\n\n`two` is already in `master`; nothing to merge.")

	// a PATH holding git alone: the pr command refuses without gh and still ends its turn
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	require.NoError(t, os.Symlink(realGit, filepath.Join(binDir, "git")))
	t.Setenv("PATH", binDir)
	requireACPStopReason(t, c.response(c.prompt(sid, "PR")), "end_turn")
	c.close()

	assert.Contains(t, c.messages(), "## Pull request\n\nNo pull request opened against `master`: pr needs GitHub CLI (gh) in PATH")
	assert.Zero(t, stub.calls(), "commands never reach the model")
	assert.DirExists(t, worktree)
}

func TestServeACPCommandFailsOnBrokenConfig(t *testing.T) {
	f, stub := newACPChatFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.cfgDir, "config"), []byte("executor = codex\n"), 0o600))
	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)

	mergeErr := requireACPError(t, c.response(c.prompt(sid, "merge")))
	chatErr := requireACPError(t, c.response(c.prompt(sid, "hello")))
	c.close()

	assert.Contains(t, mergeErr, "executor", "the command turn carries the config error")
	assert.Contains(t, chatErr, "executor", "the chat turn carries the config error")
	assert.Contains(t, chatErr, "task_model", "the error names the replacement spelling")
	assert.Zero(t, stub.calls())
}

func TestServeACPChatProviderFailureFailsTurn(t *testing.T) {
	f, stub := newACPChatFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(stub.dir, "fail"), nil, 0o600))
	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(f.repo)
	require.NoError(t, saveACPState(f.repo, sid, acpSessionState{Provider: "claude", ProviderSessionID: "keep-me"}))

	msg := requireACPError(t, c.response(c.prompt(sid, "why?")))
	c.close()

	assert.Contains(t, msg, "exit status 1")
	assert.Equal(t, 2, stub.calls(), "the failed resume is retried once as a fresh session")
	assert.Contains(t, stub.read(t, "args", 1), "--resume keep-me")
	assert.NotContains(t, stub.read(t, "args", 2), "--resume")
	state, err := loadACPState(f.repo, sid, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, acpSessionState{Provider: "claude", ProviderSessionID: "keep-me"}, state, "a failed turn saves nothing")
}

func TestServeACPChatPRSignal(t *testing.T) {
	f, stub := newACPChatFixture(t)
	worktree := acpChatWorktree(t, f)
	master := revParse(t, f.repo, "master")
	stub.setAnswer(t, "Opening a pull request.\n<<<RALPHEX:ACP_PR>>>")
	// a PATH without gh: the requested PR is refused, which proves the PR action ran and not the merge
	binDir := t.TempDir()
	for _, name := range []string{"git", "cat"} {
		path, err := exec.LookPath(name)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(binDir, name)))
	}
	t.Setenv("PATH", binDir)

	c := startACPServer(t.Context(), t, opts{ConfigDir: f.cfgDir}, io.Discard)
	sid := c.handshake(worktree)
	requireACPStopReason(t, c.response(c.prompt(sid, "open a PR please")), "end_turn")
	c.close()

	msg := c.messages()
	assert.True(t, strings.HasPrefix(msg, "Opening a pull request.\n\n## Pull request\n\nNo pull request opened against `master`"), msg)
	assert.NotContains(t, msg, "RALPHEX")
	assert.Equal(t, master, revParse(t, f.repo, "master"), "a PR request never merges")
}
