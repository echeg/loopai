//go:build !windows

// The routing tests hold run locks and share the run fixtures of runs_cmd_test.go and the scripted
// sessions of acp_provider_test.go, so like those files they build on Unix only.

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
)

const (
	// acpRoutePlan is the plan the routing tests' runs execute.
	acpRoutePlan = "docs/plans/20261022-feature.md"
	// acpRouteRunID is the id of the routing tests' main run.
	acpRouteRunID = "feature-20261022-120000-abcd"
	// acpRestartNote is T3 Code's restart note listing one background item.
	acpRestartNote = "Note: the T3 server restarted, and this background work was cancelled before it finished. " + //nolint:misspell // T3 Code's text
		"It will not report back on its own, so resume or restart what you still need and wait for its result:\n" +
		"- loopai: Task 3/7"
)

// acpLaunches replaces acpLaunchDetached for one test: it records every launch and answers with what
// launch returns.
type acpLaunches struct {
	mu    sync.Mutex
	calls []acpLaunch
}

func stubACPLaunch(t *testing.T, launch func(acpLaunch) (string, error)) *acpLaunches {
	t.Helper()
	rec := &acpLaunches{}
	acpLaunchDetached = func(_ context.Context, l acpLaunch) (string, error) {
		rec.mu.Lock()
		rec.calls = append(rec.calls, l)
		rec.mu.Unlock()
		return launch(l)
	}
	t.Cleanup(func() { acpLaunchDetached = refuseACPLaunch })
	return rec
}

func (r *acpLaunches) all() []acpLaunch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]acpLaunch(nil), r.calls...)
}

// acpTestRun is one run of the fake registry in a session cwd.
type acpTestRun struct {
	t       *testing.T
	cwd     string
	dir     string
	release func()
}

// newACPTestRun records the run desc describes under cwd, with plan acpRoutePlan on branch feature
// unless desc sets them.
func newACPTestRun(t *testing.T, cwd string, desc runs.Descriptor) *acpTestRun {
	t.Helper()
	if desc.Plan == "" {
		desc.Plan = acpRoutePlan
	}
	if desc.Branch == "" {
		desc.Branch = "feature"
	}
	return &acpTestRun{t: t, cwd: cwd, dir: saveRunsTestRun(t, cwd, desc)}
}

// start makes the run live, as its process holding the lifetime lock does.
func (r *acpTestRun) start() *acpTestRun {
	r.t.Helper()
	r.release = holdRunsTestLock(r.t, r.dir)
	return r
}

// finish records outcome for the run's current attempt and releases its lifetime lock, the order a
// run ends in.
func (r *acpTestRun) finish(outcome runs.Outcome) {
	r.t.Helper()
	saveRunsTestOutcome(r.t, r.dir, outcome)
	if r.release != nil {
		r.release()
	}
}

// attached reports whether a provider holds the run's attach.lock.
func (r *acpTestRun) attached() bool {
	held, err := runs.Probe(filepath.Join(r.dir, runs.AttachLockFile))
	require.NoError(r.t, err)
	return held
}

// waitAttached waits until a provider follows the run.
func (r *acpTestRun) waitAttached() {
	r.t.Helper()
	require.Eventually(r.t, r.attached, runsWait, 10*time.Millisecond, "no provider attached to the run")
}

// relaunch starts the run's next attempt as the launcher and its child do on a resume.
func (r *acpTestRun) relaunch(l acpLaunch) {
	r.t.Helper()
	desc, err := runs.LoadDescriptor(r.dir)
	require.NoError(r.t, err)
	require.NoError(r.t, runs.ArchiveOutcome(r.dir, desc.Attempt))
	require.NoError(r.t, runs.ClearStop(r.dir))
	desc.Attempt++
	threadID := l.threadID
	if threadID == "" && desc.Origin.Kind == runs.OriginT3Thread {
		threadID = desc.Origin.ThreadID // as detachLaunch.resume keeps a recorded loopai thread
	}
	desc.Origin = runs.Origin{Kind: runs.OriginT3Thread, ThreadID: threadID, ACPSessionID: l.sessionID}
	require.NoError(r.t, runs.SaveDescriptor(r.dir, &desc))
	r.start()
}

// launchedRun returns the run the stubbed launcher sent on launched, once it did. The channel is what
// hands the run from the prompt's goroutine to the test's.
func launchedRun(t *testing.T, launched <-chan *acpTestRun) *acpTestRun {
	t.Helper()
	select {
	case run := <-launched:
		return run
	case <-time.After(runsWait):
		t.Fatal("the run was not launched")
		return nil
	}
}

// openACPSession starts a server with the thread lookup answering lookup and opens a session in cwd,
// through session/new when sid is empty and otherwise through session/load of sid, as a new process
// does after T3 Code reaped the old one. It returns the server and the session id.
func openACPSession(t *testing.T, cwd, lookup, sid string) (*acpServer, string) {
	t.Helper()
	return openACPSessionContext(t, context.Background(), cwd, lookup, sid)
}

// openACPSessionContext is openACPSession with ctx as the server's process context.
func openACPSessionContext(t *testing.T, ctx context.Context, cwd, lookup, sid string) (*acpServer, string) {
	t.Helper()
	t.Setenv(acpLookupEnv, lookup)
	s := startACPServerContext(t, ctx, noACPEnv)
	s.send(acpInitialize("t3-code"))
	s.result(1)
	if sid == "" {
		sid = s.newSession(2, cwd, acpT3Servers())
		return s, sid
	}
	s.send(acpSessionLoad(2, sid, cwd, acpT3Servers()))
	assert.Empty(t, s.result(2), "session/load answers {}")
	return s, sid
}

