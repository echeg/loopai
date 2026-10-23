package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

const (
	// acpLivenessInterval is how long a followed run's tool call goes without an update before it is
	// retitled with the time its current step has taken, so a task that runs for hours still shows the
	// run is alive. Only transitions and this tick send updates: T3 Code stores every one.
	acpLivenessInterval = 20 * time.Minute
	// acpLivenessCheck is how often the follower checks whether the liveness retitle is due.
	acpLivenessCheck = time.Minute
	// acpFinalMessageLimit caps the JSON-escaped UTF-8 text of a run's final message; report.md in
	// the run directory keeps the full report.
	acpFinalMessageLimit = 12000
)

// acpUpdateSender sends the session/update notifications of one prompt; *acp.Updates is the one
// the provider passes.
type acpUpdateSender interface {
	Plan(entries []acp.PlanEntry) error
	ToolCall(toolCallID, title string) error
	ToolCallUpdate(toolCallID, status, title string) error
	AgentMessageChunk(text string) error
}

// acpRenderer shows a followed run in a loopai thread from its journal events alone: the plan, its
// tasks and stages, and one tool call for the current phase. It sends an update only on a
// transition, plus a liveness retitle every acpLivenessInterval without one, and never streams
// reasoning or executor output. Its methods are safe for concurrent use: the journal follower and the
// liveness ticker call them from different goroutines.
type acpRenderer struct {
	out        acpUpdateSender
	now        func() time.Time
	runID      string
	callPrefix string // makes tool call ids unique within the session across prompts

	mu        sync.Mutex
	snap      runs.Snapshot
	workKey   status.Phase    // acpCardPhase of the latest phase other than a limit wait; "" before the first
	stepTitle string          // the current step's title, without the elapsed time
	since     time.Time       // when the current step began
	plan      []acp.PlanEntry // the plan entries last sent
	callID    string          // the open tool call; "" when none is open
	callKey   status.Phase    // the phase the open tool call shows
	callTitle string          // the open tool call's title, without the elapsed time
	lastSent  time.Time       // when the last update was sent
	calls     int             // tool calls opened so far
}

// newACPRenderer returns a renderer of run runID sending through out, with now as its clock.
func newACPRenderer(out acpUpdateSender, runID string, now func() time.Time) *acpRenderer {
	return &acpRenderer{out: out, now: now, runID: runID, callPrefix: "loopai-" + randomACPHex()}
}

// randomACPHex returns 8 random bytes as hex, unique in practice within a session.
func randomACPHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return hex.EncodeToString(b)
}

// start folds the events the journal already holds and shows where the run is at once: its plan,
// and a tool call for its current phase unless the attempt already finished.
func (r *acpRenderer) start(events []runs.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range events {
		r.fold(ev)
	}
	r.trackStep(time.Time{}) // a journal without events still shows the run starting
	r.sendPlan()
	if r.snap.Finish == nil {
		r.openCall()
	}
}

// seq returns the seq of the last event the renderer folded.
func (r *acpRenderer) seq() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snap.Seq
}

// apply folds one new journal event and sends what it changed: the plan when an entry changed, a
// new tool call when the phase changed, and a retitle when the step within the phase did. An event
// that changes neither sends nothing. A finish event leaves the tool call to finish.
func (r *acpRenderer) apply(ev runs.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fold(ev)
	r.sendPlan()
	if r.snap.Finish != nil {
		return
	}
	switch {
	case r.callID == "":
		r.openCall()
	case r.workKey != r.callKey && r.callKey != "":
		r.closeCall(acp.ToolCompleted)
		r.openCall()
	case r.stepTitle != r.callTitle || r.workKey != r.callKey:
		r.callKey, r.callTitle = r.workKey, r.stepTitle
		r.send(func() error { return r.out.ToolCallUpdate(r.callID, "", r.stepTitle) })
	}
}

// tick retitles the open tool call with the time its step has taken once acpLivenessInterval passed
// without an update.
func (r *acpRenderer) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.callID == "" || r.snap.Finish != nil {
		return
	}
	now := r.now()
	if now.Sub(r.lastSent) < acpLivenessInterval {
		return
	}
	title := fmt.Sprintf("%s (%s elapsed)", r.callTitle, acpElapsed(now.Sub(r.since)))
	r.send(func() error { return r.out.ToolCallUpdate(r.callID, "", title) })
}

// finish brings the plan up to date, closes the open tool call with callStatus, and sends message as
// the run's one final agent message.
func (r *acpRenderer) finish(callStatus, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sendPlan()
	if r.callID != "" {
		r.closeCall(callStatus)
	}
	r.send(func() error { return r.out.AgentMessageChunk(message) })
}

