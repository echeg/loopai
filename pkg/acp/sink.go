package acp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
)

const (
	// thoughtInterval is the minimum spacing between two agent_thought_chunk notifications;
	// executor output arriving in between is coalesced into the next one.
	thoughtInterval = 500 * time.Millisecond
	// maxThoughtChunk caps the text of one agent_thought_chunk; output beyond it is dropped and
	// replaced by a short omission note.
	maxThoughtChunk = 8 << 10
	// heartbeatIdle is how long the sink stays silent before it reports the elapsed wait. It is
	// well below the Grok driver's 10-minute turn watchdog, which fails a turn without progress.
	heartbeatIdle = 4 * time.Minute
)

// ACP tool call statuses used by the sink.
const (
	toolInProgress = "in_progress"
	toolCompleted  = "completed"
	toolFailed     = "failed"
)

// ACP plan entry statuses.
const (
	entryPending    = "pending"
	entryInProgress = "in_progress"
	entryCompleted  = "completed"
)

// Stage is a post-task pipeline stage shown as a plan entry after the plan's own tasks.
type Stage string

// Stages a run may pass through after its tasks.
const (
	StageReview         Stage = "Review"
	StageExternalReview Stage = "External review"
	StageFinalize       Stage = "Finalize"
	StageReport         Stage = "Report"
)

// stageForPhase maps an execution phase to the plan stage it belongs to.
func stageForPhase(p status.Phase) (Stage, bool) {
	switch p {
	case status.PhaseReview:
		return StageReview, true
	case status.PhaseExternalReview, status.PhaseExternalEval, status.PhaseCodex, status.PhaseClaudeEval:
		return StageExternalReview, true
	case status.PhaseFinalize:
		return StageFinalize, true
	case status.PhaseReport:
		return StageReport, true
	default:
		return "", false
	}
}

// stageOrder is a stage's position in the pipeline. Stages run in this order, though an undeclared
// stage is appended to the plan entries when reached.
func stageOrder(st Stage) int {
	switch st {
	case StageReview:
		return 1
	case StageExternalReview:
		return 2
	case StageFinalize:
		return 3
	case StageReport:
		return 4
	default:
		return 0
	}
}

// clock abstracts time so tests can drive coalescing and the heartbeat deterministically.
type clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) stopper
}

type stopper interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }

// activity is the execution state an open tool call describes.
type activity struct {
	phase     status.Phase
	task      int // 1-indexed plan task position; 0 when unknown
	iteration int // review iteration; 0 for the first pass or when unknown
}

// stageState is one stage's plan entry.
type stageState struct {
	stage  Stage
	status string
}

// Sink turns loopai execution events into session/update notifications for one session's running
// prompt: phases become tool calls, sections refresh the plan entries, executor output becomes
// coalesced reasoning, and the final report becomes an assistant message. A nil Sink drops every
// event, and send failures are ignored: reporting is best-effort and never fails a run. The sink
// is safe for concurrent use.
type Sink struct {
	conn      *Conn
	sessionID string
	clk       clock

	mu       sync.Mutex
	finished bool

	// tool calls
	callPrefix    string // keeps ids unique across the prompts of one session
	callSeq       int
	callID        string // open tool call; empty when none
	callTitle     string
	callDetailed  bool // the open call's title already names a task or iteration
	heartbeatShow bool // the open call's title currently carries the heartbeat suffix
	cur           activity
	beforeWait    activity // activity interrupted by a provider-limit wait

	// plan entries
	planFile  string
	tasks     []plan.Task
	stages    []stageState
	lastPlan  []planEntry
	stageOpen int // index of the in-progress stage; -1 when none

	// reasoning
	thought      strings.Builder
	omitted      int // bytes dropped from the pending chunk
	lastThought  time.Time
	flushSeq     int
	flushPending bool
	flushTimer   stopper

	// heartbeat
	lastSent     time.Time // any notification, heartbeats included
	lastActivity time.Time // any notification except heartbeats
	beatSeq      int
	beatTimer    stopper
}