// acpBoundRun returns the run id the binding of threadID or sessionID names in cwd.
func acpBoundRun(t *testing.T, cwd, threadID, sessionID string) string {
	t.Helper()
	b, _ := loadBinding(cwd, threadID, sessionID)
	return b.RunID
}

// acpHeadline is the first line of a final message: how the run's attempt ended.
func acpHeadline(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// waitBound waits until the binding of threadID or sessionID in cwd names run id: a provider binds a
// run only once it holds the run's attach.lock, so the binding follows the lock.
func waitBound(t *testing.T, cwd, threadID, sessionID, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		b, _ := loadBinding(cwd, threadID, sessionID)
		return b.RunID == id
	}, runsWait, 10*time.Millisecond, "run %s is not bound", id)
}

func TestACPPlanMessageStartsAndBindsItsRun(t *testing.T) {
	tests := []struct {
		name       string
		lookup     string
		wantThread string
	}{
		{name: "the thread id is the binding key", lookup: "thread=" + acpLongThreadID, wantThread: acpLongThreadID},
		{name: "the session id when the thread cannot be looked up", lookup: "fail"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			launched := make(chan *acpTestRun, 1)
			launches := stubACPLaunch(t, func(l acpLaunch) (string, error) {
				launched <- newACPTestRun(t, l.cwd, runs.Descriptor{ID: acpRouteRunID, Argv: l.args,
					Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: l.threadID, ACPSessionID: l.sessionID}}).start()
				return acpRouteRunID, nil
			})
			s, sid := openACPSession(t, cwd, tc.lookup, "")
			s.send(acpPrompt(3, sid, acpRoutePlan+" --task-model codex:gpt-6-astra:high --external-reviewers=claude"))
			run := launchedRun(t, launched)
			run.waitAttached()

			assert.Equal(t, []acpLaunch{{cwd: cwd, threadID: tc.wantThread, sessionID: sid,
				args: []string{"--task-model=codex:gpt-6-astra:high", "--external-reviewers=claude", acpRoutePlan}}},
				launches.all())
			waitBound(t, cwd, tc.wantThread, sid, acpRouteRunID)
			if tc.wantThread == "" {
				assert.Equal(t, acpRouteRunID, readBindingFile(t, filepath.Join(cwd, ".loopai", "acp",
					"binding-session-"+sid+".json"))["run_id"])
			} else {
				assert.FileExists(t, filepath.Join(cwd, ".loopai", "acp", "binding-thread-"+runs.ThreadKey(tc.wantThread)+".json"))
			}

			run.finish(runs.Outcome{Status: runs.StatusSucceeded, Branch: "feature"})
			assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(3, sid)))
			assert.False(t, run.attached(), "the attach.lock is released with the turn")
		})
	}
}

// TestACPResumeInANewProcessRelaunchesTheBoundRun binds a run from one provider process, lets it fail,
// and, after T3 Code reaped that process, types resume into the thread: the new process's session/load
// finds the binding on disk, even with the thread lookup failing, and relaunches the run from its
// recorded arguments and options.
func TestACPResumeInANewProcessRelaunchesTheBoundRun(t *testing.T) {
	cwd := t.TempDir()
	launched := make(chan *acpTestRun, 1)
	var started *acpTestRun // touched by the prompt goroutines only, one after the other
	launches := stubACPLaunch(t, func(l acpLaunch) (string, error) {
		if started == nil {
			started = newACPTestRun(t, l.cwd, runs.Descriptor{ID: acpRouteRunID, Argv: l.args,
				EnvOptions: map[string]string{"LOOPAI_CONFIG_DIR": "/cfg"},
				Origin:     runs.Origin{Kind: runs.OriginT3Thread, ThreadID: l.threadID, ACPSessionID: l.sessionID}}).start()
		} else {
			started.relaunch(l)
		}
		launched <- started
		return acpRouteRunID, nil
	})

	first, sid := openACPSession(t, cwd, "thread=thread-1", "")
	first.send(acpPrompt(3, sid, acpRoutePlan+" --review-model claude:opus"))
	run := launchedRun(t, launched)
	run.waitAttached()
	run.finish(runs.Outcome{Status: runs.StatusFailed, Reason: "task 2 failed", Branch: "feature"})
	assert.Equal(t, "loopai run "+acpRouteRunID+" failed: task 2 failed.", acpHeadline(first.turnReply(3, sid)))
	require.NoError(t, first.in.Close()) // the idle provider is reaped

	second, _ := openACPSession(t, cwd, "fail", sid)
	second.send(acpPrompt(3, sid, "resume"))
	run = launchedRun(t, launched)
	run.waitAttached()
	require.Len(t, launches.all(), 2, "the bound run was relaunched")
	relaunch := launches.all()[1]
	assert.Equal(t, acpLaunch{cwd: cwd, threadID: "thread-1", sessionID: sid,
		args: []string{"--review-model=claude:opus", acpRoutePlan}, envOptions: map[string]string{"LOOPAI_CONFIG_DIR": "/cfg"}},
		relaunch, "the recorded argv and options, with the thread the session recorded")

	run.finish(runs.Outcome{Status: runs.StatusSucceeded, Branch: "feature"})
	assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(second.turnReply(3, sid)))
	assert.Equal(t, acpRouteRunID, acpBoundRun(t, cwd, "thread-1", sid))
	assert.Contains(t, second.stderr.String(), "using the thread the session recorded")
}

