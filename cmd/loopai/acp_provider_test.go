//go:build !windows

// The --acp tests share the run fixtures and helpers of runs_cmd_test.go and signal their helper
// processes, so like that file they build on Unix only.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/t3"
)

const (
	// acpMainEnv makes the test binary run main() as "loopai --acp".
	acpMainEnv = "LOOPAI_TEST_ACP_MAIN"
	// acpTestAuthz is the bearer value of the test sessions' MCP entry; no output may contain it.
	acpTestAuthz = "acp-test-0f9e8d7c6b5a"
	// acpTestEndpoint is the MCP endpoint of the test sessions' MCP entry.
	acpTestEndpoint = "http://127.0.0.1:3773/mcp/thread"
)

// acpAuthWords is the Go form of the pattern T3 Code uses to mark a provider unauthenticated.
var acpAuthWords = regexp.MustCompile(`(?i)\b(authenticat(e|ed|es|ing|ion)|credentials?|log([\s-]+)?in)\b`)

// replaceACPLookup replaces lookupACPThreadID for one test.
func replaceACPLookup(t *testing.T, fn func(context.Context, t3.MCPCredential) (string, error)) {
	t.Helper()
	lookupACPThreadID = fn
	t.Cleanup(func() { lookupACPThreadID = stubACPThreadLookup })
}

// acpT3Servers is the mcpServers parameter T3 Code sends a thread's session: its stdio MCP bridge,
// whose env carries the thread-scoped endpoint and bearer token.
func acpT3Servers() string {
	return `[{"name":"t3-code","command":"/Applications/T3 Code.app/Contents/MacOS/T3 Code","args":["--x","acp-mcp-bridge"],` +
		`"env":[{"name":"ELECTRON_RUN_AS_NODE","value":"1"},{"name":"T3_ACP_MCP_ENDPOINT","value":"` + acpTestEndpoint + `"},` +
		`{"name":"T3_ACP_MCP_AUTHORIZATION","value":"Bearer ` + acpTestAuthz + `"}]}]`
}

// acpInitialize is T3 Code's initialize request, id 1, with the given clientInfo name.
func acpInitialize(client string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,`+
		`"info":{"name":"t3-code","version":"0.0.0"},"capabilities":{"elicitation":{"form":{},"url":{}}},`+
		`"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,`+
		`"elicitation":{"form":{},"url":{}}},"clientInfo":{"name":%q,"version":"0.0.0"}}}`, client)
}

func acpSessionNew(id int, cwd, servers string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":%q,"mcpServers":%s}}`, id, cwd, servers)
}

func acpSessionLoad(id int, sid, cwd, servers string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/load","params":{"sessionId":%q,"cwd":%q,"mcpServers":%s}}`,
		id, sid, cwd, servers)
}

// acpPrompt is a session/prompt in T3 Code's envelope: the wrapped message and the runtime block.
func acpPrompt(id int, sid, text string) string {
	wrapped := "<t3_code_instructions>\nbe brief\n</t3_code_instructions>\n\n<user_request>\n" + text + "\n</user_request>"
	blocks, err := json.Marshal([]map[string]string{{"type": "text", "text": wrapped},
		{"type": "text", "text": "<runtime_info>T3 Code</runtime_info>"}})
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":%s}}`,
		id, sid, blocks)
}

// acpWire is the client end of an --acp connection: it writes requests and reads every line the
// provider writes, recording the raw lines.
type acpWire struct {
	t     *testing.T
	in    io.WriteCloser
	lines chan string

	mu  sync.Mutex
	raw []string
}

func newACPWire(t *testing.T, in io.WriteCloser, out io.Reader) *acpWire {
	t.Helper()
	w := &acpWire{t: t, in: in, lines: make(chan string, 256)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			w.mu.Lock()
			w.raw = append(w.raw, sc.Text())
			w.mu.Unlock()
			w.lines <- sc.Text()
		}
		close(w.lines)
	}()
	return w
}

func (w *acpWire) send(line string) {
	w.t.Helper()
	_, err := io.WriteString(w.in, line+"\n")
	require.NoError(w.t, err)
}

