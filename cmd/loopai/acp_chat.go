package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/status"
)

// thread commands loopai runs itself, without a model
const (
	acpCommandMerge = "merge"
	acpCommandPR    = "pr"
)

// acpChatNoResumeNote closes every chat answer whose provider command is a wrapper: loopai cannot
// resume a wrapper's session, so each message starts over with the run's context alone.
const acpChatNoResumeNote = "_The configured provider command is a wrapper, so this chat does not remember " +
	"earlier messages: each message starts a fresh session with the run's context._"

// acpChatEmptyAnswer stands in for a chat turn that ended without any text.
const acpChatEmptyAnswer = "The model returned no answer."

// acpCommand reports whether a thread message is one of the commands loopai runs itself: the whole
// message, trimmed and case-insensitive, is merge or pr, in English or Russian. Anything longer goes
// to the model, which can still request the action through its signal.
func acpCommand(text string) (kind string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "merge", "мерж":
		return acpCommandMerge, true
	case "pr", "пр":
		return acpCommandPR, true
	default:
		return "", false
	}
}

// acpSession is what a command or chat turn works with: the session's config, state, and
// repository, opened in the session cwd.
type acpSession struct {
	cfg    *config.Config
	state  acpSessionState
	gitSvc *git.Service
	colors *progress.Colors
}

// openSession enters the session cwd and loads its config, state, and repository. The returned
// function restores the previous working directory; it is nil when an error is returned.
func (a *acpRunner) openSession(req acp.PromptRequest) (acpSession, func(), error) {
	leave, err := enterDir(req.Cwd, a.out)
	if err != nil {
		return acpSession{}, nil, err
	}
	fail := func(err error) (acpSession, func(), error) {
		leave()
		return acpSession{}, nil, err
	}
	cfg, err := loadACPSessionConfig(a.base)
	if err != nil {
		return fail(err)
	}
	state, err := loadACPState(req.Cwd, req.SessionID, a.out)
	if err != nil {
		return fail(err)
	}
	colors := progress.NewColors(cfg.Colors)
	gitSvc, err := git.NewService(".", writerPrinter{color: colors.Info(), w: a.out}, cfg.VcsCommand)
	if err != nil {
		return fail(fmt.Errorf("open the repository: %w", err))
	}
	gitSvc.SetCommitTrailer(cfg.CommitTrailer)
	return acpSession{cfg: cfg, state: state, gitSvc: gitSvc, colors: colors}, leave, nil
}

// runCommand performs a merge or pr command typed into the thread. Every gate refusal is part of
// the answer; only a session that cannot be opened fails the turn.
func (a *acpRunner) runCommand(ctx context.Context, req acp.PromptRequest, kind string) (acp.Result, error) {
	s, leave, err := a.openSession(req)
	if err != nil {
		return acp.Result{}, err
	}
	defer leave()
	return acp.Result{Message: a.runAction(ctx, s, kind)}, nil
}

// runAction performs a requested merge or pull request and renders its outcome for the thread.
func (a *acpRunner) runAction(ctx context.Context, s acpSession, kind string) string {
	base := acpActionBase(s.gitSvc, s.cfg, s.state.LastRun)
	if kind == acpCommandPR {
		res := acpRequestedPR(ctx, s.gitSvc, s.cfg, base, s.state.LastRun)
		outcome := res.url
		if res.skipped != "" {
			outcome = "not opened: " + res.skipped
		}
		fmt.Fprintf(a.out, "acp pr: %s\n", outcome)
		return res.message()
	}
	res := acpRequestedMerge(ctx, s.gitSvc, base, s.state.LastRun)
	fmt.Fprintf(a.out, "acp merge: %s\n", res.summary())
	return res.message()
}

