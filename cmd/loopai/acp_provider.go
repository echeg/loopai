package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fatih/color"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/t3"
)

const (
	// acpLookupTimeout bounds the thread-id lookup of one session/new or session/load, whose answer
	// waits for it.
	acpLookupTimeout = 5 * time.Second
	// acpShutdownWait bounds how long --acp waits for its handlers after SIGTERM or the end of stdin.
	// T3 Code sends SIGKILL a second after SIGTERM, so the process ends well before that.
	acpShutdownWait = 250 * time.Millisecond
)

// acpPromptUsage is the message that starts a run in a loopai thread.
const acpPromptUsage = "<plan-file> [--task-model provider[:model[:effort]]] " +
	"[--review-model provider[:model[:effort]]] [--external-reviewers provider[:model[:effort]],...]"

// acpModelSwitchHint closes every status reply: conversation happens with another model.
const acpModelSwitchHint = "Continue the conversation by switching this thread's model to Claude or Codex."

// acpProbeClients are the clientInfo names of T3 Code's background connections to a provider: the
// readiness probe, the session manager, and the sign-in check, which sends initialize alone. None of
// them belongs to a thread.
var acpProbeClients = []string{"t3-code-provider-test", "t3-code-session-manager", "t3-code-provider-auth"}

// lookupACPThreadID asks T3 Code's thread-scoped MCP endpoint which thread cred belongs to. TestMain
// replaces it, so no test reaches a real T3 server.
var lookupACPThreadID = func(ctx context.Context, cred t3.MCPCredential) (string, error) {
	id, err := t3.NewMCPClient(cred, resolveVersion()).CurrentThreadID(ctx)
	if err != nil {
		return "", fmt.Errorf("look up the thread: %w", err)
	}
	return id, nil
}

// acpRequested reports whether the raw arguments ask for --acp. main needs the answer before flag
// parsing, because the version banner is printed first and stdout carries JSON-RPC only.
func acpRequested(args []string) bool {
	return rawFlagRequested(args, "--acp")
}

// validateACPFlags keeps --acp standalone. A thread's message names the plan and its flags, so a plan
// argument or an execution flag on the command line would be silently ignored. --orca is accepted
// because it may come from the environment.
func validateACPFlags(o opts) error {
	if !o.ACP {
		return nil
	}
	if o.PlanFile != "" || len(o.extraArgs) > 0 {
		return errors.New("--acp takes no plan argument; send the plan path as a message in the loopai thread")
	}
	for _, conflict := range []struct {
		flag string
		set  bool
	}{
		{"--detach", o.Detach},
		{"--detached-run", o.detachedRunSet || o.DetachedRun != ""},
		{"--t3-thread", o.t3ThreadSet || o.T3Thread != ""},
		{"--attach, --runs, or --stop", runCommandRequested(o)},
		{"--merge, --pr, or --report", closeoutRequested(o)},
		{"--clear", o.Clear},
		{"--cmux-workspace", o.CmuxWorkspace != ""},
		{"--init", o.Init},
		{"--reset", o.Reset},
		{"--dump-defaults", o.DumpDefaults != ""},
		{"--gen-agents", o.GenAgents},
		{"--serve", o.Serve},
		{"--watch", len(o.Watch) > 0},
		{"--plan", o.PlanDescription != ""},
	} {
		if conflict.set {
			return fmt.Errorf("--acp cannot be combined with %s", conflict.flag)
		}
	}
	if hasExecutionMode(o) {
		return errors.New("--acp cannot be combined with execution flags; " +
			"pass --task-model, --review-model, or --external-reviewers in the thread's message")
	}
	return nil
}

// runACPCommand is --acp: it serves the Agent Client Protocol for T3 Code's acpRegistry driver on
// the process's stdin and stdout until stdin ends or ctx is canceled. stdout carries JSON-RPC only;
// every human-readable line goes to stderr.
func runACPCommand(ctx context.Context) error {
	defer catchSIGPIPE()()
	in, out, restore, err := redirectStdioForACP()
	if err != nil {
		return err
	}
	defer restore()
	return serveACP(ctx, in, out, os.Stderr, os.Getenv)
}

// catchSIGPIPE turns a write to a closed stdout or stderr pipe into an EPIPE error. Both are pipes
// owned by the client, and without a handler Go kills the process on the first such write once the
// client is gone. Notify rather than Ignore keeps the default disposition for child processes, since
// an ignored signal survives exec. The returned function restores the default.
func catchSIGPIPE() func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE)
	return func() { signal.Stop(ch) }
}