// fold applies ev to the snapshot and tracks the phase and step it shows; the step's start is the
// time of the event that changed it.
func (r *acpRenderer) fold(ev runs.Event) {
	r.snap.Apply(ev)
	if ev.Type == runs.EventAttemptStart {
		r.workKey = ""
	}
	if r.snap.Phase != "" && r.snap.Phase != status.PhaseLimitWait {
		r.workKey = acpCardPhase(r.snap.Phase)
	}
	r.trackStep(ev.Time)
}

// acpCardPhase is the phase whose tool call shows p: an external review's evaluation stays on the
// external review's tool call, so the reviewer and evaluator turns of each iteration retitle one card
// rather than open two.
func acpCardPhase(p status.Phase) status.Phase {
	if p == status.PhaseExternalEval {
		return status.PhaseExternalReview
	}
	return p
}

// trackStep records the current step's title, and at, or now when at is zero, as its start when the
// title changed.
func (r *acpRenderer) trackStep(at time.Time) {
	title := r.titleOf()
	if title == r.stepTitle {
		return
	}
	r.stepTitle = title
	r.since = at
	if r.since.IsZero() {
		r.since = r.now()
	}
}

// openCall opens a tool call for the current step.
func (r *acpRenderer) openCall() {
	r.calls++
	id := fmt.Sprintf("%s-%d", r.callPrefix, r.calls)
	r.callID, r.callKey, r.callTitle = id, r.workKey, r.stepTitle
	r.send(func() error { return r.out.ToolCall(id, r.stepTitle) })
}

// closeCall closes the open tool call with callStatus.
func (r *acpRenderer) closeCall(callStatus string) {
	id := r.callID
	r.callID = ""
	r.send(func() error { return r.out.ToolCallUpdate(id, callStatus, "") })
}

// sendPlan sends the plan when its entries differ from the ones last sent. A run whose journal lists
// no tasks or stages yet has no plan to send.
func (r *acpRenderer) sendPlan() {
	entries := acpPlanEntries(&r.snap)
	if slices.Equal(entries, r.plan) || (len(entries) == 0 && r.plan == nil) {
		return
	}
	r.plan = entries
	r.send(func() error { return r.out.Plan(entries) })
}

// send sends one update and records when. A failed send is not retried: the client is gone or the
// prompt already ended, and T3 Code drops late updates anyway.
func (r *acpRenderer) send(fn func() error) {
	_ = fn()
	r.lastSent = r.now()
}

// titleOf is the title of the current step: the task with its position and title, the review
// section, the close-out stage, or the run starting, with a limit wait noted after it.
func (r *acpRenderer) titleOf() string {
	s := &r.snap
	var title string
	switch r.workKey {
	case "":
		title = "loopai run " + r.runID + ": starting"
	case status.PhaseTask:
		title = acpTaskTitle(s)
	case status.PhaseReview:
		title = acpReviewTitle("Review", s, runs.ReviewInternal)
	case status.PhaseExternalReview:
		title = acpReviewTitle("External review", s, runs.ReviewExternal, runs.ReviewCustom, runs.ReviewEvaluation)
	case status.PhaseFinalize:
		title = "Finalize"
	case status.PhaseReport:
		title = "Report"
	default:
		title = string(r.workKey)
	}
	if s.Phase == status.PhaseLimitWait {
		title += ": waiting for the usage limit"
	}
	return title
}

// acpTaskTitle is "Task n/m: title" for the task section the run is in, or "Tasks" before the
// first one. The task phase numbers a section past the last task when it finds none left, which
// shows as the last task.
func acpTaskTitle(s *runs.Snapshot) string {
	if s.Task <= 0 {
		return "Tasks"
	}
	n := s.Task
	if s.TaskTotal > 0 {
		n = min(n, s.TaskTotal)
	}
	title := fmt.Sprintf("Task %d", n)
	if s.TaskTotal > 0 {
		title = fmt.Sprintf("Task %d/%d", n, s.TaskTotal)
	}
	if s.TaskTitle != "" {
		title += ": " + s.TaskTitle
	}
	return title
}

// acpReviewTitle is name followed by the label of the latest review section when that section is
// one of kinds, the sections the phase runs.
func acpReviewTitle(name string, s *runs.Snapshot, kinds ...string) string {
	if s.Section.Type == runs.EventReview && s.Section.Label != "" && slices.Contains(kinds, s.Section.Kind) {
		return name + ": " + s.Section.Label
	}
	return name
}