// next returns the next message the provider wrote.
func (w *acpWire) next() map[string]any {
	w.t.Helper()
	select {
	case line, ok := <-w.lines:
		require.True(w.t, ok, "provider output ended")
		var m map[string]any
		require.NoError(w.t, json.Unmarshal([]byte(line), &m), "stdout line %q is not JSON-RPC", line)
		return m
	case <-time.After(runsWait):
		w.t.Fatal("no message from the provider")
		return nil
	}
}

// answer returns the next message, which must answer request id.
func (w *acpWire) answer(id int) map[string]any {
	w.t.Helper()
	m := w.next()
	require.InDelta(w.t, float64(id), m["id"], 0, "expected the answer to %d, got %v", id, m)
	return m
}

// result returns the result object of the answer to request id.
func (w *acpWire) result(id int) map[string]any {
	w.t.Helper()
	m := w.answer(id)
	result, ok := m["result"].(map[string]any)
	require.True(w.t, ok, "request %d failed: %v", id, m)
	return result
}

// newSession opens a session through session/new and returns its id.
func (w *acpWire) newSession(id int, cwd, servers string) string {
	w.t.Helper()
	w.send(acpSessionNew(id, cwd, servers))
	sid, ok := w.result(id)["sessionId"].(string)
	require.True(w.t, ok)
	return sid
}

// statusReply sends a prompt and returns the text of the one message it answers with before
// end_turn.
func (w *acpWire) statusReply(id int, sid, text string) string {
	w.t.Helper()
	w.send(acpPrompt(id, sid, text))
	return w.turnReply(id, sid)
}

// turnReply returns the text of the one message prompt id of session sid answers with before
// end_turn.
func (w *acpWire) turnReply(id int, sid string) string {
	w.t.Helper()
	update := w.next()
	require.Equal(w.t, "session/update", update["method"], "got %v", update)
	params, _ := update["params"].(map[string]any)
	assert.Equal(w.t, sid, params["sessionId"])
	chunk, _ := params["update"].(map[string]any)
	require.Equal(w.t, "agent_message_chunk", chunk["sessionUpdate"])
	assert.Regexp(w.t, `^loopai-final-[0-9a-f]{16}$`, chunk["messageId"])
	content, _ := chunk["content"].(map[string]any)
	assert.Equal(w.t, "text", content["type"])
	reply, _ := content["text"].(string)
	assert.Equal(w.t, map[string]any{"stopReason": "end_turn"}, w.result(id))
	return reply
}

// rawLines returns every line the provider wrote so far.
func (w *acpWire) rawLines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.raw...)
}

// acpServer is serveACP running in this process over pipes.
type acpServer struct {
	*acpWire
	stderr *syncBuffer
}

// startACPServer runs serveACP in this process with getenv as its environment. The test ends it by
// closing its input.
func startACPServer(t *testing.T, getenv func(string) string) *acpServer {
	t.Helper()
	clientToAgent, agentIn := io.Pipe()
	agentOut, agentToClient := io.Pipe()
	s := &acpServer{acpWire: newACPWire(t, agentIn, agentOut), stderr: &syncBuffer{}}
	served := make(chan error, 1)
	go func() { served <- serveACP(context.Background(), clientToAgent, agentToClient, s.stderr, getenv) }()
	t.Cleanup(func() {
		_ = agentIn.Close()
		select {
		case err := <-served:
			assert.NoError(t, err, "the end of input ends serveACP cleanly")
		case <-time.After(runsWait):
			t.Error("serveACP did not return after its input ended")
		}
		_ = agentToClient.Close()
	})
	return s
}

// noACPEnv is an environment without T3 Code's spawn-time MCP variables.
func noACPEnv(string) string { return "" }

// TestACPMainHelper runs main() as "loopai --acp" for the process tests, not a test of its own.
func TestACPMainHelper(t *testing.T) {
	if _, ok := os.LookupEnv(acpMainEnv); !ok {
		t.Skip("helper process only")
	}
	os.Args = []string{"loopai", "--acp"}
	main()
	os.Exit(0) // main returns when --acp ends without an error
}

// acpProcess is "loopai --acp" running as a child process, its stdio pipes owned by the test as T3
// Code owns them.
type acpProcess struct {
	*acpWire
	cmd     *exec.Cmd
	home    string
	stderr  *syncBuffer
	done    chan struct{}
	waitErr error
}