// TestACPContinuationRelaunchesARunThatDidNotSucceed covers every ending a continuation or resume
// relaunches: a Stop followed by T3 Code's Resume button, which sends the continuation, a crash before
// a restart, and a failure.
func TestACPContinuationRelaunchesARunThatDidNotSucceed(t *testing.T) {
	tests := []struct {
		name  string
		end   func(t *testing.T, dir string)
		text  string
		after string // the status the first attempt shows before the relaunch
	}{
		{name: "the Resume button after a Stop", text: acp.ContinuationText, after: "stopped",
			end: func(t *testing.T, dir string) { require.NoError(t, runs.RequestStop(dir)) }},
		{name: "a restart continuation after a crash", text: acpRestartNote + "\n\n" + acp.ContinuationText, after: "crashed",
			end: func(*testing.T, string) {}},
		{name: "a continuation after a session/load fallback", after: "failed",
			text: "Context handoff (full_thread_summary):\nearlier turns\n\nUser message:\n" + acp.ContinuationText,
			end: func(t *testing.T, dir string) {
				saveRunsTestOutcome(t, dir, runs.Outcome{Status: runs.StatusFailed, Reason: "task 2 failed"})
			}},
		{name: "resume in any case", text: "  Resume\n", after: "failed", end: func(t *testing.T, dir string) {
			saveRunsTestOutcome(t, dir, runs.Outcome{Status: runs.StatusFailed})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID, Argv: []string{acpRoutePlan},
				Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1", ACPSessionID: "sess-old"}})
			tc.end(t, run.dir)
			status, err := runs.Classify(cwd, run.dir)
			require.NoError(t, err)
			require.Equal(t, tc.after, string(status))
			require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))
			launched := make(chan *acpTestRun, 1)
			launches := stubACPLaunch(t, func(l acpLaunch) (string, error) {
				run.relaunch(l)
				launched <- run
				return acpRouteRunID, nil
			})

			s, sid := openACPSession(t, cwd, "thread=thread-1", "")
			s.send(acpPrompt(3, sid, tc.text))
			launchedRun(t, launched).waitAttached()
			assert.Equal(t, []acpLaunch{{cwd: cwd, threadID: "thread-1", sessionID: sid, args: []string{acpRoutePlan}}},
				launches.all())
			run.finish(runs.Outcome{Status: runs.StatusSucceeded})
			assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(3, sid)))
		})
	}
}

// TestACPReattachesTheBoundLiveRun sends every kind of text to a thread whose run is live: each one,
// a plan message naming another plan included, re-attaches the run and launches nothing. A run still
// starting, its launcher holding the launch lock, counts as live.
func TestACPReattachesTheBoundLiveRun(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		starting bool
	}{
		{name: "a restart continuation with the note", text: acpRestartNote + "\n\n" + acp.ContinuationText},
		{name: "a restart continuation without the note", text: acp.ContinuationText},
		{name: "a question", text: "how is it going?"},
		{name: "a steered plan message", text: "docs/plans/other.md --task-model codex"},
		{name: "resume", text: "resume"},
		{name: "a run still starting", text: "how is it going?", starting: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
				Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}})
			finish := func() { run.finish(runs.Outcome{Status: runs.StatusFailed, Reason: "task 1 failed"}) }
			if tc.starting {
				release, err := runs.AcquireLaunchLock(cwd, acpRoutePlan, "feature", time.Second)
				require.NoError(t, err)
				finish = func() {
					saveRunsTestOutcome(t, run.dir, runs.Outcome{Status: runs.StatusFailed, Reason: "task 1 failed"})
					require.NoError(t, release())
				}
				status, err := runs.Classify(cwd, run.dir)
				require.NoError(t, err)
				require.Equal(t, runs.StatusStarting, status)
			} else {
				run.start()
			}
			require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))
			launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })

			s, sid := openACPSession(t, cwd, "thread=thread-1", "")
			s.send(acpPrompt(3, sid, tc.text))
			run.waitAttached()
			finish()
			assert.Equal(t, "loopai run "+acpRouteRunID+" failed: task 1 failed.", acpHeadline(s.turnReply(3, sid)))
			assert.Empty(t, launches.all())
		})
	}
}

// TestACPContinuationAdoptsTheOnlyCandidate sends T3 Code's continuation to a session with no binding,
// as after a session/load fallback, with or without its thread lookup failing: of the runs in the
// registry, only the live, unattached run a loopai thread started from this cwd qualifies, and it is
// bound and attached.
func TestACPContinuationAdoptsTheOnlyCandidate(t *testing.T) {
	for _, tc := range []struct{ lookup, threadID string }{{lookup: "fail"}, {lookup: "thread=thread-1", threadID: "thread-1"}} {
		t.Run(tc.lookup, func(t *testing.T) {
			cwd := t.TempDir()
			saveACPNonCandidates(t, cwd)
			run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
				Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1", ACPSessionID: "sess-old"}}).start()
			launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })

			s, sid := openACPSession(t, cwd, tc.lookup, "")
			s.send(acpPrompt(3, sid, acpRestartNote+"\n\n"+acp.ContinuationText))
			run.waitAttached()
			waitBound(t, cwd, tc.threadID, sid, acpRouteRunID) // the adopted run is bound
			run.finish(runs.Outcome{Status: runs.StatusSucceeded})
			assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(3, sid)))
			assert.Empty(t, launches.all())
			// the adopted run stays the thread's, though its session ids differ from the run's
			assert.True(t, strings.HasPrefix(s.statusReply(4, sid, "status"), "loopai run "+acpRouteRunID+" succeeded."))
			assert.Equal(t, acpRouteRunID, acpBoundRun(t, cwd, tc.threadID, sid))
			assert.NotContains(t, s.stderr.String(), "another owner")
		})
	}
}

