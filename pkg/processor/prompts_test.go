package processor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/processor/mocks"
	"github.com/umputun/ralphex/pkg/status"
)

var promptBuilderCache sync.Map

func newPromptBuilderForTest(r *Runner) *promptBuilder {
	if cached, ok := promptBuilderCache.Load(r); ok {
		return cached.(*promptBuilder)
	}
	log := r.log
	if log == nil {
		log = newMockLogger()
	}
	locator := newPlanLocator(r.cfg)
	builder := newPromptBuilder(promptBuilderOpts{cfg: r.cfg, log: log, locator: locator})
	promptBuilderCache.Store(r, builder)
	return builder
}

func TestRunner_replacePromptVariables_TaskPrompt(t *testing.T) {
	appCfg := testAppConfig(t)
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress-test.txt", AppConfig: appCfg}, log: newMockLogger()}
	prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.TaskPrompt)

	assert.Contains(t, prompt, "docs/plans/test.md")
	assert.Contains(t, prompt, "progress-test.txt")
	assert.Contains(t, prompt, "<<<RALPHEX:ALL_TASKS_DONE>>>")
	assert.Contains(t, prompt, "<<<RALPHEX:TASK_FAILED>>>")
	assert.Contains(t, prompt, "ONE Task section per iteration")
	assert.Contains(t, prompt, "STOP HERE")
}

func TestRunner_replacePromptVariables_ReviewFirstPrompt(t *testing.T) {
	t.Run("with plan file and progress path", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress-test.txt", DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

		assert.Contains(t, prompt, "docs/plans/test.md")
		assert.Contains(t, prompt, "progress-test.txt") // progress file should be substituted
		assert.Contains(t, prompt, "git diff main...HEAD")
		assert.Contains(t, prompt, "<<<RALPHEX:REVIEW_DONE>>>")
		assert.Contains(t, prompt, "<<<RALPHEX:TASK_FAILED>>>")
		// verify expanded agent content from the 5 agents
		assert.Contains(t, prompt, "Use the Task tool to launch a general-purpose agent")
		assert.Contains(t, prompt, "security issues")          // from quality agent
		assert.Contains(t, prompt, "achieves the stated goal") // from implementation agent
		assert.Contains(t, prompt, "test coverage")            // from testing agent
		// verify no unsubstituted template variables remain
		assert.NotContains(t, prompt, "{{DEFAULT_BRANCH}}")
	})

	t.Run("without plan file uses default branch in goal", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{PlanFile: "", ProgressPath: "progress.txt", DefaultBranch: "trunk", AppConfig: appCfg}, log: newMockLogger()}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

		assert.Contains(t, prompt, "current branch vs trunk")
		assert.Contains(t, prompt, "progress.txt")
		assert.Contains(t, prompt, "<<<RALPHEX:REVIEW_DONE>>>")
	})

	t.Run("fallback to master when default branch not set", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{PlanFile: "", ProgressPath: "progress.txt", AppConfig: appCfg}, log: newMockLogger()}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

		assert.Contains(t, prompt, "current branch vs master")
	})
}

func TestRunner_replacePromptVariables_ReviewSecondPrompt(t *testing.T) {
	t.Run("with plan file and progress path", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress-test.txt", DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewSecondPrompt)

		assert.Contains(t, prompt, "docs/plans/test.md")
		assert.Contains(t, prompt, "progress-test.txt") // progress file should be substituted
		assert.Contains(t, prompt, "git diff main...HEAD")
		assert.Contains(t, prompt, "<<<RALPHEX:REVIEW_DONE>>>")
		assert.Contains(t, prompt, "<<<RALPHEX:TASK_FAILED>>>")
		// verify expanded agent content from quality and implementation agents
		assert.Contains(t, prompt, "Use the Task tool to launch a general-purpose agent")
		assert.Contains(t, prompt, "security issues")          // from quality agent
		assert.Contains(t, prompt, "achieves the stated goal") // from implementation agent
		// should NOT have testing agent (only 2 agents for second pass)
		assert.NotContains(t, prompt, "test coverage")
		// verify no unsubstituted template variables remain
		assert.NotContains(t, prompt, "{{DEFAULT_BRANCH}}")
	})

	t.Run("without plan file uses default branch in goal", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{PlanFile: "", ProgressPath: "progress.txt", DefaultBranch: "develop", AppConfig: appCfg}, log: newMockLogger()}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewSecondPrompt)

		assert.Contains(t, prompt, "current branch vs develop")
		assert.Contains(t, prompt, "progress.txt")
	})
}

func TestRunner_replacePromptVariables_NoAgentWarningsInEmbeddedPrompts(t *testing.T) {
	// regression test for issue #98: comment lines in embedded prompts contained {{agent:name}}
	// which triggered "agent not found" warnings after stripComments was removed in #90
	appCfg := testAppConfig(t)
	log := newMockLogger()
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", DefaultBranch: "main", AppConfig: appCfg}, log: log}

	newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)
	newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewSecondPrompt)

	// verify no "not found" warnings were logged
	for _, call := range log.PrintCalls() {
		assert.NotContains(t, call.Format, "not found", "unexpected agent warning: %s", call.Format)
	}
}

func TestRunner_buildCodexEvaluationPrompt(t *testing.T) {
	findings := "Issue 1: Missing error check in foo.go:42"

	r := &Runner{cfg: Config{AppConfig: testAppConfig(t)}, log: newMockLogger()}
	prompt := newPromptBuilderForTest(r).ExternalEvaluationPrompt(config.ExternalReviewToolCodex, findings)

	assert.Contains(t, prompt, findings)
	assert.Contains(t, prompt, "<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>")
	assert.Contains(t, prompt, "Codex reviewed the code")
	assert.Contains(t, prompt, "Valid issues")
	assert.Contains(t, prompt, "Invalid/irrelevant issues")
}

func TestRunner_replacePromptVariables_CustomTaskPrompt(t *testing.T) {
	appCfg := &config.Config{
		TaskPrompt: "Custom task prompt for {{PLAN_FILE}} with progress at {{PROGRESS_FILE}}",
	}
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress-test.txt", AppConfig: appCfg}}
	prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.TaskPrompt)

	assert.Equal(t, "Custom task prompt for docs/plans/test.md with progress at progress-test.txt", prompt)
	// verify it doesn't contain default prompt content
	assert.NotContains(t, prompt, "<<<RALPHEX:ALL_TASKS_DONE>>>")
}

func TestRunner_replacePromptVariables_CustomReviewFirstPrompt(t *testing.T) {
	appCfg := &config.Config{
		ReviewFirstPrompt: "Custom first review for {{GOAL}}",
	}

	t.Run("with plan file", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", AppConfig: appCfg}}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

		assert.Equal(t, "Custom first review for implementation of plan at docs/plans/test.md", prompt)
	})

	t.Run("without plan file uses default branch", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "", DefaultBranch: "main", AppConfig: appCfg}}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

		assert.Equal(t, "Custom first review for current branch vs main", prompt)
	})

	t.Run("without plan file fallback to master", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "", AppConfig: appCfg}}
		prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

		assert.Equal(t, "Custom first review for current branch vs master", prompt)
	})
}

func TestRunner_replacePromptVariables_CustomReviewSecondPrompt(t *testing.T) {
	appCfg := &config.Config{
		ReviewSecondPrompt: "Custom second review for {{GOAL}}",
	}
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", AppConfig: appCfg}}
	prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewSecondPrompt)

	assert.Equal(t, "Custom second review for implementation of plan at docs/plans/test.md", prompt)
}

func TestRunner_buildCodexEvaluationPrompt_CustomPrompt(t *testing.T) {
	appCfg := &config.Config{
		CodexPrompt: "Custom codex evaluation with output: {{CODEX_OUTPUT}} for {{GOAL}}",
	}
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", AppConfig: appCfg}}
	prompt := newPromptBuilderForTest(r).ExternalEvaluationPrompt(config.ExternalReviewToolCodex, "found bug in main.go")

	assert.Equal(t, "Custom codex evaluation with output: found bug in main.go for implementation of plan at docs/plans/test.md", prompt)
}

func TestRunner_replacePromptVariables(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		planFile     string
		progressPath string
		expected     string
	}{
		{name: "plan file variable", input: "Plan: {{PLAN_FILE}}", planFile: "docs/plans/test.md", progressPath: "", expected: "Plan: docs/plans/test.md"},
		{name: "progress file variable", input: "Progress: {{PROGRESS_FILE}}", planFile: "docs/plans/test.md", progressPath: "prog.txt", expected: "Progress: prog.txt"},
		{name: "goal variable", input: "Goal: {{GOAL}}", planFile: "docs/plans/test.md", progressPath: "", expected: "Goal: implementation of plan at docs/plans/test.md"},
		{name: "multiple variables", input: "{{PLAN_FILE}} -> {{PROGRESS_FILE}}", planFile: "docs/plans/test.md", progressPath: "p.txt", expected: "docs/plans/test.md -> p.txt"},
		{name: "no variables", input: "plain text", planFile: "docs/plans/test.md", progressPath: "", expected: "plain text"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{cfg: Config{PlanFile: tc.planFile, ProgressPath: tc.progressPath}}
			result := newPromptBuilderForTest(r).replacePromptVariables(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestRunner_replacePromptVariables_NoGoal(t *testing.T) {
	t.Run("fallback to master when default branch not set", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: ""}}
		result := newPromptBuilderForTest(r).replacePromptVariables("Goal: {{GOAL}}")
		assert.Equal(t, "Goal: current branch vs master", result)
	})

	t.Run("uses configured default branch", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "", DefaultBranch: "trunk"}}
		result := newPromptBuilderForTest(r).replacePromptVariables("Goal: {{GOAL}}")
		assert.Equal(t, "Goal: current branch vs trunk", result)
	})
}

func TestRunner_replacePromptVariables_DefaultBranch(t *testing.T) {
	t.Run("replaces DEFAULT_BRANCH variable", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).replacePromptVariables("git diff {{DEFAULT_BRANCH}}...HEAD")
		assert.Equal(t, "git diff main...HEAD", result)
	})

	t.Run("fallback to master when not configured", func(t *testing.T) {
		r := &Runner{cfg: Config{}}
		result := newPromptBuilderForTest(r).replacePromptVariables("git diff {{DEFAULT_BRANCH}}...HEAD")
		assert.Equal(t, "git diff master...HEAD", result)
	})
}

func TestRunner_getPlanFileRef(t *testing.T) {
	t.Run("with plan file", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md"}}
		assert.Equal(t, "docs/plans/test.md", newPromptBuilderForTest(r).getPlanFileRef())
	})

	t.Run("without plan file", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: ""}}
		assert.Equal(t, "(no plan file - reviewing current branch)", newPromptBuilderForTest(r).getPlanFileRef())
	})
}