// runChat answers a thread message through one review-executor session in the session cwd. The
// session continues the thread's provider session when the state holds one for the same provider,
// and the new session id is saved for the next turn unless the turn was canceled. A merge or pull
// request the answer signals is performed and its outcome appended.
func (a *acpRunner) runChat(ctx context.Context, req acp.PromptRequest, sink *acp.Sink) (acp.Result, error) {
	s, leave, err := a.openSession(req)
	if err != nil {
		return acp.Result{}, err
	}
	defer leave()

	o, mode := a.base, processor.ModeACPChat
	_, recovery, err := resolveExecutionDeps(ctx, o, s.cfg, mode, false, a.out)
	if err != nil {
		return acp.Result{}, err
	}
	// the turn writes a progress log and the session state under .loopai/, and the merge gates
	// count untracked files, so both must be ignored before anything is written
	if err = s.gitSvc.EnsureLocalGitignore(); err != nil {
		return acp.Result{}, fmt.Errorf("ensure gitignore: %w", err)
	}

	provider, resumable := acpChatProvider(s.cfg)
	resumeID := ""
	if resumable && s.state.Provider == provider {
		resumeID = s.state.ProviderSessionID
	}
	branch, _ := s.gitSvc.CurrentBranch() // a detached or unreadable HEAD leaves the branch unknown
	chat := acpChatContext(s, branch)

	keepAwake := newAwakeHolder(s.cfg.KeepAwake)
	defer keepAwake.Stop()
	holder := &status.PhaseHolder{}
	baseLog, err := progress.NewLogger(progress.Config{
		Mode: string(mode), Branch: branch, Params: runHeaderParams(o, s.cfg, mode), NoColor: o.NoColor, Stdout: a.out,
	}, s.colors, holder)
	if err != nil {
		return acp.Result{}, fmt.Errorf("create progress logger: %w", err)
	}
	var chatErr error
	defer func() {
		if chatErr != nil {
			baseLog.SetFailed(chatErr)
		}
		if closeErr := baseLog.Close(); closeErr != nil {
			fmt.Fprintf(a.out, "warning: failed to close progress log: %v\n", closeErr)
		}
	}()
	holder.OnChange(keepAwake.OnPhase)
	holder.OnChange(sink.OnPhase)
	chatLog, timer := buildRunnerLogger(nil, nil, nil, keepAwake, baseLog,
		func(l processor.Logger) processor.Logger { return sink.WrapLogger(l) })

	if resumeID != "" {
		chatLog.Print("chat: %s, resuming provider session %s", provider, resumeID)
	} else {
		chatLog.Print("chat: %s, fresh provider session", provider)
	}
	r := processor.New(processor.Config{
		Mode:               mode,
		ProgressPath:       baseLog.Path(),
		Debug:              o.Debug,
		NoColor:            o.NoColor,
		TaskRetryCount:     s.cfg.TaskRetryCount,
		ExternalReviewTool: config.ExternalReviewToolNone,
		TaskModel:          resolveSpec(o.TaskModel, s.cfg.TaskModel),
		ReviewModel:        resolveReviewSpec(o, s.cfg),
		AppConfig:          s.cfg,
		LimitRecovery:      recovery,
	}, chatLog, holder)
	r.SetChatRequest(processor.ChatRequest{Context: chat, Message: strings.TrimSpace(req.Text), ResumeID: resumeID})
	chatErr = runWithSectionTiming(ctx, r.Run, timer)
	if chatErr != nil {
		// a canceled turn keeps the previous provider session id: nothing is saved
		return acp.Result{}, chatErr
	}

	res := r.ChatResult()
	s.state.Provider, s.state.ProviderSessionID = provider, res.SessionID
	if saveErr := saveACPState(req.Cwd, req.SessionID, s.state); saveErr != nil {
		fmt.Fprintf(a.out, "warning: ACP session state not saved: %v\n", saveErr)
	}

	parts := []string{res.Answer}
	if strings.TrimSpace(res.Answer) == "" {
		parts[0] = acpChatEmptyAnswer
	}
	if !resumable {
		parts = append(parts, acpChatNoResumeNote)
	}
	switch res.Signal {
	case status.ACPMerge:
		parts = append(parts, a.runAction(ctx, s, acpCommandMerge))
	case status.ACPPR:
		parts = append(parts, a.runAction(ctx, s, acpCommandPR))
	}
	return acp.Result{Message: strings.Join(parts, "\n\n")}, nil
}

// acpChatProvider names the provider that answers chat turns, the review block's, and whether its
// sessions can be resumed: a wrapper command defines its own CLI, so the executor ignores a resume id.
func acpChatProvider(cfg *config.Config) (provider string, resumable bool) {
	if cfg.ReviewProvider == config.ExecutorCodex {
		return config.ExternalReviewToolCodex, cfg.IsRealCodexCommand()
	}
	return config.ExternalReviewToolClaude, cfg.IsRealClaudeCommand()
}

// acpChatContext gathers what a fresh chat session is told about the thread. Without a recorded
// run it still names the branch, the base, and the branch's commits.
func acpChatContext(s acpSession, branch string) processor.ACPChatContext {
	base := acpActionBase(s.gitSvc, s.cfg, s.state.LastRun)
	chat := processor.ACPChatContext{Branch: branch, Base: base, Log: acpBranchLog(s.gitSvc, base)}
	if run := s.state.LastRun; run != nil {
		chat.Report = run.Report
		chat.MergeOutcome = run.MergeSummary
	}
	return chat
}

// acpChatLogLimit caps the commits a fresh chat session is shown; the session can read the rest.
const acpChatLogLimit = 50

// acpBranchLog renders the commits on HEAD that base lacks as git log --oneline would, newest first,
// or "" when there are none or the range cannot be read.
func acpBranchLog(gitSvc *git.Service, base string) string {
	if base == "" {
		return ""
	}
	commits, err := gitSvc.CommitsBetween(base, "HEAD")
	if err != nil {
		return ""
	}
	lines := make([]string, 0, min(len(commits), acpChatLogLimit+1))
	for i, c := range commits {
		if i == acpChatLogLimit {
			lines = append(lines, fmt.Sprintf("... and %d more", len(commits)-i))
			break
		}
		lines = append(lines, shortSHA(c.Hash)+" "+c.Subject)
	}
	return strings.Join(lines, "\n")
}
