package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// CodexStreams holds both stderr and stdout from codex command.
type CodexStreams struct {
	Stderr io.Reader
	Stdout io.Reader
}

// CodexRunner abstracts command execution for codex.
// Returns both stderr (streaming progress) and stdout (final response).
type CodexRunner interface {
	Run(ctx context.Context, name string, args ...string) (streams CodexStreams, wait func() error, err error)
}

// execCodexRunner is the default command runner using os/exec for codex.
// codex outputs streaming progress to stderr, final response to stdout.
// when stdin is non-nil, it is connected to the child process's stdin (used to pass
// the prompt via pipe instead of a CLI argument to avoid Windows 8191-char cmd limit).
// stripAnthropicKey scopes ANTHROPIC_API_KEY filtering to first-class --codex runs;
// external codex review in default claude mode keeps the host env intact so custom
// codex wrappers proxying through Anthropic (e.g., scripts/codex-as-claude/codex-as-claude.sh) keep
// authenticating. CLAUDECODE is always stripped regardless of mode to prevent
// nested-session errors when codex is launched from inside a Claude Code session.
type execCodexRunner struct {
	stdin             io.Reader
	stripAnthropicKey bool
}

// childEnv builds the codex child-process env. CLAUDECODE is always stripped to
// prevent nested-session errors. ANTHROPIC_API_KEY is stripped only when the
// caller requested it (first-class --codex mode); default-claude external codex
// review passes the key through so custom Anthropic-proxying wrappers keep working.
func (r *execCodexRunner) childEnv(env []string) []string {
	if r.stripAnthropicKey {
		return filterEnv(env, "ANTHROPIC_API_KEY", "CLAUDECODE")
	}
	return filterEnv(env, "CLAUDECODE")
}