func TestRunner_resolvePlanFilePath(t *testing.T) {
	t.Run("empty plan file returns empty", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: ""}}
		assert.Empty(t, newPlanLocator(r.cfg).Path())
	})

	t.Run("file exists at original location", func(t *testing.T) {
		tmpDir := t.TempDir()
		planPath := filepath.Join(tmpDir, "docs", "plans", "test.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o700))
		require.NoError(t, os.WriteFile(planPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: planPath}}
		assert.Equal(t, planPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("file moved to completed directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "test.md")
		completedPath := filepath.Join(completedDir, "test.md")
		require.NoError(t, os.WriteFile(completedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, completedPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("file not found anywhere returns original path", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "/nonexistent/path/plan.md"}}
		assert.Equal(t, "/nonexistent/path/plan.md", newPlanLocator(r.cfg).Path())
	})

	t.Run("getPlanFileRef uses resolved path", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "test.md")
		completedPath := filepath.Join(completedDir, "test.md")
		require.NoError(t, os.WriteFile(completedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, completedPath, newPromptBuilderForTest(r).getPlanFileRef())
	})

	t.Run("getGoal uses resolved path", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "test.md")
		completedPath := filepath.Join(completedDir, "test.md")
		require.NoError(t, os.WriteFile(completedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Contains(t, newPromptBuilderForTest(r).getGoal(), completedPath)
		assert.NotContains(t, newPromptBuilderForTest(r).getGoal(), originalPath)
	})

	t.Run("dashed file moved+renamed to compact in completed", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "2026-05-12-foo.md")
		renamedPath := filepath.Join(completedDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, renamedPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("compact file moved+renamed to dashed in completed", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "20260512-foo.md")
		renamedPath := filepath.Join(completedDir, "2026-05-12-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, renamedPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("non-date basename returns original path", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		// file does not exist at original location, completed/, or any alternate; helper
		// returns "" because basename matches neither date pattern.
		originalPath := filepath.Join(plansDir, "feature-x.md")
		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, originalPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("dashed file renamed in place to compact (same dir)", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o700))

		originalPath := filepath.Join(plansDir, "2026-05-12-foo.md")
		renamedPath := filepath.Join(plansDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, renamedPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("compact file renamed in place to dashed (same dir)", func(t *testing.T) {
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o700))

		originalPath := filepath.Join(plansDir, "20260512-foo.md")
		renamedPath := filepath.Join(plansDir, "2026-05-12-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# plan"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, renamedPath, newPlanLocator(r.cfg).Path())
	})

	t.Run("in-place alt wins over stale completed copy", func(t *testing.T) {
		// when both an in-place alt-format file and a completed/<basename> exist,
		// the in-place alt takes precedence (it is the current file, the completed/ copy is stale)
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "2026-05-12-foo.md")
		inPlaceAlt := filepath.Join(plansDir, "20260512-foo.md")
		staleCompleted := filepath.Join(completedDir, "2026-05-12-foo.md")
		require.NoError(t, os.WriteFile(inPlaceAlt, []byte("# current"), 0o600))
		require.NoError(t, os.WriteFile(staleCompleted, []byte("# stale"), 0o600))

		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, inPlaceAlt, newPlanLocator(r.cfg).Path())
	})

	t.Run("8-digit non-date prefix is treated as date", func(t *testing.T) {
		// pins behavior of the loose compact regex: it does not validate that the 8 digits
		// form a real calendar date. helper produces a candidate, but file-not-found
		// short-circuits before any harm and the fallback returns the original path.
		tmpDir := t.TempDir()
		plansDir := filepath.Join(tmpDir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o700))

		originalPath := filepath.Join(plansDir, "12345678-foo.md")
		r := &Runner{cfg: Config{PlanFile: originalPath}}
		assert.Equal(t, originalPath, newPlanLocator(r.cfg).Path())
	})
}

func TestRunner_getProgressFileRef(t *testing.T) {
	t.Run("with progress path", func(t *testing.T) {
		r := &Runner{cfg: Config{ProgressPath: "progress-test.txt"}}
		assert.Equal(t, "progress-test.txt", newPromptBuilderForTest(r).getProgressFileRef())
	})

	t.Run("without progress path", func(t *testing.T) {
		r := &Runner{cfg: Config{ProgressPath: ""}}
		assert.Equal(t, "(no progress file available)", newPromptBuilderForTest(r).getProgressFileRef())
	})
}

func TestRunner_replacePromptVariables_Fallbacks(t *testing.T) {
	t.Run("empty plan file uses fallback", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "", ProgressPath: "progress.txt"}}
		result := newPromptBuilderForTest(r).replacePromptVariables("Plan: {{PLAN_FILE}}")
		assert.Equal(t, "Plan: (no plan file - reviewing current branch)", result)
	})

	t.Run("empty progress path uses fallback", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "test.md", ProgressPath: ""}}
		result := newPromptBuilderForTest(r).replacePromptVariables("Progress: {{PROGRESS_FILE}}")
		assert.Equal(t, "Progress: (no progress file available)", result)
	})

	t.Run("both empty use fallbacks", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "", ProgressPath: ""}}
		result := newPromptBuilderForTest(r).replacePromptVariables("Plan: {{PLAN_FILE}}, Progress: {{PROGRESS_FILE}}, Goal: {{GOAL}}")
		assert.Equal(t, "Plan: (no plan file - reviewing current branch), Progress: (no progress file available), Goal: current branch vs master", result)
	})
}

func TestRunner_expandAgentReferences_SingleAgent(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "security-scanner", Prompt: "scan for security vulnerabilities"}},
	}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	prompt := "Check code:\n{{agent:security-scanner}}\nDone."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	assert.Contains(t, result, "Use the Task tool to launch a general-purpose agent with this prompt:")
	assert.Contains(t, result, "scan for security vulnerabilities")
	assert.Contains(t, result, "Report findings only - no positive observations.")
	assert.NotContains(t, result, "{{agent:security-scanner}}")
}

func TestRunner_expandAgentReferences_MultipleAgents(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{
			{Name: "agent-a", Prompt: "first agent prompt"},
			{Name: "agent-b", Prompt: "second agent prompt"},
		},
	}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	prompt := "Run {{agent:agent-a}} then {{agent:agent-b}}."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	assert.Contains(t, result, "first agent prompt")
	assert.Contains(t, result, "second agent prompt")
	assert.NotContains(t, result, "{{agent:agent-a}}")
	assert.NotContains(t, result, "{{agent:agent-b}}")
}

func TestRunner_expandAgentReferences_MissingAgent(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "existing", Prompt: "exists"}},
	}
	log := newMockLogger()
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: log}

	prompt := "Run {{agent:missing-agent}} now."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	// missing agent should remain unexpanded
	assert.Contains(t, result, "{{agent:missing-agent}}")
	assert.NotContains(t, result, "Use the Task tool")

	// verify warning was logged
	calls := log.PrintCalls()
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0].Format, "[WARN]")
	assert.Contains(t, calls[0].Format, "not found")
}

func TestRunner_expandAgentReferences_NilAppConfig(t *testing.T) {
	r := &Runner{cfg: Config{AppConfig: nil}}
	prompt := "Run {{agent:test}} now."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)
	assert.Equal(t, prompt, result)
}

func TestRunner_expandAgentReferences_EmptySlice(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{}}
	r := &Runner{cfg: Config{AppConfig: appCfg}}

	prompt := "Run {{agent:test}} now."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	// empty agents slice, prompt unchanged
	assert.Equal(t, prompt, result)
}

func TestRunner_expandAgentReferences_NilAgentsSlice(t *testing.T) {
	appCfg := &config.Config{CustomAgents: nil}
	r := &Runner{cfg: Config{AppConfig: appCfg}}

	prompt := "Run {{agent:some-agent}} now."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	// nil agents slice, prompt unchanged
	assert.Equal(t, prompt, result)
}

func TestRunner_expandAgentReferences_NoReferences(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "scanner", Prompt: "scan code"}},
	}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	prompt := "Plain prompt without agent references."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	assert.Equal(t, prompt, result)
}

func TestRunner_expandAgentReferences_MixedVariables(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "reviewer", Prompt: "review the code"}},
	}
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", AppConfig: appCfg}, log: newMockLogger()}

	// test that agent refs work alongside other variables in replacePromptVariables
	prompt := "Plan: {{PLAN_FILE}}, Goal: {{GOAL}}, Agent: {{agent:reviewer}}"
	result := newPromptBuilderForTest(r).replacePromptVariables(prompt)

	assert.Contains(t, result, "Plan: docs/plans/test.md")
	assert.Contains(t, result, "Goal: implementation of plan at docs/plans/test.md")
	assert.Contains(t, result, "review the code")
	assert.NotContains(t, result, "{{agent:reviewer}}")
}

func TestRunner_expandAgentReferences_DuplicateReferences(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "scanner", Prompt: "scan for issues"}},
	}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	prompt := "First: {{agent:scanner}}\nSecond: {{agent:scanner}}"
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	// both references should be expanded
	assert.NotContains(t, result, "{{agent:scanner}}")
	// count occurrences of expansion
	assert.Equal(t, 2, strings.Count(result, "Use the Task tool to launch a general-purpose agent"))
	assert.Equal(t, 2, strings.Count(result, "scan for issues"))
}

func TestRunner_expandAgentReferences_SpecialCharactersInPrompt(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{
			{Name: "regex-agent", Prompt: "check for patterns and $variables\nwith newlines\tand tabs"},
		},
	}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	prompt := "Run {{agent:regex-agent}} now."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	// prompt with special characters preserves newlines and tabs
	assert.NotContains(t, result, "{{agent:regex-agent}}")
	assert.Contains(t, result, "Use the Task tool to launch a general-purpose agent")
	assert.Contains(t, result, "$variables")
	// verify actual newlines/tabs are preserved (not escaped as \n \t)
	assert.Contains(t, result, "\n")
	assert.Contains(t, result, "\t")
}

func TestRunner_expandAgentReferences_ExpandsVariablesInContent(t *testing.T) {
	t.Run("expands all template variables in agent content", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{
				{Name: "review", Prompt: "review changes on {{DEFAULT_BRANCH}}, plan: {{PLAN_FILE}}, goal: {{GOAL}}"},
			},
		}
		r := &Runner{cfg: Config{PlanFile: "docs/plan.md", DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}

		prompt := "Run {{agent:review}}"
		result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

		assert.Contains(t, result, "review changes on main")
		assert.Contains(t, result, "plan: docs/plan.md")
		assert.Contains(t, result, "goal: implementation of plan at docs/plan.md")
		assert.NotContains(t, result, "{{DEFAULT_BRANCH}}")
		assert.NotContains(t, result, "{{PLAN_FILE}}")
		assert.NotContains(t, result, "{{GOAL}}")
	})

	t.Run("uses fallbacks when config values not set", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{
				{Name: "review", Prompt: "diff {{DEFAULT_BRANCH}}..HEAD"},
			},
		}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

		prompt := "Run {{agent:review}}"
		result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

		assert.Contains(t, result, "diff master..HEAD")
	})
}

func TestRunner_expandAgentReferences_CaseSensitivity(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "Scanner", Prompt: "uppercase name"}},
	}

	t.Run("lowercase reference does not match uppercase agent", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
		prompt := "Run {{agent:scanner}} now."
		result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

		assert.Contains(t, result, "{{agent:scanner}}")
		assert.NotContains(t, result, "uppercase name")
	})

	t.Run("exact case matches", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
		prompt := "Run {{agent:Scanner}} now."
		result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

		assert.NotContains(t, result, "{{agent:Scanner}}")
		assert.Contains(t, result, "uppercase name")
	})
}

func TestRunner_expandAgentReferences_WithModelAndAgentType(t *testing.T) {
	t.Run("both model and agent type", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{
				{Name: "docs", Prompt: "Check docs.", Options: config.Options{Model: "haiku", AgentType: "code-reviewer"}},
			},
		}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

		result := newPromptBuilderForTest(r).expandAgentReferences("Launch {{agent:docs}}")
		assert.Contains(t, result, "model=haiku")
		assert.Contains(t, result, "code-reviewer")
		assert.Contains(t, result, "Check docs.")
		assert.NotContains(t, result, "general-purpose")
	})

	t.Run("model only uses default agent type", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{
				{Name: "lint", Prompt: "Lint code.", Options: config.Options{Model: "sonnet"}},
			},
		}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

		result := newPromptBuilderForTest(r).expandAgentReferences("Run {{agent:lint}}")
		assert.Contains(t, result, "model=sonnet")
		assert.Contains(t, result, "general-purpose")
		assert.Contains(t, result, "Lint code.")
	})

	t.Run("agent type only uses no model clause", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{
				{Name: "review", Prompt: "Review code.", Options: config.Options{AgentType: "code-reviewer"}},
			},
		}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

		result := newPromptBuilderForTest(r).expandAgentReferences("Run {{agent:review}}")
		assert.NotContains(t, result, "model=")
		assert.Contains(t, result, "code-reviewer")
		assert.Contains(t, result, "Review code.")
	})

	t.Run("no overrides uses defaults", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{
				{Name: "basic", Prompt: "Basic check."},
			},
		}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

		result := newPromptBuilderForTest(r).expandAgentReferences("Run {{agent:basic}}")
		assert.NotContains(t, result, "model=")
		assert.Contains(t, result, "general-purpose")
		assert.Contains(t, result, "Basic check.")
	})
}

