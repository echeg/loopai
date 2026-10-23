//go:build !windows

// The Stop tests signal helper processes holding run locks and share the run fixtures of
// runs_cmd_test.go and the scripted sessions of acp_provider_test.go, so like those files they build on
// Unix only.

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
)

// shortACPStopGrace shortens the Stop grace for one test and returns it.
func shortACPStopGrace(t *testing.T) time.Duration {
	t.Helper()
	saved := acpStopGraceWait
	acpStopGraceWait = 100 * time.Millisecond
	t.Cleanup(func() { acpStopGraceWait = saved })
	return acpStopGraceWait
}

// acpCancel is T3 Code's session/cancel notification for session sid.
func acpCancel(sid string) string {
	return `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sid + `"}}`
}

// acpStopTarget is a live run a Stop test follows: a holder process with a recorded worktree.
type acpStopTarget struct {
	run      *acpTestRun
	holder   *runsHolder
	worktree string
}

// startACPStopTarget records run acpRouteRunID of origin under cwd with an existing worktree, makes it
// live through a holder process, which SIGTERM ends without an outcome as the force exit does, and
// binds it to threadID, or to sessionID when threadID is empty.
func startACPStopTarget(t *testing.T, cwd string, origin runs.Origin, threadID, sessionID string) *acpStopTarget {
	t.Helper()
	worktree := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID, Origin: origin, Worktree: worktree})
	target := &acpStopTarget{run: run, holder: startRunsHolder(t, run.dir), worktree: worktree}
	require.NoError(t, bindRun(cwd, threadID, sessionID, acpRouteRunID))
	return target
}

// cancelFollowed sends prompt 3 to session sid, waits until it follows the target, then cancels it
// and checks it is answered with StopCancelled.
func (target *acpStopTarget) cancelFollowed(t *testing.T, s *acpServer, sid string) {
	t.Helper()
	s.send(acpPrompt(3, sid, acp.ContinuationText))
	target.run.waitAttached()
	s.send(acpCancel(sid))
	assert.Equal(t, map[string]any{"stopReason": string(acp.StopCancelled)}, s.resultAfterUpdates(3))
}

// assertRunning waits ten graces, long past any stop the cancel armed, and checks the run was never
// asked to stop.
func (target *acpStopTarget) assertRunning(t *testing.T, grace time.Duration) {
	t.Helper()
	time.Sleep(10 * grace)
	assert.True(t, target.holder.alive(), "the run keeps running")
	stop, err := runs.StopRequested(target.run.dir)
	require.NoError(t, err)
	assert.False(t, stop, "no stop marker is written")
}

// TestACPStopStopsTheThreadsOwnRun presses Stop in the thread following its run: once the grace passes
// without a new prompt, the run is stopped through its stop marker and SIGTERM, classifies as stopped,
// and keeps its worktree for resume. The thread read that guards the stop lets it through when it finds
// the Stop and when it fails, and a session without a thread id matches the run by its session id.
func TestACPStopStopsTheThreadsOwnRun(t *testing.T) {
	tests := []struct {
		name      string
		lookup    string
		interrupt string
	}{
		{"the thread records the Stop", "thread=thread-1", "stop"},
		{"the thread read fails", "thread=thread-1", ""},
		{"a session without a thread id", "", "steer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortACPStopGrace(t)
			t.Setenv(acpInterruptEnv, tc.interrupt)
			cwd := t.TempDir()
			s, sid := openACPSession(t, cwd, tc.lookup, "")
			origin := runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1", ACPSessionID: "sess-old"}
			threadID := "thread-1"
			if tc.lookup == "" {
				origin, threadID = runs.Origin{Kind: runs.OriginT3Thread, ACPSessionID: sid}, ""
			}
			target := startACPStopTarget(t, cwd, origin, threadID, sid)

			target.cancelFollowed(t, s, sid)
			assert.Equal(t, 1, target.holder.wait(t), "the run ends on SIGTERM")
			stop, err := runs.StopRequested(target.run.dir)
			require.NoError(t, err)
			assert.True(t, stop, "the stop marker is written before the signal")
			st, err := runs.Classify(cwd, target.run.dir)
			require.NoError(t, err)
			assert.Equal(t, runs.StatusStopped, st)
			desc, err := runs.LoadDescriptor(target.run.dir)
			require.NoError(t, err)
			assert.Equal(t, target.worktree, runs.KeptWorktree(runs.Run{Dir: target.run.dir, Descriptor: desc}),
				"a stopped run keeps its worktree")
		})
	}
}

