package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
)

// acpAttachLockWait bounds the wait for a run's attach.lock: the prompt a steer or a restart replaces
// releases it as it ends.
const acpAttachLockWait = 2 * time.Second

// acpResumeText is what a user types into a loopai thread to relaunch its run after a Stop or a
// failure, compared trimmed and case-insensitively.
const acpResumeText = "resume"

// acpPlanFlags are the options a plan message may carry after the plan, each taking a value.
var acpPlanFlags = []string{"--task-model", "--review-model", "--external-reviewers"}

// acpTurn is one prompt of a loopai thread's session, routed to the run it shows.
type acpTurn struct {
	ctx      context.Context
	req      acp.PromptRequest
	updates  *acp.Updates
	threadID string // the session's T3 thread; "" when the provider could not look it up
	stderr   io.Writer
	// stopOnCancel arms the stop of the run a canceled prompt followed; nil arms nothing.
	stopOnCancel func(runID string)
}

// route answers a prompt by the first rule that applies:
//  1. the run bound to the thread, or to the session without a thread id: a live one, starting
//     included, is attached whatever the text says; one that ended without success is relaunched from
//     its recorded arguments and attached when the text is T3 Code's continuation or resume; one whose
//     run directory is gone is unbound with one stderr warning, and the prompt is routed as unbound;
//     one acpThreadMayClaim no longer accepts, because another loopai thread or a --t3-thread launch
//     resumed it, is unbound with one stderr warning, neither shown nor relaunched, and the prompt is
//     routed as unbound without adopting a run; one that cannot be read or classified is one stderr
//     warning, and the later rules answer;
//  2. a message naming a plan starts that plan's own detached run, binds it, and attaches to it;
//  3. without a binding, a continuation adopts the one live, unattached run a loopai thread started
//     from this cwd, or lists the candidates when there are several;
//  4. otherwise a status reply.
//
// A prompt canceled before it is routed starts nothing.
func (t *acpTurn) route() acp.StopReason {
	if t.canceled() {
		return acp.StopCancelled
	}
	text := acp.ParsePrompt(t.req.Blocks)
	continuation := acp.IsContinuation(text)
	resume := strings.EqualFold(strings.TrimSpace(text), acpResumeText)
	b, _ := loadBinding(t.req.Cwd, t.threadID, t.req.SessionID) // no binding store reads as no binding
	if b.RunID != "" {
		run, err := acpRun(t.req.Cwd, b.RunID)
		switch {
		case err != nil && acpRunGone(t.req.Cwd, b.RunID):
			t.unbindGone(b.RunID)
			b = acpBinding{}
		case err != nil:
			// the bound run cannot be read or classified: the message is still answered by the rules
			// below, and the reason they ignored the binding is left on stderr
			fmt.Fprintf(t.stderr, "warning: acp: bound run %s unavailable: %v\n", b.RunID, err)
		case !acpThreadMayClaim(run.Descriptor.Origin, t.threadID):
			// another owner resumed the run: its current attempt is not this thread's to show or
			// relaunch, and the turn answers as unbound without adopting a run
			t.unbindTakenOver(b.RunID)
			b = acpBinding{}
			continuation = false
		case run.Status.Live():
			return t.attach(run.Descriptor.ID)
		case run.Status != runs.StatusSucceeded && (continuation || resume):
			return t.launch(acpLaunch{args: run.Descriptor.Argv, envOptions: run.Descriptor.EnvOptions})
		}
	}
	if acpNamesPlan(text) {
		args, err := parseACPPlanMessage(text)
		if err != nil {
			return t.reply(acpUsageReply(err))
		}
		return t.launch(acpLaunch{args: args})
	}
	if b.RunID == "" && continuation {
		return t.adopt()
	}
	return t.reply(acpStatusReply(t.req.Cwd, b.RunID))
}

// acpRunGone reports whether run id's directory no longer exists in the session cwd, as after a prune
// or a manual removal.
func acpRunGone(cwd, id string) bool {
	_, err := os.Lstat(runs.Dir(cwd, id))
	return errors.Is(err, fs.ErrNotExist)
}

// unbindGone removes the binding to run id, whose directory is gone, so the warning about it is left
// once rather than on every turn.
func (t *acpTurn) unbindGone(id string) {
	if err := unbindRun(t.req.Cwd, t.threadID, t.req.SessionID, id); err != nil {
		fmt.Fprintf(t.stderr, "warning: acp: bound run %s no longer exists, and its binding cannot be removed: %v\n", id, err)
		return
	}
	fmt.Fprintf(t.stderr, "warning: acp: bound run %s no longer exists; the thread is no longer bound to it\n", id)
}

