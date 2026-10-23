package acp

// PriorityMedium is the plan entry priority the provider sends. T3 Code requires a priority on
// every entry.
const PriorityMedium = "medium"

// Plan entry statuses.
const (
	PlanPending    = "pending"
	PlanInProgress = "in_progress"
	PlanCompleted  = "completed"
)

// Tool call statuses.
const (
	ToolInProgress = "in_progress"
	ToolCompleted  = "completed"
	ToolFailed     = "failed"
)

// toolKindOther is the only tool call kind the provider sends; phase cards are never "execute".
const toolKindOther = "other"

// messageIDPrefix starts every agent message id the provider sends.
const messageIDPrefix = "loopai-final-"

// PlanEntry is one entry of a plan update. An empty Priority is sent as PriorityMedium and an empty
// Status as PlanPending.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// Updates sends the session/update notifications of one prompt's session. Its agent message chunks
// share one messageId, so T3 Code shows them as one message. T3 Code drops updates that arrive after
// the prompt's answer, and the provider sends none after it stopped.
type Updates struct {
	p         *Provider
	sessionID string
	messageID string
}

type sessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

type planUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Entries       []PlanEntry `json:"entries"`
}

type toolCallStart struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallID    string `json:"toolCallId"`
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
}

type toolCallChange struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallID    string `json:"toolCallId"`
	Status        string `json:"status,omitempty"`
	Title         string `json:"title,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messageChunk struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Content       textContent `json:"content"`
	MessageID     string      `json:"messageId"`
}

// MessageID returns the messageId every AgentMessageChunk carries.
func (u *Updates) MessageID() string { return u.messageID }

// Plan replaces the session's plan with entries.
func (u *Updates) Plan(entries []PlanEntry) error {
	sent := make([]PlanEntry, len(entries))
	for i, e := range entries {
		if e.Priority == "" {
			e.Priority = PriorityMedium
		}
		if e.Status == "" {
			e.Status = PlanPending
		}
		sent[i] = e
	}
	return u.send(planUpdate{SessionUpdate: "plan", Entries: sent})
}

// ToolCall opens an in-progress tool call of kind "other". The id must be unique within the session.
func (u *Updates) ToolCall(toolCallID, title string) error {
	return u.send(toolCallStart{
		SessionUpdate: "tool_call", ToolCallID: toolCallID, Title: title, Kind: toolKindOther, Status: ToolInProgress,
	})
}

// ToolCallUpdate changes a tool call's status, title, or both; an empty value is left unchanged.
func (u *Updates) ToolCallUpdate(toolCallID, status, title string) error {
	return u.send(toolCallChange{SessionUpdate: "tool_call_update", ToolCallID: toolCallID, Status: status, Title: title})
}

// AgentMessageChunk appends text to the agent message identified by MessageID.
func (u *Updates) AgentMessageChunk(text string) error {
	return u.send(messageChunk{
		SessionUpdate: "agent_message_chunk", Content: textContent{Type: "text", Text: text}, MessageID: u.messageID,
	})
}

// send wraps update in a session/update notification for the session.
func (u *Updates) send(update any) error {
	return u.p.notify(methodSessionUpdate, sessionUpdateParams{SessionID: u.sessionID, Update: update})
}

// newMessageID returns a fresh agent message id.
func newMessageID() string {
	return messageIDPrefix + randomHex(8)
}
