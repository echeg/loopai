package phase

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

// chatCall records what one chat executor run received.
type chatCall struct {
	prompt   string
	resumeID string
}

// newChatExecutor returns an executor replaying results in order and recording each call's
// prompt and the resume id carried on its context.
func newChatExecutor(results ...executor.Result) (*executorMock, *[]chatCall) {
	calls := &[]chatCall{}
	exec := &executorMock{RunFunc: func(ctx context.Context, prompt string) executor.Result {
		*calls = append(*calls, chatCall{prompt: prompt, resumeID: executor.ResumeSession(ctx)})
		if len(*calls) <= len(results) {
			return results[len(*calls)-1]
		}
		return executor.Result{}
	}}
	return exec, calls
}

func newChatPhase(t *testing.T, exec Executor, log *mockLogger, policy Policy) *ChatPhase {
	t.Helper()
	cfg := Config{AppConfig: testAppConfig(t), TaskProvider: config.ExternalReviewToolCodex}
	if policy == nil {
		policy = newTestPolicy(cfg, log)
	}
	return NewChatPhase(ChatPhaseOpts{Cfg: cfg, Log: log, Exec: exec, Policy: policy, Prompts: testPrompts{}})
}

var testChatRequest = ChatRequest{Context: ChatContext{Branch: "feature"}, Message: "why medium risk?"}

func TestChatPhase_FreshTurn(t *testing.T) {
	exec, calls := newChatExecutor(executor.Result{Output: "because of X\n", SessionID: "s-1"})
	log := newMockLogger("progress.txt")
	policy := newTestPolicy(Config{}, log)
	p := newChatPhase(t, exec, log, policy)

	res, err := p.Run(t.Context(), testChatRequest)

	require.NoError(t, err)
	assert.Equal(t, ChatResult{Answer: "because of X", SessionID: "s-1"}, res)
	require.Len(t, *calls, 1)
	assert.Equal(t, chatCall{prompt: "chat prompt on feature\nwhy medium risk?"}, (*calls)[0])
	assert.Equal(t, []string{config.ExternalReviewToolClaude}, policy.toolNames, "chat runs the review provider, not the task one")
	require.Len(t, log.PrintSectionCalls(), 1)
	assert.Equal(t, "chat", log.PrintSectionCalls()[0].Section.Label)
}

func TestChatPhase_ResumedTurn(t *testing.T) {
	exec, calls := newChatExecutor(executor.Result{Output: "done", SessionID: "s-1"})
	p := newChatPhase(t, exec, newMockLogger("progress.txt"), nil)
	req := testChatRequest
	req.ResumeID = "s-1"

	res, err := p.Run(t.Context(), req)

	require.NoError(t, err)
	assert.Equal(t, ChatResult{Answer: "done", SessionID: "s-1", Resumed: true}, res)
	require.Len(t, *calls, 1)
	assert.Equal(t, chatCall{prompt: "why medium risk?", resumeID: "s-1"}, (*calls)[0], "a resumed turn sends only the message")
}

func TestChatPhase_ResumeFailureRetriesFresh(t *testing.T) {
	exec, calls := newChatExecutor(
		executor.Result{Error: errors.New("claude exited with error: exit status 1")},
		executor.Result{Output: "fresh answer", SessionID: "s-2"},
	)
	log := newMockLogger("progress.txt")
	p := newChatPhase(t, exec, log, nil)
	req := testChatRequest
	req.ResumeID = "gone"

	res, err := p.Run(t.Context(), req)

	require.NoError(t, err)
	assert.Equal(t, ChatResult{Answer: "fresh answer", SessionID: "s-2"}, res)
	require.Len(t, *calls, 2)
	assert.Equal(t, chatCall{prompt: "why medium risk?", resumeID: "gone"}, (*calls)[0])
	assert.Equal(t, chatCall{prompt: "chat prompt on feature\nwhy medium risk?"}, (*calls)[1],
		"the fresh retry carries the full context and no resume id")
	assertLogContains(t, log, "starting a fresh session")
}

func TestChatPhase_ResumeFailureThenFreshFailure(t *testing.T) {
	exec, calls := newChatExecutor(
		executor.Result{Error: errors.New("resume failed")},
		executor.Result{Error: errors.New("still broken")},
	)
	p := newChatPhase(t, exec, newMockLogger("progress.txt"), nil)
	req := testChatRequest
	req.ResumeID = "gone"

	res, err := p.Run(t.Context(), req)

	require.ErrorContains(t, err, "still broken")
	assert.Equal(t, ChatResult{}, res)
	assert.Len(t, *calls, 2, "only one fresh retry")
}