// unbindTakenOver removes the binding to run id, which another loopai thread or a T3 terminal launch
// resumed since, so the thread no longer shows or relaunches it and the warning is left once.
func (t *acpTurn) unbindTakenOver(id string) {
	if err := unbindRun(t.req.Cwd, t.threadID, t.req.SessionID, id); err != nil {
		fmt.Fprintf(t.stderr, "warning: acp: bound run %s now belongs to another owner, and its binding cannot be "+
			"removed: %v\n", id, err)
		return
	}
	fmt.Fprintf(t.stderr, "warning: acp: bound run %s now belongs to another owner; the thread is no longer bound to it\n", id)
}

// acpRun returns the run id names in the session cwd's runs directory, classified.
func acpRun(cwd, id string) (runs.Run, error) {
	dir := runs.Dir(cwd, id)
	desc, err := runs.LoadDescriptor(dir)
	if err != nil {
		return runs.Run{}, fmt.Errorf("load run %s: %w", id, err)
	}
	status, err := runs.Classify(cwd, dir)
	if err != nil {
		return runs.Run{}, fmt.Errorf("classify run %s: %w", id, err)
	}
	return runs.Run{Dir: dir, Descriptor: desc, Status: status}, nil
}

// launch starts the detached run l describes from this session, then binds and follows the run the
// launcher started once its attach.lock is held. A launcher's refusal is the turn's answer, and so is
// a run another provider took first, which this thread then does not bind.
func (t *acpTurn) launch(l acpLaunch) acp.StopReason {
	l.cwd, l.threadID, l.sessionID = t.req.Cwd, t.threadID, t.req.SessionID
	id, err := acpLaunchDetached(t.ctx, l)
	if err != nil {
		return t.reply(err.Error())
	}
	release, refusal := t.lockAttach(id)
	if release == nil {
		return t.reply(refusal)
	}
	defer release()
	t.bind(id)
	return t.follow(id)
}

// acpAdoptionCandidatesFn is acpAdoptionCandidates; tests replace it to take a candidate's attach.lock
// between the selection and the adoption.
var acpAdoptionCandidatesFn = acpAdoptionCandidates

// adopt binds and follows the one run acpAdoptionCandidates finds, or answers with the candidates
// when there are several and with the status reply when there is none. The selection only probes each
// attach.lock, so two providers can both pick a run: adopt takes the lock first and checks the run is
// still a candidate under it, and only then binds it, so the provider that loses answers without
// leaving a binding to a run it does not follow.
func (t *acpTurn) adopt() acp.StopReason {
	candidates, err := acpAdoptionCandidatesFn(t.req.Cwd, t.threadID)
	switch {
	case err != nil:
		return t.reply(fmt.Sprintf("loopai cannot read the detached runs of this checkout: %v", err))
	case len(candidates) > 1:
		return t.reply(acpCandidatesReply(candidates))
	case len(candidates) == 0:
		return t.reply(acpStatusReply(t.req.Cwd, ""))
	}
	id := candidates[0].Descriptor.ID
	release, refusal := t.lockAttach(id)
	if release == nil {
		return t.reply(refusal)
	}
	defer release()
	if run, runErr := acpRun(t.req.Cwd, id); runErr != nil || !acpAdoptable(run, t.req.Cwd, t.threadID) {
		return t.reply(acpStatusReply(t.req.Cwd, ""))
	}
	t.bind(id)
	return t.follow(id)
}

// acpAdoptionCandidates returns the runs a continuation without a binding may adopt: the runs
// acpAdoptable accepts which no provider follows, since their attach.lock is free.
func acpAdoptionCandidates(cwd, threadID string) ([]runs.Run, error) {
	all, err := runs.List(cwd)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	var candidates []runs.Run
	for _, run := range all {
		if !acpAdoptable(run, cwd, threadID) {
			continue
		}
		if held, probeErr := runs.Probe(filepath.Join(run.Dir, runs.AttachLockFile)); probeErr != nil || held {
			continue
		}
		candidates = append(candidates, run)
	}
	return candidates, nil
}

// acpAdoptable reports whether a thread of threadID in cwd may adopt run, its attach.lock aside: the
// run is live and a loopai thread started it (origin t3-thread) from cwd. A run recorded for another
// thread than threadID, when both ids are known, is never adoptable.
func acpAdoptable(run runs.Run, cwd, threadID string) bool {
	if !run.Status.Live() || !sameProgressRoot(run.Descriptor.Cwd, cwd) {
		return false
	}
	return acpThreadMayClaim(run.Descriptor.Origin, threadID)
}

