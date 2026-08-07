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
	"slices"
	"strconv"
	"strings"
	"sync"
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
	CommandTimingHandler func(command string, d time.Duration) // called for supported completed exec/exec_command records, including continuations and child rollouts; can be nil
	Debug                bool                                  // enable debug output
	ErrorPatterns        []string                              // patterns to detect in output (e.g., rate limit messages)
	LimitPatterns        []string                              // patterns to detect rate limits (checked before error patterns)
	MultiAgent           bool                                  // enable codex multi_agent feature + reviewer agent registration; set to true on the review-phase codex instance built by processor.New() for first-class --codex mode
	PassClaudeMd         bool                                  // pass project-level CLAUDE.md to codex via project_doc_fallback_filenames (set by processor.New() only when cfg.AppConfig.Executor == ExecutorCodex)
	ForceReadOnly        bool                                  // require the read-only sandbox even when the runtime disables its default sandbox; used by external review so it cannot modify the project
	IdleTimeout          time.Duration                         // kill session after this duration of no output, zero = disabled
	headerEmitted        atomic.Bool                           // tracks first invocation across Run() calls; false until first task/review then suppressed permanently — used to emit codex's resolved model/sandbox/effort once at the top of the run
	callbackMu           sync.Mutex                            // serializes output and timing handlers; runner loggers require serialized calls
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
			e.emitOutput(filtered + "\n")
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
// $CODEX_HOME/sessions/<year>/<month>/<day>/rollout-<timestamp>-<session-id>.jsonl,
// falling back to ~/.codex/sessions when CODEX_HOME is unset,
// and may take a while to create it after printing the session-id banner,
// especially under load, so we poll up to ~30s. returns "" when the file
// cannot be located.
func (e *CodexExecutor) findRolloutFile(ctx context.Context, sessionID string) string {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		root = filepath.Join(home, ".codex")
	}
	pattern := filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+sessionID+".jsonl")

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
	e.discoverRolloutStates(path, states, knownParents, discovery)
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
		e.discoverRolloutStates(path, states, knownParents, discovery)
		for _, state := range states {
			e.drainRolloutState(state, now, idleTouch, false)
		}
		select {
		case <-ctx.Done():
			// final drain after codex exits — pick up any late-flushed events
			e.discoverRolloutStates(path, states, knownParents, discovery)
			for _, state := range states {
				e.drainRolloutState(state, now, idleTouch, true)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type rolloutDiscovery struct {
	parents     map[string]string
	incomplete  map[string]rolloutFileStamp
	directories map[string]rolloutDirectoryCache
}

func newRolloutDiscovery() *rolloutDiscovery {
	return &rolloutDiscovery{
		parents:     make(map[string]string),
		incomplete:  make(map[string]rolloutFileStamp),
		directories: make(map[string]rolloutDirectoryCache),
	}
}

type rolloutFileStamp struct {
	size    int64
	modTime time.Time
}

type rolloutDirectoryCache struct {
	stamp rolloutFileStamp
	paths []string
}

func (e *CodexExecutor) discoverRolloutStates(
	rootPath string,
	states map[string]*rolloutTailState,
	knownParents map[string]bool,
	discovery *rolloutDiscovery,
) {
	for {
		added := false
		candidates := []string{rootPath}
		if e.CommandTimingHandler != nil {
			dirs := rolloutDiscoveryDirs(rootPath)
			candidates = make([]string, 0, len(dirs))
			for _, dir := range dirs {
				candidates = append(candidates, discovery.paths(dir)...)
			}
		}
		for _, candidate := range candidates {
			if _, exists := states[candidate]; exists {
				continue
			}
			render := candidate == rootPath
			if !render && !e.shouldDiscoverChildRollout(candidate, knownParents, discovery) {
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

func (d *rolloutDiscovery) paths(dir string) []string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		delete(d.directories, dir)
		return nil
	}
	stamp := rolloutFileStamp{size: info.Size(), modTime: info.ModTime()}
	if cached, ok := d.directories[dir]; ok && cached.stamp == stamp {
		return cached.paths
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "rollout-*.jsonl"))
	d.directories[dir] = rolloutDirectoryCache{stamp: stamp, paths: matches}
	return matches
}

// rolloutDiscoveryDirs covers the root session's day and the following day.
// Codex stores rollouts under local YYYY/MM/DD directories, so a session that
// crosses midnight can place newly spawned child agents beside neither root.
func rolloutDiscoveryDirs(rootPath string) []string {
	rootDir := filepath.Dir(rootPath)
	dayPath := filepath.Join(filepath.Base(filepath.Dir(filepath.Dir(rootDir))), filepath.Base(filepath.Dir(rootDir)), filepath.Base(rootDir))
	day, err := time.Parse("2006/01/02", filepath.ToSlash(dayPath))
	if err != nil {
		return []string{rootDir}
	}
	sessionsDir := filepath.Dir(filepath.Dir(filepath.Dir(rootDir)))
	next := day.AddDate(0, 0, 1)
	return []string{rootDir, filepath.Join(sessionsDir, next.Format("2006"), next.Format("01"), next.Format("02"))}
}

func (e *CodexExecutor) shouldDiscoverChildRollout(
	path string, knownParents map[string]bool, discovery *rolloutDiscovery,
) bool {
	if e.CommandTimingHandler == nil {
		return false
	}
	if parentID, inspected := discovery.parents[path]; inspected {
		return knownParents[parentID]
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	stamp := rolloutFileStamp{size: info.Size(), modTime: info.ModTime()}
	if previous, inspected := discovery.incomplete[path]; inspected && previous == stamp {
		return false
	}
	parentID, ready := rolloutParentThreadID(path)
	if !ready {
		discovery.incomplete[path] = stamp
		return false
	}
	delete(discovery.incomplete, path)
	discovery.parents[path] = parentID
	return knownParents[parentID]
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
	const maxMetadataLineSize = 1 << 20

	f, err := os.Open(path) //nolint:gosec // path is a candidate in codex's session directory
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.LimitReader(f, maxMetadataLineSize+1)).ReadBytes('\n')
	if len(line) > maxMetadataLineSize {
		return "", false
	}
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
// reasoning records are dropped by formatParsedRolloutEvent before any of those
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
	start             codexCommandStart
	sessionID         string
	requiresProof     bool
	structuredProof   bool
	sessionProofBlock int
	yieldAfter        time.Duration
	proofEventTime    time.Time
	proofArrival      time.Time
	unprovenEventTime time.Time
	unprovenArrival   time.Time
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
	customExecYieldKey       = regexp.MustCompile(`(?:"yield_time_ms"|'yield_time_ms'|\byield_time_ms)\s*:`)
	customExecYieldPattern   = regexp.MustCompile(`(?s)(?:"yield_time_ms"|'yield_time_ms'|\byield_time_ms)\s*:\s*(\d+)`)
	customDirectAwaitPattern = regexp.MustCompile(`(?s)^\s*(?:(?://[^\n]*\n)\s*)*(?:const|let|var)(?:\s+[A-Za-z_$][A-Za-z0-9_$]*|\s*\[[^\]\n]+\]|\s*\{[^}\n]+\})\s*=\s*await\s*$`)
	customAssignmentPattern  = regexp.MustCompile(`(?s)^\s*(?:(?://[^\n]*\n)\s*)*(?:const|let|var)\s+(?:[A-Za-z_$][A-Za-z0-9_$]*|\[[^\]\n]+\]|\{[^}\n]+\})\s*=\s*$`)
	customPromiseAllPattern  = regexp.MustCompile(`await\s+Promise\.all\s*\(\s*\[`)
	customAnyPromiseAll      = regexp.MustCompile(`await\s+Promise\.all\s*\(`)
	customMapPattern         = regexp.MustCompile(`\b([A-Za-z_$][A-Za-z0-9_$]*)\.map\s*\(`)
	customExecCallPattern    = regexp.MustCompile(`\btools\.exec_command\s*\(`)
	customWriteCallPattern   = regexp.MustCompile(`\btools\.write_stdin\s*\(`)
	customTextCallPattern    = regexp.MustCompile(`\btext\s*\(`)
	customRawOutput          = regexp.MustCompile(`\btext\s*\(\s*[A-Za-z_$][A-Za-z0-9_$]*\.output\s*\)`)
	customDestructuredRest   = regexp.MustCompile(`(?s)(?:const|let|var)\s*\{[^}]*\.\.\.\s*([A-Za-z_$][A-Za-z0-9_$]*)[^}]*\}\s*=\s*await\s+tools\.exec_command`)
	customResultVariable     = regexp.MustCompile(`(?s)^\s*(?:(?://[^\n]*\n)\s*)*(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*await\s*$`)
	customDirectResult       = regexp.MustCompile(`(?s)(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*await\s+tools\.(?:exec_command|write_stdin)\s*\(`)
	customPromiseResult      = regexp.MustCompile(`(?s)(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*await\s+Promise\.all\s*\(`)
	customJSONStringify      = regexp.MustCompile(`\bJSON\.stringify\s*\(`)
	customForOf              = regexp.MustCompile(`(?s)for\s*\(\s*(?:const|let|var)\s+(.+?)\s+of\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\)`)
	customIdentifier         = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)
	outputSessionIDPattern   = regexp.MustCompile(`(?im)^(?:SESSION_ID\s*=|session(?:_|\s+)id["']?\s*[:=]\s*|Process running with session ID\s+)(\d+)\s*$`)
	outputSessionProofBlock  = regexp.MustCompile(`^SESSION_ID=(\d+)$`)
	outputCellIDPattern      = regexp.MustCompile(`(?im)^(?:Script running with cell ID\s*|cell_id["']?\s*[:=]\s*)(\d+)\s*$`)
	exitCodePattern          = regexp.MustCompile(`(?im)^(?:EXIT_CODE\s*=|exit_code["']?\s*:\s*|Process exited with code\s+)(-?\d+)\s*$`)
)

// custom exec records contain JavaScript orchestration rather than structured
// nested command events. The parser below deliberately recognizes only known,
// statically attributable shapes. It is best-effort and omits unfamiliar or
// ambiguous programs instead of claiming timings that the rollout cannot prove.

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
			e.emitOutput(msg)
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
				pending.proofEventTime = eventTime
				pending.proofArrival = now()
				pending.yieldAfter = functionCallYieldAfter(payload.Arguments)
				state.continuations[payload.CallID] = []codexPendingCommand{pending}
			}
		}
	case "wait":
		if cellID := cellIDFromArguments(payload.Arguments); cellID != "" {
			if pending, ok := state.cells[cellID]; ok {
				delete(state.cells, cellID)
				for index := range pending {
					pending[index].structuredProof = true
				}
				state.waits[payload.CallID] = pending
			}
		}
	}
}