func TestRunner_expandAgentReferences_PercentInPrompt(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{
			{Name: "perf", Prompt: "check if CPU is below 80% and memory under 90%"},
		},
	}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	prompt := "Run {{agent:perf}} now."
	result := newPromptBuilderForTest(r).expandAgentReferences(prompt)

	assert.Contains(t, result, "80%")
	assert.Contains(t, result, "90%")
	assert.NotContains(t, result, "{{agent:perf}}")
}

func TestRunner_buildPlanPrompt(t *testing.T) {
	t.Run("substitutes plan description and progress file", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanDescription: "add user authentication with OAuth",
			ProgressPath:    "progress-plan-test.txt",
			AppConfig:       appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).PlanPrompt()

		// verify template substitution
		assert.Contains(t, prompt, "add user authentication with OAuth")
		assert.Contains(t, prompt, "progress-plan-test.txt")
		// verify no unsubstituted variables
		assert.NotContains(t, prompt, "{{PLAN_DESCRIPTION}}")
		assert.NotContains(t, prompt, "{{PROGRESS_FILE}}")
	})

	t.Run("uses progress file fallback when empty", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanDescription: "add feature",
			ProgressPath:    "", // empty progress path
			AppConfig:       appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).PlanPrompt()

		assert.Contains(t, prompt, "add feature")
		assert.Contains(t, prompt, "(no progress file available)")
	})

	t.Run("uses custom plans dir from config", func(t *testing.T) {
		appCfg := testAppConfig(t)
		appCfg.PlansDir = "custom/plans"
		r := &Runner{cfg: Config{
			PlanDescription: "test plan",
			ProgressPath:    "progress.txt",
			AppConfig:       appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).PlanPrompt()

		assert.Contains(t, prompt, "custom/plans/")
		assert.NotContains(t, prompt, "{{PLANS_DIR}}")
	})

	t.Run("preserves prompt structure", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanDescription: "test plan",
			ProgressPath:    "progress.txt",
			AppConfig:       appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).PlanPrompt()

		// verify key structural elements from make_plan.txt are present
		assert.Contains(t, prompt, "QUESTION")
		assert.Contains(t, prompt, "PLAN_READY")
		assert.Contains(t, prompt, "docs/plans/")
	})

	t.Run("custom prompt", func(t *testing.T) {
		appCfg := &config.Config{
			MakePlanPrompt: "Create plan for: {{PLAN_DESCRIPTION}}\nLog: {{PROGRESS_FILE}}",
		}
		r := &Runner{cfg: Config{
			PlanDescription: "custom feature",
			ProgressPath:    "custom-progress.txt",
			AppConfig:       appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).PlanPrompt()

		assert.Equal(t, "Create plan for: custom feature\nLog: custom-progress.txt", prompt)
	})
}

func TestRunner_getDiffInstruction(t *testing.T) {
	t.Run("first iteration uses branch diff", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).getDiffInstruction(true)
		assert.Equal(t, "git diff main...HEAD", result)
	})

	t.Run("subsequent iteration uses uncommitted diff", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).getDiffInstruction(false)
		assert.Equal(t, "git diff", result)
	})

	t.Run("uses default branch fallback", func(t *testing.T) {
		r := &Runner{cfg: Config{}}
		result := newPromptBuilderForTest(r).getDiffInstruction(true)
		assert.Equal(t, "git diff master...HEAD", result)
	})
}

func TestRunner_replaceVariablesWithIteration(t *testing.T) {
	t.Run("replaces DIFF_INSTRUCTION for first iteration", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Run: {{DIFF_INSTRUCTION}}", true, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")
		assert.Equal(t, "Run: git diff main...HEAD", result)
	})

	t.Run("replaces DIFF_INSTRUCTION for subsequent iteration", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Run: {{DIFF_INSTRUCTION}}", false, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")
		assert.Equal(t, "Run: git diff", result)
	})

	t.Run("replaces all variables together", func(t *testing.T) {
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			ProgressPath:  "progress.txt",
			DefaultBranch: "develop",
		}}
		prompt := "Plan: {{PLAN_FILE}}, Progress: {{PROGRESS_FILE}}, Goal: {{GOAL}}, Branch: {{DEFAULT_BRANCH}}, Diff: {{DIFF_INSTRUCTION}}"
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration(prompt, true, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")

		assert.Contains(t, result, "Plan: docs/plans/test.md")
		assert.Contains(t, result, "Progress: progress.txt")
		assert.Contains(t, result, "Goal: implementation of plan at docs/plans/test.md")
		assert.Contains(t, result, "Branch: develop")
		assert.Contains(t, result, "Diff: git diff develop...HEAD")
		assert.NotContains(t, result, "{{")
	})

	t.Run("expands agent references", func(t *testing.T) {
		appCfg := &config.Config{
			CustomAgents: []config.CustomAgent{{Name: "test-agent", Prompt: "test prompt"}},
		}
		r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Diff: {{DIFF_INSTRUCTION}}, Agent: {{agent:test-agent}}", true, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")

		assert.Contains(t, result, "Diff: git diff main...HEAD")
		assert.Contains(t, result, "test prompt")
		assert.NotContains(t, result, "{{agent:test-agent}}")
	})

	t.Run("handles prompt without DIFF_INSTRUCTION", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Plan: {{PLAN_FILE}}", true, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")
		assert.Contains(t, result, "(no plan file - reviewing current branch)")
	})
}

func TestRunner_buildCustomReviewPrompt(t *testing.T) {
	t.Run("first iteration uses branch diff", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCustom, true, "")

		assert.Contains(t, prompt, "git diff main...HEAD")
		assert.Contains(t, prompt, "docs/plans/test.md")
		assert.NotContains(t, prompt, "{{DIFF_INSTRUCTION}}")
		assert.NotContains(t, prompt, "{{PLAN_FILE}}")
		assert.NotContains(t, prompt, "{{PREVIOUS_REVIEW_CONTEXT}}")
		assert.NotContains(t, prompt, "PREVIOUS REVIEW CONTEXT")
	})

	t.Run("subsequent iteration uses uncommitted diff", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCustom, false, "")

		assert.Contains(t, prompt, "git diff")
		assert.NotContains(t, prompt, "main...HEAD")
	})

	t.Run("appends claude response context when present", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCustom, false, "I fixed the null pointer issue")

		assert.Contains(t, prompt, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, prompt, "I fixed the null pointer issue")
		assert.Contains(t, prompt, "Re-evaluate considering Claude's response")
		assert.NotContains(t, prompt, "{{PREVIOUS_REVIEW_CONTEXT}}")
	})

	t.Run("custom prompt with PREVIOUS_REVIEW_CONTEXT variable", func(t *testing.T) {
		appCfg := &config.Config{
			CustomReviewPrompt: "Review code.\n{{PREVIOUS_REVIEW_CONTEXT}}",
		}
		r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}

		t.Run("empty on first iteration", func(t *testing.T) {
			prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCustom, true, "")
			assert.Equal(t, "Review code.\n", prompt)
			assert.NotContains(t, prompt, "PREVIOUS REVIEW CONTEXT")
		})

		t.Run("populated on subsequent iteration", func(t *testing.T) {
			prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCustom, false, "addressed the race condition")
			assert.Contains(t, prompt, "PREVIOUS REVIEW CONTEXT")
			assert.Contains(t, prompt, "addressed the race condition")
			assert.NotContains(t, prompt, "{{PREVIOUS_REVIEW_CONTEXT}}")
		})
	})

	t.Run("custom prompt template", func(t *testing.T) {
		appCfg := &config.Config{
			CustomReviewPrompt: "Review {{GOAL}} using {{DIFF_INSTRUCTION}}. Branch: {{DEFAULT_BRANCH}}",
		}
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/feature.md",
			DefaultBranch: "develop",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCustom, true, "")

		assert.Contains(t, prompt, "implementation of plan at docs/plans/feature.md")
		assert.Contains(t, prompt, "git diff develop...HEAD")
		assert.Contains(t, prompt, "Branch: develop")
	})
}

func TestRunner_buildCustomEvaluationPrompt(t *testing.T) {
	t.Run("replaces CUSTOM_OUTPUT variable", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		customOutput := "Found issue in foo.go:10 - potential null pointer"
		prompt := newPromptBuilderForTest(r).ExternalEvaluationPrompt(config.ExternalReviewToolCustom, customOutput)

		assert.Contains(t, prompt, customOutput)
		assert.NotContains(t, prompt, "{{CUSTOM_OUTPUT}}")
	})

	t.Run("replaces base variables", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/feature.md",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalEvaluationPrompt(config.ExternalReviewToolCustom, "test output")

		assert.Contains(t, prompt, "docs/plans/feature.md")
		assert.NotContains(t, prompt, "{{PLAN_FILE}}")
	})

	t.Run("custom prompt template", func(t *testing.T) {
		appCfg := &config.Config{
			CustomEvalPrompt: "Evaluate output: {{CUSTOM_OUTPUT}}. Goal: {{GOAL}}",
		}
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalEvaluationPrompt(config.ExternalReviewToolCustom, "security issue found")

		assert.Equal(t, "Evaluate output: security issue found. Goal: implementation of plan at docs/plans/test.md", prompt)
	})
}

func TestRunner_buildPreviousContext(t *testing.T) {
	r := &Runner{cfg: Config{}}

	t.Run("empty on first iteration (no response)", func(t *testing.T) {
		result := newPromptBuilderForTest(r).buildExternalPreviousContext(config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")
		assert.Empty(t, result)
	})

	t.Run("populated with response on subsequent iterations", func(t *testing.T) {
		result := newPromptBuilderForTest(r).buildExternalPreviousContext(config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "I fixed the null pointer issue")
		assert.Contains(t, result, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, result, "I fixed the null pointer issue")
		assert.Contains(t, result, "Re-evaluate considering Claude's response")
	})
}

func TestRunner_buildExternalClaudePrompts(t *testing.T) {
	appCfg := testAppConfig(t)
	appCfg.Executor = config.ExecutorCodex
	appCfg.CommitTrailer = "Signed-off-by: primary-codex"
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
	builder := newPromptBuilderForTest(r)

	review := builder.ExternalReviewPrompt(config.ExternalReviewToolClaude, false, "dismissed finding")
	assert.Contains(t, review, "git diff")
	assert.Contains(t, review, "Codex (primary evaluator) responded to Claude's findings")
	assert.Contains(t, review, "dismissed finding")
	assert.Contains(t, review, "Do not edit")
	assert.Contains(t, review, "side-effecting Bash")
	assert.NotContains(t, review, "Signed-off-by", "external Claude must not receive commit instructions")

	eval := builder.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "issue in main.go:12")
	assert.Contains(t, eval, "issue in main.go:12")
	assert.NotContains(t, eval, "{{CLAUDE_OUTPUT}}")
	assert.Contains(t, eval, "primary executor owns all repository writes")
	assert.Contains(t, eval, status.ExternalReviewDone)
	assert.Contains(t, eval, "Signed-off-by: primary-codex", "primary Codex keeps configured commit instructions")
}