// redirectStdioForACP reserves the real stdin and stdout for the protocol and points os.Stdin at the
// null device and os.Stdout and color.Output at stderr, so a stray read or write anywhere in the
// process can neither consume nor corrupt JSON-RPC traffic. restore puts the original streams back.
func redirectStdioForACP() (in, out *os.File, restore func(), err error) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	in, out = os.Stdin, os.Stdout
	prevColor := color.Output
	os.Stdin, os.Stdout, color.Output = devNull, os.Stderr, color.Error
	return in, out, func() {
		os.Stdin, os.Stdout, color.Output = in, out, prevColor
		_ = devNull.Close()
	}, nil
}

// serveACP runs the provider over in and out until in ends or ctx is canceled, whichever comes
// first, then gives its handlers acpShutdownWait to return. Neither ending answers an open prompt or
// touches a run: a stop still in its grace is abandoned. getenv reads the spawn-time T3_ACP_MCP_*
// fallback.
func serveACP(ctx context.Context, in io.Reader, out, stderr io.Writer, getenv func(string) string) error {
	h := &acpHandler{stderr: stderr, getenv: getenv, threads: map[string]string{}, creds: map[string]t3.MCPCredential{},
		stops: newACPStops(stderr)}
	p := acp.NewProvider(in, out, resolveVersion(), h)
	p.SetErrorLog(stderr)
	served := make(chan error, 1)
	go func() { served <- p.Serve() }()
	var serveErr error
	select {
	case serveErr = <-served:
	case <-ctx.Done():
	}
	h.stops.close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), acpShutdownWait)
	defer cancel()
	_ = p.Shutdown(shutdownCtx) // a handler still running at the bound ends with the process
	if serveErr != nil {
		return fmt.Errorf("serve ACP: %w", serveErr)
	}
	return nil
}

// acpHandler serves the provider's sessions. It records the T3 thread of every session that belongs
// to one and routes every prompt to the run it shows.
type acpHandler struct {
	stderr io.Writer
	getenv func(string) string
	stops  *acpStops

	mu      sync.Mutex
	threads map[string]string           // session id -> T3 thread id, "" when unknown
	creds   map[string]t3.MCPCredential // session id -> its MCP credential, in memory only and never logged
}

// NewSession resolves the thread of a session/new.
func (h *acpHandler) NewSession(ctx context.Context, s acp.Session) { h.openSession(ctx, s) }

// LoadSession resolves the thread of a session/load, falling back to the thread the session's own
// binding recorded, so a new process restores it from disk.
func (h *acpHandler) LoadSession(ctx context.Context, s acp.Session) { h.openSession(ctx, s) }

// Prompt routes the prompt to the run it shows, launching or relaunching one when the message asks,
// and holds the turn open while it follows that run. A prompt of a session cancels the session's stop
// still in its grace: a steer or a restart arrives as a cancel followed by a new prompt. A followed run
// whose prompt is canceled gets a stop armed.
func (h *acpHandler) Prompt(ctx context.Context, req acp.PromptRequest, updates *acp.Updates) acp.StopReason {
	h.stops.cancel(req.SessionID)
	threadID := h.thread(req.SessionID)
	t := &acpTurn{ctx: ctx, req: req, updates: updates, threadID: threadID, stderr: h.stderr,
		stopOnCancel: func(runID string) {
			h.stops.arm(acpStopRequest{ctx: ctx, cwd: req.Cwd, threadID: threadID, sessionID: req.SessionID, runID: runID,
				credential: func() (t3.MCPCredential, bool) { return h.credential(req.SessionID) }})
		}}
	return t.route()
}

// Cancel is a no-op: a canceled prompt learns it from its Canceled channel, and a prompt that
// followed a run arms its stop as it ends.
func (h *acpHandler) Cancel(string) {}