func (e *CodexExecutor) trackCustomToolCall(payload rolloutPayload, eventTime time.Time, state *codexTimingState, now func() time.Time) {
	if payload.Name != "exec" || payload.CallID == "" {
		return
	}
	pending, includesStart := customPendingCommands(payload.Input, eventTime, state, now)
	if len(pending) == 0 {
		return
	}
	if includesStart {
		state.starts[payload.CallID] = pending
		return
	}
	state.continuations[payload.CallID] = pending
}

type customContinuationCall struct {
	start      int
	sessionID  string
	yieldAfter time.Duration
}

type customPendingCall struct {
	start        int
	pending      codexPendingCommand
	continuation customContinuationCall
	isStart      bool
}

func customPendingCommands(
	input string,
	eventTime time.Time,
	state *codexTimingState,
	now func() time.Time,
) ([]codexPendingCommand, bool) {
	arrival := now()
	calls := customStaticExecPendingCalls(input, eventTime, arrival)
	calls = append(calls, customMappedExecPendingCalls(input, eventTime, arrival)...)
	continuations := append(customMappedWriteStdinCalls(input), customStaticWriteStdinCalls(input)...)
	for _, continuation := range continuations {
		calls = append(calls, customPendingCall{start: continuation.start, continuation: continuation})
	}
	if len(calls) == 0 {
		return nil, false
	}
	slices.SortStableFunc(calls, func(a, b customPendingCall) int { return a.start - b.start })

	pending := make([]codexPendingCommand, len(calls))
	unknownIndexes := make([]int, 0, len(calls))
	tracked := 0
	includesStart := false
	structuredProof := customToolEmitsStructuredResults(input)
	for index, call := range calls {
		if call.isStart {
			pending[index] = call.pending
			tracked++
			includesStart = true
			continue
		}
		sessionID := call.continuation.sessionID
		if command, ok := state.sessions[sessionID]; ok {
			command.structuredProof = structuredProof
			command.proofEventTime = eventTime
			command.proofArrival = arrival
			command.yieldAfter = call.continuation.yieldAfter
			pending[index] = command
			tracked++
			continue
		}
		unknownIndexes = append(unknownIndexes, index)
	}
	if len(unknownIndexes) == 1 && len(state.unproven) == 1 {
		if !unprovenAssociationIsFresh(state.unproven[0], eventTime, now()) {
			state.unproven = nil
		} else {
			command := state.unproven[0]
			state.unproven = nil
			unknownIndex := unknownIndexes[0]
			sessionID := calls[unknownIndex].continuation.sessionID
			command.sessionID = sessionID
			command.requiresProof = true
			command.structuredProof = structuredProof
			command.proofEventTime = eventTime
			command.proofArrival = arrival
			command.yieldAfter = calls[unknownIndex].continuation.yieldAfter
			state.sessions[sessionID] = command
			pending[unknownIndex] = command
			tracked++
		}
	} else if len(unknownIndexes) > 0 && len(state.unproven) > 0 {
		// Multiple unresolved commands or unknown sessions have no stable
		// association. Drop them instead of guessing by call order.
		state.unproven = nil
	}
	if tracked == 0 {
		return nil, false
	}
	return pending, includesStart
}