// acpThreadMayClaim reports whether a run of origin o may be a thread of threadID's own: a loopai thread
// started or last resumed it (origin t3-thread), and no recorded thread id other than threadID says it
// is another thread's. A thread follows and relaunches its bound run only while this holds, and adopts
// only such runs, so a run another thread or a --t3-thread launch resumed is neither shown nor traded
// back. It is weaker than acpOwnsRun, which Stop requires: without a thread id on either side the
// session ids differ after a session/load fallback, and the spec's adoption accepts the run then.
func acpThreadMayClaim(o runs.Origin, threadID string) bool {
	return o.Kind == runs.OriginT3Thread && (threadID == "" || o.ThreadID == "" || o.ThreadID == threadID)
}

// bind binds run id to the thread, or to the session without a thread id. A binding that cannot be
// recorded is one warning: the attach goes on, and a later continuation can adopt the run again.
func (t *acpTurn) bind(id string) {
	if err := bindRun(t.req.Cwd, t.threadID, t.req.SessionID, id); err != nil {
		fmt.Fprintf(t.stderr, "warning: acp: run binding not recorded: %v\n", err)
	}
}

// attach follows the run id names, already bound to this thread, while holding its attach.lock.
func (t *acpTurn) attach(id string) acp.StopReason {
	release, refusal := t.lockAttach(id)
	if release == nil {
		return t.reply(refusal)
	}
	defer release()
	return t.follow(id)
}

// lockAttach takes run id's attach.lock, so no other provider adopts the run while this one follows it,
// and returns the function that releases it. When the lock cannot be taken it returns a nil release and
// the turn's answer: a run another provider follows is answered as such.
func (t *acpTurn) lockAttach(id string) (release func(), refusal string) {
	unlock, err := runs.AcquireBounded(filepath.Join(runs.Dir(t.req.Cwd, id), runs.AttachLockFile), acpAttachLockWait)
	if errors.Is(err, runs.ErrBusy) {
		return nil, fmt.Sprintf("loopai run %s is followed by another loopai provider; follow it from a shell with "+
			"loopai --attach %s.", id, id)
	}
	if err != nil {
		return nil, fmt.Sprintf("loopai cannot follow run %s: %v", id, err)
	}
	return func() {
		if releaseErr := unlock(); releaseErr != nil {
			fmt.Fprintf(t.stderr, "warning: acp: release the attach lock of run %s: %v\n", id, releaseErr)
		}
	}, ""
}

// follow follows run id, whose attach.lock the caller holds, until it is no longer live: it shows the
// run's plan and current phase through an acpRenderer, then closes the phase's tool call and sends the
// final message, which every finished run ends with, whatever its status; an attempt that ended
// without a finish event shows no plan entry in progress. A cancel closes the open tool call, ends the
// turn with StopCancelled, leaves the run running, and arms its stop through stopOnCancel; the
// provider stopping ends it unanswered.
func (t *acpTurn) follow(id string) acp.StopReason {
	dir := runs.Dir(t.req.Cwd, id)
	events, err := runs.ReadFrom(dir, 0)
	if err != nil {
		return t.reply(fmt.Sprintf("loopai cannot follow run %s: %v", id, err))
	}
	r := newACPRenderer(t.updates, id, time.Now)
	r.start(events)
	followCtx, cancel := context.WithCancel(t.ctx)
	defer cancel()
	var watcher sync.WaitGroup
	watcher.Go(func() {
		ticker := time.NewTicker(acpLivenessCheck)
		defer ticker.Stop()
		for {
			select {
			case <-t.req.Canceled:
				cancel()
				return
			case <-followCtx.Done():
				return
			case <-ticker.C:
				r.tick()
			}
		}
	})
	status, err := runs.Follow(followCtx, t.req.Cwd, dir, r.seq(), r.apply)
	cancel()
	watcher.Wait() // no liveness retitle after the final message
	switch {
	case t.canceled():
		r.abandon() // before the StopCancelled answer, after which T3 Code drops every update
		if t.stopOnCancel != nil {
			t.stopOnCancel(id) // armed before the StopCancelled answer, which a steered prompt follows
		}
		return acp.StopCancelled
	case t.ctx.Err() != nil:
		return acp.StopEndTurn // the provider stopped and answers nothing
	case err != nil:
		r.finish(acp.ToolFailed, fmt.Sprintf("loopai cannot follow run %s: %v", id, err))
		return acp.StopEndTurn
	}
	callStatus := acp.ToolFailed
	if status == runs.StatusSucceeded {
		callStatus = acp.ToolCompleted
	} else {
		r.endedWithoutFinish()
	}
	r.finish(callStatus, acpFinalMessage(dir, id, status))
	return acp.StopEndTurn
}

