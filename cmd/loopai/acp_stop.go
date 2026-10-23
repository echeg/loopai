package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/t3"
)

const (
	// acpStopGrace is how long a canceled prompt waits, from its StopCancelled answer, before stopping the
	// run it followed. T3 Code sends a steer as a cancel followed by a new prompt once the canceled one
	// is answered, and only a Stop leaves the cancel alone; a prompt within the grace keeps the run.
	// The basis is verified in T3 Code's source but not live, so a mismatch changes only this value or
	// acpStops.
	acpStopGrace = 5 * time.Second
	// acpStopGuardTimeout bounds the thread read that checks a cancel was the user's Stop.
	acpStopGuardTimeout = 5 * time.Second
)

// acpStopGraceWait is the grace the provider waits; tests shorten it, and acpStopGrace is its value.
var acpStopGraceWait = acpStopGrace

// acpThreadInterrupted reports whether T3 Code recorded a user interrupt request for the latest run of
// threadID, read with cred. TestMain replaces it, so no test reaches a real T3 server.
var acpThreadInterrupted = func(ctx context.Context, cred t3.MCPCredential, threadID string) (bool, error) {
	interrupted, err := t3.NewMCPClient(cred, resolveVersion()).InterruptRequested(ctx, threadID)
	if err != nil {
		return false, fmt.Errorf("read the thread: %w", err)
	}
	return interrupted, nil
}

// acpStopRequest is a pending stop: the run a canceled prompt followed, and who canceled it.
type acpStopRequest struct {
	ctx       context.Context // the provider's; canceled when it stops
	cwd       string
	threadID  string // "" when the session's thread is unknown
	sessionID string
	runID     string
	// credential returns the session's latest MCP credential, held in memory only; T3 Code may hand
	// the session a new one through session/load while the stop waits
	credential func() (t3.MCPCredential, bool)
}

// acpStops holds the provider's pending stops, at most one per session. A stop waits
// acpStopGraceWait; a new prompt of the session cancels it, and the provider stopping abandons every
// one. Once the grace passes, stop stops the run only when it still is the thread's bound run, started
// by a loopai thread with a matching binding key, and T3 Code recorded the cancel as the user's Stop.
type acpStops struct {
	stderr io.Writer

	mu      sync.Mutex
	closed  bool
	pending map[string]*time.Timer // session id -> grace timer
}

func newACPStops(stderr io.Writer) *acpStops {
	return &acpStops{stderr: stderr, pending: map[string]*time.Timer{}}
}

// arm starts the grace of a stop of req's run, replacing a stop the session armed before.
func (s *acpStops) arm(req acpStopRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if prev := s.pending[req.sessionID]; prev != nil {
		prev.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(acpStopGraceWait, func() {
		s.mu.Lock()
		current := !s.closed && s.pending[req.sessionID] == timer
		if current {
			delete(s.pending, req.sessionID)
		}
		s.mu.Unlock()
		if current {
			s.stop(req)
		}
	})
	s.pending[req.sessionID] = timer
}

// cancel drops the session's pending stop: a new prompt arrived within the grace.
func (s *acpStops) cancel(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if timer := s.pending[sessionID]; timer != nil {
		timer.Stop()
		delete(s.pending, sessionID)
	}
}

// close abandons every pending stop and arms no more: the provider ends on SIGTERM or the end of
// its input, as when T3 Code restarts, and a run is never stopped on the way out.
func (s *acpStops) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, timer := range s.pending {
		timer.Stop()
		delete(s.pending, id)
	}
}

// isClosed reports whether close was called.
func (s *acpStops) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// stop stops req's run once its grace passed: through stopRun, which writes the stop marker and
// sends SIGTERM, leaving the worktree in place for resume. It does nothing for a run that is no longer
// the binding's, no longer live, or not this thread's own, for a cancel the thread read shows was not a
// Stop, and once the provider stopped.
func (s *acpStops) stop(req acpStopRequest) {
	if req.ctx.Err() != nil {
		return
	}
	run, ok := acpStoppableRun(req)
	if !ok {
		return
	}
	if !s.confirmStop(req) || req.ctx.Err() != nil || s.isClosed() {
		return
	}
	if err := stopRun(req.cwd, run.Descriptor); err != nil {
		fmt.Fprintf(s.stderr, "warning: acp: run %s not stopped: %v\n", req.runID, err)
	}
}

// confirmStop reports whether a cancel should stop the run: unless T3 Code's thread shows its latest
// run carries no user interrupt request, which a steer or a cancel of T3 Code's own leaves. The read is
// best-effort: without a thread id or a credential, and when it fails, the stop goes ahead.
func (s *acpStops) confirmStop(req acpStopRequest) bool {
	if req.threadID == "" || req.credential == nil {
		return true
	}
	cred, ok := req.credential()
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(req.ctx, acpStopGuardTimeout)
	defer cancel()
	interrupted, err := acpThreadInterrupted(ctx, cred, req.threadID)
	if err != nil {
		if req.ctx.Err() == nil {
			fmt.Fprintf(s.stderr, "acp: stopping run %s without checking the thread: %v\n", req.runID, err)
		}
		return true
	}
	if !interrupted {
		fmt.Fprintf(s.stderr, "acp: run %s keeps running: the thread records no Stop for its cancel\n", req.runID)
	}
	return interrupted
}

// acpStoppableRun returns req's run when a stop may touch it: the binding of req's thread, or of its
// session without a thread id, still names it, it is live, and acpOwnsRun accepts it.
func acpStoppableRun(req acpStopRequest) (runs.Run, bool) {
	b, _ := loadBinding(req.cwd, req.threadID, req.sessionID)
	if b.RunID != req.runID {
		return runs.Run{}, false
	}
	run, err := acpRun(req.cwd, req.runID)
	if err != nil || !run.Status.Live() {
		return runs.Run{}, false
	}
	return run, acpOwnsRun(run.Descriptor.Origin, req.threadID, req.sessionID)
}

// acpOwnsRun reports whether a loopai thread's session may stop a run of origin o: a loopai thread
// started it (origin t3-thread), and its binding key matches, which is the thread id when both sides
// know it and the session id otherwise.
func acpOwnsRun(o runs.Origin, threadID, sessionID string) bool {
	if o.Kind != runs.OriginT3Thread {
		return false
	}
	if threadID != "" && o.ThreadID != "" {
		return o.ThreadID == threadID
	}
	return o.ACPSessionID != "" && o.ACPSessionID == sessionID
}