func (r *execCodexRunner) Run(ctx context.Context, name string, args ...string) (CodexStreams, func() error, error) {
	// check context before starting to avoid spawning a process that will be immediately killed
	if err := ctx.Err(); err != nil {
		return CodexStreams{}, nil, fmt.Errorf("context already canceled: %w", err)
	}

	// use exec.Command (not CommandContext) because we handle cancellation ourselves
	// to ensure the entire process group is killed, not just the direct child
	cmd := exec.Command(name, args...) //nolint:noctx // intentional: we handle context cancellation via process group kill

	cmd.Env = r.childEnv(os.Environ())

	// pass prompt via stdin when set (avoids Windows 8191-char command-line limit)
	if r.stdin != nil {
		cmd.Stdin = r.stdin
	}

	// create new process group so we can kill all descendants on cleanup
	setupProcessGroup(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return CodexStreams{}, nil, fmt.Errorf("stderr pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return CodexStreams{}, nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return CodexStreams{}, nil, fmt.Errorf("start command: %w", err)
	}

	// setup process group cleanup with graceful shutdown on context cancellation
	cleanup := newProcessGroupCleanup(cmd, ctx.Done())

	return CodexStreams{Stderr: stderr, Stdout: stdout}, cleanup.Wait, nil
}

// CodexExecutor runs codex CLI commands and filters output. Completed command
// durations prefer native rollout timestamps; the arrival-time fallback is
// approximate because buffered events can arrive during the final drain.
// Concurrent command durations can overlap.
type CodexExecutor struct {
	Command              string                                // command to execute, defaults to "codex"
	Model                string                                // model override; empty means inherit from ~/.codex/config.toml (no -c model= flag emitted)
	ReasoningEffort      string                                // reasoning effort override; empty means inherit from ~/.codex/config.toml
	TimeoutMs            int                                   // stream idle timeout in ms, defaults to 3600000
	Sandbox              string                                // sandbox mode, defaults to "read-only"
	ProjectDoc           string                                // path to project documentation file
	OutputHandler        func(text string)                     // called for each filtered output line in real-time
	CommandTimingHandler func(command string, d time.Duration) // called for each completed exec_command; can be nil
	Debug                bool                                  // enable debug output
	ErrorPatterns        []string                              // patterns to detect in output (e.g., rate limit messages)
	LimitPatterns        []string                              // patterns to detect rate limits (checked before error patterns)
	MultiAgent           bool                                  // enable codex multi_agent feature + reviewer agent registration; set to true on the review-phase codex instance built by processor.New() for first-class --codex mode
	PassClaudeMd         bool                                  // pass project-level CLAUDE.md to codex via project_doc_fallback_filenames (set by processor.New() only when cfg.AppConfig.Executor == ExecutorCodex)
	ForceReadOnly        bool                                  // require the read-only sandbox even when the runtime disables its default sandbox; used by external review so it cannot modify the project
	IdleTimeout          time.Duration                         // kill session after this duration of no output, zero = disabled
	headerEmitted        atomic.Bool                           // tracks first invocation across Run() calls; false until first task/review then suppressed permanently — used to emit codex's resolved model/sandbox/effort once at the top of the run
	runner               CodexRunner                           // for testing, nil uses default
	now                  func() time.Time                      // arrival clock fallback for rollout events without timestamps; nil uses time.Now
}

// CodexReviewerAgentName is the agent name registered with codex when
// features.multi_agent is enabled. shared with pkg/processor so the
// spawn_agent(agent=...) call in review prompts stays in sync with the
// registration here — if either side drifts, codex silently fails to
// resolve the agent and the review phase breaks.
const CodexReviewerAgentName = "reviewer"

// codexReviewerDescription is the description registered for the reviewer
// agent when features.multi_agent is enabled. behavior is driven by the task
// argument, so the description stays generic and stable.
//
// MUST stay ASCII without backslashes, control characters, or non-printable bytes:
// codexConfigOpts.cliArgs serializes this via fmt.Sprintf("...=%q", ...) which
// emits Go string-literal escapes; only the printable ASCII subset round-trips
// safely through TOML basic-string syntax.
const codexReviewerDescription = "general code review specialist; behavior driven by the task argument"

// configOverrides returns the -c key=value arg slice to splice into the codex CLI
// invocation based on the executor's MultiAgent and PassClaudeMd flags. All overrides
// are additive on top of the user's ~/.codex/config.toml.
func (e *CodexExecutor) configOverrides() []string {
	var args []string
	if e.MultiAgent {
		args = append(args,
			"-c", "features.multi_agent=true",
			"-c", fmt.Sprintf("agents.%s.description=%q", CodexReviewerAgentName, codexReviewerDescription),
		)
	}
	if e.PassClaudeMd {
		args = append(args, "-c", `project_doc_fallback_filenames=["CLAUDE.md"]`)
	}
	return args
}

// sandboxMode resolves the effective sandbox. External review must remain read-only
// and fail if Codex cannot initialize that sandbox; silently granting write access
// would let a findings-only reviewer modify the repository.
func (e *CodexExecutor) sandboxMode() string {
	if e.ForceReadOnly {
		return "read-only"
	}
	if e.Sandbox == "" {
		return "read-only"
	}
	return e.Sandbox
}

// codexFilterState tracks header separator count for filtering.
type codexFilterState struct {
	headerCount int             // tracks "--------" separators seen (show content between first two)
	seen        map[string]bool // track all shown lines for deduplication
	firstRun    bool            // when true, whitelist model/sandbox/effort lines from the header block so the user sees codex's resolved config once at the top of the run
}

// Run executes codex CLI with the given prompt and returns filtered output.
// stderr is streamed line-by-line to OutputHandler for progress indication.
// stdout is captured entirely as the final response (returned in Result.Output).
func (e *CodexExecutor) Run(ctx context.Context, prompt string) Result {
	cmd := e.Command
	if cmd == "" {
		cmd = "codex"
	}

	timeoutMs := e.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 3600000
	}

	sandbox := e.sandboxMode()

	args := []string{"exec"}
	args = append(args, e.configOverrides()...)
	// --dangerously-bypass-approvals-and-sandbox is required for unattended first-class
	// --codex runs (which use danger-full-access by default). External codex review in
	// claude mode worked on master without this flag and adding it would silently change
	// approval semantics for default-claude users; gate the flag on MultiAgent which is
	// true only in first-class --codex (set by processor.buildCodexExecutor).
	if sandbox == "danger-full-access" && e.MultiAgent {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	args = append(args, "--sandbox", sandbox)
	// model and reasoning effort are emitted only when explicitly set in loopai config,
	// so the user's ~/.codex/config.toml choice is preserved otherwise (matches the
	// "additive -c overrides" promise documented in CLAUDE.md / llms.txt).
	if e.Model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", e.Model))
	}
	if e.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+e.ReasoningEffort)
	}
	args = append(args, "-c", fmt.Sprintf("stream_idle_timeout_ms=%d", timeoutMs))

	if e.ProjectDoc != "" {
		args = append(args, "-c", fmt.Sprintf("project_doc=%q", e.ProjectDoc))
	}

	// pass prompt via stdin to avoid Windows 8191-char command-line limit;
	// codex reads from stdin when no positional prompt argument is given.
	// MultiAgent signals first-class --codex (set by processor.buildCodexExecutor only;
	// external codex review built by buildExternalCodexExecutor leaves it false), so it
	// also gates ANTHROPIC_API_KEY stripping — default-claude external codex review
	// preserves the host env so wrappers proxying through Anthropic keep working.
	stdinReader := strings.NewReader(prompt)
	runner := e.runner
	if runner == nil {
		runner = &execCodexRunner{stdin: stdinReader, stripAnthropicKey: e.MultiAgent}
	}

	// set up idle timeout: derive a cancellable context that fires when no output
	// is received for IdleTimeout duration. the touch closure resets the timer on
	// each stderr line and on each stdout read; mirrors the ClaudeExecutor pattern.
	execCtx := ctx
	idleTouch := func() {} // no-op by default
	if e.IdleTimeout > 0 {
		var idleCancel context.CancelFunc
		execCtx, idleCancel = context.WithCancel(ctx)
		defer idleCancel()
		timer := time.AfterFunc(e.IdleTimeout, idleCancel)
		defer timer.Stop()
		idleTouch = func() { timer.Reset(e.IdleTimeout) }
	}

	streams, wait, err := runner.Run(execCtx, cmd, args...)
	if err != nil {
		return Result{Error: fmt.Errorf("start codex: %w", err)}
	}

	// process stderr for progress display (header block + bold summaries).
	// sessionIDCh receives the session id once stderr's header block surfaces
	// it; the tail goroutine below uses it to follow the rollout file.
	// firstRun is true exactly once across all Run() calls on this executor —
	// gives shouldDisplay license to leak codex's resolved model/sandbox/effort
	// once at the top of the run instead of repeating the full banner per phase.
	firstRun := e.headerEmitted.CompareAndSwap(false, true)
	sessionIDCh := make(chan string, 1)
	stderrDone := make(chan stderrResult, 1)
	go func() {
		stderrDone <- e.processStderr(execCtx, streams.Stderr, stderrStreamOpts{
			idleTouch:   idleTouch,
			sessionIDCh: sessionIDCh,
			firstRun:    firstRun,
		})
	}()

	tailCancel, tailDone := e.startRolloutTail(execCtx, sessionIDCh, idleTouch)

	// read stdout entirely as final response; wrap with touch-on-read so reads
	// keep the idle timer alive even while stderr is quiet.
	stdoutReader := streams.Stdout
	if e.IdleTimeout > 0 {
		stdoutReader = &touchReader{r: streams.Stdout, touch: idleTouch}
	}
	stdoutContent, stdoutErr := e.readStdout(stdoutReader)

	// wait for stderr processing to complete
	stderrRes := <-stderrDone

	// wait for command completion; once wait() returns the codex process has
	// fully exited and flushed the last assistant message to its rollout file
	waitErr := wait()

	// codex has exited; signal tailer to do its final drain and stop. done
	// after wait() so the tailer keeps following until the rollout file is
	// guaranteed complete and the final assistant line is not dropped.
	tailCancel()
	<-tailDone

	// detect signal in stdout (the actual response)
	signal := detectSignal(stdoutContent)

	// idle timeout: derived context canceled but parent is alive — not an error.
	// mirrors the ClaudeExecutor idle-timeout completion path so callers see uniform behavior.
	if e.IdleTimeout > 0 && execCtx.Err() != nil && ctx.Err() == nil {
		e.logDroppedIdleErrors(stdoutErr, waitErr)
		return e.idleTimeoutResult(stdoutContent, signal, stderrRes)
	}

	finalErr := e.finalError(ctx, stderrRes, stdoutErr, waitErr)

	// only check error/limit patterns when the process failed (non-zero exit or stream error).
	// when codex exits cleanly, pattern matches in output are false positives from findings
	// (e.g., reviewing code that handles rate limits).
	// skip pattern checks on context cancellation — cancellation must propagate as-is.
	if finalErr != nil && ctx.Err() == nil {
		if patternErr := e.checkPatterns(stdoutContent, stderrRes); patternErr != nil {
			return Result{Output: stdoutContent, Signal: signal, Error: patternErr}
		}
	}

	// return stdout content as the result (the actual answer from codex)
	return Result{Output: stdoutContent, Signal: signal, Error: finalErr}
}

