package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
	"github.com/umputun/ralphex/pkg/t3"
)

const (
	// t3BridgeStopTimeout bounds the before-exit wait for the bridge's final title and unpin.
	t3BridgeStopTimeout = 10 * time.Second
	// t3TerminalIDPrefix starts the id of the terminal the bridge opens: loopai-<run id>, at most 107
	// characters, so never T3's own term-1.
	t3TerminalIDPrefix = "loopai-"
	// T3 error tags the bridge answers in its own way rather than as a failure.
	t3TerminalNotRunning = "TerminalNotRunningError"
	t3TerminalLookup     = "TerminalSessionLookupError"
)

// errT3ProjectNotListed reports a thread whose project T3 Code does not list, so the bridge cannot
// build the UI's launch context for its terminal.
var errT3ProjectNotListed = errors.New("T3 Code does not list the thread's project")

// t3TitlePrefix matches the title prefix the bridge writes, [loopai: <state>], with one following
// space.
var t3TitlePrefix = regexp.MustCompile(`^\[loopai: [^\]]*\] ?`)

// newT3Bridge starts the Mode 1 bridge of a detached run and returns its bounded stop; TestMain
// replaces it with a no-op, so no test reaches a T3 server unless it asks for one.
var newT3Bridge = startT3Bridge

// bridgeLogger writes one line into the run's progress log.
type bridgeLogger interface {
	Print(format string, args ...any)
}

// t3BridgeConfig is what a Mode 1 bridge needs to show one attempt of a detached run in the T3 thread
// it was started from.
type t3BridgeConfig struct {
	token      string // the T3 bearer token; held in memory only and never logged
	threadID   string // the opaque T3 thread id, sent unchanged
	runID      string
	runDir     string // the run directory, absolute: the run changes into its worktree
	attempt    int
	fromSeq    int    // the journal seq the attempt's events follow
	checkout   string // the invoking checkout, where --attach finds the run
	executable string // the absolute loopai binary the follow command runs
	log        bridgeLogger
}

// t3BridgeTiming holds the bridge's waits. Reconnects use real time; the terminal and title rules
// read now, so tests can move their clock.
type t3BridgeTiming struct {
	tick        time.Duration // how often the rules are checked without an event
	callTimeout time.Duration // bounds one T3 request
	backoffMin  time.Duration // the first wait before a reconnect
	backoffMax  time.Duration // the longest wait before a reconnect
	retypeIdle  time.Duration // how long the terminal must run nothing before a re-type
	retypeQuiet time.Duration // how long since the last type before a re-type
	regenWait   time.Duration // how long the first title write waits for T3's title regeneration
	openWait    time.Duration // how long a freshly opened terminal may take to start running
	now         func() time.Time

	// test hooks, nil in production: called after each journal event, terminal metadata item, and
	// step the bridge handled
	afterEvent func(runs.Event)
	afterItem  func(bridgeItem)
	afterStep  func()
}

// defaultT3BridgeTiming is the production timing: a reconnect backs off from 1 s to 30 s, and the
// re-type rule tolerates the metadata stream's ~30 s lag behind hasRunningSubprocess.
func defaultT3BridgeTiming() t3BridgeTiming {
	return t3BridgeTiming{tick: time.Second, callTimeout: 30 * time.Second, backoffMin: time.Second,
		backoffMax: 30 * time.Second, retypeIdle: 60 * time.Second, retypeQuiet: 90 * time.Second,
		regenWait: 2 * time.Minute, openWait: 30 * time.Second, now: time.Now}
}

// startT3Bridge starts the bridge of cfg and returns its stop, bounded by t3BridgeStopTimeout.
func startT3Bridge(cfg t3BridgeConfig) func() {
	b := openT3Bridge(cfg, defaultT3BridgeTiming())
	return func() { b.stop(t3BridgeStopTimeout) }
}

