package claudeswap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSwapCLI struct {
	mu          sync.Mutex
	active      int
	total       int
	switchCalls int
	failSwitch  bool
}

func (f *fakeSwapCLI) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) == 0 {
		return nil, errors.New("missing command")
	}
	switch args[0] {
	case "status":
		data, err := json.Marshal(map[string]any{
			"schemaVersion":        1,
			"active":               map[string]any{"number": f.active, "email": "must-not-be-persisted@example.com"},
			"totalManagedAccounts": f.total,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal status: %w", err)
		}
		return data, nil
	case "switch":
		if f.failSwitch {
			return nil, errors.New("contains sensitive diagnostic")
		}
		f.switchCalls++
		from := f.active
		f.active = f.active%f.total + 1
		data, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "switched": true,
			"from":   map[string]any{"number": from},
			"to":     map[string]any{"number": f.active},
			"reason": "switched",
		})
		if err != nil {
			return nil, fmt.Errorf("marshal switch: %w", err)
		}
		return data, nil
	default:
		return nil, fmt.Errorf("unexpected command %q", args[0])
	}
}

func newTestCoordinator(dir string, cli *fakeSwapCLI, now time.Time) *Coordinator {
	c := New("claude-swap", dir)
	c.run = cli.run
	c.now = func() time.Time { return now }
	c.settleDelay = 25 * time.Millisecond
	return c
}

func TestCoordinatorRecoverSwitchesAndPersistsSanitizedState(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cli := &fakeSwapCLI{active: 1, total: 3}
	c := newTestCoordinator(dir, cli, now)

	snapshot := c.Snapshot(t.Context())
	result, err := c.Recover(t.Context(), snapshot, 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, result.Recovered)
	assert.True(t, result.Switched)
	assert.Equal(t, 1, result.From)
	assert.Equal(t, 2, result.To)
	assert.Equal(t, 25*time.Millisecond, result.RetryAfter)
	assert.Equal(t, 1, cli.switchCalls)

	data, err := os.ReadFile(filepath.Join(dir, "claude-swap-state.json")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.NotContains(t, string(data), "example.com")
	assert.JSONEq(t, `{
      "schemaVersion": 1,
      "generation": 1,
      "activeAccount": 2,
      "lastSwapAt": "2026-07-31T12:00:00Z",
      "limitedUntil": {"1": "2026-07-31T12:10:00Z"}
    }`, string(data))
}

func TestCoordinatorRecoverSingleFlightSkipsSecondSwitch(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cli := &fakeSwapCLI{active: 1, total: 3}
	first := newTestCoordinator(dir, cli, now)
	second := newTestCoordinator(dir, cli, now)

	firstSnapshot := first.Snapshot(t.Context())
	secondSnapshot := second.Snapshot(t.Context())
	firstResult, err := first.Recover(t.Context(), firstSnapshot, 10*time.Minute)
	require.NoError(t, err)
	secondResult, err := second.Recover(t.Context(), secondSnapshot, 10*time.Minute)
	require.NoError(t, err)

	assert.True(t, firstResult.Switched)
	assert.True(t, secondResult.Recovered)
	assert.False(t, secondResult.Switched)
	assert.Equal(t, 2, secondResult.To)
	assert.Equal(t, "account already changed", secondResult.Reason)
	assert.Equal(t, 1, cli.switchCalls, "only one process may rotate a shared account generation")
}

func TestCoordinatorRecoverSkipsKnownLimitedTarget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cli := &fakeSwapCLI{active: 1, total: 3}
	c := newTestCoordinator(dir, cli, now)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude-swap-state.json"), []byte(`{
      "schemaVersion": 1,
      "generation": 0,
      "activeAccount": 1,
      "limitedUntil": {"2": "2026-07-31T12:10:00Z"}
    }`), 0o600))

	result, err := c.Recover(t.Context(), c.Snapshot(t.Context()), 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, result.Switched)
	assert.Equal(t, 3, result.To)
	assert.Equal(t, 2, cli.switchCalls, "slot 2 is rotated past because loopai recently observed its limit")
}

func TestCoordinatorRecoverFallsBackWithoutLeakingCommandError(t *testing.T) {
	cli := &fakeSwapCLI{active: 1, total: 3, failSwitch: true}
	c := newTestCoordinator(t.TempDir(), cli, time.Now())

	result, err := c.Recover(t.Context(), c.Snapshot(t.Context()), 10*time.Minute)
	require.Error(t, err)
	assert.Equal(t, "switch unavailable", result.Reason)
	assert.NotContains(t, err.Error(), "sensitive diagnostic")
}