func customMappedWriteStdinCalls(input string) []customContinuationCall {
	calls := make([]customContinuationCall, 0)
	for _, match := range customMapPattern.FindAllStringSubmatchIndex(input, -1) {
		if len(match) < 4 || !customPositionInAwaitedPromiseAll(input, match[0]) {
			continue
		}
		mapEnd, ok := customDelimitedEnd(input, match[1]-1)
		if !ok {
			continue
		}
		expression, call, params, ok := customMapWriteStdinExpression(input[match[1]:mapEnd])
		if !ok {
			continue
		}
		source := input[match[2]:match[3]]
		items, ok := customStaticArrayValues(input, source, match[0])
		if !ok {
			continue
		}
		selector, ok := customMapValueSelector(params, expression)
		if !ok {
			continue
		}
		yieldAfter := customWriteStdinYieldAfter(call)
		for _, item := range items {
			value, selected := selector(item)
			if !selected {
				continue
			}
			sessionID, decoded := customStaticSessionID(value)
			if !decoded {
				continue
			}
			calls = append(calls, customContinuationCall{start: match[0], sessionID: sessionID, yieldAfter: yieldAfter})
		}
	}
	return calls
}

type customExecCall struct {
	start   int
	command string
	call    string
}

func customStaticExecCalls(input string) []customExecCall {
	calls := make([]customExecCall, 0)
	for _, location := range customExecCallPattern.FindAllStringIndex(input, -1) {
		object, call, ok := customCallObject(input, location)
		if !ok {
			continue
		}
		value, ok := customObjectProperty(object, "cmd")
		if !ok {
			continue
		}
		command, ok := decodeJSStringLiteral(strings.TrimSpace(value))
		if !ok || command == "" {
			continue
		}
		calls = append(calls, customExecCall{start: location[0], command: command, call: call})
	}
	return calls
}

func customStaticWriteStdinCalls(input string) []customContinuationCall {
	calls := make([]customContinuationCall, 0)
	for _, location := range customWriteCallPattern.FindAllStringIndex(input, -1) {
		if !customCallIsAwaited(input, location[0], customWriteCallPattern) {
			continue
		}
		object, call, ok := customCallObject(input, location)
		if !ok {
			continue
		}
		value, ok := customObjectProperty(object, "session_id")
		if !ok {
			continue
		}
		sessionID, ok := customStaticSessionID(value)
		if !ok {
			continue
		}
		calls = append(calls, customContinuationCall{
			start:      location[0],
			sessionID:  sessionID,
			yieldAfter: customWriteStdinYieldAfter(call),
		})
	}
	return calls
}

func customCallObject(input string, location []int) (object, call string, ok bool) {
	if len(location) != 2 || location[1] <= 0 {
		return "", "", false
	}
	end, ok := customDelimitedEnd(input, location[1]-1)
	if !ok {
		return "", "", false
	}
	arguments, ok := splitCustomTopLevel(input[location[1]:end], ',')
	if !ok || len(arguments) != 1 {
		return "", "", false
	}
	object = strings.TrimSpace(arguments[0])
	if object == "" || object[0] != '{' {
		return "", "", false
	}
	objectEnd, closed := customDelimitedEnd(object, 0)
	if !closed || objectEnd != len(object)-1 {
		return "", "", false
	}
	return object, input[location[0] : end+1], true
}

func customStaticExecPendingCalls(input string, eventTime, arrival time.Time) []customPendingCall {
	calls := customStaticExecCalls(input)
	accepted := make([]customExecCall, 0, len(calls))
	for _, call := range calls {
		if customExecCallIsAwaited(input, call.start) {
			accepted = append(accepted, call)
		}
	}
	structuredProof := customToolEmitsStructuredResults(input)
	sessionProofBlock := 0
	if len(accepted) == 1 {
		sessionProofBlock = customSessionProofBlock(input, accepted[0].start)
	}
	pending := make([]customPendingCall, 0, len(accepted))
	for _, call := range accepted {
		pending = append(pending, customPendingCall{
			start:   call.start,
			isStart: true,
			pending: codexPendingCommand{
				start:             codexCommandStart{command: call.command, eventTime: eventTime, arrival: arrival},
				requiresProof:     true,
				structuredProof:   structuredProof,
				sessionProofBlock: sessionProofBlock,
				yieldAfter:        customExecYieldAfter(call.call),
				proofEventTime:    eventTime,
				proofArrival:      arrival,
			},
		})
	}
	return pending
}

// customSessionProofBlock returns the output block occupied by the standard
// session-ID text emission for a single directly assigned exec result. Custom
// tool output block zero is the tool status envelope; subsequent blocks map to
// text(...) calls in source order. Requiring the marker to be the final text
// call lets the result parser distinguish it from command stdout.
func customSessionProofBlock(input string, callStart int) int {
	statementStart, ok := customTopLevelStatementStart(input, callStart)
	if !ok {
		return 0
	}
	match := customResultVariable.FindStringSubmatch(input[statementStart:callStart])
	if len(match) != 2 {
		return 0
	}
	arguments := customTextCallArguments(input)
	if len(arguments) == 0 {
		return 0
	}
	variable := match[1]
	expected := "`SESSION_ID=${" + variable + ".session_id}`"
	if strings.TrimSpace(arguments[len(arguments)-1]) != expected {
		return 0
	}
	return len(arguments)
}