// NewSink returns a sink publishing updates for the given session on conn. Its heartbeat starts
// immediately; Finish stops it. Tool call ids carry a random prefix: ACP requires them to be unique
// within a session, and one session spans many prompts, each with its own sink, and survives a
// process restart through session/load.
func NewSink(conn *Conn, sessionID string) *Sink {
	s := newSinkWithClock(conn, sessionID, realClock{})
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		s.callPrefix = "loopai-" + hex.EncodeToString(b[:])
	}
	return s
}

func newSinkWithClock(conn *Conn, sessionID string, clk clock) *Sink {
	now := clk.Now()
	s := &Sink{conn: conn, sessionID: sessionID, clk: clk, callPrefix: "loopai", stageOpen: -1,
		lastSent: now, lastActivity: now}
	s.mu.Lock()
	s.armHeartbeatLocked(heartbeatIdle)
	s.mu.Unlock()
	return s
}

// disabled reports a nil sink; every method is then a no-op.
func (s *Sink) disabled() bool {
	return s == nil
}

// textContent is an ACP text content block.
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// chunkUpdate is an agent_message_chunk or agent_thought_chunk session update.
type chunkUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Content       textContent `json:"content"`
}

// toolCall opens a tool call.
type toolCall struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallID    string `json:"toolCallId"`
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
}

// toolCallUpdate changes an open tool call's title or status.
type toolCallUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallID    string `json:"toolCallId"`
	Title         string `json:"title,omitempty"`
	Status        string `json:"status,omitempty"`
}

type planEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

type planUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Entries       []planEntry `json:"entries"`
}

type sessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// sendLocked writes one notification and records it as activity for the heartbeat.
func (s *Sink) sendLocked(update any) {
	s.sendRawLocked(update)
	s.lastActivity = s.lastSent
}

func (s *Sink) sendRawLocked(update any) {
	_ = s.conn.Notify("session/update", sessionUpdateParams{SessionID: s.sessionID, Update: update})
	s.lastSent = s.clk.Now()
}

// Message sends text as an assistant message chunk. Empty text sends nothing. Pending reasoning
// is flushed first so the message follows it.
func (s *Sink) Message(text string) {
	if s.disabled() || text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushThoughtLocked()
	s.sendLocked(chunkUpdate{SessionUpdate: "agent_message_chunk", Content: textContent{Type: "text", Text: text}})
}

// SetPlan names the plan file whose tasks become plan entries, followed by the stages the run is
// expected to reach. A stage reached without being declared is appended when it starts.
func (s *Sink) SetPlan(planFile string, stages ...Stage) {
	if s.disabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.planFile = planFile
	s.stages = s.stages[:0]
	s.stageOpen = -1
	for _, st := range stages {
		if s.stageIndexLocked(st) < 0 {
			s.stages = append(s.stages, stageState{stage: st, status: entryPending})
		}
	}
	s.reparseLocked()
	s.refreshPlanLocked()
}

// OnPhase opens a tool call for the new phase and closes the previous one. Its signature matches
// status.PhaseHolder.OnChange.
func (s *Sink) OnPhase(old, cur status.Phase) {
	if s.disabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || cur == "" {
		return
	}
	s.flushThoughtLocked()

	next := activity{phase: cur}
	switch {
	case cur == status.PhaseLimitWait:
		s.beforeWait = s.cur
	case old == status.PhaseLimitWait && cur == s.beforeWait.phase:
		// resuming the interrupted activity keeps its task or iteration
		next = s.beforeWait
	}
	s.cur = next
	s.openCallLocked(activityTitle(next, len(s.tasks)), next.task > 0 || next.iteration > 0)

	if st, ok := stageForPhase(cur); ok {
		s.enterStageLocked(st)
	}
	s.reparseLocked()
	s.refreshPlanLocked()
}