// finalError reconciles stderr/stdout/wait errors into the single error returned
// from Run. stderr and stdout errors win over wait errors so callers see the
// root cause rather than the cascade exit code; ctx.Err() short-circuits to
// preserve cancellation semantics; non-zero exit with stderr tail produces a
// readable diagnostic that includes the last few stderr lines.
func (e *CodexExecutor) finalError(ctx context.Context, stderrRes stderrResult, stdoutErr, waitErr error) error {
	switch {
	case stderrRes.err != nil && !errors.Is(stderrRes.err, context.Canceled):
		return stderrRes.err
	case stdoutErr != nil:
		return stdoutErr
	case waitErr != nil:
		if ctx.Err() != nil {
			return fmt.Errorf("context error: %w", ctx.Err())
		}
		if len(stderrRes.lastLines) > 0 {
			return fmt.Errorf("codex exited with error: %w\nstderr: %s",
				waitErr, strings.Join(stderrRes.lastLines, "\n"))
		}
		return fmt.Errorf("codex exited with error: %w", waitErr)
	}
	return nil
}

// touchReader wraps an io.Reader to invoke touch on each successful Read.
// used to keep the idle-timeout timer alive while stdout is being drained.
type touchReader struct {
	r     io.Reader
	touch func()
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.touch != nil {
		t.touch()
	}
	return n, err //nolint:wrapcheck // pass-through reader; preserve EOF and original error semantics
}

// logDroppedIdleErrors surfaces concurrent stream/wait errors that would otherwise
// be discarded by the idle-timeout completion path. operators need this to
// distinguish "agent went silent" from "stream broke" before retrying.
func (e *CodexExecutor) logDroppedIdleErrors(stdoutErr, waitErr error) {
	if stdoutErr != nil {
		log.Printf("codex idle timeout fired with concurrent stdout error: %v", stdoutErr)
	}
	if waitErr != nil {
		log.Printf("codex idle timeout fired with concurrent wait error: %v", waitErr)
	}
}

// idleTimeoutResult builds the Result returned when the idle-timeout timer
// canceled the derived execution context (parent ctx still alive). limit and
// error patterns are still checked across stdout and stderr so a wait-and-retry
// triggered by a real quota diagnostic survives idle-timeout cancellation;
// otherwise IdleTimedOut is set and the caller treats this as a soft kill.
func (e *CodexExecutor) idleTimeoutResult(stdoutContent, signal string, stderr stderrResult) Result {
	if patternErr := e.checkPatterns(stdoutContent, stderr); patternErr != nil {
		return Result{Output: stdoutContent, Signal: signal, Error: patternErr}
	}
	return Result{Output: stdoutContent, Signal: signal, IdleTimedOut: true}
}

// checkPatterns scans stdout AND the stderr matches captured live during streaming
// for limit/error patterns. codex emits OpenAI/ChatGPT plan-quota errors (e.g.,
// "ERROR: You've hit your usage limit") to stderr while stdout is empty on failure;
// processStderr matches each line on the fly so detection is not subject to the
// 5-line / 256-rune tail truncation used for human-readable error context.
//
// Priority is limit-first across both sources before any error match: a real
// stderr quota diagnostic (already filtered through the CLI-error prefix gate
// in processStderr) must not be downgraded to a non-retryable PatternMatchError
// just because partial stdout happens to match a configured ErrorPattern. Within
// each severity class, stdout wins over stderr so an explicit stdout limit/error
// takes precedence when both sources fire.
//
// Order:
//  1. stdout LimitPatterns
//  2. stderr.limitMatch (prefix-gated)
//  3. stdout ErrorPatterns
//  4. stderr.errorMatch (prefix-gated)
//
// returns LimitPatternError or PatternMatchError when a pattern matches; nil otherwise.
func (e *CodexExecutor) checkPatterns(stdoutContent string, stderr stderrResult) error {
	// limit-class first — across both sources
	if pattern := matchPattern(stdoutContent, e.LimitPatterns); pattern != "" {
		return &LimitPatternError{Pattern: pattern, HelpCmd: "codex /status"}
	}
	if stderr.limitMatch != "" {
		return &LimitPatternError{Pattern: stderr.limitMatch, HelpCmd: "codex /status"}
	}

	// error-class second
	if pattern := matchPattern(stdoutContent, e.ErrorPatterns); pattern != "" {
		return &PatternMatchError{Pattern: pattern, HelpCmd: "codex /status"}
	}
	if stderr.errorMatch != "" {
		return &PatternMatchError{Pattern: stderr.errorMatch, HelpCmd: "codex /status"}
	}

	return nil
}

// stderrResult holds processed stderr output and any error from reading.
// limitMatch and errorMatch capture the FIRST limit/error pattern that fires
// during streaming, on the untruncated, un-evicted line — so detection is not
// subject to the lastLines tail truncation (5 lines, 256 runes per line).
type stderrResult struct {
	lastLines  []string // last few lines of stderr for error context
	limitMatch string   // first matched limit pattern seen on stderr (live scan)
	errorMatch string   // first matched error pattern seen on stderr (live scan)
	err        error
}

// stderrStreamOpts bundles the per-invocation streaming inputs for processStderr.
type stderrStreamOpts struct {
	idleTouch   func()        // invoked for every stderr line to reset the idle-timeout timer; pass a no-op when idle timeout is disabled
	sessionIDCh chan<- string // when non-nil, receives the first detected "session id: <uuid>" (non-blocking, buffered channel expected)
	firstRun    bool          // gates the one-time emission of codex's resolved model/sandbox/effort header lines
}