func customMappedExecPendingCalls(input string, eventTime, arrival time.Time) []customPendingCall {
	structuredProof := customToolEmitsStructuredResults(input)
	pending := make([]customPendingCall, 0)
	for _, match := range customMapPattern.FindAllStringSubmatchIndex(input, -1) {
		if len(match) < 4 || !customPositionInAwaitedPromiseAll(input, match[0]) {
			continue
		}
		mapEnd, ok := customDelimitedEnd(input, match[1]-1)
		if !ok {
			continue
		}
		expression, call, params, ok := customMapCommandExpression(input[match[1]:mapEnd])
		if !ok {
			continue
		}
		source := input[match[2]:match[3]]
		items, ok := customStaticArrayValues(input, source, match[0])
		if !ok {
			continue
		}
		selector, ok := customMapValueSelector(params, expression)
		if !ok {
			continue
		}
		for _, item := range items {
			value, ok := selector(item)
			if !ok {
				continue
			}
			command, ok := customCommandFromStaticValue(value)
			if !ok || command == "" {
				continue
			}
			pending = append(pending, customPendingCall{
				start:   match[0],
				isStart: true,
				pending: codexPendingCommand{
					start:           codexCommandStart{command: command, eventTime: eventTime, arrival: arrival},
					requiresProof:   true,
					structuredProof: structuredProof,
					yieldAfter:      customExecYieldAfter(call + "\n" + value),
					proofEventTime:  eventTime,
					proofArrival:    arrival,
				},
			})
		}
	}
	return pending
}

func customPositionInAwaitedPromiseAll(input string, position int) bool {
	for _, location := range customAnyPromiseAll.FindAllStringIndex(input, -1) {
		if location[1] > position {
			continue
		}
		statementStart, ok := customTopLevelStatementStart(input, location[0])
		if !ok || !customAssignmentPattern.MatchString(input[statementStart:location[0]]) {
			continue
		}
		end, ok := customDelimitedEnd(input, location[1]-1)
		if ok && position < end {
			return true
		}
	}
	return false
}

func customMapCommandExpression(callback string) (expression, call, params string, ok bool) {
	params, body, found := strings.Cut(callback, "=>")
	if !found {
		return "", "", "", false
	}
	params = strings.TrimSpace(params)
	params = strings.TrimSpace(strings.TrimPrefix(params, "async"))
	if strings.HasPrefix(params, "(") {
		end, closed := customDelimitedEnd(params, 0)
		if !closed || end != len(params)-1 {
			return "", "", "", false
		}
		params = strings.TrimSpace(params[1:end])
	}
	matches := customExecCallPattern.FindAllStringIndex(body, -1)
	if len(matches) != 1 {
		return "", "", "", false
	}
	end, found := customDelimitedEnd(body, matches[0][1]-1)
	if !found {
		return "", "", "", false
	}
	call = body[matches[0][0] : end+1]
	arguments := strings.TrimSpace(body[matches[0][1]:end])
	parts, found := splitCustomTopLevel(arguments, ',')
	if !found || len(parts) != 1 {
		return "", "", "", false
	}
	expression = strings.TrimSpace(parts[0])
	if isCustomIdentifier(expression) || isCustomMemberExpression(expression) {
		return expression, call, params, true
	}
	if !strings.HasPrefix(expression, "{") {
		return "", "", "", false
	}
	value, found := customObjectProperty(expression, "cmd")
	if !found {
		return "", "", "", false
	}
	return value, call, params, true
}

func customMapWriteStdinExpression(callback string) (expression, call, params string, ok bool) {
	params, body, found := strings.Cut(callback, "=>")
	if !found {
		return "", "", "", false
	}
	params = strings.TrimSpace(params)
	params = strings.TrimSpace(strings.TrimPrefix(params, "async"))
	if strings.HasPrefix(params, "(") {
		end, closed := customDelimitedEnd(params, 0)
		if !closed || end != len(params)-1 {
			return "", "", "", false
		}
		params = strings.TrimSpace(params[1:end])
	}
	matches := customWriteCallPattern.FindAllStringIndex(body, -1)
	if len(matches) != 1 {
		return "", "", "", false
	}
	end, closed := customDelimitedEnd(body, matches[0][1]-1)
	if !closed {
		return "", "", "", false
	}
	call = body[matches[0][0] : end+1]
	arguments := strings.TrimSpace(body[matches[0][1]:end])
	parts, split := splitCustomTopLevel(arguments, ',')
	if !split || len(parts) != 1 {
		return "", "", "", false
	}
	expression, found = customObjectProperty(parts[0], "session_id")
	if !found {
		return "", "", "", false
	}
	return strings.TrimSpace(expression), call, params, true
}

func customStaticArrayValues(input, name string, before int) ([]string, bool) {
	pattern := regexp.MustCompile(`\b(?:const|let|var)\s+` + regexp.QuoteMeta(name) + `\s*=\s*\[`)
	var selected []string
	for _, location := range pattern.FindAllStringIndex(input[:before], -1) {
		end, ok := customDelimitedEnd(input, location[1]-1)
		if !ok || end >= before {
			continue
		}
		values, ok := splitCustomTopLevel(input[location[1]:end], ',')
		if ok {
			selected = values
		}
	}
	return selected, len(selected) > 0
}

func customMapValueSelector(params, expression string) (func(string) (string, bool), bool) {
	if _, ok := decodeJSStringLiteral(expression); ok {
		return func(string) (string, bool) { return expression, true }, true
	}
	if selector, ok := customMapObjectValueSelector(params, expression); ok {
		return selector, true
	}
	if !isCustomIdentifier(expression) {
		return nil, false
	}
	if isCustomIdentifier(params) && params == expression {
		return func(item string) (string, bool) { return strings.TrimSpace(item), true }, true
	}
	params = strings.TrimSpace(params)
	if !strings.HasPrefix(params, "[") {
		return nil, false
	}
	end, ok := customDelimitedEnd(params, 0)
	if !ok || end != len(params)-1 {
		return nil, false
	}
	bindings, ok := splitCustomTopLevel(params[1:end], ',')
	if !ok {
		return nil, false
	}
	selected := -1
	for index, binding := range bindings {
		if strings.TrimSpace(binding) == expression {
			selected = index
			break
		}
	}
	if selected < 0 {
		return nil, false
	}
	return func(item string) (string, bool) {
		item = strings.TrimSpace(item)
		if !strings.HasPrefix(item, "[") {
			return "", false
		}
		itemEnd, found := customDelimitedEnd(item, 0)
		if !found || itemEnd != len(item)-1 {
			return "", false
		}
		values, found := splitCustomTopLevel(item[1:itemEnd], ',')
		if !found || selected >= len(values) {
			return "", false
		}
		return strings.TrimSpace(values[selected]), true
	}, true
}