// OnSection names the task or review iteration the current phase is working on and refreshes the
// plan entries.
func (s *Sink) OnSection(section status.Section) {
	if s.disabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	next := s.cur
	switch section.Type {
	case status.SectionTaskIteration:
		next = activity{phase: status.PhaseTask, task: section.Iteration}
	case status.SectionInternalReview:
		next = activity{phase: status.PhaseReview, iteration: section.Iteration}
	case status.SectionExternalReviewIteration, status.SectionCustomIteration:
		next = activity{phase: status.PhaseExternalReview, iteration: section.Iteration}
	default:
	}
	if next != s.cur {
		s.flushThoughtLocked()
		title := activityTitle(next, len(s.tasks))
		switch {
		case s.callID != "" && !s.callDetailed && next.phase == s.cur.phase:
			// the call opened by the phase change gains its task or iteration in place
			s.callTitle, s.callDetailed, s.heartbeatShow = title, true, false
			s.sendLocked(toolCallUpdate{SessionUpdate: "tool_call_update", ToolCallID: s.callID, Title: title})
		default:
			s.openCallLocked(title, true)
		}
		s.cur = next
	}
	s.reparseLocked()
	s.refreshPlanLocked()
}

// Output queues executor output as reasoning. Notifications are spaced at least thoughtInterval
// apart; text arriving in between is coalesced and capped at maxThoughtChunk.
func (s *Sink) Output(text string) {
	if s.disabled() || text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if room := maxThoughtChunk - s.thought.Len(); s.omitted > 0 || len(text) > room {
		if s.omitted == 0 && room > 0 {
			head := truncateUTF8(text, room)
			s.thought.WriteString(head)
			text = text[len(head):]
		}
		s.omitted += len(text)
	} else {
		s.thought.WriteString(text)
	}

	if s.flushPending {
		return
	}
	wait := thoughtInterval - s.clk.Now().Sub(s.lastThought)
	if s.lastThought.IsZero() || wait <= 0 {
		s.flushThoughtLocked()
		return
	}
	s.flushSeq++
	seq := s.flushSeq
	s.flushPending = true
	s.flushTimer = s.clk.AfterFunc(wait, func() { s.flushTimed(seq) })
}

func (s *Sink) flushTimed(seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq != s.flushSeq || !s.flushPending {
		return
	}
	s.flushThoughtLocked()
}

// flushThoughtLocked sends the pending reasoning, if any, and cancels a scheduled flush.
func (s *Sink) flushThoughtLocked() {
	if s.flushPending {
		s.flushPending = false
		s.flushSeq++
		if s.flushTimer != nil {
			s.flushTimer.Stop()
			s.flushTimer = nil
		}
	}
	if s.thought.Len() == 0 && s.omitted == 0 {
		return
	}
	text := s.thought.String()
	if s.omitted > 0 {
		text += fmt.Sprintf("[%d bytes of output omitted]\n", s.omitted)
	}
	s.thought.Reset()
	s.omitted = 0
	s.sendLocked(chunkUpdate{SessionUpdate: "agent_thought_chunk", Content: textContent{Type: "text", Text: text}})
	s.lastThought = s.clk.Now()
	s.restoreTitleLocked()
}

// restoreTitleLocked drops the heartbeat suffix from the open call once output resumes.
func (s *Sink) restoreTitleLocked() {
	if !s.heartbeatShow || s.callID == "" {
		return
	}
	s.heartbeatShow = false
	s.sendLocked(toolCallUpdate{SessionUpdate: "tool_call_update", ToolCallID: s.callID, Title: s.callTitle})
}