func TestRunner_buildSameProviderExternalClaudePromptsAreRoleNeutral(t *testing.T) {
	appCfg := testAppConfig(t)
	appCfg.Executor = config.ExecutorClaude
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
	builder := newPromptBuilderForTest(r)

	review := builder.ExternalReviewPrompt(config.ExternalReviewToolClaude, true, "")
	eval := builder.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "NO ISSUES FOUND")

	assert.Contains(t, review, "primary executor")
	assert.NotContains(t, review, "primary Codex executor")
	assert.Contains(t, eval, "primary executor owns all repository writes")
	assert.NotContains(t, eval, "Codex owns all repository writes")
}

func TestRunner_replaceVariablesWithIteration_PreviousReviewContext(t *testing.T) {
	t.Run("empty when no claude response", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Review:\n{{PREVIOUS_REVIEW_CONTEXT}}", true, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")
		assert.Equal(t, "Review:\n", result)
		assert.NotContains(t, result, "PREVIOUS REVIEW CONTEXT")
	})

	t.Run("populated when claude response present", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main"}}
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Review:\n{{PREVIOUS_REVIEW_CONTEXT}}", false, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "fixed the bug")
		assert.Contains(t, result, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, result, "fixed the bug")
		assert.NotContains(t, result, "{{PREVIOUS_REVIEW_CONTEXT}}")
	})

	t.Run("works with all variables together", func(t *testing.T) {
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", DefaultBranch: "main", ProgressPath: "progress.txt"}}
		prompt := "Plan: {{PLAN_FILE}}, Diff: {{DIFF_INSTRUCTION}}\n{{PREVIOUS_REVIEW_CONTEXT}}"
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration(prompt, false, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "previous response")

		assert.Contains(t, result, "Plan: docs/plans/test.md")
		assert.Contains(t, result, "Diff: git diff")
		assert.Contains(t, result, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, result, "previous response")
		assert.NotContains(t, result, "{{")
	})

	t.Run("agent refs in claude response not expanded", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: &config.Config{
			CustomAgents: []config.CustomAgent{{Name: "quality", Prompt: "check quality"}},
		}}}
		prompt := "Review:\n{{PREVIOUS_REVIEW_CONTEXT}}"
		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration(prompt, false, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "use {{agent:quality}} for analysis")

		// agent ref in prompt template should be expanded (none here), but agent ref
		// in claude response must stay as literal text - prevents prompt injection
		assert.Contains(t, result, "{{agent:quality}}")
		assert.NotContains(t, result, "subagent_type")
	})
}

func TestRunner_buildCodexPrompt(t *testing.T) {
	t.Run("first iteration with plan file", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			ProgressPath:  "progress.txt",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, true, "")

		assert.Contains(t, prompt, "docs/plans/test.md")
		assert.Contains(t, prompt, "progress.txt")
		assert.Contains(t, prompt, "git diff main...HEAD")
		assert.Contains(t, prompt, "NO ISSUES FOUND")
		assert.NotContains(t, prompt, "PREVIOUS REVIEW CONTEXT")
		assert.NotContains(t, prompt, "{{DIFF_INSTRUCTION}}")
		assert.NotContains(t, prompt, "{{PLAN_FILE}}")
		assert.NotContains(t, prompt, "{{PROGRESS_FILE}}")
		assert.NotContains(t, prompt, "{{PREVIOUS_REVIEW_CONTEXT}}")
	})

	t.Run("subsequent iteration with claude response", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			ProgressPath:  "progress.txt",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, false, "I fixed the null pointer issue")

		assert.Contains(t, prompt, "git diff")
		assert.NotContains(t, prompt, "main...HEAD")
		assert.Contains(t, prompt, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, prompt, "I fixed the null pointer issue")
		assert.Contains(t, prompt, "Re-evaluate considering Claude's response")
	})

	t.Run("first iteration without claude response has no context block", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, true, "")

		assert.NotContains(t, prompt, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, prompt, "Plan: (no plan file - reviewing current branch)")
	})

	t.Run("replaces goal variable", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/feature.md",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, true, "")

		assert.Contains(t, prompt, "implementation of plan at docs/plans/feature.md")
		assert.NotContains(t, prompt, "{{GOAL}}")
	})

	t.Run("agent refs in claude response are not expanded", func(t *testing.T) {
		appCfg := testAppConfig(t)
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/test.md",
			DefaultBranch: "main",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		// simulate claude response containing agent template variable (potential prompt injection)
		response := "I used {{agent:quality}} to check and {{agent:testing}} found issues"
		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, false, response)

		// agent refs must remain as literal text, not expanded into Task tool instructions
		assert.Contains(t, prompt, "{{agent:quality}}")
		assert.Contains(t, prompt, "{{agent:testing}}")
		assert.NotContains(t, prompt, "subagent_type")
	})

	t.Run("custom prompt template", func(t *testing.T) {
		appCfg := &config.Config{
			CodexReviewPrompt: "Review {{GOAL}} using {{DIFF_INSTRUCTION}}. Branch: {{DEFAULT_BRANCH}}\n{{PREVIOUS_REVIEW_CONTEXT}}",
		}
		r := &Runner{cfg: Config{
			PlanFile:      "docs/plans/feature.md",
			DefaultBranch: "develop",
			AppConfig:     appCfg,
		}, log: newMockLogger()}

		prompt := newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, true, "")
		assert.Contains(t, prompt, "implementation of plan at docs/plans/feature.md")
		assert.Contains(t, prompt, "git diff develop...HEAD")
		assert.Contains(t, prompt, "Branch: develop")
		assert.NotContains(t, prompt, "{{")

		prompt = newPromptBuilderForTest(r).ExternalReviewPrompt(config.ExternalReviewToolCodex, false, "fixed the bug")
		assert.Contains(t, prompt, "PREVIOUS REVIEW CONTEXT")
		assert.Contains(t, prompt, "fixed the bug")
		assert.Contains(t, prompt, "git diff")
		assert.NotContains(t, prompt, "develop...HEAD")
	})
}

func TestRunner_appendCommitTrailerInstruction(t *testing.T) {
	t.Run("appends trailer instruction when configured", func(t *testing.T) {
		appCfg := &config.Config{CommitTrailer: "Co-authored-by: ralphex <noreply@ralphex.com>"}
		r := &Runner{cfg: Config{AppConfig: appCfg}}

		result := newPromptBuilderForTest(r).appendCommitTrailerInstruction("do the task")

		assert.Contains(t, result, "do the task")
		assert.Contains(t, result, "When making git commits, add the following trailer")
		assert.Contains(t, result, "Co-authored-by: ralphex <noreply@ralphex.com>")
	})

	t.Run("no change when trailer is empty", func(t *testing.T) {
		appCfg := &config.Config{CommitTrailer: ""}
		r := &Runner{cfg: Config{AppConfig: appCfg}}

		result := newPromptBuilderForTest(r).appendCommitTrailerInstruction("do the task")

		assert.Equal(t, "do the task", result)
	})

	t.Run("no change when AppConfig is nil", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: nil}}

		result := newPromptBuilderForTest(r).appendCommitTrailerInstruction("do the task")

		assert.Equal(t, "do the task", result)
	})
}

func TestRunner_replaceBaseVariables_CommitTrailer(t *testing.T) {
	t.Run("replaceBaseVariables does not append trailer", func(t *testing.T) {
		appCfg := &config.Config{CommitTrailer: "Signed-off-by: bot <bot@example.com>"}
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", DefaultBranch: "main", AppConfig: appCfg}}

		result := newPromptBuilderForTest(r).replaceBaseVariables("Plan: {{PLAN_FILE}}, Branch: {{DEFAULT_BRANCH}}")

		assert.Contains(t, result, "Plan: docs/plans/test.md")
		assert.Contains(t, result, "Branch: main")
		assert.NotContains(t, result, "trailer", "replaceBaseVariables should not append trailer to avoid duplication in agent expansions")
	})

	t.Run("prompt with empty trailer is unchanged", func(t *testing.T) {
		appCfg := &config.Config{CommitTrailer: ""}
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", DefaultBranch: "main", AppConfig: appCfg}}

		result := newPromptBuilderForTest(r).replaceBaseVariables("Plan: {{PLAN_FILE}}")

		assert.Equal(t, "Plan: docs/plans/test.md", result)
		assert.NotContains(t, result, "trailer")
	})

	t.Run("trailer instruction propagates through replacePromptVariables", func(t *testing.T) {
		appCfg := &config.Config{CommitTrailer: "Co-authored-by: test <test@test.com>"}
		r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", AppConfig: appCfg}}

		result := newPromptBuilderForTest(r).replacePromptVariables("Task: {{GOAL}}")

		assert.Contains(t, result, "implementation of plan at docs/plans/test.md")
		assert.Contains(t, result, "Co-authored-by: test <test@test.com>")
	})

	t.Run("trailer instruction propagates through replaceVariablesWithIteration", func(t *testing.T) {
		appCfg := &config.Config{CommitTrailer: "Co-authored-by: test <test@test.com>"}
		r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}}

		result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("Diff: {{DIFF_INSTRUCTION}}", true, config.ExternalReviewToolCodex, config.ExternalReviewToolClaude, "")

		assert.Contains(t, result, "git diff main...HEAD")
		assert.Contains(t, result, "Co-authored-by: test <test@test.com>")
	})
}

func TestRunner_formatAgentExpansion_ClaudeShape(t *testing.T) {
	appCfg := &config.Config{
		CustomAgents: []config.CustomAgent{{Name: "scanner", Prompt: "scan code"}},
	}
	// no agentSyntax set: defaults to claude shape (ExecutorClaude is "")
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}

	result := newPromptBuilderForTest(r).expandAgentReferences("Run {{agent:scanner}} now.")

	assert.Contains(t, result, "Use the Task tool to launch a general-purpose agent with this prompt:")
	assert.Contains(t, result, "scan code")
	assert.Contains(t, result, "git diff master...HEAD", "agent body must carry the review-context lead-in")
	assert.Contains(t, result, "Report findings only - no positive observations.")
	assert.NotContains(t, result, "spawn_agent")
	assert.NotContains(t, result, "{{agent:scanner}}")
}

func TestRunner_reviewContextInstruction(t *testing.T) {
	t.Run("uses default branch fallback", func(t *testing.T) {
		r := &Runner{cfg: Config{}, log: newMockLogger()}
		got := newPromptBuilderForTest(r).reviewContextInstruction()
		assert.Contains(t, got, "git diff master...HEAD")
		assert.Contains(t, got, "git diff --stat master...HEAD")
		assert.Contains(t, got, "read the changed source files")
	})

	t.Run("respects configured default branch", func(t *testing.T) {
		r := &Runner{cfg: Config{DefaultBranch: "develop"}, log: newMockLogger()}
		got := newPromptBuilderForTest(r).reviewContextInstruction()
		assert.Contains(t, got, "git diff develop...HEAD")
		assert.NotContains(t, got, "master")
	})
}

func TestRunner_formatAgentExpansion_CodexShape(t *testing.T) {
	appCfg := &config.Config{
		Executor:     config.ExecutorCodex,
		CustomAgents: []config.CustomAgent{{Name: "scanner", Prompt: "scan code"}},
	}
	r := &Runner{
		cfg: Config{AppConfig: appCfg},
		log: newMockLogger(),
	}

	result := newPromptBuilderForTest(r).expandAgentReferences("Run {{agent:scanner}} now.")

	assert.Contains(t, result, "spawn_agent(agent='reviewer', task='")
	assert.Contains(t, result, "scan code')", "agent body is the tail of the task argument")
	assert.Contains(t, result, `git diff master...HEAD`, "agent body must carry the review-context lead-in")
	assert.Contains(t, result, "Report findings only - no positive observations.")
	// fork_context guidance lives in the section-level codexReviewGuidance block
	// (injected by prependCodexReviewGuidance), not in the per-agent expansion.
	assert.NotContains(t, result, "do not set fork_context")
	assert.NotContains(t, result, "Use the Task tool")
	assert.NotContains(t, result, "{{agent:scanner}}")
}