// acpPlanEntries is the plan a snapshot shows: one entry per task, then one per pipeline stage, each
// with the medium priority T3 Code requires.
func acpPlanEntries(s *runs.Snapshot) []acp.PlanEntry {
	entries := make([]acp.PlanEntry, 0, len(s.Tasks)+len(s.Stages))
	for _, t := range s.Tasks {
		content := fmt.Sprintf("Task %d", t.N)
		if t.Title != "" {
			content += ": " + t.Title
		}
		entries = append(entries, acp.PlanEntry{Content: content, Priority: acp.PriorityMedium, Status: acpPlanStatus(t.Status)})
	}
	for _, st := range s.Stages {
		entries = append(entries, acp.PlanEntry{Content: acpStageLabel(st.Name), Priority: acp.PriorityMedium,
			Status: acpPlanStatus(st.Status)})
	}
	return entries
}

// acpPlanStatus maps a snapshot progress state to a plan entry status.
func acpPlanStatus(progress string) string {
	switch progress {
	case runs.ProgressActive:
		return acp.PlanInProgress
	case runs.ProgressDone:
		return acp.PlanCompleted
	default:
		return acp.PlanPending
	}
}

// acpStageLabel is how the plan names a pipeline stage.
func acpStageLabel(stage string) string {
	switch stage {
	case runs.StageReview:
		return "Review"
	case runs.StageExternalReview:
		return "External review"
	case runs.StageFinalize:
		return "Finalize"
	case runs.StageReport:
		return "Report"
	default:
		return stage
	}
}

// acpElapsed formats d in whole minutes: "40m", or "2h05m" from an hour on.
func acpElapsed(d time.Duration) string {
	minutes := int(d / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}

// acpFinalMessage is the message that ends a followed run's turn: how its current attempt ended,
// with its reason, then the report summary, the risk, the branch, and where the full report is,
// capped by acpCapMessage.
func acpFinalMessage(dir, id string, st runs.Status) string {
	desc, _ := runs.LoadDescriptor(dir) // an unreadable descriptor leaves the attempt 0 and no branch
	outcome, err := runs.LoadOutcome(dir)
	if err != nil || outcome.Attempt != desc.Attempt {
		outcome = runs.Outcome{}
	}
	var b strings.Builder
	b.WriteString(acpOutcomeLine(dir, id, st))
	if summary := strings.TrimSpace(outcome.ReportSummary); summary != "" {
		b.WriteString("\n\n" + summary)
	}
	branch := outcome.Branch
	if branch == "" {
		branch = desc.Branch
	}
	// report.md is written only when the run produced a report (tasks-only and early failures do
	// not), so a missing one is left out rather than named, and the truncation pointer names the
	// run directory instead
	reportPath := filepath.Join(dir, runs.ReportFile)
	_, statErr := os.Stat(reportPath)
	hasReport := statErr == nil
	shownReport := ""
	if hasReport {
		shownReport = reportPath
	}
	var fields []string
	for _, field := range []struct{ name, value string }{
		{"Risk", outcome.Risk},
		{"Branch", branch},
		{"Full report", shownReport},
	} {
		if field.value != "" {
			fields = append(fields, field.name+": "+field.value)
		}
	}
	if len(fields) > 0 {
		b.WriteString("\n\n" + strings.Join(fields, "\n"))
	}
	pointer := "\n\n[truncated; the full report is in " + reportPath + "]"
	if !hasReport {
		pointer = "\n\n[truncated; the run's records are in " + dir + "]"
	}
	return acpCapMessage(b.String(), pointer)
}

// acpCapMessage returns text when its JSON-escaped UTF-8 form fits acpFinalMessageLimit, else the
// longest prefix ending at a rune boundary that fits together with pointer, followed by pointer.
func acpCapMessage(text, pointer string) string {
	if acpJSONLen(text) <= acpFinalMessageLimit {
		return text
	}
	budget := acpFinalMessageLimit - acpJSONLen(pointer)
	starts := make([]int, 0, len(text))
	for i := range text { // i walks rune starts; an invalid byte counts as a rune of its own
		starts = append(starts, i)
	}
	// JSON escaping is per rune, so the escaped length of a prefix grows with it
	n := sort.Search(len(starts), func(i int) bool { return acpJSONLen(text[:starts[i]]) > budget })
	cut := 0
	if n > 0 {
		cut = starts[n-1]
	}
	return strings.TrimRightFunc(text[:cut], func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }) + pointer
}

// acpJSONLen is the length of s escaped as a JSON string the way the provider's encoder writes it,
// without the quotes.
func acpJSONLen(s string) int {
	data, err := json.Marshal(s)
	if err != nil { // a string always encodes
		return len(s)
	}
	return len(data) - 2
}
