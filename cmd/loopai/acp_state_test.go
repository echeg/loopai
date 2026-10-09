package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/config"
)

func TestACPStateSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	state := acpSessionState{Provider: "claude", ProviderSessionID: "6f1c", LastRun: &acpLastRun{
		Plan: "docs/plans/x.md", Branch: "x", Base: "main", Tip: "abc123", Report: "# Report: X",
		MergeSummary: "merged x into main (fast-forward), not pushed",
	}}
	require.NoError(t, saveACPState(dir, "loopai-0123", state))

	path := filepath.Join(dir, ".loopai", "acp", "loopai-0123.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	data, err := os.ReadFile(path) //nolint:gosec // test file
	require.NoError(t, err)
	assert.Contains(t, string(data), `"provider_session_id":"6f1c"`)
	assert.Contains(t, string(data), `"merge_summary":"merged x into main`)
	assert.NotContains(t, string(data), "merge_skipped", "empty fields are omitted")

	var warnings bytes.Buffer
	loaded, err := loadACPState(dir, "loopai-0123", &warnings)
	require.NoError(t, err)
	assert.Equal(t, state, loaded)
	assert.Empty(t, warnings.String())
}

func TestACPStateAtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, saveACPState(dir, "s1", acpSessionState{Provider: "claude", ProviderSessionID: "old"}))
	require.NoError(t, saveACPState(dir, "s1", acpSessionState{Provider: "codex", ProviderSessionID: "new"}))

	loaded, err := loadACPState(dir, "s1", &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, acpSessionState{Provider: "codex", ProviderSessionID: "new"}, loaded)
	entries, err := os.ReadDir(filepath.Join(dir, ".loopai", "acp"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file is left behind")
	assert.Equal(t, "s1.json", entries[0].Name())
}

func TestLoadACPStateMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	var warnings bytes.Buffer
	state, err := loadACPState(dir, "missing", &warnings)
	require.NoError(t, err)
	assert.Equal(t, acpSessionState{}, state)
	assert.Empty(t, warnings.String())

	path := filepath.Join(dir, ".loopai", "acp", "broken.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	state, err = loadACPState(dir, "broken", &warnings)
	require.NoError(t, err, "a corrupt file is recovered from, not a failed turn")
	assert.Equal(t, acpSessionState{}, state)
	assert.Contains(t, warnings.String(), "warning: ignoring corrupt ACP session state")

	require.NoError(t, saveACPState(dir, "broken", acpSessionState{ProviderSessionID: "fresh"}))
	state, err = loadACPState(dir, "broken", &warnings)
	require.NoError(t, err)
	assert.Equal(t, "fresh", state.ProviderSessionID, "the next save replaces the corrupt file")
}

func TestLoadACPStateReadError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "acp", "dir.json"), 0o750))
	_, err := loadACPState(dir, "dir", &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read ACP session state")
}

func TestACPStateRejectsUnsafeSessionID(t *testing.T) {
	for _, id := range []string{"", ".", "..", ".hidden", "../escape", "a/b", `a\b`, "a b", "-flag",
		strings.Repeat("a", 129)} {
		t.Run(id, func(t *testing.T) {
			dir := t.TempDir()
			require.Error(t, saveACPState(dir, id, acpSessionState{}))
			_, err := loadACPState(dir, id, &bytes.Buffer{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsafe ACP session id")
			assert.NoDirExists(t, filepath.Join(dir, ".loopai"), "nothing is written for an unsafe id")
		})
	}
	for _, id := range []string{"loopai-0123abcd", "6f1c2d3e-aaaa-4bbb-8ccc-0123456789ab", "A.b_c-1",
		strings.Repeat("a", 128)} {
		_, err := acpStatePath(t.TempDir(), id)
		assert.NoError(t, err, id)
	}
}

func TestACPStateWithLastRunDropsProviderSession(t *testing.T) {
	merge := &acpMergeResult{base: "main", skipped: "Risk is high"}
	run := newACPLastRun("docs/plans/x.md", "x", "main",
		planExecutionOutcome{succeeded: true, branchTip: "abc", report: "# Report: X"}, merge)
	assert.Equal(t, &acpLastRun{Plan: "docs/plans/x.md", Branch: "x", Base: "main", Tip: "abc", Report: "# Report: X",
		MergeSummary: "not merged into main: Risk is high", MergeSkipped: "Risk is high"}, run)
	assert.Equal(t, &acpLastRun{Plan: "p.md"}, newACPLastRun("p.md", "", "", planExecutionOutcome{}, nil),
		"no merge attempted records no merge outcome")

	prev := acpSessionState{Provider: "claude", ProviderSessionID: "old", LastRun: &acpLastRun{Plan: "old.md"}}
	next := prev.withLastRun(run)
	assert.Equal(t, acpSessionState{LastRun: run}, next)
	assert.Equal(t, "old", prev.ProviderSessionID, "the receiver is not modified")
}

func TestACPRunnerRecordLastRun(t *testing.T) {
	repo := setupTestRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	execReq := executePlanRequest{Colors: testColors(), DefaultBranch: "origin/master",
		Outcome: &planExecutionOutcome{succeeded: true, branchTip: "abc", report: "# Report: F"}}
	req := acp.PromptRequest{SessionID: "loopai-0001", Cwd: repo}

	// a chat turn of an earlier runner left a provider session behind
	require.NoError(t, saveACPState(repo, req.SessionID, acpSessionState{Provider: "codex", ProviderSessionID: "thread-1",
		LastRun: &acpLastRun{Plan: "docs/plans/old.md"}}))

	var out bytes.Buffer
	(&acpRunner{out: &out}).recordLastRun(req, "docs/plans/f.md", &config.Config{}, execReq, &acpMergeResult{merged: true,
		kind: "fast-forward", base: "master", feature: "feature"})
	assert.Empty(t, out.String())

	// a fresh runner, as after an agent restart and session/load with the same id, reads it back
	state, err := loadACPState(repo, req.SessionID, &out)
	require.NoError(t, err)
	assert.Empty(t, state.Provider)
	assert.Empty(t, state.ProviderSessionID, "a new run resets the provider session")
	require.NotNil(t, state.LastRun)
	assert.Equal(t, acpLastRun{Plan: "docs/plans/f.md", Branch: "feature", Base: "master", Tip: "abc",
		Report: "# Report: F", MergeSummary: "merged feature into master (fast-forward), not pushed"}, *state.LastRun)
}

func TestACPRunnerRecordLastRunWarnsOnFailure(t *testing.T) {
	execReq := executePlanRequest{Colors: testColors(), Outcome: &planExecutionOutcome{}}
	var out bytes.Buffer
	(&acpRunner{out: &out}).recordLastRun(acp.PromptRequest{SessionID: "../escape", Cwd: t.TempDir()}, "p.md",
		&config.Config{}, execReq, nil)
	assert.Contains(t, out.String(), "warning: ACP session state not saved: unsafe ACP session id")
}