// TestACPAdoptionBindsOnlyUnderTheAttachLock lets a candidate change between the selection, which
// only probes its attach.lock, and the adoption: when another provider takes the lock in between, or
// the run ends, the continuation answers without binding the run.
func TestACPAdoptionBindsOnlyUnderTheAttachLock(t *testing.T) {
	tests := []struct {
		name  string
		race  func(t *testing.T, run *acpTestRun)
		reply string
	}{
		{
			name: "another provider takes the lock",
			race: func(t *testing.T, run *acpTestRun) {
				unlock, err := runs.AcquireBounded(filepath.Join(run.dir, runs.AttachLockFile), time.Second)
				require.NoError(t, err)
				t.Cleanup(func() { _ = unlock() })
			},
			reply: "loopai run " + acpRouteRunID + " is followed by another loopai provider; follow it from a shell " +
				"with loopai --attach " + acpRouteRunID + ".",
		},
		{
			name:  "the run ends",
			race:  func(_ *testing.T, run *acpTestRun) { run.finish(runs.Outcome{Status: runs.StatusFailed}) },
			reply: "No loopai run belongs to this thread yet",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
				Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
			t.Cleanup(func() { acpAdoptionCandidatesFn = acpAdoptionCandidates })
			acpAdoptionCandidatesFn = func(cwd, threadID string) ([]runs.Run, error) {
				candidates, err := acpAdoptionCandidates(cwd, threadID)
				if len(candidates) == 1 {
					tc.race(t, run)
				}
				return candidates, err
			}
			launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })

			s, sid := openACPSession(t, cwd, "thread=thread-1", "")
			assert.Contains(t, s.statusReply(3, sid, acp.ContinuationText), tc.reply)
			assert.Empty(t, acpBoundRun(t, cwd, "thread-1", sid), "the run is not bound")
			assert.Empty(t, launches.all())
		})
	}
}

// TestACPLaunchedRunAnotherProviderTakesFirstIsNotBound lets another provider take the attach.lock of a
// run the plan message just launched before this thread follows it: the thread answers as for any run
// another provider follows and binds nothing.
func TestACPLaunchedRunAnotherProviderTakesFirstIsNotBound(t *testing.T) {
	cwd := t.TempDir()
	stubACPLaunch(t, func(l acpLaunch) (string, error) {
		run := newACPTestRun(t, l.cwd, runs.Descriptor{ID: acpRouteRunID,
			Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: l.threadID, ACPSessionID: l.sessionID}}).start()
		unlock, err := runs.AcquireBounded(filepath.Join(run.dir, runs.AttachLockFile), time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = unlock() })
		return acpRouteRunID, nil
	})

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	assert.Equal(t, "loopai run "+acpRouteRunID+" is followed by another loopai provider; follow it from a shell with "+
		"loopai --attach "+acpRouteRunID+".", s.statusReply(3, sid, acpRoutePlan))
	assert.Empty(t, acpBoundRun(t, cwd, "thread-1", sid), "the run is not bound")
}

// saveACPNonCandidates records under cwd one run of each kind a continuation must never adopt and
// returns their directories: origin cli and t3-terminal, a loopai thread's run recorded for another
// cwd, one another provider follows, and one that is not live.
func saveACPNonCandidates(t *testing.T, cwd string) map[string]string {
	t.Helper()
	thread := func(id string) runs.Origin { return runs.Origin{Kind: runs.OriginT3Thread, ThreadID: id} }
	dirs := map[string]string{}
	for _, nc := range []struct {
		name   string
		origin runs.Origin
		live   bool
	}{
		{name: "cli", origin: runs.Origin{Kind: runs.OriginCLI}, live: true},
		{name: "terminal", origin: runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "thread-1"}, live: true},
		{name: "elsewhere", origin: thread(""), live: true},
		{name: "followed", origin: thread(""), live: true},
		{name: "ended", origin: thread("")},
	} {
		id := "nc-" + nc.name + "-20261022-110000-abcd"
		run := newACPTestRun(t, cwd, runs.Descriptor{ID: id, Plan: "docs/plans/" + nc.name + ".md", Branch: nc.name,
			Origin: nc.origin})
		dirs[nc.name] = run.dir
		if nc.live {
			run.start()
		}
		switch nc.name {
		case "elsewhere":
			desc, err := runs.LoadDescriptor(run.dir)
			require.NoError(t, err)
			desc.Cwd = t.TempDir()
			require.NoError(t, runs.SaveDescriptor(run.dir, &desc))
		case "followed":
			unlock, err := runs.AcquireBounded(filepath.Join(run.dir, runs.AttachLockFile), time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = unlock() })
		case "ended":
			saveRunsTestOutcome(t, run.dir, runs.Outcome{Status: runs.StatusFailed})
		}
	}
	return dirs
}

