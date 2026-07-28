package cmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner records argv of every call instead of spawning the real cmux binary.
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	err    error         // returned from run when set
	block  time.Duration // when set, run waits for it or for ctx cancellation
	ctxErr error         // context error observed by the last blocking call
}

func (f *fakeRunner) run(ctx context.Context, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	block := f.block
	err := f.err
	f.mu.Unlock()

	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			f.mu.Lock()
			f.ctxErr = ctx.Err()
			f.mu.Unlock()
			return ctx.Err() //nolint:wrapcheck // test double
		}
	}
	return err
}

func (f *fakeRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// testReporter builds a reporter wired to a fake runner, bypassing environment detection.
func testReporter(t *testing.T, runner commandRunner) *Reporter {
	t.Helper()
	return &Reporter{runner: runner, timeout: time.Second}
}

// writeFakeBin creates an executable file in dir so exec.LookPath can find it.
func writeFakeBin(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755)) //nolint:gosec // test fixture must be executable
}

func TestNew(t *testing.T) {
	binDir := t.TempDir()
	writeFakeBin(t, binDir, binName)
	emptyDir := t.TempDir()

	tests := []struct {
		name      string
		workspace string
		path      string
		wantNil   bool
	}{
		{name: "workspace and binary present", workspace: "ws-1", path: binDir},
		{name: "no workspace id", workspace: "", path: binDir, wantNil: true},
		{name: "blank workspace id", workspace: "  \t ", path: binDir, wantNil: true},
		{name: "no binary in path", workspace: "ws-1", path: emptyDir, wantNil: true},
		{name: "no workspace and no binary", workspace: "", path: emptyDir, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(workspaceEnv, tt.workspace)
			t.Setenv("PATH", tt.path)

			r := New("docs/plans/feature.md")
			if tt.wantNil {
				assert.Nil(t, r)
				return
			}
			require.NotNil(t, r)
			assert.Equal(t, "docs/plans/feature.md", r.planFile)
			assert.Equal(t, execTimeout, r.timeout)
			assert.NotNil(t, r.runner)
		})
	}
}

func TestNewUnsetWorkspaceEnv(t *testing.T) {
	binDir := t.TempDir()
	writeFakeBin(t, binDir, binName)
	t.Setenv("PATH", binDir)
	t.Setenv(workspaceEnv, "ws-1")
	require.NoError(t, os.Unsetenv(workspaceEnv))

	assert.Nil(t, New(""), "unset workspace env must disable the reporter")
}

func TestReporterExecNilSafe(t *testing.T) {
	var r *Reporter
	assert.NotPanics(t, func() { r.exec("workspace", "loading", "on") })
}

func TestReporterExecNoRunner(t *testing.T) {
	r := &Reporter{timeout: time.Second}
	assert.NotPanics(t, func() { r.exec("clear-status", "ralphex") })
}

func TestReporterExec(t *testing.T) {
	tests := []struct {
		name     string
		runErr   error
		args     []string
		wantArgs []string
	}{
		{name: "successful call", args: []string{"clear-progress"}, wantArgs: []string{"clear-progress"}},
		{
			name:     "runner error is swallowed",
			runErr:   errors.New("cmux exploded"),
			args:     []string{"set-status", "ralphex", "task"},
			wantArgs: []string{"set-status", "ralphex", "task"},
		},
		{name: "no args", args: nil, wantArgs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{err: tt.runErr}
			r := testReporter(t, runner)

			assert.NotPanics(t, func() { r.exec(tt.args...) })

			calls := runner.recorded()
			require.Len(t, calls, 1)
			assert.Equal(t, tt.wantArgs, calls[0])
		})
	}
}

func TestReporterExecTimeout(t *testing.T) {
	runner := &fakeRunner{block: 5 * time.Second}
	r := &Reporter{runner: runner, timeout: 20 * time.Millisecond}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.exec("notify", "--title", "ralphex")
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("exec did not return after the per-call timeout expired")
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	assert.ErrorIs(t, runner.ctxErr, context.DeadlineExceeded, "runner must observe the deadline")
}

func TestExecRunner(t *testing.T) {
	t.Run("command succeeds", func(t *testing.T) {
		r := &execRunner{bin: "true"}
		assert.NoError(t, r.run(t.Context()))
	})

	t.Run("command fails", func(t *testing.T) {
		r := &execRunner{bin: "false"}
		err := r.run(t.Context(), "set-status")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "set-status")
	})

	t.Run("binary missing", func(t *testing.T) {
		r := &execRunner{bin: filepath.Join(t.TempDir(), "no-such-binary")}
		assert.Error(t, r.run(t.Context()))
	})

	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := &execRunner{bin: "true"}
		assert.Error(t, r.run(ctx))
	})
}