func customMapObjectValueSelector(params, expression string) (func(string) (string, bool), bool) {
	if root, property, ok := customMemberExpression(expression); ok {
		if !isCustomIdentifier(params) || params != root {
			return nil, false
		}
		return func(item string) (string, bool) {
			return customObjectProperty(item, property)
		}, true
	}
	params = strings.TrimSpace(params)
	if !isCustomIdentifier(expression) || !strings.HasPrefix(params, "{") {
		return nil, false
	}
	property, ok := customObjectBindingProperty(params, expression)
	if !ok {
		return nil, false
	}
	return func(item string) (string, bool) {
		return customObjectProperty(item, property)
	}, true
}

func customObjectBindingProperty(params, expression string) (string, bool) {
	end, ok := customDelimitedEnd(params, 0)
	if !ok || end != len(params)-1 {
		return "", false
	}
	bindings, ok := splitCustomTopLevel(params[1:end], ',')
	if !ok {
		return "", false
	}
	for _, binding := range bindings {
		parts, valid := splitCustomTopLevel(binding, ':')
		if !valid || len(parts) > 2 {
			continue
		}
		property := strings.Trim(strings.TrimSpace(parts[0]), "\"'")
		bound := property
		if len(parts) == 2 {
			bound = strings.TrimSpace(parts[1])
		}
		if isCustomIdentifier(property) && bound == expression {
			return property, true
		}
	}
	return "", false
}

func isCustomMemberExpression(value string) bool {
	_, _, ok := customMemberExpression(value)
	return ok
}

func customMemberExpression(value string) (root, property string, ok bool) {
	root, property, found := strings.Cut(strings.TrimSpace(value), ".")
	if !found || strings.Contains(property, ".") || !isCustomIdentifier(root) || !isCustomIdentifier(property) {
		return "", "", false
	}
	return root, property, true
}

func customStaticSessionID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if decoded, ok := decodeJSStringLiteral(value); ok {
		return decoded, decoded != ""
	}
	if value == "" {
		return "", false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return "", false
		}
	}
	return value, true
}

func customCommandFromStaticValue(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if command, ok := decodeJSStringLiteral(value); ok {
		return command, true
	}
	if !strings.HasPrefix(value, "{") {
		return "", false
	}
	command, ok := customObjectProperty(value, "cmd")
	if !ok {
		return "", false
	}
	return decodeJSStringLiteral(strings.TrimSpace(command))
}

func customObjectProperty(object, wanted string) (string, bool) {
	object = strings.TrimSpace(object)
	if !strings.HasPrefix(object, "{") {
		return "", false
	}
	end, ok := customDelimitedEnd(object, 0)
	if !ok || end != len(object)-1 {
		return "", false
	}
	properties, ok := splitCustomTopLevel(object[1:end], ',')
	if !ok {
		return "", false
	}
	for _, property := range properties {
		parts, valid := splitCustomTopLevel(property, ':')
		if !valid || len(parts) > 2 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(parts[0]), "\"'")
		if key != wanted {
			continue
		}
		if len(parts) == 1 {
			return wanted, true
		}
		return strings.TrimSpace(parts[1]), true
	}
	return "", false
}

func splitCustomTopLevel(input string, delimiter byte) ([]string, bool) {
	result := make([]string, 0)
	start := 0
	braces, brackets, parentheses := 0, 0, 0
	for index := 0; index < len(input); {
		next, token, ok := nextCustomExecSyntax(input, index, len(input))
		if !ok {
			return nil, false
		}
		if token == delimiter && braces == 0 && brackets == 0 && parentheses == 0 {
			result = append(result, strings.TrimSpace(input[start:index]))
			start = next
			index = next
			continue
		}
		switch token {
		case '{':
			braces++
		case '}':
			braces--
		case '[':
			brackets++
		case ']':
			brackets--
		case '(':
			parentheses++
		case ')':
			parentheses--
		}
		if braces < 0 || brackets < 0 || parentheses < 0 {
			return nil, false
		}
		index = next
	}
	if braces != 0 || brackets != 0 || parentheses != 0 {
		return nil, false
	}
	result = append(result, strings.TrimSpace(input[start:]))
	return result, true
}

func customDelimitedEnd(input string, open int) (int, bool) {
	if open < 0 || open >= len(input) {
		return 0, false
	}
	opening := input[open]
	closing := map[byte]byte{'(': ')', '[': ']', '{': '}'}[opening]
	if closing == 0 {
		return 0, false
	}
	depth := 0
	for index := open; index < len(input); {
		next, token, ok := nextCustomExecSyntax(input, index, len(input))
		if !ok {
			return 0, false
		}
		switch token {
		case opening:
			depth++
		case closing:
			depth--
			if depth == 0 {
				return index, true
			}
		}
		index = next
	}
	return 0, false
}

func isCustomIdentifier(value string) bool {
	if value == "" || !isCustomIdentifierStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !isCustomIdentifierPart(value[index]) {
			return false
		}
	}
	return true
}

