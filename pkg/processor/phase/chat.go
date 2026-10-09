package phase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

// ChatContext is what the first turn of an ACP chat provider session knows about the
// thread: its branch and base, the branch's commits, and the last run's report and merge outcome.
type ChatContext struct {
	Branch       string
	Base         string
	Log          string
	Report       string
	MergeOutcome string
}

// ChatRequest is one ACP chat turn. ResumeID names the provider session the turn continues;
// empty starts a fresh session that receives the full context.
type ChatRequest struct {
	Context  ChatContext
	Message  string
	ResumeID string
}

// ChatResult is the outcome of one ACP chat turn. Answer has every protocol signal stripped;
// Signal is ACPMerge, ACPPR, or empty. SessionID is the provider session a later turn can
// resume, empty when the provider reported none.
type ChatResult struct {
	Answer    string
	Signal    string
	SessionID string
}

// ChatPrompts renders an ACP chat turn.
type ChatPrompts interface {
	ACPChatPrompt(chat ChatContext, message string, resumed bool) string
}

// ChatLogger records ACP chat turn progress.
type ChatLogger interface {
	Logger
	PrintSection(section status.Section)
}

// ChatPhase runs one ACP chat turn through the review executor: a single session that
// continues the thread's provider session when it has one. It deliberately leaves the phase
// holder alone, because the ACP sink turns every phase into a tool-call card and a chat turn
// shows no cards; the policy still switches to the limit-wait phase while it sleeps.
type ChatPhase struct {
	cfg     Config
	log     ChatLogger
	exec    Executor
	policy  Policy
	prompts ChatPrompts
}

// ChatPhaseOpts contains dependencies for ChatPhase.
type ChatPhaseOpts struct {
	Cfg     Config
	Log     ChatLogger
	Exec    Executor
	Policy  Policy
	Prompts ChatPrompts
}

// NewChatPhase creates an ACP chat phase engine.
func NewChatPhase(opts ChatPhaseOpts) *ChatPhase {
	return &ChatPhase{cfg: opts.Cfg, log: opts.Log, exec: opts.Exec, policy: opts.Policy, prompts: opts.Prompts}
}

// Run executes one chat turn. A resumed session that fails for any reason other than a
// limit, a timeout, or cancellation is retried once as a fresh session with the full context,
// since the provider may have discarded the session or be unable to continue it. A canceled
// turn returns the context error and no result, so the caller keeps the previous session id.
func (p *ChatPhase) Run(ctx context.Context, req ChatRequest) (ChatResult, error) {
	p.log.PrintSection(status.NewGenericSection("chat"))

	resumeID := req.ResumeID
	execResult := p.runSession(ctx, req, resumeID)
	if resumeID != "" && resumeFailed(ctx, execResult) {
		p.log.Print("chat: resuming provider session %s failed (%v), starting a fresh session", resumeID, execResult.Result.Error)
		execResult = p.runSession(ctx, req, "")
	}

	if ctx.Err() != nil {
		return ChatResult{}, fmt.Errorf("chat turn: %w", ctx.Err())
	}
	execName := p.cfg.reviewExecutorName()
	result := execResult.Result
	if err := wrapExecutorError(p.policy, result.Error, execName); err != nil {
		return ChatResult{}, err
	}
	if execResult.TimedOut {
		return ChatResult{}, errors.New("chat session timed out")
	}

	// claude's Output joins the narration of every step; the answer is its final message
	answer := result.Reply
	if answer == "" {
		answer = result.Output
	}
	return ChatResult{
		Answer:    strings.TrimSpace(status.StripSignals(answer)),
		Signal:    chatSignal(result),
		SessionID: result.SessionID,
	}, nil
}

// runSession renders the prompt for a fresh or resumed session and runs it under the policy.
func (p *ChatPhase) runSession(ctx context.Context, req ChatRequest, resumeID string) ExecutionResult {
	prompt := p.prompts.ACPChatPrompt(req.Context, req.Message, resumeID != "")
	return p.policy.Run(executor.WithResumeSession(ctx, resumeID), p.exec.Run, prompt, p.cfg.reviewExecutorName())
}

// resumeFailed reports whether a resumed session failed in a way a fresh session can fix.
// a limit would fail the fresh session too, and a canceled or timed-out turn did not fail
// because of the resume. a configured error pattern is retried: the default claude patterns
// include the API 400/404/413 errors an unusable resumed transcript produces, and without the
// retry the stored id would fail every later message until the next plan run replaced it.
func resumeFailed(ctx context.Context, res ExecutionResult) bool {
	err := res.Result.Error
	if err == nil || res.TimedOut || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	if _, ok := errors.AsType[*executor.LimitPatternError](err); ok {
		return false
	}
	return true
}

// chatSignal returns the action signal of a chat answer. merge wins over PR, matching the
// executor's detection order. the output is searched as well as the result signal, because a
// later Claude text block carrying another signal overwrites the stream's.
func chatSignal(result executor.Result) string {
	for _, sig := range []string{status.ACPMerge, status.ACPPR} {
		if result.Signal == sig || strings.Contains(result.Output, sig) {
			return sig
		}
	}
	return ""
}
