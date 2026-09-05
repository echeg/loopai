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
		Mode: processor.ModeFull, Branch: "feature", Plan: "docs/plans/plan.md",
		Reviewers: []string{"claude:opus:high"},
		Stages: []processor.ReviewStage{{
			Stage: "internal_review", Head: "abc123", CompletedAt: time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC),
		}},
	}

	require.NoError(t, store.Save(want))
	got, found, err := store.Load()
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, reviewCheckpointStateVersion, got.Version)
	want.Version = reviewCheckpointStateVersion
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
	assert.True(t, modeUsesReviewCheckpoints(processor.ModeReview))
	assert.True(t, modeUsesReviewCheckpoints(processor.ModeCodexOnly))
	assert.False(t, modeUsesReviewCheckpoints(processor.ModeTasksOnly))
	assert.False(t, modeUsesReviewCheckpoints(processor.ModePlan))
	assert.False(t, modeUsesReviewCheckpoints(processor.ModeGenAgents))
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