func isCustomIdentifierStart(char byte) bool {
	return char == '_' || char == '$' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func isCustomIdentifierPart(char byte) bool {
	return isCustomIdentifierStart(char) || char >= '0' && char <= '9'
}

func customExecCallIsAwaited(input string, callStart int) bool {
	return customCallIsAwaited(input, callStart, customExecCallPattern)
}

func customCallIsAwaited(input string, callStart int, callPattern *regexp.Regexp) bool {
	if callStart < 0 || callStart > len(input) {
		return false
	}
	if statementStart, ok := customTopLevelStatementStart(input, callStart); ok &&
		customDirectAwaitPattern.MatchString(input[statementStart:callStart]) {
		return true
	}
	for _, location := range customPromiseAllPattern.FindAllStringIndex(input, -1) {
		statementStart, ok := customTopLevelStatementStart(input, location[0])
		if location[1] > callStart || !ok || !customAssignmentPattern.MatchString(input[statementStart:location[0]]) {
			continue
		}
		arrayOpen := location[1] - 1
		arrayClose, closed := customDelimitedEnd(input, arrayOpen)
		if closed && callStart < arrayClose && customDirectArrayElement(input, arrayOpen, arrayClose, callStart, callPattern) {
			return true
		}
	}
	return false
}

// customDirectArrayElement reports whether callStart begins the complete array
// element that contains it. Compound expressions can skip a textual call at
// runtime, so they are not safe inputs for output-only completion inference.
func customDirectArrayElement(input string, arrayOpen, arrayClose, callStart int, callPattern *regexp.Regexp) bool {
	if arrayOpen < 0 || arrayClose > len(input) || arrayOpen >= arrayClose ||
		callStart <= arrayOpen || callStart >= arrayClose {
		return false
	}
	location := callPattern.FindStringIndex(input[callStart:arrayClose])
	if len(location) != 2 || location[0] != 0 {
		return false
	}
	callEnd, ok := customDelimitedEnd(input, callStart+location[1]-1)
	if !ok || callEnd >= arrayClose {
		return false
	}

	elements, valid := splitCustomTopLevel(input[arrayOpen+1:arrayClose], ',')
	if !valid {
		return false
	}
	cursor := arrayOpen + 1
	for _, element := range elements {
		offset := strings.Index(input[cursor:arrayClose], element)
		if offset < 0 {
			return false
		}
		elementStart := cursor + offset
		elementEnd := elementStart + len(element)
		if callStart >= elementStart && callStart < elementEnd {
			return element == input[callStart:callEnd+1]
		}
		cursor = elementEnd
	}
	return false
}

func customTopLevelStatementStart(input string, end int) (int, bool) {
	if end < 0 || end > len(input) {
		return 0, false
	}
	start := 0
	braces, brackets, parentheses := 0, 0, 0
	for index := 0; index < end; {
		next, token, ok := nextCustomExecSyntax(input, index, end)
		if !ok {
			return 0, false
		}
		switch token {
		case '{':
			braces++
		case '[':
			brackets++
		case '(':
			parentheses++
		case '}':
			braces--
		case ']':
			brackets--
		case ')':
			parentheses--
		}
		if customTopLevelStatementBoundary(input, end, index, token, braces, brackets, parentheses) {
			start = index + 1
		}
		if braces < 0 || brackets < 0 || parentheses < 0 {
			return 0, false
		}
		index = next
	}
	return start, braces == 0 && brackets == 0 && parentheses == 0
}

func customTopLevelStatementBoundary(input string, end, index int, token byte, braces, brackets, parentheses int) bool {
	if braces != 0 || brackets != 0 || parentheses != 0 {
		return false
	}
	if token == ';' {
		return true
	}
	if token != '\r' && token != '\n' {
		return false
	}
	prefix := input[index+1 : end]
	return customDirectAwaitPattern.MatchString(prefix) || customAssignmentPattern.MatchString(prefix)
}

func nextCustomExecSyntax(input string, index, end int) (int, byte, bool) {
	token := input[index]
	if token == '\'' || token == '"' || token == '`' {
		next, ok := skipCustomExecQuoted(input, index, end)
		return next, 0, ok
	}
	if token != '/' || index+1 >= end {
		return index + 1, token, true
	}
	if input[index+1] == '/' {
		index += 2
		for index < end && input[index] != '\n' {
			index++
		}
		return index, 0, true
	}
	if input[index+1] == '*' {
		closeIndex := strings.Index(input[index+2:end], "*/")
		if closeIndex < 0 {
			return 0, 0, false
		}
		return index + closeIndex + 4, 0, true
	}
	return index + 1, token, true
}

func skipCustomExecQuoted(input string, start, end int) (int, bool) {
	delimiter := input[start]
	for index := start + 1; index < end; index++ {
		if input[index] == '\\' {
			index++
			continue
		}
		if input[index] == delimiter {
			return index + 1, true
		}
	}
	return 0, false
}

func customToolEmitsStructuredResults(input string) bool {
	arguments := customTextCallArguments(input)
	if !customRawOutput.MatchString(input) && customJSONStringifyArgumentsUseResults(input, arguments) {
		return true
	}
	for _, match := range customDestructuredRest.FindAllStringSubmatch(input, -1) {
		if len(match) < 2 {
			continue
		}
		for _, argument := range arguments {
			if strings.TrimSpace(argument) == match[1] {
				return true
			}
		}
	}
	return false
}

func customJSONStringifyArgumentsUseResults(input string, arguments []string) bool {
	resultVariables := customResultVariables(input)
	found := false
	for _, argument := range arguments {
		for _, location := range customJSONStringify.FindAllStringIndex(argument, -1) {
			end, ok := customDelimitedEnd(argument, location[1]-1)
			if !ok || !customJSONStringifyUsesResult(argument[location[1]:end], resultVariables) {
				return false
			}
			found = true
		}
	}
	return found
}

func customResultVariables(input string) map[string]bool {
	result := make(map[string]bool)
	for _, match := range customDirectResult.FindAllStringSubmatch(input, -1) {
		result[match[1]] = true
	}
	for _, location := range customPromiseResult.FindAllStringSubmatchIndex(input, -1) {
		if len(location) < 4 {
			continue
		}
		end, ok := customDelimitedEnd(input, location[1]-1)
		if !ok {
			continue
		}
		body := input[location[1]:end]
		if customExecCallPattern.MatchString(body) || customWriteCallPattern.MatchString(body) {
			result[input[location[2]:location[3]]] = true
		}
	}

	for changed := true; changed; {
		changed = false
		for variable := range result {
			pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(variable) + `\.forEach\s*\(\s*(?:async\s+)?(?:\(\s*)?([A-Za-z_$][A-Za-z0-9_$]*)`)
			for _, match := range pattern.FindAllStringSubmatch(input, -1) {
				if !result[match[1]] {
					result[match[1]] = true
					changed = true
				}
			}
		}
		for _, match := range customForOf.FindAllStringSubmatch(input, -1) {
			if !result[match[2]] {
				continue
			}
			for _, variable := range customIdentifier.FindAllString(match[1], -1) {
				if !result[variable] {
					result[variable] = true
					changed = true
				}
			}
		}
	}
	return result
}

func customJSONStringifyUsesResult(argument string, resultVariables map[string]bool) bool {
	argument = strings.TrimSpace(argument)
	if resultVariables[argument] {
		return true
	}
	if len(argument) < 2 || argument[0] != '{' || argument[len(argument)-1] != '}' {
		return false
	}
	fields, ok := splitCustomTopLevel(argument[1:len(argument)-1], ',')
	if !ok || len(fields) == 0 {
		return false
	}
	last := strings.TrimSpace(fields[len(fields)-1])
	if !strings.HasPrefix(last, "...") {
		return false
	}
	return resultVariables[strings.TrimSpace(strings.TrimPrefix(last, "..."))]
}

func customTextCallArguments(input string) []string {
	arguments := make([]string, 0)
	for _, location := range customTextCallPattern.FindAllStringIndex(input, -1) {
		end, ok := customDelimitedEnd(input, location[1]-1)
		if ok {
			arguments = append(arguments, input[location[1]:end])
		}
	}
	return arguments
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
	index       int
	hasIndex    bool
	sessionID   string
	cellID      string
	hasExit     bool
	duration    time.Duration
	hasDuration bool
}