// startACPProcess starts "loopai --acp" in dir with HOME, USERPROFILE, and the config directory
// redirected to a temp directory and env added to the environment.
func startACPProcess(t *testing.T, dir string, env ...string) *acpProcess {
	t.Helper()
	p := &acpProcess{home: t.TempDir(), stderr: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestACPMainHelper$")
	p.cmd.Dir = dir
	// the race detector sleeps a second before a process exits; the exit-time tests measure loopai,
	// not that sleep
	p.cmd.Env = append(os.Environ(), acpMainEnv+"=1", "HOME="+p.home, "USERPROFILE="+p.home,
		"LOOPAI_CONFIG_DIR="+filepath.Join(p.home, "config"), "XDG_CONFIG_HOME="+filepath.Join(p.home, ".config"),
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	p.cmd.Env = append(p.cmd.Env, env...)
	stdin, err := p.cmd.StdinPipe()
	require.NoError(t, err)
	// a pipe of our own, not StdoutPipe: Wait would close that one and could drop the last lines
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	p.cmd.Stdout, p.cmd.Stderr = stdoutW, p.stderr
	require.NoError(t, p.cmd.Start())
	require.NoError(t, stdoutW.Close())
	p.acpWire = newACPWire(t, stdin, stdoutR)
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-p.done:
		case <-time.After(runsWait):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		_ = stdoutR.Close()
	})
	return p
}

// closeInput ends the provider's stdin, as T3 Code closing the connection does.
func (p *acpProcess) closeInput() {
	p.t.Helper()
	require.NoError(p.t, p.in.Close())
}

// wait waits for the process to exit and returns its exit code; an exit by a signal fails the test.
func (p *acpProcess) wait() int {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(runsWait):
		p.t.Fatal("loopai --acp did not exit")
	}
	if p.waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(p.t, p.waitErr, &exitErr)
	require.True(p.t, exitErr.Exited(), "loopai --acp exited by itself: %v; stderr:\n%s", p.waitErr, p.stderr.String())
	return exitErr.ExitCode()
}

// drained returns the lines the provider wrote after the ones the test read, once its output ended.
func (p *acpProcess) drained() []string {
	p.t.Helper()
	var rest []string
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				return rest
			}
			rest = append(rest, line)
		case <-time.After(runsWait):
			p.t.Fatal("the provider's output did not end")
			return nil
		}
	}
}

// bannerOnly asserts that the process wrote nothing to stderr but the version banner.
func (p *acpProcess) bannerOnly() {
	p.t.Helper()
	assert.Equal(p.t, "loopai "+resolveVersion()+"\n", p.stderr.String(), "no output besides the banner")
}

// treeSnapshot maps every file under root to its content and modification time.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // test temp tree
		require.NoError(t, err)
		info, err := d.Info()
		require.NoError(t, err)
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		snap[rel] = info.ModTime().String() + "\n" + string(data)
		return nil
	}))
	return snap
}

// saveACPTestRun records a finished run with outcome under root, as a Mode 2 launch would.
func saveACPTestRun(t *testing.T, root, id string, outcome runs.Outcome) {
	t.Helper()
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: id, Plan: "docs/plans/feature.md", Branch: "feature",
		Origin: runs.Origin{Kind: runs.OriginT3Thread}})
	saveRunsTestOutcome(t, dir, outcome)
}

// TestACPProbeSequenceTouchesNothing replays T3 Code's readiness probe against "loopai --acp": it
// succeeds from the server's cwd without reading config or a repository and writes no binding, even
// when its session/new carries MCP servers.
func TestACPProbeSequenceTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	p := startACPProcess(t, dir)
	p.send(acpInitialize("t3-code-provider-test"))
	initResult := p.result(1)
	assert.InDelta(t, 1, initResult["protocolVersion"], 0)
	assert.NotContains(t, initResult, "info", "an info key would switch T3 Code to ACP v2")
	caps, _ := initResult["agentCapabilities"].(map[string]any)
	assert.Equal(t, true, caps["loadSession"])

	assert.Regexp(t, `^loopai-[0-9a-f]{32}$`, p.newSession(2, "/", "[]"))
	p.newSession(3, dir, "[]")
	p.newSession(4, dir, acpT3Servers()) // a probe by name, whatever it carries
	p.closeInput()
	assert.Equal(t, 0, p.wait())
	assert.Empty(t, p.drained())
	p.bannerOnly()

	assert.Empty(t, treeSnapshot(t, dir), "no binding, no .loopai directory")
	assert.Empty(t, treeSnapshot(t, p.home), "no config directory")
}