// processStderr reads stderr line-by-line, filters for progress display, and
// scans each line for configured limit/error patterns. shows header block
// (between first two "--------" separators) and bold summaries. captures last
// lines of unfiltered output for error reporting AND records the first
// limit/error pattern hit (untruncated, un-evicted) so callers can rely on it
// regardless of how much chatter follows. see stderrStreamOpts for the
// per-invocation streaming inputs.
func (e *CodexExecutor) processStderr(ctx context.Context, r io.Reader, opts stderrStreamOpts) stderrResult {
	const maxTailLines = 5    // keep last N lines for error context
	const maxLineLength = 256 // truncate long lines to avoid oversized error strings

	state := &codexFilterState{firstRun: opts.firstRun}
	var tail []string
	var limitMatch, errorMatch string
	sessionIDSent := false

	err := readLines(ctx, r, func(line string) {
		if opts.idleTouch != nil {
			opts.idleTouch() // reset idle timer on every stderr line
		}
		// scan untruncated line for patterns first; record only the first hit
		// per category so detection is eviction- and truncation-resistant.
		// restricted to CLI-error-prefixed lines (see scanLineForPatterns).
		e.scanLineForPatterns(line, &limitMatch, &errorMatch)

		// surface session id from header block to caller (once) so the rollout
		// file can be tailed in parallel for assistant-message streaming.
		if !sessionIDSent && opts.sessionIDCh != nil {
			if id := e.extractSessionID(line); id != "" {
				select {
				case opts.sessionIDCh <- id:
				default:
				}
				sessionIDSent = true
			}
		}

		// capture non-empty lines for error context, preserving original formatting
		if strings.TrimSpace(line) != "" {
			stored := line
			if runes := []rune(stored); len(runes) > maxLineLength {
				stored = string(runes[:maxLineLength]) + "..."
			}
			tail = append(tail, stored)
			if len(tail) > maxTailLines {
				copy(tail, tail[1:])
				tail = tail[:maxTailLines]
			}
		}

		if show, filtered := e.shouldDisplay(line, state); show {
			if e.OutputHandler != nil {
				e.OutputHandler(filtered + "\n")
			}
		}
	})

	if err != nil {
		return stderrResult{lastLines: tail, limitMatch: limitMatch, errorMatch: errorMatch, err: fmt.Errorf("read stderr: %w", err)}
	}
	return stderrResult{lastLines: tail, limitMatch: limitMatch, errorMatch: errorMatch}
}

// scanLineForPatterns updates limitMatch / errorMatch with the first matching
// limit/error pattern found in line, gated by isCodexErrorLine so progress
// chatter cannot trigger false positives. Once each match has been recorded
// it sticks for the rest of the run.
func (e *CodexExecutor) scanLineForPatterns(line string, limitMatch, errorMatch *string) {
	if !isCodexErrorLine(line) {
		return
	}
	if *limitMatch == "" {
		if pattern := matchPattern(line, e.LimitPatterns); pattern != "" {
			*limitMatch = pattern
		}
	}
	if *errorMatch == "" {
		if pattern := matchPattern(line, e.ErrorPatterns); pattern != "" {
			*errorMatch = pattern
		}
	}
}

// isCodexErrorLine reports whether a stderr line looks like a CLI error message
// codex reliably prefixes diagnostics. limit/error pattern matching is gated on
// this prefix so progress text on stderr (header banners, bold summaries, model
// chatter that may legitimately mention "rate limit" while reviewing code) does
// not trigger false-positive matches.
func isCodexErrorLine(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" {
		return false
	}
	// case-insensitive prefix match; codex uses "ERROR:" today, others are
	// defensive against possible future variants.
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "error:") ||
		strings.HasPrefix(lower, "fatal:") ||
		strings.HasPrefix(lower, "panic:")
}

// readStdout reads the entire stdout content as the final response.
func (e *CodexExecutor) readStdout(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read stdout: %w", err)
	}
	return string(data), nil
}

// shouldDisplay implements a simple filter for codex stderr output.
// shows: bold reasoning summaries codex emits as live progress; on the very
// first codex invocation across this executor's lifetime (state.firstRun)
// also shows codex's resolved model/sandbox/effort lines from the header
// block so the user sees what codex actually picked from ~/.codex/config.toml.
// per-iteration header repetition (workdir/provider/approval/session id) is
// always suppressed to match ClaudeExecutor's empty-banner UX. session id
// detection in processStderr is independent of display so the rollout tailer
// still works whether the line is forwarded or not.
// also deduplicates lines to avoid non-consecutive repeats.
func (e *CodexExecutor) shouldDisplay(line string, state *codexFilterState) (bool, string) {
	s := strings.TrimSpace(line)
	if s == "" {
		return false, ""
	}

	var show bool
	var filtered string

	switch {
	case strings.HasPrefix(s, "--------"):
		// track separators only so subsequent header lines stay suppressed;
		// never displayed.
		state.headerCount++
	case state.headerCount == 1:
		// inside the header block. on the first run let codex's resolved
		// config (model / sandbox / reasoning effort) leak through so the
		// banner reflects what codex actually picked when loopai did not
		// explicitly override these fields.
		if state.firstRun && e.isHeaderConfigLine(s) {
			show = true
			filtered = s
		}
	case strings.HasPrefix(s, "**"):
		// show bold summaries after header (progress indication)
		show = true
		filtered = e.stripBold(s)
	}

	// deduplicate displayed lines
	if show {
		if state.seen == nil {
			state.seen = make(map[string]bool)
		}
		if state.seen[filtered] {
			return false, "" // skip duplicate
		}
		state.seen[filtered] = true
	}

	return show, filtered
}

// isHeaderConfigLine returns true when line is one of codex's header-block
// lines describing the resolved per-session config that loopai doesn't know
// up front (model picked from ~/.codex/config.toml, sandbox, reasoning effort).
// other header lines (workdir, provider, approval, reasoning summaries,
// session id) are either obvious from context or not useful to the user.
func (e *CodexExecutor) isHeaderConfigLine(s string) bool {
	return strings.HasPrefix(s, "model:") ||
		strings.HasPrefix(s, "sandbox:") ||
		strings.HasPrefix(s, "reasoning effort:")
}