// t3Bridge mirrors one attempt of a detached run into the T3 thread it was started from: a
// replaceable [loopai: <state>] title prefix written on transitions, a pin held while the run is
// live, and a terminal tab that follows the run through loopai --attach and is restored after a T3
// restart. It never affects the run: an unrecoverable T3 error disables it with one progress-log
// line. One goroutine owns its state; a follower feeds it the journal and a reader per connection
// the terminal metadata.
type t3Bridge struct {
	cfg        t3BridgeConfig
	timing     t3BridgeTiming
	client     *t3.Client
	terminalID string
	followCmd  string

	ctx    context.Context
	cancel context.CancelFunc
	events chan runs.Event
	done   chan struct{} // closed when run returns
	wg     sync.WaitGroup

	// the fields below belong to run's goroutine
	snap       runs.Snapshot
	state      string // the title state, "task 2/5" or "done"; empty before the first one
	final      bool   // the attempt's finish event arrived
	titleDirty bool   // the state changed since the title last matched it
	titled     bool   // the thread already carries a prefix the bridge wrote or found
	regenSince time.Time
	pinDecided bool
	pinned     bool // the bridge pinned the thread, so it unpins it at the end
	unpinned   bool
	disabled   bool

	conn    *bridgeConn
	retry   <-chan time.Time // fires when a reconnect is due; nil while connected or connecting at once
	backoff time.Duration
	server  string // the server start of the latest connection
	opened  string // the server start the bridge opened its terminal in
	term    bridgeTerminal
}

// bridgeConn is one live connection: its terminal metadata reader and how to end it. Each has a
// channel of its own, so an item of a dropped connection never reaches the loop.
type bridgeConn struct {
	server string
	items  chan bridgeItem
	closed chan struct{}
	stop   context.CancelFunc
}

// bridgeItem is one terminal metadata event, or the error that ended the stream.
type bridgeItem struct {
	event t3.TerminalMetadataEvent
	err   error
}

// bridgeTerminal is what the bridge knows of its terminal on the current server.
type bridgeTerminal struct {
	present   bool
	status    string
	busy      bool      // hasRunningSubprocess
	idleSince time.Time // when the terminal was first seen running nothing; zero while busy or unseen
	sawBusy   bool      // the terminal was seen busy since the bridge last typed
	typedAt   time.Time // when the bridge last typed the follow command
	closed    bool      // a remove arrived on a live connection: the user closed it
	pending   bool      // the terminal was opened and the follow command waits for it to run
	pendingTo time.Time // when the bridge stops waiting for it
}

// openT3Bridge starts the bridge's journal follower and its loop.
func openT3Bridge(cfg t3BridgeConfig, timing t3BridgeTiming) *t3Bridge {
	ctx, cancel := context.WithCancel(context.Background())
	b := &t3Bridge{
		cfg:        cfg,
		timing:     timing,
		client:     t3.NewClient(cfg.token),
		terminalID: t3TerminalIDPrefix + cfg.runID,
		followCmd:  t3FollowCommand(cfg.checkout, cfg.executable, cfg.runID),
		ctx:        ctx,
		cancel:     cancel,
		events:     make(chan runs.Event),
		done:       make(chan struct{}),
	}
	b.cfg.token = "" // the client holds the only copy
	b.wg.Add(2)
	go b.followJournal()
	go b.run()
	return b
}

// stop waits up to timeout for the bridge to write the final title and unpin, then ends it.
func (b *t3Bridge) stop(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-b.done:
	case <-timer.C:
	}
	b.cancel()
	<-b.done
	b.wg.Wait()
}

// followJournal passes the attempt's journal events to run until its finish event.
func (b *t3Bridge) followJournal() {
	defer b.wg.Done()
	defer close(b.events)
	_, _ = runs.FollowUntilFinish(b.ctx, b.cfg.runDir, b.cfg.attempt, b.cfg.fromSeq, func(ev runs.Event) {
		select {
		case b.events <- ev:
		case <-b.ctx.Done():
		}
	})
}

// run is the bridge's loop. It returns once the final title and the unpin are done, when the bridge
// is disabled, or when stop ends it.
func (b *t3Bridge) run() {
	defer b.wg.Done()
	defer close(b.done)
	defer b.cancel()
	defer func() { _ = b.client.Close() }()
	defer b.closeConn()
	ticker := time.NewTicker(b.timing.tick)
	defer ticker.Stop()
	events := b.events
	for {
		b.step()
		if b.ended() {
			return
		}
		var items <-chan bridgeItem
		if b.conn != nil {
			items = b.conn.items
		}
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			b.apply(ev)
		case item := <-items:
			b.handleItem(item)
		case <-b.retry:
			b.retry = nil
		case <-ticker.C:
		}
	}
}