func (e *CodexExecutor) resolvePendingOutputs(pending []codexPendingCommand, outputs []string, eventTime time.Time, state *codexTimingState, now func() time.Time) {
	results := trustedPendingResults(pending, outputs)
	if len(pending) > 1 && len(results) == 0 {
		if outer, found := parseTextCommandResult(strings.Join(outputs, "\n")); found && outer.cellID != "" {
			state.cells[outer.cellID] = append(state.cells[outer.cellID], pending...)
			return
		}
	}
	arrival := now()
	for index, command := range pending {
		if command.start.command == "" {
			continue
		}
		result, hasResult := results[index]
		if !command.requiresProof && !hasResult {
			e.emitResolvedCommandTiming(command, len(pending), codexCommandResult{}, eventTime, arrival)
			continue
		}
		if !hasResult {
			if commandCompletedBeforeYield(command, eventTime, arrival) {
				if command.sessionID != "" {
					delete(state.sessions, command.sessionID)
				}
				e.emitResolvedCommandTiming(command, len(pending), codexCommandResult{}, eventTime, arrival)
			} else if command.sessionID == "" {
				command.unprovenEventTime = eventTime
				command.unprovenArrival = arrival
				state.unproven = append(state.unproven, command)
			}
			continue
		}
		if result.hasExit {
			if command.sessionID != "" {
				delete(state.sessions, command.sessionID)
			}
			e.emitResolvedCommandTiming(command, len(pending), result, eventTime, arrival)
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

func trustedPendingResults(pending []codexPendingCommand, outputs []string) map[int]codexCommandResult {
	structuredProof := true
	tracked := 0
	for _, command := range pending {
		if command.start.command == "" {
			continue
		}
		tracked++
		structuredProof = structuredProof && command.structuredProof
	}
	results := make(map[int]codexCommandResult)
	if structuredProof && tracked > 0 {
		results = parseCodexCommandResults(outputs, len(pending))
	} else if len(pending) == 1 {
		if result, found := parseTextCommandResult(strings.Join(outputs, "\n")); found {
			results[0] = result
		}
	}
	if len(pending) == 1 {
		if _, found := results[0]; !found {
			if result, ok := parseSessionProofBlock(outputs, pending[0].sessionProofBlock); ok {
				results[0] = result
			}
		}
	}
	return results
}

func parseSessionProofBlock(outputs []string, block int) (codexCommandResult, bool) {
	if block <= 0 || len(outputs) != block+1 {
		return codexCommandResult{}, false
	}
	if _, ok := commandStatusEnvelope(outputs[0]); !ok {
		return codexCommandResult{}, false
	}
	matches := outputSessionProofBlock.FindStringSubmatch(strings.TrimSpace(outputs[block]))
	if len(matches) != 2 {
		return codexCommandResult{}, false
	}
	return codexCommandResult{sessionID: matches[1]}, true
}

func (e *CodexExecutor) emitResolvedCommandTiming(command codexPendingCommand, pendingCount int, result codexCommandResult, eventTime, arrival time.Time) {
	if result.hasDuration && command.sessionID == "" {
		e.emitCommandTimingHandler(command.start.command, result.duration)
		return
	}
	if pendingCount == 1 || command.sessionID != "" {
		e.emitCommandTiming(command.start, eventTime, arrival)
	}
}

func customExecYieldAfter(call string) time.Duration {
	const (
		defaultYield = 10 * time.Second
		minYield     = 250 * time.Millisecond
		maxYield     = 30 * time.Second
	)
	yieldAfter, configured := explicitYieldAfter(call)
	if !configured {
		return defaultYield
	}
	if yieldAfter <= 0 {
		return 0
	}
	return max(minYield, min(yieldAfter, maxYield))
}

func customWriteStdinYieldAfter(call string) time.Duration {
	location := customWriteCallPattern.FindStringIndex(call)
	object, _, ok := customCallObject(call, location)
	if !ok {
		return 0
	}
	chars := ""
	if value, found := customObjectProperty(object, "chars"); found {
		decoded, known := decodeJSStringLiteral(strings.TrimSpace(value))
		if !known {
			return 0
		}
		chars = decoded
	}
	yieldAfter, configured := explicitYieldAfter(call)
	return effectiveWriteStdinYield(yieldAfter, configured, chars)
}

func explicitYieldAfter(call string) (time.Duration, bool) {
	matches := customExecYieldPattern.FindStringSubmatch(call)
	if len(matches) < 2 {
		if customExecYieldKey.MatchString(call) {
			return 0, true
		}
		return 0, false
	}
	milliseconds, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil || milliseconds <= 0 {
		return 0, true
	}
	return time.Duration(milliseconds) * time.Millisecond, true
}

func functionCallYieldAfter(arguments string) time.Duration {
	var args struct {
		Chars       string `json:"chars"`
		YieldTimeMS *int64 `json:"yield_time_ms"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return 0
	}
	if args.YieldTimeMS == nil {
		return effectiveWriteStdinYield(0, false, args.Chars)
	}
	if *args.YieldTimeMS <= 0 {
		return 0
	}
	return effectiveWriteStdinYield(time.Duration(*args.YieldTimeMS)*time.Millisecond, true, args.Chars)
}

func effectiveWriteStdinYield(yieldAfter time.Duration, configured bool, chars string) time.Duration {
	const (
		defaultWriteYield = 250 * time.Millisecond
		minEmptyPollYield = 5 * time.Second
		maxWriteYield     = 30 * time.Second
		maxEmptyPollYield = 300 * time.Second
	)
	if chars == "" {
		if !configured {
			return minEmptyPollYield
		}
		if yieldAfter <= 0 {
			return 0
		}
		return max(minEmptyPollYield, min(yieldAfter, maxEmptyPollYield))
	}
	if !configured {
		return defaultWriteYield
	}
	return max(defaultWriteYield, min(yieldAfter, maxWriteYield))
}

func commandCompletedBeforeYield(command codexPendingCommand, eventTime, arrival time.Time) bool {
	const timestampMargin = 100 * time.Millisecond
	if command.yieldAfter <= timestampMargin || command.proofEventTime.IsZero() || eventTime.IsZero() ||
		eventTime.Before(command.proofEventTime) || command.proofArrival.IsZero() || arrival.Before(command.proofArrival) {
		return false
	}
	return eventTime.Sub(command.proofEventTime)+timestampMargin < command.yieldAfter
}

func (e *CodexExecutor) emitCommandTiming(start codexCommandStart, eventTime, arrival time.Time) {
	duration := arrival.Sub(start.arrival)
	if !start.eventTime.IsZero() && !eventTime.IsZero() && !eventTime.Before(start.eventTime) {
		duration = eventTime.Sub(start.eventTime)
	}
	if duration < 0 {
		duration = 0
	}
	e.emitCommandTimingHandler(start.command, duration)
}

func (e *CodexExecutor) emitOutput(text string) {
	if e.OutputHandler == nil {
		return
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.OutputHandler(text)
}

func (e *CodexExecutor) emitCommandTimingHandler(command string, duration time.Duration) {
	if e.CommandTimingHandler == nil {
		return
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.CommandTimingHandler(command, duration)
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
	parsed := make([]codexCommandResult, 0, len(outputs))
	for _, output := range outputs {
		results, ok := parseStructuredCommandResults(output)
		if ok {
			parsed = append(parsed, results...)
		}
	}

	results := make(map[int]codexCommandResult)
	if pendingCount > 1 {
		indexed, valid := indexedBatchResults(parsed, pendingCount)
		if !valid {
			return results
		}
		if indexed != nil {
			return indexed
		}
		if len(parsed) != pendingCount {
			return results
		}
	}
	for index, result := range parsed {
		if index >= pendingCount {
			break
		}
		results[index] = result
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

func parseStructuredCommandResults(output string) ([]codexCommandResult, bool) {
	trimmed := strings.TrimSpace(output)
	if result, ok := parseStructuredCommandResultJSON(trimmed); ok {
		return []codexCommandResult{result}, true
	}
	var rawResults []json.RawMessage
	if json.Unmarshal([]byte(trimmed), &rawResults) == nil && len(rawResults) > 0 {
		results := make([]codexCommandResult, 0, len(rawResults))
		for _, raw := range rawResults {
			result, ok := parseStructuredCommandResultJSON(strings.TrimSpace(string(raw)))
			if !ok {
				return nil, false
			}
			results = append(results, result)
		}
		return results, true
	}
	result, ok := parseStructuredCommandResult(output)
	if !ok {
		return nil, false
	}
	return []codexCommandResult{result}, true
}

func indexedBatchResults(parsed []codexCommandResult, pendingCount int) (map[int]codexCommandResult, bool) {
	if len(parsed) == 0 {
		return nil, true
	}
	results := make(map[int]codexCommandResult)
	indexed := parsed[0].hasIndex
	for _, result := range parsed {
		if result.hasIndex != indexed || (indexed && (result.index < 1 || result.index > pendingCount)) {
			return nil, false
		}
		if !indexed {
			continue
		}
		index := result.index - 1
		if _, duplicate := results[index]; duplicate {
			return nil, false
		}
		results[index] = result
	}
	if !indexed {
		return nil, true
	}
	return results, true
}

func parseStructuredCommandResult(output string) (codexCommandResult, bool) {
	if result, ok := parseStructuredCommandResultJSON(strings.TrimSpace(output)); ok {
		return result, true
	}
	var result codexCommandResult
	found := false
	for line := range strings.Lines(output) {
		candidate, ok := parseStructuredCommandResultJSON(strings.TrimSpace(line))
		if !ok {
			continue
		}
		if found {
			return codexCommandResult{}, false
		}
		result, found = candidate, true
	}
	return result, found
}

func parseStructuredCommandResultJSON(output string) (codexCommandResult, bool) {
	var raw struct {
		Index           *int            `json:"index"`
		SessionID       json.RawMessage `json:"session_id"`
		CellID          json.RawMessage `json:"cell_id"`
		ExitCode        json.RawMessage `json:"exit_code"`
		WallTimeSeconds *float64        `json:"wall_time_seconds"`
	}
	if json.Unmarshal([]byte(output), &raw) != nil {
		return codexCommandResult{}, false
	}
	result := codexCommandResult{}
	if raw.Index != nil {
		result.index = *raw.Index
		result.hasIndex = true
	}
	result.sessionID = rawIDString(raw.SessionID)
	result.cellID = rawIDString(raw.CellID)
	var exitCode *int
	result.hasExit = json.Unmarshal(raw.ExitCode, &exitCode) == nil && exitCode != nil
	if raw.WallTimeSeconds != nil && *raw.WallTimeSeconds >= 0 {
		result.duration = time.Duration(*raw.WallTimeSeconds * float64(time.Second))
		result.hasDuration = true
	}
	return result, result.sessionID != "" || result.cellID != "" || result.hasExit
}

func parseTextCommandResult(output string) (codexCommandResult, bool) {
	output, ok := commandStatusEnvelope(output)
	if !ok {
		return codexCommandResult{}, false
	}
	result := codexCommandResult{
		sessionID: extractPatternValue(outputSessionIDPattern, output),
		cellID:    extractPatternValue(outputCellIDPattern, output),
	}
	_, result.hasExit = parseExitCode(output)
	markers := 0
	if result.sessionID != "" {
		markers++
	}
	if result.cellID != "" {
		markers++
	}
	if result.hasExit {
		markers++
	}
	if markers != 1 {
		return codexCommandResult{}, false
	}
	return result, true
}

// commandStatusEnvelope returns only the tool-generated status prefix. Current
// custom exec output can append arbitrary process stdout after one of these
// payload headers; that content must not be interpreted as session or exit
// metadata.
func commandStatusEnvelope(output string) (string, bool) {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	lines := strings.Split(output, "\n")
	for index, line := range lines {
		if line == "Live output:" || line == "Final output:" || line == "Output:" {
			output = strings.Join(lines[:index], "\n")
			break
		}
	}
	output = strings.TrimSpace(output)
	return output, strings.HasPrefix(output, "Script ") || strings.HasPrefix(output, "Process ")
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

func cellIDFromArguments(arguments string) string {
	var args struct {
		CellID json.RawMessage `json:"cell_id"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil || len(args.CellID) == 0 {
		return ""
	}
	return rawIDString(args.CellID)
}

func unprovenAssociationIsFresh(command codexPendingCommand, eventTime, arrival time.Time) bool {
	const associationWindow = 10 * time.Second
	duration := arrival.Sub(command.unprovenArrival)
	if !command.unprovenEventTime.IsZero() && !eventTime.IsZero() && !eventTime.Before(command.unprovenEventTime) {
		duration = eventTime.Sub(command.unprovenEventTime)
	}
	return duration >= 0 && duration <= associationWindow
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