func TestRunner_prependCodexReviewGuidance(t *testing.T) {
	body := "Review the changes."

	t.Run("codex executor prepends guidance", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: &config.Config{Executor: config.ExecutorCodex}}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).prependCodexReviewGuidance(body)

		assert.True(t, strings.HasPrefix(result, "=== Codex orchestration directives ==="), "guidance block must be at the top")
		assert.Contains(t, result, "Do NOT set fork_context", "spawn_agent guidance present")
		assert.Contains(t, result, "wait_agent", "wait_agent retry guidance present")
		assert.Contains(t, result, "Re-spawn the missing agents ONCE", "explicit one-retry cap")
		assert.True(t, strings.HasSuffix(result, body), "original prompt preserved at the end")
	})

	t.Run("claude executor returns prompt unchanged", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: &config.Config{Executor: "claude"}}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).prependCodexReviewGuidance(body)

		assert.Equal(t, body, result, "non-codex executor must not see codex-specific directives")
	})

	t.Run("empty executor (default claude) returns prompt unchanged", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: &config.Config{}}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).prependCodexReviewGuidance(body)

		assert.Equal(t, body, result, "unset executor must not see codex-specific directives")
	})
}

func TestRunner_prependCodexTaskGuidance(t *testing.T) {
	body := "Execute the next task."

	t.Run("codex executor prepends guidance", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: &config.Config{Executor: config.ExecutorCodex}}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).prependCodexTaskGuidance(body)

		assert.True(t, strings.HasPrefix(result, "=== Codex task-execution directives ==="), "guidance block must be at the top")
		assert.Contains(t, result, "do NOT follow that skill", "skill-precedence directive present")
		assert.Contains(t, result, "this prompt takes precedence", "authoritative-prompt directive present")
		assert.NotContains(t, result, "plan-execution", "directive must stay generic — no specific skill named")
		assert.True(t, strings.HasSuffix(result, body), "original prompt preserved at the end")
	})

	t.Run("claude executor returns prompt unchanged", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: &config.Config{Executor: "claude"}}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).prependCodexTaskGuidance(body)

		assert.Equal(t, body, result, "non-codex executor must not see codex-specific directives")
	})

	t.Run("empty executor (default claude) returns prompt unchanged", func(t *testing.T) {
		r := &Runner{cfg: Config{AppConfig: &config.Config{}}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).prependCodexTaskGuidance(body)

		assert.Equal(t, body, result, "unset executor must not see codex-specific directives")
	})
}

func TestRunner_formatAgentExpansion_AllFiveDefaultAgents(t *testing.T) {
	// load the 5 embedded default agents (quality, implementation, testing, simplification, documentation)
	appCfg := testAppConfig(t)
	require.NotEmpty(t, appCfg.CustomAgents, "embedded defaults must include the 5 agents")

	names := []string{"quality", "implementation", "testing", "simplification", "documentation"}

	// build name -> body map for assertion convenience
	byName := make(map[string]string, len(appCfg.CustomAgents))
	for _, a := range appCfg.CustomAgents {
		byName[a.Name] = a.Prompt
	}
	for _, name := range names {
		require.Contains(t, byName, name, "default agent %q missing from embedded defaults", name)
	}

	for _, name := range names {
		t.Run("claude_"+name, func(t *testing.T) {
			r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
			result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:" + name + "}}")

			assert.Contains(t, result, "Use the Task tool to launch a general-purpose agent with this prompt:")
			assert.NotContains(t, result, "spawn_agent")
			assert.NotContains(t, result, "{{agent:"+name+"}}")
			// inlined agent body present verbatim
			assert.Contains(t, result, byName[name])
		})

		t.Run("codex_"+name, func(t *testing.T) {
			codexCfg := *appCfg
			codexCfg.Executor = config.ExecutorCodex
			r := &Runner{
				cfg: Config{AppConfig: &codexCfg},
				log: newMockLogger(),
			}
			result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:" + name + "}}")

			assert.Contains(t, result, "spawn_agent(agent='reviewer', task='")
			assert.NotContains(t, result, "Use the Task tool")
			assert.NotContains(t, result, "{{agent:"+name+"}}")
			// inlined agent body present with codex single-quoted escaping applied
			// (escapeCodexSingleQuoted: backslash first, then single-quote, then CR, then LF)
			escaped := strings.ReplaceAll(byName[name], `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `'`, `\'`)
			escaped = strings.ReplaceAll(escaped, "\r", `\r`)
			escaped = strings.ReplaceAll(escaped, "\n", `\n`)
			assert.Contains(t, result, escaped)
		})
	}
}

func TestRunner_formatAgentExpansion_CodexIgnoresFrontmatterOverrides(t *testing.T) {
	// codex registers a single reviewer agent globally; frontmatter Model/AgentType
	// overrides on the agent file do not apply because the per-call behavior is
	// carried in the inlined task argument, not in the agent registration.
	appCfg := &config.Config{
		Executor: config.ExecutorCodex,
		CustomAgents: []config.CustomAgent{{
			Name:    "reviewer",
			Prompt:  "do a review",
			Options: config.Options{Model: "opus", AgentType: "qa-expert"},
		}},
	}
	r := &Runner{
		cfg: Config{AppConfig: appCfg},
		log: newMockLogger(),
	}

	mockLog := newMockLogger()
	r.log = mockLog

	result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:reviewer}}")

	assert.Contains(t, result, "spawn_agent(agent='reviewer', task='")
	assert.Contains(t, result, "do a review')", "agent body is the tail of the task argument")
	assert.NotContains(t, result, "qa-expert")
	assert.NotContains(t, result, "with model=opus")

	// verify the warning fires when frontmatter is discarded
	var foundWarn bool
	for _, call := range mockLog.PrintCalls() {
		if strings.Contains(call.Format, "codex mode ignores frontmatter") {
			foundWarn = true
			break
		}
	}
	assert.True(t, foundWarn, "expected codex frontmatter-discard warning")

	// second expansion of the same agent must NOT fire a second warning (dedup by agent name)
	callsBefore := len(mockLog.PrintCalls())
	newPromptBuilderForTest(r).expandAgentReferences("{{agent:reviewer}}")
	newCalls := mockLog.PrintCalls()[callsBefore:]
	for _, call := range newCalls {
		assert.NotContains(t, call.Format, "codex mode ignores frontmatter",
			"warning must fire only once per agent name")
	}
}

func TestRunner_expandAgentReferences_NoCodexWarnWhenFrontmatterEmpty(t *testing.T) {
	// no Model/AgentType set → no warning should fire under codex mode
	appCfg := &config.Config{
		Executor: config.ExecutorCodex,
		CustomAgents: []config.CustomAgent{{
			Name:   "reviewer",
			Prompt: "do a review",
		}},
	}
	mockLog := newMockLogger()
	r := &Runner{
		cfg: Config{AppConfig: appCfg},
		log: mockLog,
	}

	newPromptBuilderForTest(r).expandAgentReferences("{{agent:reviewer}}")

	for _, call := range mockLog.PrintCalls() {
		assert.NotContains(t, call.Format, "codex mode ignores frontmatter",
			"no warning expected when frontmatter is empty")
	}
}

func TestRunner_formatAgentExpansion_PicksShapeFromExecutor(t *testing.T) {
	// formatAgentExpansion reads cfg.AppConfig.Executor directly (no cached agentSyntax
	// field). verifies the per-executor expansion shape choice.
	t.Run("default executor produces claude shape", func(t *testing.T) {
		appCfg := testAppConfig(t)
		appCfg.Executor = config.ExecutorClaude
		appCfg.CustomAgents = []config.CustomAgent{{Name: "scanner", Prompt: "scan"}}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:scanner}}")
		assert.Contains(t, result, "Use the Task tool")
		assert.NotContains(t, result, "spawn_agent")
	})

	t.Run("codex executor produces codex shape", func(t *testing.T) {
		appCfg := testAppConfig(t)
		appCfg.Executor = config.ExecutorCodex
		appCfg.CustomAgents = []config.CustomAgent{{Name: "scanner", Prompt: "scan"}}
		r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
		result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:scanner}}")
		assert.Contains(t, result, "spawn_agent")
		assert.NotContains(t, result, "Use the Task tool")
	})

	t.Run("nil AppConfig defaults to claude shape (no expansion since no agents)", func(t *testing.T) {
		r := &Runner{cfg: Config{}, log: newMockLogger()}
		// without AppConfig, expandAgentReferences returns the prompt unchanged
		result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:scanner}}")
		assert.Equal(t, "{{agent:scanner}}", result)
	})
}

func TestRunner_formatAgentExpansionCodex_EscapesSingleQuotedLiteral(t *testing.T) {
	// regression: agent bodies contain apostrophes (don't, what's, isn't) and
	// occasionally backslashes (path examples). when expanded into
	// spawn_agent(task='<body>'), the body MUST be escaped or it terminates the
	// surrounding single-quoted Python-style literal that codex parses.
	tests := []struct {
		name     string
		input    string
		expected string // the escaped portion that must appear in the output
	}{
		{name: "apostrophe", input: "don't fix this", expected: `don\'t fix this`},
		{name: "multiple apostrophes", input: "what's wrong with don't?", expected: `what\'s wrong with don\'t?`},
		{name: "backslash", input: `path: c:\foo`, expected: `path: c:\\foo`},
		{name: "backslash before apostrophe", input: `\' tricky`, expected: `\\\' tricky`},
		{name: "no escaping needed", input: "plain ascii text", expected: "plain ascii text"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{
				cfg: Config{AppConfig: &config.Config{
					Executor:     config.ExecutorCodex,
					CustomAgents: []config.CustomAgent{{Name: "x", Prompt: tc.input}},
				}},
				log: newMockLogger(),
			}
			result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:x}}")

			// the escaped form must appear inside the spawn_agent(...) wrapper
			assert.Contains(t, result, tc.expected, "escaped body must appear in spawn_agent output")
			// the single-quoted literal wrapper must be balanced: the start marker
			// is preceded by zero or no escape, and the final ') must be there.
			assert.Contains(t, result, "spawn_agent(agent='reviewer', task='")
			assert.Contains(t, result, "')\n\nReport findings only")
		})
	}
}

func TestEscapeCodexSingleQuoted(t *testing.T) {
	// directly test the helper to lock in the escape-order invariant:
	// backslash MUST be escaped first so the apostrophe and newline escapes do not
	// get re-escaped (apostrophe -> \\\' or newline -> \\n).
	r := &Runner{}
	tests := []struct {
		in, want string
	}{
		{in: "", want: ""},
		{in: "plain", want: "plain"},
		{in: "don't", want: `don\'t`},
		{in: `a\b`, want: `a\\b`},
		{in: `mixed: \ and ' end`, want: `mixed: \\ and \' end`},
		{in: `already\'escaped`, want: `already\\\'escaped`}, // backslash-then-quote round-trips
		{in: "line1\nline2", want: `line1\nline2`},
		{in: "line1\r\nline2", want: `line1\r\nline2`},
		{in: "with\ttab", want: `with\ttab`},
		{in: "multi\nline\ndon't", want: `multi\nline\ndon\'t`},
		{in: `path\to\file` + "\n" + "next", want: `path\\to\\file\nnext`},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, newPromptBuilderForTest(r).escapeCodexSingleQuoted(tc.in))
		})
	}
}