// ended reports whether the bridge has nothing left to do.
func (b *t3Bridge) ended() bool {
	return b.disabled || b.ctx.Err() != nil || (b.final && !b.titleDirty && (!b.pinned || b.unpinned))
}

// step connects when due, then brings the thread up to date: the pin, the title, the unpin at the
// end, and the terminal's follow command.
func (b *t3Bridge) step() {
	if b.timing.afterStep != nil {
		defer b.timing.afterStep()
	}
	if b.conn == nil && b.retry == nil {
		b.connect()
	}
	for _, update := range []func(){b.syncPin, b.syncTitle, b.syncUnpin, b.syncTerminal} {
		if b.conn == nil || b.disabled {
			return
		}
		update()
	}
}

// apply folds a journal event into the run's state and marks the title for an update when the state
// it shows changed.
func (b *t3Bridge) apply(ev runs.Event) {
	if b.timing.afterEvent != nil {
		defer b.timing.afterEvent(ev)
	}
	b.snap.Apply(ev)
	if b.snap.Finish != nil {
		b.final = true
	}
	if state := t3TitleState(&b.snap); state != "" && state != b.state {
		b.state, b.titleDirty = state, true
	}
}

// connect opens a connection, re-reading the server runtime file, and subscribes to terminal
// metadata. A failure is retried after the backoff, or disables the bridge when it is unrecoverable.
func (b *t3Bridge) connect() {
	ctx, cancel := b.callContext()
	rt, err := b.client.Connect(ctx)
	cancel()
	if err != nil {
		b.fail(err)
		return
	}
	subCtx, stop := context.WithCancel(b.ctx)
	sub, err := b.client.SubscribeTerminalMetadata(subCtx)
	if err != nil {
		stop()
		b.fail(err)
		return
	}
	conn := &bridgeConn{server: t3ServerStart(rt), items: make(chan bridgeItem),
		closed: make(chan struct{}), stop: stop}
	b.conn, b.backoff = conn, 0
	if conn.server != b.server {
		// a new server start: its terminals are its own
		b.server = conn.server
		b.term = bridgeTerminal{closed: b.term.closed, typedAt: b.term.typedAt, sawBusy: b.term.sawBusy}
	}
	b.wg.Add(1)
	go b.readMetadata(sub, conn)
}

// readMetadata passes conn's terminal metadata events to run until the stream ends or the
// connection is dropped.
func (b *t3Bridge) readMetadata(sub *t3.TerminalMetadataSubscription, conn *bridgeConn) {
	defer b.wg.Done()
	defer func() { _ = sub.Close() }()
	for {
		ev, err := sub.Next()
		select {
		case conn.items <- bridgeItem{event: ev, err: err}:
		case <-conn.closed:
			return
		}
		if err != nil {
			return
		}
	}
}

// closeConn drops the current connection's subscription; the client is reconnected or closed by
// the caller.
func (b *t3Bridge) closeConn() {
	if b.conn == nil {
		return
	}
	b.conn.stop()
	close(b.conn.closed)
	b.conn = nil
}

// fail handles a failed T3 request: an unrecoverable error disables the bridge, anything else drops
// the connection and schedules a reconnect, backing off from backoffMin to backoffMax.
func (b *t3Bridge) fail(err error) {
	if b.ctx.Err() != nil {
		return
	}
	if t3BridgeFatal(err) {
		b.disable(err)
		return
	}
	b.closeConn()
	b.backoff = min(max(2*b.backoff, b.timing.backoffMin), b.timing.backoffMax)
	b.retry = time.After(b.backoff)
}

// disable stops the bridge with its one progress-log line; the run is never affected.
func (b *t3Bridge) disable(err error) {
	b.disabled = true
	b.cfg.log.Print("T3 thread updates stopped, the run continues without them: %v", err)
}

// callContext bounds one T3 request.
func (b *t3Bridge) callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(b.ctx, b.timing.callTimeout)
}

