package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/processor"
)

func TestRunRecordStoreRoundTripAndMode(t *testing.T) {
	store := newRunRecordStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	want := processor.RunRecord{
		Version: 1, Plan: "docs/plans/plan.md", Branch: "feature", Mode: processor.ModeFull,
		StartedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
		Tasks:     processor.TaskRunRecord{Iterations: 3, FailedRetries: 1},
	}

	require.NoError(t, store.Save(want))
	got, found, err := store.Load()
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, want, got)
	info, err := os.Stat(store.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestRunRecordStoreMissingAndCorrupt(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		store := newRunRecordStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
		got, found, err := store.Load()
		require.NoError(t, err)
		assert.False(t, found)
		assert.Equal(t, processor.RunRecord{}, got)
	})

	t.Run("corrupt", func(t *testing.T) {
		store := newRunRecordStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
		require.NoError(t, os.WriteFile(store.path, []byte("not-json"), 0o600))
		_, found, err := store.Load()
		assert.False(t, found)
		require.ErrorContains(t, err, "parse run record")
	})
}

func TestRunRecordStoreRemoveIsIdempotent(t *testing.T) {
	store := newRunRecordStore(filepath.Join(t.TempDir(), "progress-plan.txt"))
	require.NoError(t, store.Save(processor.RunRecord{Version: 1}))
	require.NoError(t, store.Remove())
	require.NoError(t, store.Remove())
	_, err := os.Stat(store.path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRunRecordStorePathUsesProgressStem(t *testing.T) {
	dir := t.TempDir()
	store := newRunRecordStore(filepath.Join(dir, "progress-feature.log"))
	assert.Equal(t, filepath.Join(dir, "progress-feature.run.json"), store.path)
}

func TestRunRecordStoreSaveCleansTempFileOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	store := newRunRecordStore(filepath.Join(dir, "progress-plan.txt"))
	require.NoError(t, os.Mkdir(store.path, 0o700))

	require.ErrorContains(t, store.Save(processor.RunRecord{}), "replace run record")
	matches, err := filepath.Glob(filepath.Join(dir, ".run-record-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, matches)
}