func TestACPContinuationNeverAdoptsANonCandidate(t *testing.T) {
	cwd := t.TempDir()
	dirs := saveACPNonCandidates(t, cwd)
	dirs["other"] = newACPTestRun(t, cwd, runs.Descriptor{ID: "nc-other-20261022-110000-abcd", Plan: "docs/plans/other.md",
		Branch: "other", Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-2"}}).start().dir
	before := map[string]bool{}
	for name, dir := range dirs {
		_, err := os.Stat(filepath.Join(dir, runs.AttachLockFile))
		before[name] = err == nil
	}
	launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })

	// the thread is known: a live, unattached run recorded for another thread is not this one's
	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	reply := s.statusReply(3, sid, acp.ContinuationText)
	assert.Contains(t, reply, "No loopai run belongs to this thread yet")
	assert.Contains(t, reply, acpModelSwitchHint)
	assert.Empty(t, acpBoundRun(t, cwd, "thread-1", sid), "nothing is bound")
	assert.Empty(t, launches.all())
	for name, dir := range dirs {
		_, err := os.Stat(filepath.Join(dir, runs.AttachLockFile))
		assert.Equal(t, before[name], err == nil, "%s: no attach.lock taken", name)
	}

	t.Run("an unknown thread may adopt another thread's run, which is then the only candidate", func(t *testing.T) {
		candidates, err := acpAdoptionCandidates(cwd, "")
		require.NoError(t, err)
		require.Len(t, candidates, 1)
		assert.Equal(t, filepath.Base(dirs["other"]), candidates[0].Descriptor.ID)
	})
}

func TestACPContinuationWithSeveralCandidatesListsThem(t *testing.T) {
	cwd := t.TempDir()
	first := newACPTestRun(t, cwd, runs.Descriptor{ID: "alpha-20261022-120000-aaaa", Plan: "docs/plans/alpha.md",
		Branch: "alpha", Origin: runs.Origin{Kind: runs.OriginT3Thread}}).start()
	second := newACPTestRun(t, cwd, runs.Descriptor{ID: "beta-20261022-120000-bbbb", Plan: "docs/plans/beta.md",
		Branch: "beta", Origin: runs.Origin{Kind: runs.OriginT3Thread}}).start()

	s, sid := openACPSession(t, cwd, "fail", "")
	reply := s.statusReply(3, sid, acp.ContinuationText)
	assert.Contains(t, reply, "Several live loopai runs started from T3 Code threads in this checkout could belong to "+
		"this thread, so none was attached")
	assert.Contains(t, reply, "\n- alpha-20261022-120000-aaaa: docs/plans/alpha.md on branch alpha, running")
	assert.Contains(t, reply, "\n- beta-20261022-120000-bbbb: docs/plans/beta.md on branch beta, running")
	assert.Empty(t, acpBoundRun(t, cwd, "", sid), "neither is bound")
	assert.False(t, first.attached())
	assert.False(t, second.attached())
	assert.False(t, acpAuthWords.MatchString(reply))
}

// TestACPPlanMessageNeverAdoptsAnotherRun sends a plan message to a session with no binding while a
// live, unattached run of another plan from this cwd waits for a provider: the message starts its own
// plan's run, and a refused launch answers with the launcher's refusal, attaching nothing.
func TestACPPlanMessageNeverAdoptsAnotherRun(t *testing.T) {
	t.Run("the plan's own run", func(t *testing.T) {
		cwd := t.TempDir()
		other := newACPTestRun(t, cwd, runs.Descriptor{ID: "other-20261022-110000-abcd", Plan: "docs/plans/other.md",
			Branch: "other", Origin: runs.Origin{Kind: runs.OriginT3Thread}}).start()
		launched := make(chan *acpTestRun, 1)
		stubACPLaunch(t, func(l acpLaunch) (string, error) {
			launched <- newACPTestRun(t, l.cwd, runs.Descriptor{ID: acpRouteRunID,
				Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: l.threadID, ACPSessionID: l.sessionID}}).start()
			return acpRouteRunID, nil
		})
		s, sid := openACPSession(t, cwd, "thread=thread-1", "")
		s.send(acpPrompt(3, sid, acpRoutePlan))
		run := launchedRun(t, launched)
		run.waitAttached()
		assert.False(t, other.attached(), "the other run is left alone")
		waitBound(t, cwd, "thread-1", sid, acpRouteRunID)
		run.finish(runs.Outcome{Status: runs.StatusSucceeded})
		assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(3, sid)))
	})

	t.Run("a refused launch", func(t *testing.T) {
		cwd := t.TempDir()
		other := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
			Origin: runs.Origin{Kind: runs.OriginT3Thread}}).start()
		const refusal = "error: run " + acpRouteRunID + " of " + acpRoutePlan + " is already running; " +
			"follow it with loopai --attach " + acpRouteRunID
		launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New(refusal) })
		s, sid := openACPSession(t, cwd, "thread=thread-1", "")
		assert.Equal(t, refusal, s.statusReply(3, sid, acpRoutePlan), "the launcher's refusal is the final message")
		assert.Len(t, launches.all(), 1)
		assert.Empty(t, acpBoundRun(t, cwd, "thread-1", sid), "a refused launch binds nothing")
		assert.False(t, other.attached(), "the plan's live run of another thread is not attached")
	})
}