// stripBold removes markdown bold markers (**text**) from text.
func (e *CodexExecutor) stripBold(s string) string {
	// replace **text** with text
	result := s
	for {
		start := strings.Index(result, "**")
		if start == -1 {
			break
		}
		end := strings.Index(result[start+2:], "**")
		if end == -1 {
			break
		}
		// remove both markers
		result = result[:start] + result[start+2:start+2+end] + result[start+2+end+2:]
	}
	return result
}

// sessionIDPattern matches the "session id: <uuid>" line codex emits in its
// startup banner. capture group 1 is the session id (lowercase hex + dashes).
var sessionIDPattern = regexp.MustCompile(`(?i)\bsession id:\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)

// extractSessionID returns the codex session id from a stderr line that
// includes "session id: <uuid>", or "" when the line does not match. used
// by processStderr to surface the id to the rollout-tail goroutine.
func (e *CodexExecutor) extractSessionID(line string) string {
	m := sessionIDPattern.FindStringSubmatch(line)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// startRolloutTail spawns the rollout-tail goroutine and returns a cancel
// function plus a done channel. tail goroutine waits for the session id on
// sessionIDCh, then follows codex's session rollout file until the returned
// cancel is called. caller must invoke tailCancel and wait on tailDone before
// returning so the tailer drains remaining file content and exits cleanly.
// the goroutine is a no-op when both rollout handlers are nil — extracted
// from Run() to keep its cyclomatic complexity in check.
func (e *CodexExecutor) startRolloutTail(parent context.Context, sessionIDCh <-chan string, idleTouch func()) (context.CancelFunc, <-chan struct{}) {
	tailCtx, tailCancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-tailCtx.Done():
			return
		case id := <-sessionIDCh:
			e.tailRolloutFile(tailCtx, id, idleTouch)
		}
	}()
	return tailCancel, done
}

// findRolloutFile resolves the path to codex's session-rollout JSONL file
// for the given session id. codex stores the file under
// ~/.codex/sessions/<year>/<month>/<day>/rollout-<timestamp>-<session-id>.jsonl
// and may take a while to create it after printing the session-id banner,
// especially under load, so we poll up to ~30s. returns "" when the file
// cannot be located.
func (e *CodexExecutor) findRolloutFile(ctx context.Context, sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	pattern := filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+sessionID+".jsonl")

	deadline := time.Now().Add(30 * time.Second)
	for {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			return matches[0]
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// tailRolloutFile follows codex's session rollout JSONL file like `tail -f`,
// parses each event, and emits human-readable progress lines via OutputHandler.
// runs until ctx is canceled. on cancellation, drains any remaining buffered
// lines before returning so late writes (e.g. codex flushing the final
// assistant message just before exit) are not lost.
func (e *CodexExecutor) tailRolloutFile(ctx context.Context, sessionID string, idleTouch func()) {
	if e.OutputHandler == nil && e.CommandTimingHandler == nil {
		return
	}
	path := e.findRolloutFile(ctx, sessionID)
	if path == "" {
		// suppress the diagnostic when the session was canceled — findRolloutFile
		// also returns "" on ctx.Done(), and that is not a failure worth logging.
		if ctx.Err() == nil {
			log.Printf("codex rollout file not found for session %s; assistant output streaming disabled for this session", sessionID)
		}
		return
	}
	now := e.now
	if now == nil {
		now = time.Now
	}
	states := make(map[string]*rolloutTailState)
	knownParents := map[string]bool{sessionID: true}
	discovery := newRolloutDiscovery()
	rootInfo, _ := os.Stat(path)
	e.discoverRolloutStates(path, rootInfo, states, knownParents, discovery)
	if _, ok := states[path]; !ok {
		log.Printf("codex rollout file open failed (%s); assistant output streaming disabled for this session", path)
		return
	}
	defer func() {
		for _, state := range states {
			_ = state.file.Close()
		}
	}()

	for {
		e.discoverRolloutStates(path, rootInfo, states, knownParents, discovery)
		for _, state := range states {
			e.drainRolloutState(state, now, idleTouch, false)
		}
		select {
		case <-ctx.Done():
			// final drain after codex exits — pick up any late-flushed events
			e.discoverRolloutStates(path, rootInfo, states, knownParents, discovery)
			for _, state := range states {
				e.drainRolloutState(state, now, idleTouch, true)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type rolloutDiscovery struct {
	parents map[string]string
}

func newRolloutDiscovery() *rolloutDiscovery {
	return &rolloutDiscovery{parents: make(map[string]string)}
}

func (e *CodexExecutor) discoverRolloutStates(
	rootPath string,
	rootInfo os.FileInfo,
	states map[string]*rolloutTailState,
	knownParents map[string]bool,
	discovery *rolloutDiscovery,
) {
	for {
		added := false
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(rootPath), "rollout-*.jsonl"))
		for _, candidate := range matches {
			if _, exists := states[candidate]; exists {
				continue
			}
			render := candidate == rootPath
			if !render && !e.shouldDiscoverChildRollout(candidate, rootInfo, knownParents, discovery) {
				continue
			}
			state, err := newRolloutTailState(candidate, render)
			if err != nil {
				continue
			}
			states[candidate] = state
			if id := rolloutIDFromPath(candidate); id != "" {
				knownParents[id] = true
			}
			added = true
		}
		if !added {
			return
		}
	}
}

func (e *CodexExecutor) shouldDiscoverChildRollout(
	path string, rootInfo os.FileInfo, knownParents map[string]bool, discovery *rolloutDiscovery,
) bool {
	if e.CommandTimingHandler == nil {
		return false
	}
	if parentID, inspected := discovery.parents[path]; inspected {
		return knownParents[parentID]
	}
	parentID, ready := inspectRolloutParent(path, rootInfo)
	if !ready {
		return false
	}
	discovery.parents[path] = parentID
	return knownParents[parentID]
}

func inspectRolloutParent(path string, rootInfo os.FileInfo) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if rootInfo != nil && info.ModTime().Before(rootInfo.ModTime()) {
		return "", true
	}
	return rolloutParentThreadID(path)
}

type rolloutTailState struct {
	file   *os.File
	acc    []byte
	timing *codexTimingState
	render bool
}

func newRolloutTailState(path string, render bool) (*rolloutTailState, error) {
	f, err := os.Open(path) //nolint:gosec // paths come from codex's own session directory
	if err != nil {
		return nil, fmt.Errorf("open rollout file: %w", err)
	}
	return &rolloutTailState{file: f, timing: newCodexTimingState(), render: render}, nil
}

func (e *CodexExecutor) drainRolloutState(state *rolloutTailState, now func() time.Time, idleTouch func(), final bool) {
	chunk := make([]byte, 4096)
	for {
		n, readErr := state.file.Read(chunk)
		if n > 0 {
			if idleTouch != nil {
				idleTouch()
			}
			state.acc = append(state.acc, chunk[:n]...)
			for {
				i := bytes.IndexByte(state.acc, '\n')
				if i < 0 {
					break
				}
				e.processRolloutLine(state.acc[:i], state.timing, now, state.render)
				state.acc = state.acc[i+1:]
			}
		}
		if readErr == io.EOF || n == 0 {
			break
		}
		if readErr != nil {
			break
		}
	}
	if final {
		if line := bytes.TrimSpace(state.acc); len(line) > 0 {
			e.processRolloutLine(line, state.timing, now, state.render)
		}
		state.acc = nil
	}
}

func rolloutParentThreadID(path string) (string, bool) {
	f, err := os.Open(path) //nolint:gosec // path is a candidate in codex's session directory
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return "", false
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Source json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &meta) != nil {
		return "", false
	}
	if meta.Type != "session_meta" {
		return "", true
	}
	var source struct {
		Subagent struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(meta.Payload.Source, &source) != nil {
		return "", true
	}
	return source.Subagent.ThreadSpawn.ParentThreadID, true
}

func rolloutIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(base) < 36 {
		return ""
	}
	return base[len(base)-36:]
}

// rolloutEvent is the outer wrapper for each line in codex's session rollout
// JSONL file. only `type` and `payload` are needed; we re-parse payload based
// on the type.
type rolloutEvent struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// rolloutPayload covers the response_item payload shape we render: assistant
// messages (payload.type=message, role=assistant). function_call records and
// reasoning records are dropped by formatRolloutEvent before any of those
// fields would be read, so the struct only carries the subset we actually
// consume.
type rolloutPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type codexCommandStart struct {
	command   string
	eventTime time.Time
	arrival   time.Time
}

type codexPendingCommand struct {
	start         codexCommandStart
	sessionID     string
	requiresProof bool
	yieldAfter    time.Duration
}

type codexTimingState struct {
	starts        map[string][]codexPendingCommand
	sessions      map[string]codexPendingCommand
	continuations map[string][]codexPendingCommand
	cells         map[string][]codexPendingCommand
	waits         map[string][]codexPendingCommand
	unproven      []codexPendingCommand
}

func newCodexTimingState() *codexTimingState {
	return &codexTimingState{
		starts:        make(map[string][]codexPendingCommand),
		sessions:      make(map[string]codexPendingCommand),
		continuations: make(map[string][]codexPendingCommand),
		cells:         make(map[string][]codexPendingCommand),
		waits:         make(map[string][]codexPendingCommand),
	}
}

var (
	customExecYieldKey       = regexp.MustCompile("(?:\\\"yield-time_ms\\\"|'yield-time_ms'|\\byield_time_ms)\\s*:")
	customExecCommandPattern = regexp.MustCompile("(?s)tools\\.exec_command\\s*\\(\\s*\\{.*?(?:\"cmd\"|'cmd'|\\bcmd)\\s*:\\s*(\"(?:\\\\.|[^\"\\\\])*\"|'(?:\\\\.|[^'\\\\])*'|`(?:\\\\.|[^`\\\\])*`)")
	customExecYieldPattern   = regexp.MustCompile(`(?s)(?:"yield_time_ms"|'yield-time_ms'|\byield_time_ms)\s*:\s*(\d+)`)
	customWriteStdinPattern  = regexp.MustCompile(`(?s)tools\.write_stdin\s*\(\s*\{.*?(?:"session_id"|'session_id'|\bsession_id)\s*:\s*(?:"([^"]+)"|'([^']+)'|(\d+))`)
	outputSessionIDPattern   = regexp.MustCompile(`(?i)(?:SESSION_ID\s*=|session(?:_|\s+)id["']?\s*[:=]\s*|Process running with session ID\s+)(\d+)`)
	outputCellIDPattern      = regexp.MustCompile(`(?i)(?:Script running with cell ID\s*|cell_id["']?\s*[:=]\s*)(\d+)`)
	exitCodePattern          = regexp.MustCompile(`(?i)(?:EXIT_CODE\s*=|exit_code["']?\s*:\s*|Process exited with code\s+)(-?\d+)`)
)

func parseRolloutRecord(line []byte) (rolloutEvent, rolloutPayload, bool) {
	var ev rolloutEvent
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "response_item" {
		return rolloutEvent{}, rolloutPayload{}, false
	}
	var payload rolloutPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return rolloutEvent{}, rolloutPayload{}, false
	}
	return ev, payload, true
}