// projection reads the thread.
func (b *t3Bridge) projection() (t3.ThreadProjection, error) {
	ctx, cancel := b.callContext()
	defer cancel()
	th, err := b.client.GetThreadProjection(ctx, b.cfg.threadID)
	if err != nil {
		return t3.ThreadProjection{}, fmt.Errorf("read the thread: %w", err)
	}
	return th, nil
}

// syncPin pins the thread once, at the start, unless it is pinned already, so T3 does not
// auto-settle it during a long run. A thread T3 refuses to pin, such as an archived one, stays
// unpinned.
func (b *t3Bridge) syncPin() {
	if b.pinDecided || b.final {
		return
	}
	th, err := b.projection()
	if err != nil {
		b.fail(err)
		return
	}
	if th.PinnedAt != nil {
		b.pinDecided = true
		return
	}
	ctx, cancel := b.callContext()
	err = b.client.Pin(ctx, b.cfg.threadID)
	cancel()
	var remote *t3.RemoteError
	switch {
	case err == nil:
		b.pinDecided, b.pinned = true, true
	case errors.As(err, &remote):
		b.pinDecided = true
	default:
		b.fail(err)
	}
}

// syncUnpin unpins the thread at the end, only when the bridge pinned it.
func (b *t3Bridge) syncUnpin() {
	if !b.final || !b.pinned || b.unpinned {
		return
	}
	ctx, cancel := b.callContext()
	err := b.client.Unpin(ctx, b.cfg.threadID)
	cancel()
	var remote *t3.RemoteError
	if err == nil || errors.As(err, &remote) {
		b.unpinned = true
		return
	}
	b.fail(err)
}

// syncTitle writes the title of a changed state. The current title is read first, so a name the
// user gave the thread is kept behind the prefix, and nothing is written when it already shows the
// state. Before the first prefix, a title T3 is still generating gets regenWait to finish, because
// any title write cancels it.
func (b *t3Bridge) syncTitle() {
	if !b.titleDirty {
		return
	}
	th, err := b.projection()
	if err != nil {
		b.fail(err)
		return
	}
	title := t3PrefixedTitle(b.state, th.Title)
	if title != th.Title {
		if !b.titled && th.TitleRegeneration != nil && b.awaitRegeneration() {
			return
		}
		ctx, cancel := b.callContext()
		err = b.client.SetTitle(ctx, b.cfg.threadID, title)
		cancel()
		if err != nil {
			b.fail(err)
			return
		}
	}
	b.titleDirty, b.titled = false, true
}

// awaitRegeneration reports whether the first title write still waits for a title generation,
// which it does for at most regenWait from the first time it found one.
func (b *t3Bridge) awaitRegeneration() bool {
	now := b.timing.now()
	if b.regenSince.IsZero() {
		b.regenSince = now
	}
	return now.Sub(b.regenSince) < b.timing.regenWait
}

// handleItem applies one terminal metadata event; a stream that ended fails the connection.
func (b *t3Bridge) handleItem(item bridgeItem) {
	if b.timing.afterItem != nil {
		defer b.timing.afterItem(item)
	}
	if item.err != nil {
		b.fail(item.err)
		return
	}
	ev := item.event
	switch ev.Type {
	case t3.MetadataSnapshot:
		b.onSnapshot(ev.Terminals)
	case t3.MetadataUpsert:
		if b.ownTerminal(ev.Terminal.ThreadID, ev.Terminal.TerminalID) {
			b.observe(ev.Terminal)
		}
	case t3.MetadataRemove:
		if b.ownTerminal(ev.ThreadID, ev.TerminalID) {
			// T3 drops sessions silently when it stops, so a remove on a live connection is the
			// user closing the tab: the bridge never brings it back
			b.term.closed, b.term.present, b.term.pending = true, false, false
		}
	}
}

// ownTerminal reports whether a metadata event is about the bridge's terminal.
func (b *t3Bridge) ownTerminal(threadID, terminalID string) bool {
	return threadID == b.cfg.threadID && terminalID == b.terminalID
}