func TestACPPlanMessageWithAnUnparsableFlagGetsTheUsage(t *testing.T) {
	cwd := t.TempDir()
	launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })
	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	for i, tc := range []struct{ text, wantErr string }{
		{text: acpRoutePlan + " --tasks-only", wantErr: `unsupported option "--tasks-only"`},
		{text: acpRoutePlan + " --task-model", wantErr: "--task-model requires a value"},
		{text: acpRoutePlan + " --task-model=codex 'quoted'", wantErr: `unexpected argument "'quoted'"`},
		{text: acpRoutePlan + " --task-model=codex;rm", wantErr: `invalid --task-model value "codex;rm"`},
	} {
		reply := s.statusReply(3+i, sid, tc.text)
		assert.True(t, strings.HasPrefix(reply, "loopai cannot start a run from this message: "), reply)
		assert.Contains(t, reply, tc.wantErr)
		assert.Contains(t, reply, acpPromptUsage)
	}
	assert.Empty(t, launches.all(), "no run is started")
	assert.Empty(t, acpBoundRun(t, cwd, "thread-1", sid))
}

// TestACPStatusAfterTheRun asks about a thread's run once it succeeded: a question, the continuation,
// and resume all get the status reply with the outcome summary and the model-switch hint, and a
// succeeded run is never relaunched.
func TestACPStatusAfterTheRun(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}})
	run.finish(runs.Outcome{Status: runs.StatusSucceeded, ReportSummary: "Added the provider.", Branch: "feature"})
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))
	launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	want := "loopai run " + acpRouteRunID + " succeeded.\n\nAdded the provider.\n\nBranch: feature\n\n" + acpModelSwitchHint
	for i, text := range []string{"how did it go?", acp.ContinuationText, "resume"} {
		assert.Equal(t, want, s.statusReply(3+i, sid, text), text)
	}
	assert.Empty(t, launches.all())
}

// TestACPCanceledAttachLeavesTheRunRunning cancels a prompt that follows a run, as a steer does: the
// prompt is answered as canceled, the run keeps running, and the next prompt re-attaches it.
func TestACPCanceledAttachLeavesTheRunRunning(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	s.send(acpPrompt(3, sid, acp.ContinuationText))
	run.waitAttached()
	s.send(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sid + `"}}`)
	assert.Equal(t, map[string]any{"stopReason": "cancelled"}, s.resultAfterUpdates(3)) //nolint:misspell // ACP wire value
	status, err := runs.Classify(cwd, run.dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusRunning, status, "a cancel never stops the run")

	s.send(acpPrompt(4, sid, "docs/plans/other.md"))
	run.waitAttached()
	run.finish(runs.Outcome{Status: runs.StatusSucceeded})
	assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(4, sid)))
}

// TestACPAttachToARunAnotherProviderFollows answers instead of waiting when the bound run's
// attach.lock stays held past the bound.
func TestACPAttachToARunAnotherProviderFollows(t *testing.T) {
	cwd := t.TempDir()
	run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))
	unlock, err := runs.AcquireBounded(filepath.Join(run.dir, runs.AttachLockFile), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unlock() })

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	assert.Equal(t, "loopai run "+acpRouteRunID+" is followed by another loopai provider; follow it from a shell with "+
		"loopai --attach "+acpRouteRunID+".", s.statusReply(3, sid, acp.ContinuationText))
}

// TestACPUnavailableBindingStoreReadsAsNoBinding plants a binding behind a symlinked .loopai/acp: it is
// never read, so the thread has no run, and a launched run is still attached although its binding
// cannot be recorded.
func TestACPUnavailableBindingStoreReadsAsNoBinding(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(cwd, ".loopai"), 0o750))
	require.NoError(t, os.Symlink(outside, filepath.Join(cwd, ".loopai", "acp")))
	newACPTestRun(t, cwd, runs.Descriptor{ID: "old-20261022-110000-abcd", Plan: "docs/plans/old.md", Branch: "old",
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "binding-thread-"+runs.ThreadKey("thread-1")+".json"),
		[]byte(`{"thread_id":"thread-1","run_id":"old-20261022-110000-abcd"}`), 0o600))
	launched := make(chan *acpTestRun, 1)
	stubACPLaunch(t, func(l acpLaunch) (string, error) {
		launched <- newACPTestRun(t, l.cwd, runs.Descriptor{ID: acpRouteRunID, Origin: runs.Origin{Kind: runs.OriginT3Thread}}).start()
		return acpRouteRunID, nil
	})

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	assert.Contains(t, s.statusReply(3, sid, "status"), "No loopai run belongs to this thread yet")
	s.send(acpPrompt(4, sid, acpRoutePlan))
	run := launchedRun(t, launched)
	run.waitAttached()
	run.finish(runs.Outcome{Status: runs.StatusSucceeded})
	assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(4, sid)))
	assert.Equal(t, 2, strings.Count(s.stderr.String(), "is a symbolic link"),
		"one warning for the session binding and one for the run binding: "+s.stderr.String())
	assert.Contains(t, s.stderr.String(), "warning: acp: run binding not recorded:")
}