// canceled reports whether the client canceled the prompt.
func (t *acpTurn) canceled() bool {
	select {
	case <-t.req.Canceled:
		return true
	default:
		return false
	}
}

// reply answers the prompt with text and ends the turn.
func (t *acpTurn) reply(text string) acp.StopReason {
	_ = t.updates.AgentMessageChunk(text) // nothing to do when the client is gone
	return acp.StopEndTurn
}

// acpOutcomeLine is how the current attempt of run id in dir ended: its status and the reason its
// outcome or, without one, its finish event records. It opens the final message.
func acpOutcomeLine(dir, id string, status runs.Status) string {
	attempt := 0
	if desc, err := runs.LoadDescriptor(dir); err == nil {
		attempt = desc.Attempt
	}
	reason := missingOutcomeReason(dir, attempt, status)
	if outcome, err := runs.LoadOutcome(dir); err == nil && outcome.Attempt == attempt {
		reason = outcome.Reason
	}
	line := fmt.Sprintf("loopai run %s %s", id, status)
	if reason != "" {
		line += ": " + reason
	}
	return line + "."
}

// acpCandidatesReply lists the runs a continuation could adopt when there is more than one, none of
// which it binds.
func acpCandidatesReply(candidates []runs.Run) string {
	var b strings.Builder
	b.WriteString("Several live loopai runs started from T3 Code threads in this checkout could belong to this " +
		"thread, so none was attached:\n")
	for _, run := range candidates {
		d := run.Descriptor
		fmt.Fprintf(&b, "\n- %s: %s on branch %s, %s", d.ID, d.Plan, d.Branch, run.Status)
	}
	b.WriteString("\n\nFollow one from a shell with loopai --attach <run>, or stop it with loopai --stop <run>.\n\n" +
		acpModelSwitchHint)
	return b.String()
}

// acpUsageReply answers a plan message whose flags cannot start a run.
func acpUsageReply(err error) string {
	return fmt.Sprintf("loopai cannot start a run from this message: %v.\n\nSend the plan path, optionally with model "+
		"flags:\n\n    %s", err, acpPromptUsage)
}

// acpNamesPlan reports whether a message names a plan: its first word is a Markdown file path.
func acpNamesPlan(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && acpPlanToken(fields[0])
}

// acpPlanToken reports whether tok is a plan path: a .md path that is not an option.
func acpPlanToken(tok string) bool {
	return !strings.HasPrefix(tok, "-") && strings.HasSuffix(strings.ToLower(tok), ".md")
}

// parseACPPlanMessage returns the arguments of the run a plan message asks for: each model flag it
// carries, as --flag=value in the message's order, then the plan. The message names exactly one plan,
// first, followed only by --task-model, --review-model, and --external-reviewers, each at most once, in
// its --flag value or --flag=value form, with a value validatePassThroughValues accepts.
func parseACPPlanMessage(text string) ([]string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !acpPlanToken(fields[0]) {
		return nil, errors.New("the message must start with the plan file")
	}
	plan := fields[0]
	if strings.Contains(plan, ",") {
		return nil, errors.New("a loopai thread runs exactly one plan; plan chains are not supported")
	}
	var o opts
	values := map[string]*string{"--task-model": &o.TaskModel, "--review-model": &o.ReviewModel,
		"--external-reviewers": &o.ExternalReviewers}
	var args []string
	for i := 1; i < len(fields); i++ {
		name, value, inline := strings.Cut(fields[i], "=")
		target, ok := values[name]
		switch {
		case !strings.HasPrefix(fields[i], "-"):
			return nil, fmt.Errorf("unexpected argument %q: the message names exactly one plan file", fields[i])
		case !ok:
			return nil, fmt.Errorf("unsupported option %q; a plan message takes only %s", name,
				strings.Join(acpPlanFlags, ", "))
		case *target != "":
			return nil, fmt.Errorf("%s given more than once", name)
		}
		if !inline && i+1 < len(fields) {
			i++
			value = fields[i]
		}
		if value == "" || strings.HasPrefix(value, "-") {
			return nil, fmt.Errorf("%s requires a value", name)
		}
		*target = value
		args = append(args, name+"="+value)
	}
	if err := validatePassThroughValues("plan message", o); err != nil {
		return nil, err
	}
	return append(args, plan), nil
}