// TestACPCancelKeepsTheRun cancels a followed prompt in every way that must not stop the run: a prompt
// within the grace, as a steer or restart sends, which re-attaches the run even when it names a plan; a
// cancel the thread shows was not a Stop; and a followed run that another thread, a T3 terminal launch,
// or a shell took over before the cancel.
func TestACPCancelKeepsTheRun(t *testing.T) {
	t.Run("a prompt within the grace re-attaches the run", func(t *testing.T) {
		grace := shortACPStopGrace(t)
		t.Setenv(acpInterruptEnv, "stop")
		cwd := t.TempDir()
		launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })
		s, sid := openACPSession(t, cwd, "thread=thread-1", "")
		target := startACPStopTarget(t, cwd, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}, "thread-1", sid)

		target.cancelFollowed(t, s, sid)
		s.send(acpPrompt(4, sid, "docs/plans/other.md"))
		target.run.waitAttached()
		target.assertRunning(t, grace)
		assert.Empty(t, launches.all(), "a steered plan message re-attaches the live run")

		target.holder.open(t)
		assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(4, sid)))
	})
	t.Run("the thread records no Stop", func(t *testing.T) {
		grace := shortACPStopGrace(t)
		t.Setenv(acpInterruptEnv, "steer")
		cwd := t.TempDir()
		s, sid := openACPSession(t, cwd, "thread=thread-1", "")
		target := startACPStopTarget(t, cwd, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}, "thread-1", sid)

		target.cancelFollowed(t, s, sid)
		target.assertRunning(t, grace)
		assert.Contains(t, s.stderr.String(), "run "+acpRouteRunID+" keeps running")
		assert.NotContains(t, s.stderr.String(), acpTestAuthz)
	})
	// a run another owner resumed is never followed again by the thread that bound it, so the stop
	// guard is reached by a takeover while the thread's prompt still follows the run, as when the
	// attempt it followed ended and another owner relaunched it before the follow noticed
	for _, tc := range []struct {
		name   string
		origin runs.Origin
	}{
		{"the followed run is taken over by another thread", runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-2",
			ACPSessionID: "sess-2"}},
		{"the followed run is taken over by a --t3-thread launch", runs.Origin{Kind: runs.OriginT3Terminal,
			ThreadID: "thread-1"}},
		{"the followed run is taken over by a shell", runs.Origin{Kind: runs.OriginCLI}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grace := shortACPStopGrace(t)
			t.Setenv(acpInterruptEnv, "stop")
			cwd := t.TempDir()
			s, sid := openACPSession(t, cwd, "thread=thread-1", "")
			target := startACPStopTarget(t, cwd, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}, "thread-1", sid)

			s.send(acpPrompt(3, sid, acp.ContinuationText))
			target.run.waitAttached()
			desc, err := runs.LoadDescriptor(target.run.dir)
			require.NoError(t, err)
			desc.Origin = tc.origin
			require.NoError(t, runs.SaveDescriptor(target.run.dir, &desc))
			s.send(acpCancel(sid))
			assert.Equal(t, map[string]any{"stopReason": string(acp.StopCancelled)}, s.resultAfterUpdates(3),
				"the open prompt is canceled")
			target.assertRunning(t, grace)
			assert.Equal(t, acpRouteRunID, acpBoundRun(t, cwd, "thread-1", sid), "a cancel leaves the binding alone")
		})
	}
}

// TestACPProviderEndAbandonsAPendingStop ends the provider during the grace, as T3 Code's restart does
// with SIGTERM or by closing its input: the pending stop is dropped and the run keeps running.
func TestACPProviderEndAbandonsAPendingStop(t *testing.T) {
	for _, ending := range []string{"SIGTERM", "EOF"} {
		t.Run(ending, func(t *testing.T) {
			grace := shortACPStopGrace(t)
			t.Setenv(acpInterruptEnv, "stop")
			cwd := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, sid := openACPSessionContext(t, ctx, cwd, "thread=thread-1", "")
			target := startACPStopTarget(t, cwd, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}, "thread-1", sid)

			target.cancelFollowed(t, s, sid)
			if ending == "SIGTERM" {
				cancel()
			} else {
				require.NoError(t, s.in.Close())
			}
			target.assertRunning(t, grace)
		})
	}
}

func TestACPOwnsRun(t *testing.T) {
	tests := []struct {
		name      string
		origin    runs.Origin
		threadID  string
		sessionID string
		want      bool
	}{
		{"the thread's run", runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "t1", ACPSessionID: "s0"}, "t1", "s1", true},
		{"another thread's run", runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "t2", ACPSessionID: "s1"}, "t1", "s1", false},
		{"the session's run without thread ids", runs.Origin{Kind: runs.OriginT3Thread, ACPSessionID: "s1"}, "", "s1", true},
		{"another session's run without thread ids", runs.Origin{Kind: runs.OriginT3Thread, ACPSessionID: "s2"}, "", "s1", false},
		{"a run recorded without a thread, from this session", runs.Origin{Kind: runs.OriginT3Thread, ACPSessionID: "s1"},
			"t1", "s1", true},
		{"a thread's run read by a session that lost its thread", runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "t1",
			ACPSessionID: "s0"}, "", "s1", false},
		{"a shell's run", runs.Origin{Kind: runs.OriginCLI}, "t1", "s1", false},
		{"a T3 terminal's run", runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "t1"}, "t1", "s1", false},
		{"no ids on either side", runs.Origin{Kind: runs.OriginT3Thread}, "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, acpOwnsRun(tc.origin, tc.threadID, tc.sessionID))
		})
	}
}

func TestACPStopsCancelAndClose(t *testing.T) {
	shortACPStopGrace(t)
	s := newACPStops(&syncBuffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a canceled context makes a stop that fires do nothing but return
	s.arm(acpStopRequest{ctx: ctx, sessionID: "s1", runID: "r1"})
	s.cancel("s1")
	s.arm(acpStopRequest{ctx: ctx, sessionID: "s2", runID: "r2"})
	s.close()
	s.arm(acpStopRequest{ctx: ctx, sessionID: "s3", runID: "r3"})
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	assert.Zero(t, pending, "a canceled or abandoned stop is no longer pending, and a closed set arms none")
}