// TestACPUnreadableBoundRunWarnsAndFallsThrough binds the thread to a run whose descriptor does not
// decode: every message is still answered by the later rules, a plan message starts its run, and each
// turn that met the binding leaves one warning on stderr naming the run, never the MCP credential.
func TestACPUnreadableBoundRunWarnsAndFallsThrough(t *testing.T) {
	cwd := t.TempDir()
	const brokenID = "old-20261022-110000-abcd"
	broken := newACPTestRun(t, cwd, runs.Descriptor{ID: brokenID, Plan: "docs/plans/old.md", Branch: "old",
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}})
	require.NoError(t, os.WriteFile(filepath.Join(broken.dir, runs.DescriptorFile), []byte("{not json"), 0o600))
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", brokenID))
	launched := make(chan *acpTestRun, 1)
	launches := stubACPLaunch(t, func(l acpLaunch) (string, error) {
		launched <- newACPTestRun(t, l.cwd, runs.Descriptor{ID: acpRouteRunID,
			Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()
		return acpRouteRunID, nil
	})

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	assert.Equal(t, "loopai run "+brokenID+" has no recorded outcome.\n\n"+acpModelSwitchHint,
		s.statusReply(3, sid, acp.ContinuationText), "a continuation neither relaunches nor adopts")
	warning := "warning: acp: bound run " + brokenID + " unavailable: load run " + brokenID + ":"
	assert.Equal(t, 1, strings.Count(s.stderr.String(), warning), s.stderr.String())
	assert.Empty(t, launches.all())

	s.send(acpPrompt(4, sid, acpRoutePlan))
	run := launchedRun(t, launched)
	run.waitAttached()
	run.finish(runs.Outcome{Status: runs.StatusSucceeded})
	assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(4, sid)))
	assert.Equal(t, 2, strings.Count(s.stderr.String(), warning), "the plan message fell through to its own run")
	assert.Equal(t, acpRouteRunID, acpBoundRun(t, cwd, "thread-1", sid))
	assert.NotContains(t, s.stderr.String(), acpTestAuthz)
}

// TestACPBindingToARemovedRunIsDropped binds the thread to a run whose directory was pruned: the first
// turn removes the binding with one warning and is answered as for an unbound thread, so a continuation
// may adopt; later turns warn no more.
func TestACPBindingToARemovedRunIsDropped(t *testing.T) {
	cwd := t.TempDir()
	const goneID = "old-20261022-110000-abcd"
	require.NoError(t, bindRun(cwd, "thread-1", "sess-old", goneID))
	adoptable := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID,
		Origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-1"}}).start()

	s, sid := openACPSession(t, cwd, "thread=thread-1", "")
	s.send(acpPrompt(3, sid, acp.ContinuationText))
	adoptable.waitAttached()
	adoptable.finish(runs.Outcome{Status: runs.StatusSucceeded})
	assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(s.turnReply(3, sid)),
		"the unbound thread's continuation adopts the live run")
	warning := "warning: acp: bound run " + goneID + " no longer exists"
	assert.Equal(t, 1, strings.Count(s.stderr.String(), warning), s.stderr.String())
	assert.Equal(t, acpRouteRunID, acpBoundRun(t, cwd, "thread-1", sid))

	s.statusReply(4, sid, "status")
	assert.Equal(t, 1, strings.Count(s.stderr.String(), warning), "the binding was removed once")
	assert.NotContains(t, s.stderr.String(), "unavailable")
}

// TestACPBindingToATakenOverRunIsDropped binds the thread to a run another owner resumed since, by
// a loopai thread's plan message or a --t3-thread launch: the thread neither shows nor relaunches it,
// whatever it sends, adopts nothing either, and removes its binding with one warning, while the new
// owner's thread still follows the run.
func TestACPBindingToATakenOverRunIsDropped(t *testing.T) {
	tests := []struct {
		name   string
		origin runs.Origin
		live   bool
		text   string
	}{
		{name: "a live run another thread resumed, on a continuation", live: true, text: acp.ContinuationText,
			origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-2", ACPSessionID: "sess-2"}},
		{name: "a live run a --t3-thread launch resumed, on a question", live: true, text: "how is it going?",
			origin: runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "thread-1"}},
		{name: "a failed run another thread resumed, on resume", text: "resume",
			origin: runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-2", ACPSessionID: "sess-2"}},
		{name: "a failed run a --t3-thread launch resumed, on a continuation", text: acp.ContinuationText,
			origin: runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "thread-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			run := newACPTestRun(t, cwd, runs.Descriptor{ID: acpRouteRunID, Argv: []string{acpRoutePlan}, Origin: tc.origin})
			if tc.live {
				run.start()
			} else {
				saveRunsTestOutcome(t, run.dir, runs.Outcome{Status: runs.StatusFailed, Reason: "task 2 failed"})
			}
			require.NoError(t, bindRun(cwd, "thread-1", "sess-old", acpRouteRunID))
			require.NoError(t, bindRun(cwd, "thread-2", "sess-2", acpRouteRunID))
			launches := stubACPLaunch(t, func(acpLaunch) (string, error) { return "", errors.New("no launch expected") })

			s, sid := openACPSession(t, cwd, "thread=thread-1", "")
			assert.Equal(t, acpStatusReply(cwd, ""), s.statusReply(3, sid, tc.text),
				"the thread answers as unbound and adopts nothing")
			assert.False(t, run.attached(), "the run is not shown")
			assert.Empty(t, launches.all(), "the run is not relaunched")
			assert.Empty(t, acpBoundRun(t, cwd, "thread-1", sid), "the thread is no longer bound to the run")
			warning := "warning: acp: bound run " + acpRouteRunID + " now belongs to another owner; " +
				"the thread is no longer bound to it"
			assert.Equal(t, 1, strings.Count(s.stderr.String(), warning), s.stderr.String())
			s.statusReply(4, sid, tc.text)
			assert.Equal(t, 1, strings.Count(s.stderr.String(), warning), "the binding was removed once")
			assert.Equal(t, acpRouteRunID, acpBoundRun(t, cwd, "thread-2", "sess-2"), "the new owner's binding is kept")
			if !tc.live || tc.origin.Kind != runs.OriginT3Thread {
				return
			}
			owner, ownerSID := openACPSession(t, cwd, "thread=thread-2", "")
			owner.send(acpPrompt(3, ownerSID, "how is it going?"))
			run.waitAttached()
			run.finish(runs.Outcome{Status: runs.StatusSucceeded})
			assert.Equal(t, "loopai run "+acpRouteRunID+" succeeded.", acpHeadline(owner.turnReply(3, ownerSID)),
				"the new owner still follows the run")
			assert.NotContains(t, owner.stderr.String(), "another owner")
		})
	}
}

