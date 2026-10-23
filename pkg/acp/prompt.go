package acp

import (
	"encoding/json"
	"slices"
	"strings"
)

// ContinuationText is the message T3 Code sends to continue an interrupted turn: after a server
// restart, from the Resume button after a Stop, and on usage-limit auto-resume. A restart note or,
// after a session/load fallback, a context handoff may precede it.
const ContinuationText = "Continue where you left off."

// Markers of T3 Code's prompt envelope.
const (
	// userRequestOpen and userRequestClose enclose the message when T3 Code prepends its
	// <t3_code_instructions> block: in a session's first prompt and whenever its instruction state
	// changes. A steered prompt after a cancel usually arrives without them.
	userRequestOpen  = "<user_request>\n"
	userRequestClose = "\n</user_request>"
	// userMessageMarker separates a context handoff or restart history from the user's text; the
	// history may contain the marker itself, so the last one counts.
	userMessageMarker = "\n\nUser message:\n"
	// runtimeInfoOpen starts the separate block T3 Code appends to every prompt that is not a slash
	// command. It is the first text block only when the message itself has no text.
	runtimeInfoOpen = "<runtime_info>"
)

// ParsePrompt returns the user's message from session/prompt content blocks: the first text block,
// the inner text of its <user_request> envelope when present, the text after the last
// "\n\nUser message:\n" marker, trimmed. A prompt without message text returns "".
func ParsePrompt(blocks []json.RawMessage) string {
	text, ok := firstText(blocks)
	if !ok || strings.HasPrefix(text, runtimeInfoOpen) {
		return ""
	}
	text = unwrapUserRequest(text)
	if i := strings.LastIndex(text, userMessageMarker); i >= 0 {
		text = text[i+len(userMessageMarker):]
	}
	return strings.TrimSpace(text)
}

// IsContinuation reports whether text is T3 Code's continuation: its last non-empty line, trimmed,
// is ContinuationText.
func IsContinuation(text string) bool {
	for _, line := range slices.Backward(strings.Split(text, "\n")) {
		if line = strings.TrimSpace(line); line != "" {
			return line == ContinuationText
		}
	}
	return false
}

// firstText returns the text of the first content block of type "text".
func firstText(blocks []json.RawMessage) (string, bool) {
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &block) == nil && block.Type == "text" {
			return block.Text, true
		}
	}
	return "", false
}

// unwrapUserRequest returns the text between the first "<user_request>\n" and a closing
// "\n</user_request>" that ends text, or text unchanged when it is not wrapped. T3 Code's
// instructions precede the opening tag and never contain it, while the message may.
func unwrapUserRequest(text string) string {
	_, inner, found := strings.Cut(strings.TrimRight(text, " \t\r\n"), userRequestOpen)
	if !found {
		return text
	}
	message, closed := strings.CutSuffix(inner, userRequestClose)
	if !closed {
		return text
	}
	return message
}