// truncateUTF8 returns the longest prefix of text no longer than n bytes that ends on a rune boundary.
func truncateUTF8(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

// Finish flushes pending reasoning, closes the open tool call as completed or failed, publishes
// the final plan entries, and stops the heartbeat. Later events are dropped; Message still works,
// so the final report can follow. It is safe to call more than once.
func (s *Sink) Finish(success bool) {
	if s.disabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.flushThoughtLocked()
	callStatus := toolFailed
	if success {
		callStatus = toolCompleted
	}
	s.closeCallLocked(callStatus)
	// an unfinished stage drops back to pending, as an unfinished task does once cur is cleared;
	// a successful run has passed every stage, including any a review checkpoint let it skip
	if s.stageOpen >= 0 {
		s.stages[s.stageOpen].status = entryPending
		s.stageOpen = -1
	}
	if success {
		for i := range s.stages {
			s.stages[i].status = entryCompleted
		}
	}
	s.cur = activity{}
	s.reparseLocked()
	s.refreshPlanLocked()
	s.finished = true
	s.beatSeq++
	if s.beatTimer != nil {
		s.beatTimer.Stop()
		s.beatTimer = nil
	}
}

// openCallLocked closes the open tool call as completed and opens a new one.
func (s *Sink) openCallLocked(title string, detailed bool) {
	s.closeCallLocked(toolCompleted)
	s.callSeq++
	s.callID = fmt.Sprintf("%s-%d", s.callPrefix, s.callSeq)
	s.callTitle, s.callDetailed, s.heartbeatShow = title, detailed, false
	s.sendLocked(toolCall{SessionUpdate: "tool_call", ToolCallID: s.callID, Title: title, Kind: "other", Status: toolInProgress})
}

func (s *Sink) closeCallLocked(callStatus string) {
	if s.callID == "" {
		return
	}
	upd := toolCallUpdate{SessionUpdate: "tool_call_update", ToolCallID: s.callID, Status: callStatus}
	if s.heartbeatShow {
		upd.Title = s.callTitle
	}
	s.sendLocked(upd)
	s.callID, s.callTitle, s.callDetailed, s.heartbeatShow = "", "", false, false
}

// enterStageLocked marks a stage in progress and every stage earlier in the pipeline completed,
// which covers both the previously running stage and any a resumed run skipped through its review
// checkpoint. A completed stage never reopens: the review that follows external review findings
// runs under the external review stage, so progress does not go backwards.
func (s *Sink) enterStageLocked(st Stage) {
	idx := s.stageIndexLocked(st)
	if idx < 0 {
		s.stages = append(s.stages, stageState{stage: st, status: entryPending})
		idx = len(s.stages) - 1
	}
	if s.stages[idx].status == entryCompleted {
		return
	}
	if s.stageOpen >= 0 && s.stageOpen != idx {
		s.stages[s.stageOpen].status = entryCompleted
	}
	for i := range s.stages {
		if stageOrder(s.stages[i].stage) < stageOrder(st) {
			s.stages[i].status = entryCompleted
		}
	}
	s.stages[idx].status = entryInProgress
	s.stageOpen = idx
}

func (s *Sink) stageIndexLocked(st Stage) int {
	for i, cur := range s.stages {
		if cur.stage == st {
			return i
		}
	}
	return -1
}

// reparseLocked rereads the plan's tasks. A plan archived to completed/ at the end of the run is
// read from there; when neither copy parses, the last parsed tasks are kept.
func (s *Sink) reparseLocked() {
	if s.planFile == "" {
		return
	}
	for _, path := range []string{s.planFile, filepath.Join(filepath.Dir(s.planFile), "completed", filepath.Base(s.planFile))} {
		if parsed, err := plan.ParsePlanFile(path); err == nil {
			s.tasks = parsed.Tasks
			return
		}
	}
}

// refreshPlanLocked sends the plan entries when they differ from the last ones sent.
func (s *Sink) refreshPlanLocked() {
	entries := make([]planEntry, 0, len(s.tasks)+len(s.stages))
	for i, t := range s.tasks {
		st := entryPending
		switch {
		case plan.DetermineTaskStatus(t.Checkboxes) == plan.TaskStatusDone:
			st = entryCompleted
		case s.cur.phase == status.PhaseTask && s.cur.task == i+1:
			st = entryInProgress
		}
		entries = append(entries, planEntry{Content: taskContent(t), Priority: "medium", Status: st})
	}
	for _, st := range s.stages {
		entries = append(entries, planEntry{Content: string(st.stage), Priority: "medium", Status: st.status})
	}
	if len(entries) == 0 {
		return
	}
	if slices.Equal(entries, s.lastPlan) {
		return
	}
	s.lastPlan = entries
	s.sendLocked(planUpdate{SessionUpdate: "plan", Entries: entries})
}

func taskContent(t plan.Task) string {
	if t.Title == "" {
		return fmt.Sprintf("Task %d", t.Number)
	}
	return fmt.Sprintf("Task %d: %s", t.Number, t.Title)
}

// armHeartbeatLocked schedules the next idle check d from now.
func (s *Sink) armHeartbeatLocked(d time.Duration) {
	s.beatSeq++
	seq := s.beatSeq
	s.beatTimer = s.clk.AfterFunc(d, func() { s.heartbeat(seq) })
}

// heartbeat reports the elapsed wait on the open tool call when nothing was sent for
// heartbeatIdle, so the client's turn watchdog sees progress during a long quiet stretch.
func (s *Sink) heartbeat(seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || seq != s.beatSeq {
		return
	}
	now := s.clk.Now()
	if idle := now.Sub(s.lastSent); idle < heartbeatIdle {
		s.armHeartbeatLocked(heartbeatIdle - idle)
		return
	}
	waited := now.Sub(s.lastActivity)
	if s.callID == "" {
		// the heartbeat's own call is not activity, or later beats would measure from it
		last := s.lastActivity
		s.openCallLocked("loopai", false)
		s.lastActivity = last
	}
	title := s.callTitle + " · waiting " + formatWait(waited)
	s.heartbeatShow = true
	s.sendRawLocked(toolCallUpdate{SessionUpdate: "tool_call_update", ToolCallID: s.callID, Title: title})
	s.armHeartbeatLocked(heartbeatIdle)
}

// formatWait renders a wait rounded down to whole minutes, such as "4m" or "1h8m".
func formatWait(d time.Duration) string {
	m := int(d / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}

// activityTitle names a tool call, such as "task 2/5" or "review · iteration 1". total is the
// plan's task count, or zero when unknown.
func activityTitle(a activity, total int) string {
	switch a.phase {
	case status.PhaseTask:
		switch {
		case a.task > 0 && total > 0:
			return fmt.Sprintf("task %d/%d", a.task, total)
		case a.task > 0:
			return fmt.Sprintf("task %d", a.task)
		default:
			return "task"
		}
	case status.PhaseReview:
		return iterationTitle("review", a.iteration)
	case status.PhaseExternalReview, status.PhaseCodex:
		return iterationTitle("external review", a.iteration)
	case status.PhaseExternalEval, status.PhaseClaudeEval:
		return "external review evaluation"
	case status.PhasePlan:
		return iterationTitle("plan", a.iteration)
	case status.PhaseFinalize:
		return "finalize"
	case status.PhaseReport:
		return "completion report"
	case status.PhaseLimitWait:
		return "waiting for provider limit"
	default:
		return string(a.phase)
	}
}

func iterationTitle(label string, iteration int) string {
	if iteration <= 0 {
		return label
	}
	return fmt.Sprintf("%s · iteration %d", label, iteration)
}

// Logger is the execution logger surface the sink observes; it matches processor.Logger.
type Logger interface {
	Print(format string, args ...any)
	PrintRaw(format string, args ...any)
	PrintSection(section status.Section)
	PrintAligned(text string)
	LogQuestion(question string, options []string)
	LogAnswer(answer string)
	LogDraftReview(action string, feedback string)
	Path() string
}

// WrapLogger decorates a logger so structured sections and executor output reach the session.
// Every call is forwarded to the inner logger first. A nil sink returns the logger unchanged.
func (s *Sink) WrapLogger(logger Logger) Logger {
	if s == nil {
		return logger
	}
	return &sinkLogger{Logger: logger, sink: s}
}

type sinkLogger struct {
	Logger
	sink *Sink
}

func (l *sinkLogger) PrintSection(section status.Section) {
	l.Logger.PrintSection(section)
	l.sink.OnSection(section)
}

func (l *sinkLogger) PrintAligned(text string) {
	l.Logger.PrintAligned(text)
	l.sink.Output(text)
}