func TestParseACPPlanMessage(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    []string
		wantErr string
	}{
		{name: "a plan", text: acpRoutePlan, want: []string{acpRoutePlan}},
		{name: "both flag forms", text: acpRoutePlan + " --task-model codex::medium --review-model=claude:opus:high " +
			"--external-reviewers codex,claude:sonnet", want: []string{"--task-model=codex::medium",
			"--review-model=claude:opus:high", "--external-reviewers=codex,claude:sonnet", acpRoutePlan}},
		{name: "an uppercase extension", text: "docs/plans/X.MD", want: []string{"docs/plans/X.MD"}},
		{name: "no plan first", text: "--task-model codex " + acpRoutePlan, wantErr: "the message must start with the plan file"},
		{name: "a chain", text: "docs/plans/a.md,docs/plans/b.md", wantErr: "plan chains are not supported"},
		{name: "two plans", text: acpRoutePlan + " docs/plans/b.md", wantErr: `unexpected argument "docs/plans/b.md"`},
		{name: "another flag", text: acpRoutePlan + " --worktree", wantErr: `unsupported option "--worktree"`},
		{name: "a flag given twice", text: acpRoutePlan + " --task-model=codex --task-model claude",
			wantErr: "--task-model given more than once"},
		{name: "a missing value", text: acpRoutePlan + " --review-model", wantErr: "--review-model requires a value"},
		{name: "an empty value", text: acpRoutePlan + " --external-reviewers=", wantErr: "--external-reviewers requires a value"},
		{name: "a flag as the value", text: acpRoutePlan + " --task-model --review-model claude",
			wantErr: "--task-model requires a value"},
		{name: "a shell character", text: acpRoutePlan + " --external-reviewers=codex$(x)",
			wantErr: `plan message: invalid --external-reviewers value "codex$(x)"`},
		{name: "a model without its provider", text: acpRoutePlan + " --task-model opus",
			wantErr: `plan message: --task-model "opus" needs a claude or codex provider prefix`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseACPPlanMessage(tc.text)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestACPNamesPlan(t *testing.T) {
	for text, want := range map[string]bool{
		acpRoutePlan:                         true,
		acpRoutePlan + " --task-model x":     true,
		"README.md is outdated":              true,
		"  docs/plans/x.MD":                  true,
		"--task-model codex " + acpRoutePlan: false,
		"-x.md":                              false,
		acp.ContinuationText:                 false,
		"resume":                             false,
		"docs/plans/x":                       false,
		"":                                   false,
	} {
		assert.Equal(t, want, acpNamesPlan(text), "%q", text)
	}
}

func TestACPOutcomeLine(t *testing.T) {
	cwd := t.TempDir()
	failed := newACPTestRun(t, cwd, runs.Descriptor{ID: "a-20261022-120000-aaaa"})
	failed.finish(runs.Outcome{Status: runs.StatusFailed, Reason: "task 2 failed"})
	assert.Equal(t, "loopai run a-20261022-120000-aaaa failed: task 2 failed.",
		acpOutcomeLine(failed.dir, "a-20261022-120000-aaaa", runs.StatusFailed))

	crashed := newACPTestRun(t, cwd, runs.Descriptor{ID: "b-20261022-120000-bbbb", Attempt: 2})
	saveRunsTestOutcome(t, crashed.dir, runs.Outcome{Status: runs.StatusFailed, Reason: "an earlier attempt"})
	desc, err := runs.LoadDescriptor(crashed.dir)
	require.NoError(t, err)
	desc.Attempt = 3
	require.NoError(t, runs.SaveDescriptor(crashed.dir, &desc))
	assert.Equal(t, "loopai run b-20261022-120000-bbbb crashed: the run process ended without recording an outcome.",
		acpOutcomeLine(crashed.dir, "b-20261022-120000-bbbb", runs.StatusCrashed), "an earlier attempt's outcome is not this one's")

	succeeded := newACPTestRun(t, cwd, runs.Descriptor{ID: "c-20261022-120000-cccc"})
	succeeded.finish(runs.Outcome{Status: runs.StatusSucceeded})
	assert.Equal(t, "loopai run c-20261022-120000-cccc succeeded.",
		acpOutcomeLine(succeeded.dir, "c-20261022-120000-cccc", runs.StatusSucceeded))
}