func TestChatPhase_FreshFailureIsNotRetried(t *testing.T) {
	exec, calls := newChatExecutor(executor.Result{Error: errors.New("boom")})
	p := newChatPhase(t, exec, newMockLogger("progress.txt"), nil)

	_, err := p.Run(t.Context(), testChatRequest)

	require.ErrorContains(t, err, "claude execution: boom")
	assert.Len(t, *calls, 1)
}

func TestChatPhase_PatternErrorsAreNotRetriedFresh(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "limit", err: &executor.LimitPatternError{Pattern: "You've hit your limit", HelpCmd: "claude /usage"}},
		{name: "error pattern", err: &executor.PatternMatchError{Pattern: "API Error", HelpCmd: "claude /status"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec, calls := newChatExecutor(executor.Result{Error: tc.err})
			log := newMockLogger("progress.txt")
			p := newChatPhase(t, exec, log, nil)
			req := testChatRequest
			req.ResumeID = "s-1"

			_, err := p.Run(t.Context(), req)

			require.ErrorIs(t, err, tc.err)
			require.ErrorContains(t, err, "claude pattern handling")
			assert.Len(t, *calls, 1, "a fresh session would hit the same pattern")
			assertLogContains(t, log, "error: detected %q in %s output")
		})
	}
}

func TestChatPhase_CancellationReturnsNoSessionID(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	exec := &executorMock{RunFunc: func(_ context.Context, _ string) executor.Result {
		cancel()
		return executor.Result{Output: "partial", SessionID: "s-new", Error: context.Canceled}
	}}
	p := newChatPhase(t, exec, newMockLogger("progress.txt"), nil)
	req := testChatRequest
	req.ResumeID = "s-old"

	res, err := p.Run(ctx, req)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ChatResult{}, res)
	assert.Len(t, exec.RunCalls(), 1, "a canceled resumed turn is not retried fresh")
}

func TestChatPhase_TimeoutIsAnError(t *testing.T) {
	exec, _ := newChatExecutor()
	log := newMockLogger("progress.txt")
	p := newChatPhase(t, exec, log, newScriptedTestPolicy(log, ExecutionResult{TimedOut: true}))
	req := testChatRequest
	req.ResumeID = "s-1"

	res, err := p.Run(t.Context(), req)

	require.ErrorContains(t, err, "timed out")
	assert.Equal(t, ChatResult{}, res)
	assert.Len(t, exec.RunCalls(), 1, "a timeout is not a resume failure")
}

func TestChatPhase_Signals(t *testing.T) {
	tests := []struct {
		name       string
		result     executor.Result
		wantSignal string
		wantAnswer string
	}{
		{name: "merge", result: executor.Result{Output: "merging now\n" + status.ACPMerge + "\n", Signal: status.ACPMerge},
			wantSignal: status.ACPMerge, wantAnswer: "merging now"},
		{name: "pr", result: executor.Result{Output: "opening a PR\n" + status.ACPPR, Signal: status.ACPPR},
			wantSignal: status.ACPPR, wantAnswer: "opening a PR"},
		{name: "signal overwritten by a later block", result: executor.Result{Output: "ok\n" + status.ACPPR + "\nanything else?"},
			wantSignal: status.ACPPR, wantAnswer: "ok\nanything else?"},
		{name: "merge wins over pr", result: executor.Result{Output: status.ACPPR + "\n" + status.ACPMerge, Signal: status.ACPPR},
			wantSignal: status.ACPMerge, wantAnswer: ""},
		{name: "other signal ignored", result: executor.Result{Output: "fine\n" + status.Completed, Signal: status.Completed},
			wantSignal: "", wantAnswer: "fine"},
		{name: "no signal", result: executor.Result{Output: "just an answer"}, wantAnswer: "just an answer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec, _ := newChatExecutor(tc.result)
			p := newChatPhase(t, exec, newMockLogger("progress.txt"), nil)

			res, err := p.Run(t.Context(), testChatRequest)

			require.NoError(t, err)
			assert.Equal(t, tc.wantSignal, res.Signal)
			assert.Equal(t, tc.wantAnswer, res.Answer)
			assert.NotContains(t, res.Answer, "<<<RALPHEX:")
		})
	}
}