// onSnapshot handles a connection's first metadata event. A terminal present is reused; an absent
// one is opened, at most once per server start and never after the user closed it.
func (b *t3Bridge) onSnapshot(terminals []t3.TerminalSummary) {
	for _, term := range terminals {
		if b.ownTerminal(term.ThreadID, term.TerminalID) {
			b.observe(term)
			return
		}
	}
	b.term.present, b.term.pending, b.term.busy, b.term.idleSince = false, false, false, time.Time{}
	if b.term.closed || b.final || b.opened == b.conn.server {
		return
	}
	b.openTerminal()
}

// observe records what a metadata summary reports of the bridge's terminal.
func (b *t3Bridge) observe(summary t3.TerminalSummary) {
	t := &b.term
	t.present, t.status = true, summary.Status
	switch {
	case summary.HasRunningSubprocess:
		t.busy, t.idleSince, t.sawBusy = true, time.Time{}, true
	case t.busy || t.idleSince.IsZero():
		t.busy, t.idleSince = false, b.timing.now()
	}
}

// openTerminal opens the bridge's terminal with the launch context the T3 UI uses for the thread,
// so a later UI attach or reopen only resizes it: the thread's worktree, else its project root, as
// cwd, the worktree only when there is one, and the project's two environment variables. The follow
// command is typed once the terminal runs.
func (b *t3Bridge) openTerminal() {
	in, err := b.terminalLaunch()
	if err != nil {
		b.fail(err)
		return
	}
	ctx, cancel := b.callContext()
	session, err := b.client.TerminalOpen(ctx, in)
	cancel()
	if err != nil {
		b.fail(fmt.Errorf("open the terminal: %w", err))
		return
	}
	b.opened = b.conn.server
	b.term.present, b.term.status = true, session.Status
	b.term.pending, b.term.pendingTo = true, b.timing.now().Add(b.timing.openWait)
	b.typePending()
}

// terminalLaunch builds the terminal.open input from the thread and its project.
func (b *t3Bridge) terminalLaunch() (t3.TerminalOpenInput, error) {
	th, err := b.projection()
	if err != nil {
		return t3.TerminalOpenInput{}, err
	}
	ctx, cancel := b.callContext()
	projects, err := b.client.Projects(ctx)
	cancel()
	if err != nil {
		return t3.TerminalOpenInput{}, fmt.Errorf("list projects: %w", err)
	}
	root := ""
	for _, p := range projects {
		if p.ID == th.ProjectID {
			root = p.WorkspaceRoot
		}
	}
	if root == "" {
		return t3.TerminalOpenInput{}, fmt.Errorf("%w: %s", errT3ProjectNotListed, th.ProjectID)
	}
	in := t3.TerminalOpenInput{ThreadID: b.cfg.threadID, TerminalID: b.terminalID, Cwd: root,
		Env: map[string]string{"T3CODE_PROJECT_ROOT": root}}
	if th.WorktreePath != nil && *th.WorktreePath != "" {
		in.Cwd, in.WorktreePath = *th.WorktreePath, *th.WorktreePath
		in.Env["T3CODE_WORKTREE_PATH"] = *th.WorktreePath
	}
	return in, nil
}

// syncTerminal types the follow command into a terminal the bridge opened once it runs, and again
// into one that has run nothing for retypeIdle when the bridge typed nothing for retypeQuiet, which
// tolerates the metadata lag. A re-type also needs the previous command to have been seen running,
// so a command that fails at once is not typed every retypeQuiet for the rest of the run.
func (b *t3Bridge) syncTerminal() {
	if b.final {
		return
	}
	if b.term.pending {
		b.typePending()
		return
	}
	t, now := b.term, b.timing.now()
	switch {
	case t.closed || !t.present || t.status != t3.TerminalRunning || t.busy || t.idleSince.IsZero():
	case now.Sub(t.idleSince) < b.timing.retypeIdle:
	case !t.typedAt.IsZero() && (!t.sawBusy || now.Sub(t.typedAt) < b.timing.retypeQuiet):
	default:
		b.typeFollow()
	}
}

// typePending types the follow command into the terminal the bridge opened. A terminal not running
// yet is retried until pendingTo; the re-type rule takes over after that.
func (b *t3Bridge) typePending() {
	if b.typeFollow() || !b.term.present || b.timing.now().After(b.term.pendingTo) {
		b.term.pending = false
	}
}