// TestACPInitializeOnlyThenEOF replays T3 Code's sign-in check: initialize alone, then EOF, has no
// side effects and no error output.
func TestACPInitializeOnlyThenEOF(t *testing.T) {
	dir := t.TempDir()
	p := startACPProcess(t, dir)
	p.send(acpInitialize("t3-code-provider-auth"))
	assert.JSONEq(t, `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,`+
		`"promptCapabilities":{"image":false,"audio":false,"embeddedContext":false},"mcpCapabilities":{"http":false,"sse":false}},`+
		`"authMethods":[],"agentInfo":{"name":"loopai","version":"`+resolveVersion()+`"}}`, mustJSON(t, p.result(1)))
	p.closeInput()
	assert.Equal(t, 0, p.wait())
	assert.Empty(t, p.drained())
	p.bannerOnly()
	assert.Empty(t, treeSnapshot(t, dir))
	assert.Empty(t, treeSnapshot(t, p.home))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func TestACPNewSessionRecordsTheThread(t *testing.T) {
	tests := []struct {
		name     string
		servers  string
		env      map[string]string
		wantCred t3.MCPCredential
	}{
		{name: "the session's MCP entry wins over the spawn environment", servers: acpT3Servers(),
			env:      map[string]string{t3.EnvMCPEndpoint: "http://env/mcp", t3.EnvMCPAuthorization: "Bearer env-token"},
			wantCred: t3.MCPCredential{Endpoint: acpTestEndpoint, Authorization: "Bearer " + acpTestAuthz}},
		{name: "the spawn environment without an MCP entry", servers: "[]",
			env:      map[string]string{t3.EnvMCPEndpoint: "http://env/mcp", t3.EnvMCPAuthorization: "Bearer env-token"},
			wantCred: t3.MCPCredential{Endpoint: "http://env/mcp", Authorization: "Bearer env-token"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			var mu sync.Mutex
			var creds []t3.MCPCredential
			replaceACPLookup(t, func(ctx context.Context, cred t3.MCPCredential) (string, error) {
				_, hasDeadline := ctx.Deadline()
				assert.True(t, hasDeadline, "the lookup is bounded")
				mu.Lock()
				creds = append(creds, cred)
				mu.Unlock()
				return "thread-1", nil
			})
			s := startACPServer(t, func(key string) string { return tc.env[key] })
			s.send(acpInitialize("t3-code"))
			s.result(1)
			sid := s.newSession(2, cwd, tc.servers)

			m := readBindingFile(t, filepath.Join(cwd, ".loopai", "acp", "binding-session-"+sid+".json"))
			assert.Equal(t, "thread-1", m["thread_id"])
			assert.Equal(t, sid, m["session_id"])
			assert.Equal(t, cwd, m["cwd"])
			mu.Lock()
			assert.Equal(t, []t3.MCPCredential{tc.wantCred}, creds)
			mu.Unlock()

			reply := s.statusReply(3, sid, "what can you do?")
			assert.Contains(t, reply, "No loopai run belongs to this thread yet")
			assert.Contains(t, reply, acpPromptUsage)
			assert.Contains(t, reply, acpModelSwitchHint)
			assert.NotContains(t, s.stderr.String(), acpTestAuthz)
			assert.NotContains(t, s.stderr.String(), "env-token")
		})
	}
}

// TestACPSessionBindingStaysOutOfGitStatus opens a thread's sessions in a fresh repository, as T3
// Code does in the new worktree of a thread, which never has the generated .loopai/.gitignore: the
// bindings are written, and Git status stays empty.
func TestACPSessionBindingStaysOutOfGitStatus(t *testing.T) {
	cwd := setupTestRepo(t)
	t.Setenv(acpLookupEnv, "thread=thread-1")
	s := startACPServer(t, noACPEnv)
	s.send(acpInitialize("t3-code"))
	s.result(1)
	sid := s.newSession(2, cwd, acpT3Servers())
	s.send(acpSessionLoad(3, "sess-restored", cwd, acpT3Servers()))
	assert.Empty(t, s.result(3), "session/load answers {}")

	assert.FileExists(t, filepath.Join(cwd, ".loopai", "acp", "binding-session-"+sid+".json"))
	assert.FileExists(t, filepath.Join(cwd, ".loopai", "acp", "binding-session-sess-restored.json"))
	assert.Empty(t, gitOutput(t, cwd, "status", "--porcelain"))
	assert.Empty(t, gitOutput(t, cwd, "status", "--porcelain", "--untracked-files=all"))
	assert.NotContains(t, s.stderr.String(), "warning:")
}

// TestACPLoadSessionInANewProcessRestoresTheBinding starts a session in one --acp process, binds the
// thread's run, and checks that a session/load in a new process finds it from disk alone: with the
// lookup failing, the session's own binding names the thread.
func TestACPLoadSessionInANewProcessRestoresTheBinding(t *testing.T) {
	cwd := t.TempDir()
	first := startACPProcess(t, cwd, acpLookupEnv+"=thread="+acpLongThreadID)
	first.send(acpInitialize("t3-code"))
	first.result(1)
	sid := first.newSession(2, cwd, acpT3Servers())
	first.closeInput()
	require.Equal(t, 0, first.wait())

	const runID = "feature-20261022-120000-abcd"
	saveACPTestRun(t, cwd, runID, runs.Outcome{Status: runs.StatusSucceeded, ReportSummary: "Added the provider.",
		Branch: "feature"})
	require.NoError(t, bindRun(cwd, acpLongThreadID, sid, runID))

	for _, lookup := range []string{"fail", "thread=" + acpLongThreadID} {
		t.Run(lookup, func(t *testing.T) {
			second := startACPProcess(t, cwd, acpLookupEnv+"="+lookup)
			second.send(acpInitialize("t3-code"))
			second.result(1)
			second.send(acpSessionLoad(2, sid, cwd, acpT3Servers()))
			assert.Empty(t, second.result(2), "session/load answers {}")
			reply := second.statusReply(3, sid, acp.ContinuationText)
			assert.Equal(t, "loopai run "+runID+" succeeded.\n\nAdded the provider.\n\nBranch: feature\n\n"+acpModelSwitchHint, reply)
			second.closeInput()
			assert.Equal(t, 0, second.wait())
			assert.NotContains(t, second.stderr.String(), acpTestAuthz)
			if lookup == "fail" {
				assert.Contains(t, second.stderr.String(), "thread id lookup failed: t3 mcp initialize: HTTP 401; "+
					"using the thread the session recorded")
			}
		})
	}
}

func TestACPLongThreadIDBinding(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv(acpLookupEnv, "thread="+acpLongThreadID)
	s := startACPServer(t, noACPEnv)
	sid := s.newSession(1, cwd, acpT3Servers())
	m := readBindingFile(t, filepath.Join(cwd, ".loopai", "acp", "binding-session-"+sid+".json"))
	assert.Equal(t, acpLongThreadID, m["thread_id"], "the session binding keeps the raw thread id")

	const runID = "feature-20261022-120000-abcd"
	saveACPTestRun(t, cwd, runID, runs.Outcome{Status: runs.StatusFailed, Reason: "task 2 failed", Branch: "feature"})
	require.NoError(t, bindRun(cwd, acpLongThreadID, sid, runID))
	assert.FileExists(t, filepath.Join(cwd, ".loopai", "acp", "binding-thread-"+runs.ThreadKey(acpLongThreadID)+".json"))

	reply := s.statusReply(2, sid, "how did it go?")
	assert.True(t, strings.HasPrefix(reply, "loopai run "+runID+" failed: task 2 failed."), reply)
}

// TestACPLookupFailureFallsBackToTheSessionKey checks a session whose thread id cannot be found: the
// session id becomes its binding key, and a run bound through it is found again.
func TestACPLookupFailureFallsBackToTheSessionKey(t *testing.T) {
	tests := []struct {
		name    string
		servers string
		warning string
	}{
		{name: "the lookup fails", servers: acpT3Servers(),
			warning: "warning: acp: thread id lookup failed: t3 mcp initialize: HTTP 401; binding the session by its id\n"},
		{name: "no T3 MCP entry", servers: `[{"name":"other","command":"x","args":[],"env":[]}]`,
			warning: "warning: acp: the session has no T3 MCP endpoint; binding the session by its id\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			s := startACPServer(t, noACPEnv) // TestMain's stub fails without acpLookupEnv
			sid := s.newSession(1, cwd, tc.servers)
			assert.Contains(t, s.stderr.String(), tc.warning)
			assert.NotContains(t, s.stderr.String(), acpTestAuthz)
			m := readBindingFile(t, filepath.Join(cwd, ".loopai", "acp", "binding-session-"+sid+".json"))
			assert.Empty(t, m["thread_id"])
			assert.Equal(t, sid, m["session_id"])

			const runID = "feature-20261022-120000-abcd"
			saveACPTestRun(t, cwd, runID, runs.Outcome{Status: runs.StatusStopped, Branch: "feature"})
			require.NoError(t, bindRun(cwd, "", sid, runID))
			reply := s.statusReply(2, sid, "status")
			assert.True(t, strings.HasPrefix(reply, "loopai run "+runID+" stopped."), reply)
			matches, err := filepath.Glob(filepath.Join(cwd, ".loopai", "acp", "binding-thread-*"))
			require.NoError(t, err)
			assert.Empty(t, matches, "no thread binding without a thread id")
		})
	}
}

