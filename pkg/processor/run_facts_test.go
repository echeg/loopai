package processor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/config"
	gitpkg "github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/plan"
)

type runFactsSourceStub struct {
	commits    []gitpkg.Commit
	commitsErr error
	files      []gitpkg.FileChange
	filesErr   error
	stats      gitpkg.DiffStats
	statsErr   error
	bases      []string
	head       string
}

func (s *runFactsSourceStub) CommitsBetween(base, head string) ([]gitpkg.Commit, error) {
	s.bases = append(s.bases, base)
	s.head = head
	return s.commits, s.commitsErr
}

func (s *runFactsSourceStub) DiffNameStatus(base string) ([]gitpkg.FileChange, error) {
	s.bases = append(s.bases, base)
	return s.files, s.filesErr
}

func (s *runFactsSourceStub) DiffStats(base string) (gitpkg.DiffStats, error) {
	s.bases = append(s.bases, base)
	return s.stats, s.statsErr
}

func TestRunnerCollectRunFacts(t *testing.T) {
	t.Run("collects repository plan and backlog facts", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		planPath := filepath.Join("docs", "plans", "feature.md")
		backlogPath := filepath.Join("custom", "backlog", "follow-up.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Dir(backlogPath), 0o750))
		planContent := "# Feature\n\n" +
			"- [x] ➕ added during work\n" +
			"- [ ] ⚠️ waiting on upstream\n" +
			"- [x] manual check (skipped - not automatable)\n\n" +
			"## Validation Commands\n\n" +
			"- `make test`\n"
		require.NoError(t, os.WriteFile(planPath, []byte(planContent), 0o600))
		require.NoError(t, os.WriteFile(backlogPath, []byte("# Follow-up issue\n\nDetails.\n"), 0o600))

		source := &runFactsSourceStub{
			commits: []gitpkg.Commit{{Hash: "abc", Subject: "feature"}},
			files: []gitpkg.FileChange{
				{Status: "A", Path: filepath.ToSlash(backlogPath)},
				{Status: "M", Path: "pkg/processor/runner.go"},
			},
			stats: gitpkg.DiffStats{Files: 2, Additions: 12, Deletions: 3},
		}
		runner := &Runner{
			cfg: Config{
				PlanFile: planPath, DefaultBranch: "main",
				AppConfig: &config.Config{BacklogDir: filepath.Join("custom", "backlog")},
			},
			log: newMockLogger(), factsSource: source,
		}

		got := runner.collectRunFacts(t.Context())

		assert.Equal(t, source.commits, got.Commits)
		assert.Equal(t, source.files, got.Files)
		assert.Equal(t, source.stats, got.DiffStats)
		assert.Equal(t, []BacklogFile{{
			Status: "A", Path: filepath.ToSlash(backlogPath), Title: "Follow-up issue",
		}}, got.Backlog)
		assert.Equal(t, []string{"make test"}, got.ValidationCommands)
		assert.Equal(t, plan.Drift{
			Added:   []string{"➕ added during work"},
			Blocked: []string{"⚠️ waiting on upstream"},
			Skipped: []string{"manual check (skipped - not automatable)"},
		}, got.Drift)
		assert.Equal(t, []string{"main", "main", "main"}, source.bases)
		assert.Equal(t, "HEAD", source.head)
	})

	t.Run("keeps successful fields when individual sources fail", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		planPath := "plan.md"
		require.NoError(t, os.WriteFile(planPath, []byte("# Feature\n"), 0o600))
		source := &runFactsSourceStub{
			commits:    []gitpkg.Commit{{Hash: "ignored"}},
			commitsErr: errors.New("log unavailable"),
			files:      []gitpkg.FileChange{{Status: "M", Path: "kept.go"}},
			stats:      gitpkg.DiffStats{Files: 99},
			statsErr:   errors.New("stats unavailable"),
		}
		logger := newMockLogger()
		runner := &Runner{
			cfg: Config{PlanFile: planPath, DefaultBranch: "master"},
			log: logger, factsSource: source,
		}

		got := runner.collectRunFacts(t.Context())

		assert.Empty(t, got.Commits)
		assert.Equal(t, source.files, got.Files)
		assert.Equal(t, gitpkg.DiffStats{}, got.DiffStats)
		messages := make([]string, 0, len(logger.PrintCalls()))
		for _, call := range logger.PrintCalls() {
			messages = append(messages, fmt.Sprintf(call.Format, call.Args...))
		}
		assert.Contains(t, messages, "run facts: commits: log unavailable")
		assert.Contains(t, messages, "run facts: diff stats: stats unavailable")
	})

	t.Run("backlog read failure preserves all paths and readable titles", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		require.NoError(t, os.WriteFile("plan.md", []byte("# Feature\n"), 0o600))
		require.NoError(t, os.MkdirAll("docs/backlog", 0o750))
		require.NoError(t, os.WriteFile("docs/backlog/readable.md", []byte("# Readable issue\n"), 0o600))
		source := &runFactsSourceStub{
			files: []gitpkg.FileChange{
				{Status: "A", Path: "docs/backlog/missing.md"},
				{Status: "A", Path: "docs/backlog/readable.md"},
				{Status: "A", Path: "docs/backlog/also-missing.md"},
			},
		}
		logger := newMockLogger()
		runner := &Runner{
			cfg: Config{PlanFile: "plan.md"}, log: logger, factsSource: source,
		}

		got := runner.collectRunFacts(t.Context())

		assert.Equal(t, []BacklogFile{
			{Status: "A", Path: "docs/backlog/missing.md"},
			{Status: "A", Path: "docs/backlog/readable.md", Title: "Readable issue"},
			{Status: "A", Path: "docs/backlog/also-missing.md"},
		}, got.Backlog)
		assert.Contains(t, factsOnlyReport(RunRecord{}, got), "| A | docs/backlog/readable.md | Readable issue |")
		assert.Equal(t, source.files, got.Files)
		require.Len(t, logger.PrintCalls(), 1)
		assert.Contains(t, fmt.Sprintf(logger.PrintCalls()[0].Format, logger.PrintCalls()[0].Args...), "run facts: backlog:")
	})
}