func (e *CodexExecutor) processRolloutLine(line []byte, state *codexTimingState, now func() time.Time, render bool) {
	ev, payload, ok := parseRolloutRecord(line)
	if !ok {
		return
	}
	if e.CommandTimingHandler != nil {
		e.trackRolloutCommandTiming(ev, payload, state, now)
	}
	if render && e.OutputHandler != nil {
		if msg := formatParsedRolloutEvent(payload); msg != "" {
			e.OutputHandler(msg)
		}
	}
}

// trackRolloutCommandTiming pairs both legacy exec_command records and current
// custom exec-tool records with their eventual process completion. Yielded
// sessions remain pending across write_stdin/wait continuations.
func (e *CodexExecutor) trackRolloutCommandTiming(ev rolloutEvent, payload rolloutPayload, state *codexTimingState, now func() time.Time) {
	eventTime, _ := time.Parse(time.RFC3339Nano, ev.Timestamp)
	switch payload.Type {
	case "function_call":
		e.trackFunctionCall(payload, eventTime, state, now)
	case "custom_tool_call":
		e.trackCustomToolCall(payload, eventTime, state, now)
	case "function_call_output", "custom_tool_call_output":
		e.trackToolOutput(payload, eventTime, state, now)
	}
}

func (e *CodexExecutor) trackFunctionCall(payload rolloutPayload, eventTime time.Time, state *codexTimingState, now func() time.Time) {
	if payload.CallID == "" {
		return
	}
	switch payload.Name {
	case "exec_command":
		var args struct {
			Cmd string `json:"cmd"`
		}
		if json.Unmarshal([]byte(payload.Arguments), &args) == nil && args.Cmd != "" {
			state.starts[payload.CallID] = []codexPendingCommand{{
				start: codexCommandStart{command: args.Cmd, eventTime: eventTime, arrival: now()},
			}}
		}
	case "write_stdin":
		if sessionID := sessionIDFromArguments(payload.Arguments); sessionID != "" {
			if pending, ok := state.sessions[sessionID]; ok {
				pending.requiresProof = true
				state.continuations[payload.CallID] = []codexPendingCommand{pending}
			}
		}
	case "wait":
		var args struct {
			CellID string `json:"cell_id"`
		}
		if json.Unmarshal([]byte(payload.Arguments), &args) == nil {
			if pending, ok := state.cells[args.CellID]; ok {
				delete(state.cells, args.CellID)
				state.waits[payload.CallID] = pending
			}
		}
	}
}