// openSession resolves the T3 thread a session belongs to and records it, in memory for the
// session's prompts and on disk in the session's binding. A probe is left alone: it belongs to no
// thread, and it must neither read config nor write into the server's cwd. When the thread id cannot
// be looked up, the session's own binding supplies a thread recorded earlier, and otherwise the
// session id is the binding key. A provider stopping meanwhile records nothing.
func (h *acpHandler) openSession(ctx context.Context, s acp.Session) {
	if acpIsProbe(s, h.getenv) {
		return
	}
	cred, hasCred := t3.MCPCredentialFromServers(s.MCPServers, h.getenv)
	threadID, lookupErr := h.lookupThread(ctx, cred, hasCred)
	if ctx.Err() != nil {
		return
	}
	if threadID == "" {
		if b, ok := loadBinding(s.Cwd, "", s.ID); ok {
			threadID = b.ThreadID
		}
	}
	switch {
	case lookupErr != nil && threadID != "":
		fmt.Fprintf(h.stderr, "warning: acp: %v; using the thread the session recorded\n", lookupErr)
	case lookupErr != nil:
		fmt.Fprintf(h.stderr, "warning: acp: %v; binding the session by its id\n", lookupErr)
	}
	h.mu.Lock()
	h.threads[s.ID] = threadID
	if hasCred {
		h.creds[s.ID] = cred
	} else {
		delete(h.creds, s.ID)
	}
	h.mu.Unlock()
	if err := saveSessionThread(s.Cwd, s.ID, threadID); err != nil {
		fmt.Fprintf(h.stderr, "warning: acp: session binding not recorded: %v\n", err)
	}
}

// lookupThread returns the T3 thread id of a session from T3 Code's MCP endpoint, which cred
// addresses, within acpLookupTimeout. A session without an endpoint and a failed lookup return "" and
// the reason, which never carries the authorization.
func (h *acpHandler) lookupThread(ctx context.Context, cred t3.MCPCredential, ok bool) (string, error) {
	if !ok {
		return "", errors.New("the session has no T3 MCP endpoint")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, acpLookupTimeout)
	defer cancel()
	threadID, err := lookupACPThreadID(lookupCtx, cred)
	if err != nil {
		return "", fmt.Errorf("thread id lookup failed: %w", err)
	}
	return threadID, nil
}

// credential returns the MCP credential the session's latest session/new or session/load carried.
func (h *acpHandler) credential(sessionID string) (t3.MCPCredential, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cred, ok := h.creds[sessionID]
	return cred, ok
}

// thread returns the T3 thread id recorded for a session.
func (h *acpHandler) thread(sessionID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.threads[sessionID]
}

// acpIsProbe reports whether a session/new or session/load comes from one of T3 Code's background
// connections rather than a thread: by its clientInfo name, or by carrying no MCP servers with no
// T3_ACP_MCP_* endpoint in the environment either.
func acpIsProbe(s acp.Session, getenv func(string) string) bool {
	if slices.Contains(acpProbeClients, s.ClientName) {
		return true
	}
	return !acpHasMCPServers(s.MCPServers) && getenv(t3.EnvMCPEndpoint) == "" && getenv(t3.EnvMCPAuthorization) == ""
}

// acpHasMCPServers reports whether an mcpServers parameter lists at least one server. An absent,
// null, or malformed value lists none.
func acpHasMCPServers(raw json.RawMessage) bool {
	var servers []json.RawMessage
	return json.Unmarshal(raw, &servers) == nil && len(servers) > 0
}

// acpStatusReply is the answer to a prompt that starts or follows no run: the last outcome of the
// thread's run when runID names one, else how to start a run, and in both cases how to continue the
// conversation.
func acpStatusReply(cwd, runID string) string {
	if runID == "" {
		return "No loopai run belongs to this thread yet. To start one, send the plan path, optionally with " +
			"model flags:\n\n    " + acpPromptUsage + "\n\n" + acpModelSwitchHint
	}
	outcome, err := runs.LoadOutcome(runs.Dir(cwd, runID))
	if err != nil {
		return fmt.Sprintf("loopai run %s has no recorded outcome.\n\n%s", runID, acpModelSwitchHint)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "loopai run %s %s", runID, outcome.Status)
	if outcome.Reason != "" {
		fmt.Fprintf(&b, ": %s", outcome.Reason)
	}
	b.WriteString(".")
	if summary := strings.TrimSpace(outcome.ReportSummary); summary != "" {
		b.WriteString("\n\n" + summary)
	}
	if outcome.Branch != "" {
		fmt.Fprintf(&b, "\n\nBranch: %s", outcome.Branch)
	}
	b.WriteString("\n\n" + acpModelSwitchHint)
	return b.String()
}