// typeFollow types the follow command, with a leading space that keeps it out of the shell history.
// It reports whether the command was typed: a terminal not running is left for a later attempt, one
// that is gone is forgotten, and any other failure fails the connection.
func (b *t3Bridge) typeFollow() bool {
	ctx, cancel := b.callContext()
	err := b.client.TerminalWrite(ctx, b.cfg.threadID, b.terminalID, b.followCmd)
	cancel()
	var remote *t3.RemoteError
	switch {
	case err == nil:
		b.term.typedAt, b.term.sawBusy = b.timing.now(), false
		return true
	case errors.As(err, &remote) && remote.Tag == t3TerminalNotRunning:
	case errors.As(err, &remote) && remote.Tag == t3TerminalLookup:
		b.term.present = false
	default:
		b.fail(fmt.Errorf("type into the terminal: %w", err))
	}
	return false
}

// t3BridgeFatal reports whether a T3 error disables the bridge: a server that does not speak
// protocol 2 or sent a message too large to read, a rejected token or request (an HTTP 4xx other
// than a timeout or rate limit), a typed error the server answered a request with, or a thread whose
// project is not listed. Everything else, such as a server that is not running or a lost
// connection, is retried.
func t3BridgeFatal(err error) bool {
	var handshake *t3.HandshakeError
	var remote *t3.RemoteError
	switch {
	case errors.Is(err, t3.ErrProtocolUnsupported), errors.Is(err, t3.ErrReplyTooLarge),
		errors.Is(err, errT3ProjectNotListed):
		return true
	case errors.As(err, &handshake):
		code := handshake.StatusCode
		return code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
	case errors.As(err, &remote):
		return true
	}
	return false
}

// t3ServerStart identifies a server start by its pid and start time.
func t3ServerStart(rt t3.Runtime) string {
	return strconv.Itoa(rt.PID) + "@" + rt.StartedAt
}

// t3TitleState is the state a run's title prefix shows: the final done, failed, or stopped, the
// limit wait, the task and the task count, or the review stage; empty when no state is known yet.
func t3TitleState(s *runs.Snapshot) string {
	if s.Finish != nil {
		switch s.Finish.Status {
		case runs.StatusSucceeded:
			return "done"
		case runs.StatusStopped:
			return "stopped"
		default:
			return "failed"
		}
	}
	switch s.Phase {
	case status.PhaseLimitWait:
		return "limit wait"
	case status.PhaseTask, "":
		return t3TaskState(s.Task, s.TaskTotal)
	case status.PhaseReview:
		return "review"
	case status.PhaseExternalReview, status.PhaseExternalEval:
		return "external review"
	case status.PhaseFinalize:
		return "finalize"
	case status.PhaseReport:
		return "report"
	default:
		return ""
	}
}

// t3TaskState is "task n/m", or "" before the first task section. The task phase numbers a section
// past the last task when it finds none left, which shows as the last task.
func t3TaskState(n, m int) string {
	switch {
	case n <= 0:
		return ""
	case m <= 0:
		return "task " + strconv.Itoa(n)
	}
	return fmt.Sprintf("task %d/%d", min(n, m), m)
}

// t3PrefixedTitle is title with every leading [loopai: …] prefix replaced by the prefix of state;
// the prefix alone when nothing else is left.
func t3PrefixedTitle(state, title string) string {
	base := title
	for loc := t3TitlePrefix.FindStringIndex(base); loc != nil; loc = t3TitlePrefix.FindStringIndex(base) {
		base = base[loc[1]:]
	}
	prefix := "[loopai: " + state + "]"
	if base = strings.TrimSpace(base); base == "" {
		return prefix
	}
	return prefix + " " + base
}

// t3FollowCommand is the command the bridge types into its terminal: a leading space that keeps it
// out of the shell history, then loopai --attach of the run from the invoking checkout, both paths
// single-quoted for a POSIX shell since T3's terminal shell has T3's own PATH and working directory,
// and a carriage return. It is never exec'd and never ends the shell, so the tab outlives the run.
func t3FollowCommand(checkout, executable, runID string) string {
	return " cd " + posixQuote(checkout) + " && " + posixQuote(executable) + " --attach " + runID + "\r"
}

// posixQuote single-quotes s for a POSIX shell.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