// TestACPIgnoresStaleOrCorruptState loads a session whose directory holds the removed ACP chat's
// state file and a corrupt binding: neither fails the session, and the binding is written anew.
func TestACPIgnoresStaleOrCorruptState(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".loopai", "acp")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	const old = `{"provider":"codex","provider_session_id":"p-1","last_run":{"plan":"docs/plans/a.md","report":"# Report"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sess-1.json"), []byte(old), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "binding-session-sess-1.json"), []byte(`{"thread_id":`), 0o600))

	s := startACPServer(t, noACPEnv)
	s.send(acpSessionLoad(1, "sess-1", cwd, acpT3Servers()))
	assert.Empty(t, s.result(1))
	reply := s.statusReply(2, "sess-1", "status")
	assert.Contains(t, reply, "No loopai run belongs to this thread yet")

	m := readBindingFile(t, filepath.Join(dir, "binding-session-sess-1.json"))
	assert.Equal(t, "sess-1", m["session_id"], "the corrupt binding is replaced")
	data, err := os.ReadFile(filepath.Join(dir, "sess-1.json")) //nolint:gosec // test temp file
	require.NoError(t, err)
	assert.JSONEq(t, old, string(data), "the old chat state is left alone")
}

// TestACPStdoutCarriesOnlyJSONRPC makes a handler print to stdout, as stray code in a run could:
// the line lands on stderr, and every stdout line stays a JSON-RPC message.
func TestACPStdoutCarriesOnlyJSONRPC(t *testing.T) {
	cwd := t.TempDir()
	p := startACPProcess(t, cwd, acpLookupEnv+"=stray=thread-1")
	p.send(acpInitialize("t3-code"))
	p.result(1)
	sid := p.newSession(2, cwd, acpT3Servers())
	p.statusReply(3, sid, "hello")
	p.closeInput()
	assert.Equal(t, 0, p.wait())
	assert.Empty(t, p.drained())

	lines := p.rawLines()
	require.NotEmpty(t, lines)
	for _, line := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "stdout line %q", line)
		assert.Equal(t, "2.0", m["jsonrpc"], line)
	}
	assert.Contains(t, p.stderr.String(), acpStrayLine)
}

// TestACPEndsPromptlyWithoutAnswering ends "loopai --acp" while a session/load waits on its thread
// lookup and a prompt waits for that session: on EOF or SIGTERM the process exits at once, well
// before T3 Code's SIGKILL a second later, answers neither request, and leaves every file of the
// session's bound run and binding as it was.
func TestACPEndsPromptlyWithoutAnswering(t *testing.T) {
	for _, end := range []string{"EOF", "SIGTERM"} {
		t.Run(end, func(t *testing.T) {
			cwd := t.TempDir()
			const runID = "feature-20261022-120000-abcd"
			saveACPTestRun(t, cwd, runID, runs.Outcome{Status: runs.StatusFailed, Branch: "feature"})
			require.NoError(t, bindRun(cwd, "", "sess-1", runID))
			before := treeSnapshot(t, cwd)

			p := startACPProcess(t, cwd, acpLookupEnv+"=block")
			p.send(acpInitialize("t3-code"))
			p.result(1)
			p.send(acpSessionLoad(2, "sess-1", cwd, acpT3Servers()))
			p.send(acpPrompt(3, "sess-1", acp.ContinuationText))
			require.Eventually(t, func() bool { return strings.Contains(p.stderr.String(), acpLookupStarted) }, runsWait,
				10*time.Millisecond, "the lookup never started")

			start := time.Now()
			if end == "EOF" {
				p.closeInput()
			} else {
				require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
			}
			assert.Equal(t, 0, p.wait())
			assert.Less(t, time.Since(start), time.Second, "exit at once, before T3 Code's SIGKILL")
			assert.Empty(t, p.drained(), "neither the load nor the prompt is answered")
			assert.Equal(t, before, treeSnapshot(t, cwd), "no run or binding file touched")
			assert.NotContains(t, p.stderr.String(), "warning:", "a lookup canceled by the ending is no failure")
		})
	}
}

func TestValidateACPFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // empty when the flags are valid
	}{
		{name: "alone", args: []string{"--acp"}},
		{name: "with output flags", args: []string{"--acp", "--debug", "--no-color", "--config-dir", "d"}},
		{name: "with orca, which the environment may set", args: []string{"--acp", "--orca"}},
		{name: "a plan", args: []string{"--acp", "docs/plans/x.md"}, wantErr: "--acp takes no plan argument"},
		{name: "detach", args: []string{"--acp", "--detach"}, wantErr: "--acp cannot be combined with --detach"},
		{name: "detach and a plan", args: []string{"--detach", "--acp", "docs/plans/x.md"},
			wantErr: "--acp takes no plan argument"},
		{name: "the detached child", args: []string{"--acp", "--detached-run=x-1"},
			wantErr: "--acp cannot be combined with --detached-run"},
		{name: "t3 thread", args: []string{"--acp", "--t3-thread", "t-1"}, wantErr: "--acp cannot be combined with --t3-thread"},
		{name: "attach", args: []string{"--acp", "--attach"}, wantErr: "--acp cannot be combined with --attach, --runs, or --stop"},
		{name: "runs", args: []string{"--runs", "--acp"}, wantErr: "--attach, --runs, or --stop"},
		{name: "merge", args: []string{"--acp", "--merge"}, wantErr: "--acp cannot be combined with --merge, --pr, or --report"},
		{name: "report", args: []string{"--acp", "--report"}, wantErr: "--merge, --pr, or --report"},
		{name: "clear", args: []string{"--acp", "--clear"}, wantErr: "--acp cannot be combined with --clear"},
		{name: "cmux workspace", args: []string{"--acp", "--cmux-workspace"}, wantErr: "--acp cannot be combined with --cmux-workspace"},
		{name: "init", args: []string{"--acp", "--init"}, wantErr: "--acp cannot be combined with --init"},
		{name: "reset", args: []string{"--acp", "--reset"}, wantErr: "--acp cannot be combined with --reset"},
		{name: "dump defaults", args: []string{"--acp", "--dump-defaults", "d"}, wantErr: "--acp cannot be combined with --dump-defaults"},
		{name: "gen agents", args: []string{"--acp", "--gen-agents"}, wantErr: "--acp cannot be combined with --gen-agents"},
		{name: "serve", args: []string{"--acp", "--serve"}, wantErr: "--acp cannot be combined with --serve"},
		{name: "watch", args: []string{"--acp", "--watch", "d"}, wantErr: "--acp cannot be combined with --watch"},
		{name: "plan creation", args: []string{"--acp", "--plan", "add x"}, wantErr: "--acp cannot be combined with --plan"},
		{name: "a model flag", args: []string{"--acp", "--task-model", "claude"}, wantErr: "--acp cannot be combined with execution flags"},
		{name: "an explicit zero", args: []string{"--acp", "--max-iterations=0"}, wantErr: "execution flags"},
		{name: "tasks only", args: []string{"--acp", "--tasks-only"}, wantErr: "execution flags"},
		{name: "a removed flag names its replacement first", args: []string{"--acp", "--t3"}, wantErr: "--t3 was removed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFlags(parseTestOpts(t, tc.args...))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestIsStandaloneCommandACP(t *testing.T) {
	assert.True(t, isStandaloneCommand(opts{ACP: true}))
}

func TestACPRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, false},
		{"flag", []string{"--acp"}, true},
		{"with other flags", []string{"--debug", "--acp"}, true},
		{"after terminator", []string{"--", "--acp"}, false},
		{"prefix only", []string{"--acp-thread=x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, acpRequested(tc.args))
		})
	}
}

func TestACPIsProbe(t *testing.T) {
	servers := json.RawMessage(acpT3Servers())
	envWith := func(key string) func(string) string {
		return func(k string) string {
			if k == key {
				return "x"
			}
			return ""
		}
	}
	tests := []struct {
		name   string
		s      acp.Session
		getenv func(string) string
		want   bool
	}{
		{name: "readiness probe by name", s: acp.Session{ClientName: "t3-code-provider-test", MCPServers: servers}, want: true},
		{name: "session manager by name", s: acp.Session{ClientName: "t3-code-session-manager", MCPServers: servers}, want: true},
		{name: "sign-in check by name", s: acp.Session{ClientName: "t3-code-provider-auth"}, want: true},
		{name: "no servers and no environment", s: acp.Session{ClientName: "t3-code", MCPServers: json.RawMessage(`[]`)}, want: true},
		{name: "absent servers", s: acp.Session{ClientName: "t3-code"}, want: true},
		{name: "null servers", s: acp.Session{MCPServers: json.RawMessage(`null`)}, want: true},
		{name: "malformed servers", s: acp.Session{MCPServers: json.RawMessage(`"oops"`)}, want: true},
		{name: "a thread's servers", s: acp.Session{ClientName: "t3-code", MCPServers: servers}},
		{name: "a thread without a client name", s: acp.Session{MCPServers: servers}},
		{name: "an endpoint in the environment", s: acp.Session{ClientName: "t3-code", MCPServers: json.RawMessage(`[]`)},
			getenv: envWith(t3.EnvMCPEndpoint)},
		{name: "an authorization in the environment", s: acp.Session{ClientName: "t3-code"},
			getenv: envWith(t3.EnvMCPAuthorization)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := tc.getenv
			if getenv == nil {
				getenv = noACPEnv
			}
			assert.Equal(t, tc.want, acpIsProbe(tc.s, getenv))
		})
	}
}

func TestACPStatusReply(t *testing.T) {
	cwd := t.TempDir()
	saveACPTestRun(t, cwd, "done-20261022-120000-abcd", runs.Outcome{Status: runs.StatusSucceeded,
		ReportSummary: "  Added the provider.\n", Branch: "done"})
	saveACPTestRun(t, cwd, "broke-20261022-120000-abcd", runs.Outcome{Status: runs.StatusFailed, Reason: "task 2 failed"})
	saveRunsTestRun(t, cwd, runs.Descriptor{ID: "crashed-20261022-120000-abcd", Plan: "docs/plans/c.md", Branch: "c"})

	tests := []struct {
		name  string
		runID string
		want  string
	}{
		{name: "no run", want: "No loopai run belongs to this thread yet. To start one, send the plan path, optionally with " +
			"model flags:\n\n    " + acpPromptUsage + "\n\n" + acpModelSwitchHint},
		{name: "a succeeded run", runID: "done-20261022-120000-abcd",
			want: "loopai run done-20261022-120000-abcd succeeded.\n\nAdded the provider.\n\nBranch: done\n\n" + acpModelSwitchHint},
		{name: "a failed run", runID: "broke-20261022-120000-abcd",
			want: "loopai run broke-20261022-120000-abcd failed: task 2 failed.\n\n" + acpModelSwitchHint},
		{name: "a run without an outcome", runID: "crashed-20261022-120000-abcd",
			want: "loopai run crashed-20261022-120000-abcd has no recorded outcome.\n\n" + acpModelSwitchHint},
		{name: "a run that is gone", runID: "gone-20261022-120000-abcd",
			want: "loopai run gone-20261022-120000-abcd has no recorded outcome.\n\n" + acpModelSwitchHint},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := acpStatusReply(cwd, tc.runID)
			assert.Equal(t, tc.want, reply)
			assert.False(t, acpAuthWords.MatchString(reply))
		})
	}
}