func TestRunner_formatAgentExpansionCodex_MultiLineAgentBodyStaysSingleLine(t *testing.T) {
	// regression: default agent bodies in pkg/config/defaults/agents/*.txt contain
	// embedded newlines. when expanded into spawn_agent(task='<body>'), the body
	// MUST have newlines escaped or codex's Python-style single-quoted string parser
	// will treat the newline as a terminator of the task=' literal. verify the entire
	// spawn_agent(...) call stays on a single line and the original newlines appear
	// as the literal escape \n (two chars: backslash + n).
	multiLineBody := "first line\nsecond line\nthird line"
	r := &Runner{
		cfg: Config{AppConfig: &config.Config{
			Executor:     config.ExecutorCodex,
			CustomAgents: []config.CustomAgent{{Name: "ml", Prompt: multiLineBody}},
		}},
		log: newMockLogger(),
	}
	result := newPromptBuilderForTest(r).expandAgentReferences("{{agent:ml}}")

	// extract just the spawn_agent(...) call by isolating the line that starts the wrapper
	// the result includes a trailing "Report findings only..." block on subsequent lines.
	lines := strings.Split(result, "\n")
	require.NotEmpty(t, lines)
	spawnLine := lines[0]
	assert.True(t, strings.HasPrefix(spawnLine, "spawn_agent(agent='reviewer', task='"), "spawn_agent call must start at first line")
	assert.True(t, strings.HasSuffix(spawnLine, "')"), "spawn_agent call must end with ')' on the same line; got %q", spawnLine)
	// embedded newlines from the agent body must NOT appear as raw newlines in the task='...' literal
	assert.NotContains(t, spawnLine, "first line\nsecond line", "raw newline leaked into single-quoted literal")
	// they must appear as the literal escape sequence \n inside the spawn_agent call
	assert.Contains(t, spawnLine, `first line\nsecond line\nthird line`)
}

func TestRunner_expandDynamicAgentCatalog(t *testing.T) {
	tests := []struct {
		name        string
		executor    string
		agents      []config.CustomAgent
		contains    []string
		notContains []string
	}{
		{
			name:     "no agents at all",
			agents:   nil,
			contains: []string{"(no project-specific agents configured)"},
			notContains: []string{"### Available project-specific agents",
				"{{agents:dynamic}}"},
		},
		{
			name: "agents without description are not dynamic",
			agents: []config.CustomAgent{
				{Name: "quality", Prompt: "review quality"},
				{Name: "testing", Prompt: "review tests", Options: config.Options{Description: "  "}},
			},
			contains:    []string{"(no project-specific agents configured)"},
			notContains: []string{"review quality", "review tests"},
		},
		{
			name: "single dynamic agent, claude executor",
			agents: []config.CustomAgent{
				{Name: "sql-guard", Prompt: "check sql queries", Options: config.Options{Description: "reviews raw SQL for injection"}},
			},
			contains: []string{
				"### Available project-specific agents",
				"- sql-guard — reviews raw SQL for injection",
				"Use the Task tool to launch a general-purpose agent with this prompt:",
				"check sql queries",
			},
			notContains: []string{"(no project-specific agents configured)"},
		},
		{
			name: "several dynamic agents sorted by name, non-dynamic excluded",
			agents: []config.CustomAgent{
				{Name: "zebra", Prompt: "zebra body", Options: config.Options{Description: "last one"}},
				{Name: "plain", Prompt: "plain body"},
				{Name: "alpha", Prompt: "alpha body", Options: config.Options{Description: "first one"}},
			},
			contains: []string{
				"- alpha — first one",
				"- zebra — last one",
				"alpha body",
				"zebra body",
			},
			notContains: []string{"plain", "plain body", "(no project-specific agents configured)"},
		},
		{
			name:     "codex executor uses spawn_agent snippet",
			executor: config.ExecutorCodex,
			agents: []config.CustomAgent{
				{Name: "sql-guard", Prompt: "check sql queries", Options: config.Options{Description: "reviews raw SQL"}},
			},
			contains: []string{
				"- sql-guard — reviews raw SQL",
				"spawn_agent(agent='reviewer', task='",
				`check sql queries`,
			},
			notContains: []string{"Use the Task tool"},
		},
		{
			name: "frontmatter model override reflected in claude snippet",
			agents: []config.CustomAgent{
				{Name: "perf", Prompt: "check hot paths", Options: config.Options{Description: "perf review", Model: "opus", AgentType: "code-reviewer"}},
			},
			contains: []string{"Use the Task tool with model=opus to launch a code-reviewer agent with this prompt:"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			appCfg := &config.Config{Executor: tc.executor, CustomAgents: tc.agents}
			r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
			result := newPromptBuilderForTest(r).expandDynamicAgentCatalog("Catalog:\n{{agents:dynamic}}\nEnd.", nil)

			assert.NotContains(t, result, "{{agents:dynamic}}")
			assert.Contains(t, result, "Catalog:")
			assert.Contains(t, result, "End.")
			for _, want := range tc.contains {
				assert.Contains(t, result, want)
			}
			for _, notWant := range tc.notContains {
				assert.NotContains(t, result, notWant)
			}
		})
	}
}

func TestRunner_expandDynamicAgentCatalog_SortedOrder(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "zebra", Prompt: "z", Options: config.Options{Description: "z desc"}},
		{Name: "alpha", Prompt: "a", Options: config.Options{Description: "a desc"}},
		{Name: "middle", Prompt: "m", Options: config.Options{Description: "m desc"}},
	}}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
	result := newPromptBuilderForTest(r).expandDynamicAgentCatalog("{{agents:dynamic}}", nil)

	assert.Less(t, strings.Index(result, "- alpha —"), strings.Index(result, "- middle —"))
	assert.Less(t, strings.Index(result, "- middle —"), strings.Index(result, "- zebra —"))
}

func TestRunner_expandDynamicAgentCatalog_NilAppConfig(t *testing.T) {
	r := &Runner{cfg: Config{AppConfig: nil}, log: newMockLogger()}
	result := newPromptBuilderForTest(r).expandDynamicAgentCatalog("{{agents:dynamic}}", nil)
	assert.Equal(t, "(no project-specific agents configured)", result)
}

func TestRunner_expandDynamicAgentCatalog_NoPlaceholder(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "dyn", Prompt: "body", Options: config.Options{Description: "desc"}},
	}}
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: newMockLogger()}
	prompt := "no placeholders here"
	assert.Equal(t, prompt, newPromptBuilderForTest(r).expandDynamicAgentCatalog(prompt, nil))
}

// a review_first.txt copy installed before the catalog existed drops every dynamic
// agent, so the drop must at least be visible in the progress log
func TestPromptBuilder_FirstReviewPrompt_WarnsWhenCatalogPlaceholderMissing(t *testing.T) {
	assertWarned := func(t *testing.T, log *mocks.LoggerMock, want bool) {
		t.Helper()
		var warned int
		for _, call := range log.PrintCalls() {
			if strings.Contains(fmt.Sprintf(call.Format, call.Args...), "has no {{agents:dynamic}} placeholder") {
				warned++
			}
		}
		if !want {
			assert.Zero(t, warned, "unexpected warning: %#v", log.PrintCalls())
			return
		}
		assert.Equal(t, 1, warned, "warn exactly once per run: %#v", log.PrintCalls())
	}

	t.Run("warns once for dropped dynamic agents", func(t *testing.T) {
		appCfg := &config.Config{
			ReviewFirstPrompt: "customized review prompt without the catalog",
			CustomAgents: []config.CustomAgent{
				{Name: "dyn", Prompt: "body", Options: config.Options{Description: "desc"}},
				{Name: "quality", Prompt: "body"},
			},
		}
		log := newMockLogger()
		b := newPromptBuilderForTest(&Runner{cfg: Config{AppConfig: appCfg}, log: log})

		b.FirstReviewPrompt()
		b.FirstReviewPrompt()

		assertWarned(t, log, true)
		for _, call := range log.PrintCalls() {
			if strings.Contains(call.Format, "placeholder") {
				assert.Contains(t, fmt.Sprintf(call.Format, call.Args...), "dyn")
			}
		}
	})

	t.Run("silent without dynamic agents", func(t *testing.T) {
		appCfg := &config.Config{
			ReviewFirstPrompt: "customized review prompt without the catalog",
			CustomAgents:      []config.CustomAgent{{Name: "quality", Prompt: "body"}},
		}
		log := newMockLogger()
		newPromptBuilderForTest(&Runner{cfg: Config{AppConfig: appCfg}, log: log}).FirstReviewPrompt()

		assertWarned(t, log, false)
	})

	t.Run("silent when the prompt inlines the agent directly", func(t *testing.T) {
		appCfg := &config.Config{
			ReviewFirstPrompt: "run {{agent:dyn}} now",
			CustomAgents: []config.CustomAgent{
				{Name: "dyn", Prompt: "body", Options: config.Options{Description: "desc"}},
			},
		}
		log := newMockLogger()
		newPromptBuilderForTest(&Runner{cfg: Config{AppConfig: appCfg}, log: log}).FirstReviewPrompt()

		assertWarned(t, log, false)
	})

	t.Run("silent when the placeholder is present", func(t *testing.T) {
		appCfg := &config.Config{
			ReviewFirstPrompt: "catalog:\n{{agents:dynamic}}",
			CustomAgents: []config.CustomAgent{
				{Name: "dyn", Prompt: "body", Options: config.Options{Description: "desc"}},
			},
		}
		log := newMockLogger()
		newPromptBuilderForTest(&Runner{cfg: Config{AppConfig: appCfg}, log: log}).FirstReviewPrompt()

		assertWarned(t, log, false)
	})
}

func TestRunner_expandDynamicAgentCatalog_AgentBodyVariablesExpanded(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "dyn", Prompt: "review {{PLAN_FILE}} on {{DEFAULT_BRANCH}}", Options: config.Options{Description: "desc"}},
	}}
	r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
	result := newPromptBuilderForTest(r).expandDynamicAgentCatalog("{{agents:dynamic}}", nil)

	assert.Contains(t, result, "review docs/plans/test.md on main")
	assert.NotContains(t, result, "{{PLAN_FILE}}")
}

func TestRunner_expandDynamicAgentCatalog_SnippetIndentedUnderEntry(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "dyn", Prompt: "line one\n\nline two", Options: config.Options{Description: "desc"}},
	}}
	r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
	result := newPromptBuilderForTest(r).expandDynamicAgentCatalog("{{agents:dynamic}}", nil)

	_, entry, found := strings.Cut(result, "- dyn — desc\n")
	require.True(t, found, "catalog entry header present in %q", result)

	var indented int
	for line := range strings.SplitSeq(entry, "\n") {
		if line == "" {
			continue
		}
		assert.True(t, strings.HasPrefix(line, "  "), "snippet line must be indented under its entry: %q", line)
		indented++
	}
	assert.Positive(t, indented, "entry must carry an invocation snippet")
	assert.Contains(t, result, "  line one")
	assert.Contains(t, result, "  line two")
}

// a described agent that the same prompt already inlines through {{agent:name}} must not
// also appear in the catalog: the review prompt would carry its body twice and launch it twice.
func TestRunner_replacePromptVariables_CatalogSkipsInlinedAgent(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "quality", Prompt: "quality body", Options: config.Options{Description: "described base copy"}},
		{Name: "sql", Prompt: "sql body", Options: config.Options{Description: "reviews raw SQL"}},
	}}
	r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}

	result := newPromptBuilderForTest(r).replacePromptVariables("Step 2: {{agent:quality}}\nStep 2b:\n{{agents:dynamic}}")

	assert.Equal(t, 1, strings.Count(result, "quality body"), "inlined agent body must appear once")
	assert.NotContains(t, result, "- quality —", "inlined agent must not be listed in the catalog")
	assert.Contains(t, result, "- sql — reviews raw SQL", "other dynamic agents still listed")
}