func (e *CodexExecutor) trackCustomToolCall(payload rolloutPayload, eventTime time.Time, state *codexTimingState, now func() time.Time) {
	if payload.Name != "exec" || payload.CallID == "" {
		return
	}
	if allMatches := customExecCommandPattern.FindAllStringSubmatchIndex(payload.Input, -1); len(allMatches) > 0 {
		arrival := now()
		pending := make([]codexPendingCommand, 0, len(allMatches))
		for index, matches := range allMatches {
			literal := payload.Input[matches[2]:matches[3]]
			if command, ok := decodeJSStringLiteral(literal); ok && command != "" {
				end := len(payload.Input)
				if index+1 < len(allMatches) {
					end = allMatches[index+1][0]
				}
				pending = append(pending, codexPendingCommand{
					start:         codexCommandStart{command: command, eventTime: eventTime, arrival: arrival},
					requiresProof: true,
					yieldAfter:    customExecYieldAfter(payload.Input[matches[0]:end]),
				})
			}
		}
		if len(pending) > 0 {
			state.starts[payload.CallID] = pending
		}
		return
	}
	allMatches := customWriteStdinPattern.FindAllStringSubmatch(payload.Input, -1)
	pending := make([]codexPendingCommand, 0, len(allMatches))
	for _, matches := range allMatches {
		sessionID := firstNonEmpty(matches[1:]...)
		if command, ok := state.sessions[sessionID]; ok {
			pending = append(pending, command)
			continue
		}
		if len(state.unproven) > 0 {
			command := state.unproven[0]
			state.unproven = state.unproven[1:]
			command.sessionID = sessionID
			command.requiresProof = true
			state.sessions[sessionID] = command
			pending = append(pending, command)
		}
	}
	if len(pending) > 0 {
		state.continuations[payload.CallID] = pending
	}
}

func (e *CodexExecutor) trackToolOutput(payload rolloutPayload, eventTime time.Time, state *codexTimingState, now func() time.Time) {
	outputs := rolloutOutputTexts(payload.Output)
	if pending, ok := state.starts[payload.CallID]; ok {
		delete(state.starts, payload.CallID)
		e.resolvePendingOutputs(pending, outputs, eventTime, state, now)
		return
	}
	if pending, ok := state.continuations[payload.CallID]; ok {
		delete(state.continuations, payload.CallID)
		e.resolvePendingOutputs(pending, outputs, eventTime, state, now)
		return
	}
	if pending, ok := state.waits[payload.CallID]; ok {
		delete(state.waits, payload.CallID)
		e.resolvePendingOutputs(pending, outputs, eventTime, state, now)
	}
}

type codexCommandResult struct {
	index     int
	sessionID string
	cellID    string
	hasExit   bool
}

func (e *CodexExecutor) resolvePendingOutputs(pending []codexPendingCommand, outputs []string, eventTime time.Time, state *codexTimingState, now func() time.Time) {
	results := parseCodexCommandResults(outputs, len(pending))
	arrival := now()
	for index, command := range pending {
		result, hasResult := results[index]
		if !command.requiresProof && !hasResult {
			e.emitCommandTiming(command.start, eventTime, arrival)
			continue
		}
		if !hasResult {
			if commandCompletedBeforeYield(command, eventTime) {
				e.emitCommandTiming(command.start, eventTime, arrival)
			} else if command.sessionID == "" {
				state.unproven = append(state.unproven, command)
			}
			continue
		}
		if result.hasExit {
			if command.sessionID != "" {
				delete(state.sessions, command.sessionID)
			}
			e.emitCommandTiming(command.start, eventTime, arrival)
			continue
		}
		if result.sessionID != "" {
			if command.sessionID != "" && command.sessionID != result.sessionID {
				delete(state.sessions, command.sessionID)
			}
			command.sessionID = result.sessionID
			command.requiresProof = true
			state.sessions[result.sessionID] = command
			continue
		}
		if result.cellID != "" {
			state.cells[result.cellID] = append(state.cells[result.cellID], command)
			continue
		}
	}
}

