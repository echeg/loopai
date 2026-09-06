package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/processor"
)

func TestReviewCheckpointStoreRoundTrip(t *testing.T) {
	store := newReviewCheckpointStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	want := processor.ReviewCheckpoint{
		Version: 7, Mode: processor.ModeFull, Branch: "feature", Plan: "docs/plans/plan.md",
		Reviewers: []string{"claude:opus:high"}, TaskStartedAtHead: "task-head",
		Stages: []processor.ReviewStage{{
			Stage: "internal_review", Head: "abc123", CompletedAt: time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC),
		}},
	}

	require.NoError(t, store.Save(want))
	got, found, err := store.Load()
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, want, got)

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(store.path)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestReviewCheckpointStoreMissing(t *testing.T) {
	store := newReviewCheckpointStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	got, found, err := store.Load()
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, processor.ReviewCheckpoint{}, got)
}

func TestReviewCheckpointStoreCorruptJSON(t *testing.T) {
	store := newReviewCheckpointStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	require.NoError(t, os.WriteFile(store.path, []byte("{"), 0o600))
	_, found, err := store.Load()
	assert.False(t, found)
	require.ErrorContains(t, err, "parse review checkpoint")
	assert.ErrorIs(t, err, processor.ErrReviewCheckpointCorrupt)
}

func TestReviewCheckpointStoreRemoveIsIdempotent(t *testing.T) {
	store := newReviewCheckpointStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	require.NoError(t, store.Save(processor.ReviewCheckpoint{}))
	require.NoError(t, store.Remove())
	require.NoError(t, store.Remove())
	_, err := os.Stat(store.path)
	assert.True(t, os.IsNotExist(err))
}

func TestReviewCheckpointStoreCleansTempFileOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	store := newReviewCheckpointStore(filepath.Join(dir, "progress-plan.txt"))
	require.NoError(t, os.Mkdir(store.path, 0o750))

	require.ErrorContains(t, store.Save(processor.ReviewCheckpoint{}), "replace review checkpoint")
	matches, err := filepath.Glob(filepath.Join(dir, ".review-checkpoint-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}

func TestReviewCheckpointStoreSaveReportsDirectoryError(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(parentFile, []byte("file"), 0o600))
	store := newReviewCheckpointStore(filepath.Join(parentFile, "progress-plan.txt"))

	require.ErrorContains(t, store.Save(processor.ReviewCheckpoint{}), "create review checkpoint directory")
}

func TestReviewCheckpointStoreRemoveReportsError(t *testing.T) {
	store := newReviewCheckpointStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	require.NoError(t, os.Mkdir(store.path, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(store.path, "child"), []byte("file"), 0o600))

	require.ErrorContains(t, store.Remove(), "remove review checkpoint")
}

func TestReviewCheckpointPath(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		log  string
		want string
	}{
		{"progress-example.txt", "progress-example.review.json"},
		{"progress-review.txt", "progress-review.review.json"},
		{"progress-codex.txt", "progress-codex.review.json"},
	}
	for _, tt := range tests {
		t.Run(tt.log, func(t *testing.T) {
			store := newReviewCheckpointStore(filepath.Join(dir, tt.log))
			assert.Equal(t, filepath.Join(dir, tt.want), store.path)
		})
	}
}

func TestModeUsesReviewCheckpoints(t *testing.T) {
	assert.True(t, modeUsesReviewCheckpoints(processor.ModeFull))
	assert.True(t, modeUsesReviewCheckpoints(processor.ModeTasksOnly))
	assert.True(t, modeUsesReviewCheckpoints(processor.ModeReview))
	assert.True(t, modeUsesReviewCheckpoints(processor.ModeCodexOnly))
	assert.False(t, modeUsesReviewCheckpoints(processor.ModePlan))
	assert.False(t, modeUsesReviewCheckpoints(processor.ModeGenAgents))
}

func TestReviewCheckpointStoreForMode(t *testing.T) {
	progressPath := filepath.Join(t.TempDir(), "progress-plan.txt")

	storeForPlan := reviewCheckpointStoreForMode(processor.ModePlan, progressPath)
	assert.Equal(t, processor.ReviewCheckpointStore(nil), storeForPlan, "must return a nil interface, not a typed nil pointer")
	assert.NotNil(t, reviewCheckpointStoreForMode(processor.ModeTasksOnly, progressPath))
	store, ok := reviewCheckpointStoreForMode(processor.ModeFull, progressPath).(*reviewCheckpointStore)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(filepath.Dir(progressPath), "progress-plan.review.json"), store.path)
}

func TestReadProgressAssociationsIgnoresReviewCheckpoint(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".loopai", "progress")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "progress-plan.review.json"), []byte(`{
  "plan": "docs/plans/incorrect.md",
  "branch": "incorrect"
}`), 0o600))

	associations, err := readProgressAssociations(repo)
	require.NoError(t, err)
	assert.Empty(t, associations)
}