// with every dynamic agent inlined, the catalog renders as empty rather than as a partial list.
func TestRunner_replacePromptVariables_CatalogEmptyWhenAllInlined(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "quality", Prompt: "quality body", Options: config.Options{Description: "described base copy"}},
	}}
	r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}

	result := newPromptBuilderForTest(r).replacePromptVariables("{{agent:quality}}\n{{agents:dynamic}}")

	assert.Contains(t, result, emptyDynamicCatalog)
	assert.Equal(t, 1, strings.Count(result, "quality body"))
}

func TestRunner_agentRefNames(t *testing.T) {
	assert.Empty(t, agentRefNames("no refs here"))
	assert.Equal(t, map[string]bool{"one": true, "two": true},
		agentRefNames("{{agent:one}} {{agent:two}} {{agent:one}} {{agents:dynamic}}"))
}

func TestRunner_expandDynamicAgentCatalog_CodexWarnsFrontmatterDiscarded(t *testing.T) {
	appCfg := &config.Config{Executor: config.ExecutorCodex, CustomAgents: []config.CustomAgent{
		{Name: "dyn", Prompt: "body", Options: config.Options{Description: "desc", Model: "opus", AgentType: "code-reviewer"}},
	}}
	mockLog := newMockLogger()
	r := &Runner{cfg: Config{AppConfig: appCfg}, log: mockLog}

	result := newPromptBuilderForTest(r).expandDynamicAgentCatalog("{{agents:dynamic}}", nil)

	assert.NotContains(t, result, "with model=opus", "codex snippet drops frontmatter overrides")
	assertLogContains(t, mockLog, "codex mode ignores frontmatter")
}

// a {{agents:dynamic}} token inside an agent body must not survive into the catalog pass:
// it would be substituted after the body was already escaped into the codex task='...'
// literal, whose quoting the raw catalog text breaks.
func TestRunner_replacePromptVariables_CatalogPlaceholderInsideAgentBody(t *testing.T) {
	appCfg := &config.Config{Executor: config.ExecutorCodex, CustomAgents: []config.CustomAgent{
		{Name: "base", Prompt: "base body\n{{agents:dynamic}}\ntail"},
		{Name: "dyn", Prompt: "dynamic body", Options: config.Options{Description: "won't be nested"}},
	}}
	r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}

	result := newPromptBuilderForTest(r).replacePromptVariables("{{agent:base}}")

	assert.NotContains(t, result, "{{agents:dynamic}}", "placeholder is stripped from agent bodies")
	assert.NotContains(t, result, "- dyn — won't be nested", "catalog must not be nested inside an agent block")
	assert.Contains(t, result, "base body")
	assert.Contains(t, result, "tail")
	assert.Equal(t, 1, strings.Count(result, "spawn_agent(agent='reviewer', task='"), "exactly one spawn_agent block")
	// the only unescaped apostrophes are the four spawn_agent delimiters:
	// agent='reviewer' and task='...'
	unescaped := strings.Count(result, "'") - strings.Count(result, `\'`)
	assert.Equal(t, 4, unescaped, "catalog text must not inject unescaped quotes into the codex literal")
}

func TestRunner_replacePromptVariables_ExpandsDynamicCatalog(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "base", Prompt: "base body"},
		{Name: "dyn", Prompt: "dynamic body", Options: config.Options{Description: "project specific"}},
	}}
	r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
	result := newPromptBuilderForTest(r).replacePromptVariables("{{agent:base}}\n\n{{agents:dynamic}}")

	assert.NotContains(t, result, "{{agents:dynamic}}")
	assert.NotContains(t, result, "{{agent:base}}")
	assert.Contains(t, result, "base body")
	assert.Contains(t, result, "- dyn — project specific")
	assert.Contains(t, result, "dynamic body")
}

func TestRunner_replacePromptVariables_EmbeddedReviewFirstDynamicCatalog(t *testing.T) {
	tests := []struct {
		name        string
		executor    string
		withDynamic bool
		contains    []string
		notContains []string
	}{
		{
			name:        "claude executor without dynamic agents",
			contains:    []string{"(no project-specific agents configured)", "Step 2b"},
			notContains: []string{"### Available project-specific agents"},
		},
		{
			name:        "claude executor with dynamic agent",
			withDynamic: true,
			contains:    []string{"### Available project-specific agents", "- sql-guard — reviews raw SQL", "check sql queries"},
			notContains: []string{"(no project-specific agents configured)"},
		},
		{
			name:        "codex executor without dynamic agents",
			executor:    config.ExecutorCodex,
			contains:    []string{"(no project-specific agents configured)"},
			notContains: []string{"### Available project-specific agents"},
		},
		{
			name:        "codex executor with dynamic agent",
			executor:    config.ExecutorCodex,
			withDynamic: true,
			contains:    []string{"- sql-guard — reviews raw SQL", "spawn_agent(agent='reviewer', task='"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			appCfg := testAppConfig(t)
			appCfg.Executor = tc.executor
			if tc.withDynamic {
				appCfg.CustomAgents = append(appCfg.CustomAgents, config.CustomAgent{Name: "sql-guard",
					Prompt: "check sql queries", Options: config.Options{Description: "reviews raw SQL"}})
			}
			r := &Runner{cfg: Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt",
				DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
			prompt := newPromptBuilderForTest(r).replacePromptVariables(appCfg.ReviewFirstPrompt)

			assert.NotContains(t, prompt, "{{agents:dynamic}}", "catalog placeholder must be expanded")
			// the placeholder appears once in the template, so the catalog must not be duplicated
			// (e.g. by a header comment line reintroducing the literal placeholder)
			rendered := strings.Count(prompt, "### Available project-specific agents") +
				strings.Count(prompt, "(no project-specific agents configured)")
			assert.Equal(t, 1, rendered, "catalog must be rendered exactly once")
			// the 5 base agents still expand unchanged
			assert.Contains(t, prompt, "security issues")
			for _, want := range tc.contains {
				assert.Contains(t, prompt, want)
			}
			for _, notWant := range tc.notContains {
				assert.NotContains(t, prompt, notWant)
			}
		})
	}
}

func TestRunner_replaceExternalVariablesWithIteration_ExpandsDynamicCatalog(t *testing.T) {
	appCfg := &config.Config{CustomAgents: []config.CustomAgent{
		{Name: "dyn", Prompt: "dynamic body", Options: config.Options{Description: "project specific"}},
	}}
	r := &Runner{cfg: Config{DefaultBranch: "main", AppConfig: appCfg}, log: newMockLogger()}
	result := newPromptBuilderForTest(r).replaceExternalVariablesWithIteration("{{agents:dynamic}}", true, "codex", "claude", "")

	assert.NotContains(t, result, "{{agents:dynamic}}")
	assert.Contains(t, result, "- dyn — project specific")
}

// TestPromptBuilder_BacklogDirPlaceholder covers {{BACKLOG_DIR}} expansion across every
// builder that expands {{PLANS_DIR}}: the placeholder is substituted from the configured
// backlog_dir and falls back to docs/backlog when the key is unset.
func TestPromptBuilder_BacklogDirPlaceholder(t *testing.T) {
	// each prompt field carries the placeholder so the builder output is the resolved path alone
	promptWithPlaceholder := func() *config.Config {
		return &config.Config{
			TaskPrompt:                 "{{BACKLOG_DIR}}",
			ReviewFirstPrompt:          "{{BACKLOG_DIR}}",
			ReviewSecondPrompt:         "{{BACKLOG_DIR}}",
			CodexReviewPrompt:          "{{BACKLOG_DIR}}",
			CodexPrompt:                "{{BACKLOG_DIR}}",
			CustomReviewPrompt:         "{{BACKLOG_DIR}}",
			CustomEvalPrompt:           "{{BACKLOG_DIR}}",
			ExternalClaudeReviewPrompt: "{{BACKLOG_DIR}}",
			ExternalClaudeEvalPrompt:   "{{BACKLOG_DIR}}",
			MakePlanPrompt:             "{{BACKLOG_DIR}}",
			FinalizePrompt:             "{{BACKLOG_DIR}}",
			GenAgentsPrompt:            "{{BACKLOG_DIR}}",
		}
	}

	builders := []struct {
		name  string
		build func(b *promptBuilder) string
	}{
		{"task", func(b *promptBuilder) string { return b.TaskPrompt() }},
		{"review first", func(b *promptBuilder) string { return b.FirstReviewPrompt() }},
		{"review second", func(b *promptBuilder) string { return b.SecondReviewPrompt("") }},
		{"external review codex", func(b *promptBuilder) string {
			return b.ExternalReviewPrompt(config.ExternalReviewToolCodex, true, "")
		}},
		{"external review claude", func(b *promptBuilder) string {
			return b.ExternalReviewPrompt(config.ExternalReviewToolClaude, true, "")
		}},
		{"external review custom", func(b *promptBuilder) string {
			return b.ExternalReviewPrompt(config.ExternalReviewToolCustom, true, "")
		}},
		{"eval codex", func(b *promptBuilder) string {
			return b.ExternalEvaluationPrompt(config.ExternalReviewToolCodex, "findings")
		}},
		{"eval claude", func(b *promptBuilder) string {
			return b.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "findings")
		}},
		{"eval custom", func(b *promptBuilder) string {
			return b.ExternalEvaluationPrompt(config.ExternalReviewToolCustom, "findings")
		}},
		{"plan", func(b *promptBuilder) string { return b.PlanPrompt() }},
		{"finalize", func(b *promptBuilder) string { return b.FinalizePrompt() }},
		{"gen agents", func(b *promptBuilder) string { return b.GenAgentsPrompt() }},
	}

	tests := []struct {
		name       string
		backlogDir string
		want       string
	}{
		{name: "configured value", backlogDir: "custom/backlog", want: "custom/backlog"},
		{name: "unset falls back to default", backlogDir: "", want: "docs/backlog"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			appCfg := promptWithPlaceholder()
			appCfg.BacklogDir = tc.backlogDir
			cfg := Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", DefaultBranch: "main", AppConfig: appCfg}
			builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})
			for _, bc := range builders {
				t.Run(bc.name, func(t *testing.T) {
					prompt := bc.build(builder)
					assert.Contains(t, prompt, tc.want)
					assert.NotContains(t, prompt, "{{BACKLOG_DIR}}")
				})
			}
		})
	}
}

// TestPromptBuilder_BacklogDirNoLiteralLeak guards the embedded prompts: whichever of them
// carry {{BACKLOG_DIR}}, none may reach the executor with the raw placeholder intact.
func TestPromptBuilder_BacklogDirNoLiteralLeak(t *testing.T) {
	appCfg := testAppConfig(t)
	appCfg.BacklogDir = "docs/backlog"
	cfg := Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", DefaultBranch: "main",
		PlanDescription: "add feature", AppConfig: appCfg}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	prompts := map[string]string{
		"task":                   builder.TaskPrompt(),
		"review first":           builder.FirstReviewPrompt(),
		"review second":          builder.SecondReviewPrompt(""),
		"external review codex":  builder.ExternalReviewPrompt(config.ExternalReviewToolCodex, true, ""),
		"external review claude": builder.ExternalReviewPrompt(config.ExternalReviewToolClaude, true, ""),
		"external review custom": builder.ExternalReviewPrompt(config.ExternalReviewToolCustom, true, ""),
		"eval codex":             builder.ExternalEvaluationPrompt(config.ExternalReviewToolCodex, "findings"),
		"eval claude":            builder.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "findings"),
		"eval custom":            builder.ExternalEvaluationPrompt(config.ExternalReviewToolCustom, "findings"),
		"plan":                   builder.PlanPrompt(),
		"finalize":               builder.FinalizePrompt(),
		"gen agents":             builder.GenAgentsPrompt(),
	}
	for name, prompt := range prompts {
		assert.NotContains(t, prompt, "{{BACKLOG_DIR}}", "%s prompt leaks the raw placeholder", name)
	}
}