func customExecYieldAfter(call string) time.Duration {
	const defaultYield = 10 * time.Second
	matches := customExecYieldPattern.FindStringSubmatch(call)
	if len(matches) < 2 {
		if customExecYieldKey.MatchString(call) {
			return 0
		}
		return defaultYield
	}
	milliseconds, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil || milliseconds <= 0 {
		return 0
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func commandCompletedBeforeYield(command codexPendingCommand, eventTime time.Time) bool {
	const timestampMargin = 100 * time.Millisecond
	if command.yieldAfter <= timestampMargin || command.start.eventTime.IsZero() || eventTime.IsZero() || eventTime.Before(command.start.eventTime) {
		return false
	}
	return eventTime.Sub(command.start.eventTime)+timestampMargin < command.yieldAfter
}

func (e *CodexExecutor) emitCommandTiming(start codexCommandStart, eventTime, arrival time.Time) {
	duration := arrival.Sub(start.arrival)
	if !start.eventTime.IsZero() && !eventTime.IsZero() && !eventTime.Before(start.eventTime) {
		duration = eventTime.Sub(start.eventTime)
	}
	if duration < 0 {
		duration = 0
	}
	e.CommandTimingHandler(start.command, duration)
}

func rolloutOutputTexts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []string{text}
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	result := make([]string, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, block.Text)
	}
	return result
}

func parseCodexCommandResults(outputs []string, pendingCount int) map[int]codexCommandResult {
	results := make(map[int]codexCommandResult)
	next := 0
	for _, output := range outputs {
		result, ok := parseStructuredCommandResult(output)
		if !ok {
			continue
		}
		index := result.index - 1
		if result.index == 0 {
			for next < pendingCount {
				if _, exists := results[next]; !exists {
					break
				}
				next++
			}
			index = next
		}
		if index >= 0 && index < pendingCount {
			results[index] = result
			next = index + 1
		}
	}
	if pendingCount == 1 {
		if _, ok := results[0]; !ok {
			if result, found := parseTextCommandResult(strings.Join(outputs, "\n")); found {
				results[0] = result
			}
		}
	}
	return results
}

func parseStructuredCommandResult(output string) (codexCommandResult, bool) {
	var raw struct {
		Index     int             `json:"index"`
		SessionID json.RawMessage `json:"session_id"`
		CellID    json.RawMessage `json:"cell_id"`
		ExitCode  json.RawMessage `json:"exit_code"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &raw) != nil {
		return codexCommandResult{}, false
	}
	result := codexCommandResult{index: raw.Index}
	result.sessionID = rawIDString(raw.SessionID)
	result.cellID = rawIDString(raw.CellID)
	var exitCode *int
	result.hasExit = json.Unmarshal(raw.ExitCode, &exitCode) == nil && exitCode != nil
	return result, result.sessionID != "" || result.cellID != "" || result.hasExit
}

func parseTextCommandResult(output string) (codexCommandResult, bool) {
	result := codexCommandResult{
		sessionID: extractPatternValue(outputSessionIDPattern, output),
		cellID:    extractPatternValue(outputCellIDPattern, output),
	}
	_, result.hasExit = parseExitCode(output)
	return result, result.sessionID != "" || result.cellID != "" || result.hasExit
}

func rawIDString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return strconv.FormatInt(number, 10)
	}
	return ""
}

func decodeJSStringLiteral(literal string) (string, bool) {
	if len(literal) < 2 {
		return "", false
	}
	delimiter := literal[0]
	if literal[len(literal)-1] != delimiter || (delimiter != '"' && delimiter != '\'' && delimiter != '`') {
		return "", false
	}
	if delimiter == '"' {
		var value string
		if json.Unmarshal([]byte(literal), &value) == nil {
			return value, true
		}
	}
	content := literal[1 : len(literal)-1]
	if delimiter == '`' && containsTemplateInterpolation(content) {
		return "", false
	}
	var result strings.Builder
	index := 0
	for index < len(content) {
		if content[index] != '\\' {
			result.WriteByte(content[index])
			index++
			continue
		}
		if !writeJSStringEscape(&result, content, &index) {
			return "", false
		}
		index++
	}
	return result.String(), true
}

func writeJSStringEscape(result *strings.Builder, content string, index *int) bool {
	(*index)++
	if *index >= len(content) {
		return false
	}
	switch content[*index] {
	case 'n':
		result.WriteByte('\n')
	case 'r':
		result.WriteByte('\r')
	case 't':
		result.WriteByte('\t')
	case 'b':
		result.WriteByte('\b')
	case 'f':
		result.WriteByte('\f')
	case 'v':
		result.WriteByte('\v')
	case '\n':
		// JavaScript line continuation contributes no character.
	case '\r':
		if *index+1 < len(content) && content[*index+1] == '\n' {
			(*index)++
		}
	default:
		result.WriteByte(content[*index])
	}
	return true
}

func containsTemplateInterpolation(content string) bool {
	for index := 0; index+1 < len(content); index++ {
		if content[index] == '\\' {
			index++
			continue
		}
		if content[index] == '$' && content[index+1] == '{' {
			return true
		}
	}
	return false
}

func sessionIDFromArguments(arguments string) string {
	var args struct {
		SessionID json.RawMessage `json:"session_id"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil || len(args.SessionID) == 0 {
		return ""
	}
	return rawIDString(args.SessionID)
}

func extractPatternValue(pattern *regexp.Regexp, value string) string {
	matches := pattern.FindStringSubmatch(value)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func parseExitCode(output string) (int, bool) {
	value := extractPatternValue(exitCodePattern, output)
	if value == "" {
		return 0, false
	}
	code, err := strconv.Atoi(value)
	return code, err == nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// formatRolloutEvent turns one JSONL rollout line into a display string for
// OutputHandler, or "" when the event has no user-visible substance. only
// assistant message text (the model's actual reply, the codex equivalent of
// claude's stream-json text blocks) is forwarded.
//
// reasoning records are skipped because their summaries are already streamed
// live from stderr. all function_call records (exec_command for git/grep/file
// reads, spawn_agent for parallel reviewer dispatch) and their outputs are
// skipped because they are tool-machinery noise — the assistant message text
// already announces what the model is doing narratively (e.g. "I'll launch
// the five review agents together"). showing both yields redundant chatter.
func (e *CodexExecutor) formatRolloutEvent(line []byte) string {
	_, payload, ok := parseRolloutRecord(line)
	if !ok {
		return ""
	}
	return formatParsedRolloutEvent(payload)
}

func formatParsedRolloutEvent(payload rolloutPayload) string {
	if payload.Type != "message" || payload.Role != "assistant" {
		return ""
	}
	var sb strings.Builder
	for _, c := range payload.Content {
		if c.Type != "output_text" || c.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(c.Text)
	}
	return sb.String()
}
