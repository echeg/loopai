package acp

// Sink sends session/update notifications for one session's running prompt. A nil Sink drops
// every update, and send failures are ignored: reporting is best-effort and never fails a run.
type Sink struct {
	conn      *Conn
	sessionID string
}

// NewSink returns a sink publishing updates for the given session on conn.
func NewSink(conn *Conn, sessionID string) *Sink {
	return &Sink{conn: conn, sessionID: sessionID}
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

type sessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// Update sends one session/update notification carrying the given update object.
func (s *Sink) Update(update any) {
	if s == nil || s.conn == nil {
		return
	}
	_ = s.conn.Notify("session/update", sessionUpdateParams{SessionID: s.sessionID, Update: update})
}

// Message sends text as an assistant message chunk. Empty text sends nothing.
func (s *Sink) Message(text string) {
	if text == "" {
		return
	}
	s.Update(chunkUpdate{SessionUpdate: "agent_message_chunk", Content: textContent{Type: "text", Text: text}})
}