// TestPromptBuilder_BacklogCaptureInstructions covers the embedded capture convention: the
// prompts on the four capture paths (task, internal review, external evaluation, planning)
// carry the instruction with the configured directory expanded, while the prompts sent to the
// read-only external reviewers deliberately do not - the primary evaluator is the only funnel.
func TestPromptBuilder_BacklogCaptureInstructions(t *testing.T) {
	appCfg := testAppConfig(t)
	appCfg.BacklogDir = "custom/backlog"
	cfg := Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", DefaultBranch: "main",
		PlanDescription: "add feature", AppConfig: appCfg}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	// each capture path carries the same entry contract; phase differs per path, and only the
	// prompts that commit in-phase may instruct a commit - see the mustNotCommit note below.
	capturing := map[string]struct {
		prompt string
		phase  string
		// review and evaluation prompts gate their completion signal on whether issues were
		// found, so they must state that filing is not one; task and plan creation do not.
		notAFix bool
		// evaluation prompts accumulate fixes uncommitted across reviewer iterations; the
		// other capture paths commit in-phase.
		mustNotCommit bool
		// every path that owns a completion signal can end a run green once filing is
		// dismissal-equivalent, so a defect in the branch's own work must never be filable.
		// plan creation is the only exception: it writes no code and emits no such signal.
		ownWorkBound bool
		// review and evaluation additionally run with no plan at all under --review,
		// --external-only, and --codex-only, so their bound must not rest on the plan's scope.
		noPlanBound bool
		// the exact commit rule the path states; the paths are not interchangeable, so a bare
		// "commit" substring would pass on unrelated prompt text.
		commitRule string
		// paths that can fix must not let out-of-scope become an escape hatch from the
		// pre-existing-issues rule; plan creation fixes nothing, so it files instead.
		fixCapable bool
		// the exact per-entry stage instruction, lowercased. asserting a bare "git add" would be
		// satisfied by the end-of-phase commit's own `git add <paths>` and would not notice the
		// stage bullet being deleted, and plan creation stages a pathspec rather than the entry.
		stageRule string
	}{
		"task":          {builder.TaskPrompt(), "phase: task", false, false, true, false, "The entry is committed with this task's changes", true, "stage the new entry with `git add <path>`"},
		"review first":  {builder.FirstReviewPrompt(), "phase: internal review", true, false, true, true, "commit the entry with your fixes, or on its own as `git commit -m \"docs: add backlog entry\" -- <entry>`", true, "stage the new entry with `git add <path>`"},
		"review second": {builder.SecondReviewPrompt(""), "phase: internal review", true, false, true, true, "commit the entry with your fixes, or on its own as `git commit -m \"docs: add backlog entry\" -- <entry>`", true, "stage the new entry with `git add <path>`"},
		"eval codex":    {builder.ExternalEvaluationPrompt(config.ExternalReviewToolCodex, "findings"), "phase: evaluation", true, true, true, true, "", true, ""},
		"eval claude":   {builder.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "findings"), "phase: evaluation", true, true, true, true, "", true, ""},
		"eval custom":   {builder.ExternalEvaluationPrompt(config.ExternalReviewToolCustom, "findings"), "phase: evaluation", true, true, true, true, "", true, ""},
		"plan":          {builder.PlanPrompt(), "phase: planning", false, false, false, false, `git commit -m "docs: add backlog entry" -- <entry>`, false, "run `git add <entry>` then `git commit -m \"docs: add backlog entry\" -- <entry>`"},
	}
	for name, tc := range capturing {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, tc.prompt, "custom/backlog", "expanded backlog directory missing")
			assert.NotContains(t, tc.prompt, "{{BACKLOG_DIR}}")
			assert.NotContains(t, tc.prompt, "docs/backlog", "default path must not be hardcoded in the prompt")

			// the entry format is what makes an entry adoptable by loopai-adopt
			assert.Contains(t, tc.prompt, tc.phase, "entry must record the capture phase")
			assert.Contains(t, tc.prompt, "- severity: minor|major", "entry must record severity")
			assert.Contains(t, tc.prompt, "- area: <primary file or package>", "entry must record area")
			assert.Contains(t, tc.prompt, "update it instead of creating a duplicate",
				"capture must instruct dedup against existing entries")

			// out-of-scope is otherwise an escape hatch from the pre-existing-issues rule. plan
			// creation is exempt: it fixes nothing, so filing is the only thing it can do.
			if tc.fixCapable {
				assert.Contains(t, tc.prompt, "never out of scope - fix it, do not file it",
					"capture must exempt pre-existing linter errors and failing tests")
			}

			// filing is dismissal-equivalent for the signal, so an unbounded out-of-scope category
			// would be a new way to end a review green with a defect this branch introduced. the
			// bound holds with or without a plan: under --review, --external-only, and --codex-only
			// there is no plan for a finding to be out of scope of at all.
			if tc.ownWorkBound {
				assert.Contains(t, tc.prompt, "never file a finding about code this branch changed",
					"paths that own a completion signal must bound the out-of-scope category")
			}
			if tc.noPlanBound {
				assert.Contains(t, tc.prompt, "in scope whether or",
					"the bound must not depend on the no-plan fallback")
			}
			// --review, --external-only, and --codex-only create no branch and no worktree, and the
			// task phase runs without one whenever CreateBranchForPlan short-circuits on an
			// already-checked-out feature branch, which skips its dirty-tree gate entirely. a
			// `git add -A` on any of those paths commits the user's unrelated work in progress.
			assert.Equal(t, strings.Count(tc.prompt, "git add -A"),
				strings.Count(tc.prompt, "Do NOT `git add -A`"),
				"capture prompts must not instruct a sweep of a checkout that was never proved clean")

			// filing must never read as a fix, or an out-of-scope finding would keep a loop alive
			if tc.notAFix {
				assert.Contains(t, tc.prompt, "not a fix", "filing must not count as a fix in the signal logic")
			}

			// the external evaluation prompts accumulate fixes uncommitted across reviewer
			// iterations and commit once at EXTERNAL_REVIEW_DONE; getDiffInstruction shows the
			// reviewer only the uncommitted diff after the first round, so a mid-loop commit
			// hides the accumulated fixes and lets the loop terminate early.
			if tc.mustNotCommit {
				assert.NotContains(t, tc.prompt, "docs: add backlog entry",
					"evaluation prompts must not instruct a mid-loop commit")
				assert.Contains(t, strings.ToLower(tc.prompt), "leave the entry uncommitted",
					"evaluation prompts must defer the entry to the final commit")
				// that final sweep is described as reviewing `git diff`, which never shows a new
				// untracked file, so the entry has to be staged or it is silently dropped
				assert.Contains(t, strings.ToLower(tc.prompt), "stage that one file with `git add` and nothing else",
					"evaluation prompts must stage the entry so the final commit picks it up")
				// with the index deliberately non-empty, a bare `git commit -m` no longer fails with
				// "no changes added to commit" - it succeeds and commits only the staged entry, so the
				// final commit has to stage the unstaged fixes explicitly. it must not do that with
				// `git add -A`: --external-only and --codex-only create no worktree and run in the
				// user's own checkout, where a sweep commits their unrelated work in progress.
				assert.Contains(t, strings.ToLower(tc.prompt), "stage every file you created, modified, or deleted",
					"the final commit must stage the accumulated fixes, not just the staged entry")
				// `git diff HEAD` never lists an untracked file, so enumerating the commit from it
				// alone silently drops a new test or helper written while fixing findings
				assert.Contains(t, tc.prompt, "git status --porcelain",
					"the final review must also enumerate new untracked files")
				assert.Contains(t, strings.ToLower(tc.prompt), "do not `git add -a`",
					"the final commit must not sweep the user's own checkout")
				assert.Contains(t, tc.prompt, "git diff HEAD",
					"the final review must use a staged-inclusive diff")
			} else {
				assert.Contains(t, tc.prompt, tc.commitRule, "in-phase capture paths must state their own commit rule")
				// a backlog entry is always a new untracked file and no capture path sweeps, so
				// nothing picks it up implicitly. the review prompts also offer an entry-only
				// `docs: add backlog entry` commit, and both it and plan creation use the pathspec
				// form, which rejects an untracked path, so each states its own per-entry stage
				assert.Contains(t, strings.ToLower(tc.prompt), tc.stageRule,
					"in-phase capture paths must stage the untracked entry before committing")
			}
		})
	}

	// the task phase is the only capture path whose in-phase commit sits behind a success gate:
	// STEP 3 runs only after validation passes. a fresh --worktree run is removed with --force on
	// failure, so a staged but uncommitted entry dies with it unless TASK_FAILED commits it first.
	taskPrompt := builder.TaskPrompt()
	assert.Contains(t, taskPrompt, `commit it on its own first with`+"\n"+
		"`git commit -m \"docs: add backlog entry\" -- <entry>`",
		"the task phase must commit a filed entry before failing, or the worktree removal discards it")
	// the bound names the branch, not the current iteration: the task phase runs one task per
	// iteration on the same branch, so an earlier task's defect must not become filable.
	assert.Contains(t, taskPrompt, "a defect in this branch's own work - this task's or an\nearlier task's - is in scope",
		"the task phase's own-work bound must cover earlier tasks on the same branch")

	readOnly := map[string]string{
		"external review codex":  builder.ExternalReviewPrompt(config.ExternalReviewToolCodex, true, ""),
		"external review claude": builder.ExternalReviewPrompt(config.ExternalReviewToolClaude, true, ""),
		"external review custom": builder.ExternalReviewPrompt(config.ExternalReviewToolCustom, true, ""),
	}
	for name, prompt := range readOnly {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, prompt, "custom/backlog", "read-only reviewers must not be told to write backlog entries")
		})
	}
}

// TestPromptBuilder_DecisionLogConvention covers the plan-revision convention: the planning
// prompt tells the model to record accepted and rejected critique points in the plan's
// `## Decision Log` section, and that section never carries checkboxes - a checkbox outside a
// task section makes the plan read as unfinished work and costs extra loop iterations.
func TestPromptBuilder_DecisionLogConvention(t *testing.T) {
	cfg := Config{PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", DefaultBranch: "main",
		PlanDescription: "add feature", AppConfig: testAppConfig(t)}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})
	prompt := builder.PlanPrompt()

	require.Contains(t, prompt, "## Decision Log", "planning prompt must define the decision log section")
	assert.Contains(t, prompt, "**accepted**", "decision log must record accepted points")
	assert.Contains(t, prompt, "**rejected**", "decision log must record rejected points")
	assert.Contains(t, prompt, "NEVER put checkboxes in this section",
		"decision log must forbid checkboxes so the plan parser stays inert")
	assert.Contains(t, prompt, "does not re-raise a point already rejected",
		"revision path must state why the log exists")

	// the template block for the section must not itself contain a checkbox. the pattern mirrors
	// plan.checkboxPattern so the guard and the parser cannot disagree about what a checkbox is:
	// a literal "- [ ]"/"- [x]" scan misses "- [X]" and "-  [ ]", which the parser accepts.
	checkbox := regexp.MustCompile(`^\s*-\s+\[[ xX]\]`)
	lines := strings.Split(prompt, "\n")
	idx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Decision Log" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "decision log heading not found")
	for _, line := range lines[idx:] {
		if strings.HasPrefix(strings.TrimSpace(line), "## ") && strings.TrimSpace(line) != "## Decision Log" {
			break
		}
		assert.NotRegexp(t, checkbox, line, "decision log section must not contain checkboxes")
	}
}
