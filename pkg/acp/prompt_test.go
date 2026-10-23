package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// t3Instructions stands in for T3 Code's ACP instruction text, which precedes <user_request>.
const t3Instructions = "You are running inside T3 Code.\n\nUse the T3 Code MCP tools for threads and previews.\n\n" +
	"Never ask the user to restate their request."

// restartNote is T3 Code's restart note listing two background items.
const restartNote = "Note: the T3 server restarted, and this background work was cancelled before it finished. " + //nolint:misspell // T3 Code's text
	"It will not report back on its own, so resume or restart what you still need and wait for its result:\n" +
	"- subagent: Review the diff\n- command: make test"

// runtimeInfo is the separate block T3 Code appends to every prompt that is not a slash command.
const runtimeInfo = "<runtime_info>In case you're asked: you are running in T3 Code through the acpRegistry harness " +
	"(model default). No need to mention this otherwise.</runtime_info>\n\nLink pull requests you open."

// wrapT3 wraps a message the way T3 Code does in a session's first prompt.
func wrapT3(message string) string {
	return "<t3_code_instructions>\n" + t3Instructions + "\n</t3_code_instructions>\n\n<user_request>\n" + message + "\n</user_request>"
}

// handoff renders a context handoff before the user's text, as T3 Code does after a session/load
// fallback.
func handoff(summary, userText string) string {
	return "Context handoff (full_thread_summary):\n" + summary + "\n\nUser message:\n" + userText
}

func textBlock(t *testing.T, text string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]string{"type": "text", "text": text})
	require.NoError(t, err)
	return data
}

// largeHandoffSummary returns a summary of about 16k tokens (64 KiB) whose history quotes earlier
// user messages with the same marker T3 Code puts before the current one.
func largeHandoffSummary() string {
	var b strings.Builder
	for i := 0; b.Len() < 64<<10; i++ {
		fmt.Fprintf(&b, "Turn %d.\n\nUser message:\nrun docs/plans/old-%d.md please\n\n", i, i)
		b.WriteString("Assistant: the run finished; Task 3 needed two attempts and review found nothing. ")
		b.WriteString(strings.Repeat("Details of the previous turn. ", 20))
		b.WriteString("\n\n")
	}
	return b.String()
}

func TestParsePrompt(t *testing.T) {
	plan := "docs/plans/20261022-feature.md --task-model claude:opus"
	twiceSummary := "The user asked to run a plan.\n\nUser message:\nrun docs/plans/a.md\n\nAssistant: started.\n\n" +
		"User message:\nwhat is the status?\n\nAssistant: Task 2 of 5."
	image := json.RawMessage(`{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}`)

	tests := []struct {
		name   string
		blocks func(t *testing.T) []json.RawMessage
		want   string
	}{
		{name: "wrapped first prompt", want: plan, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3(plan)), textBlock(t, runtimeInfo)}
		}},
		{name: "handoff history containing the marker twice", want: ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3(handoff(twiceSummary, ContinuationText))), textBlock(t, runtimeInfo)}
		}},
		{name: "16k-token handoff before the continuation", want: ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3(handoff(largeHandoffSummary(), ContinuationText))), textBlock(t, runtimeInfo)}
		}},
		{name: "restart note continuation wrapped", want: restartNote + "\n\n" + ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3(restartNote+"\n\n"+ContinuationText)), textBlock(t, runtimeInfo)}
		}},
		{name: "restart note continuation unwrapped", want: restartNote + "\n\n" + ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, restartNote+"\n\n"+ContinuationText), textBlock(t, runtimeInfo)}
		}},
		{name: "restart note after a handoff", want: restartNote + "\n\n" + ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3(handoff(twiceSummary, restartNote+"\n\n"+ContinuationText)))}
		}},
		{name: "plain continuation unwrapped", want: ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, ContinuationText), textBlock(t, runtimeInfo)}
		}},
		{name: "plain continuation wrapped", want: ContinuationText, blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3(ContinuationText)), textBlock(t, runtimeInfo)}
		}},
		{name: "raw slash command", want: "/compact keep the plan", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, "/compact keep the plan")}
		}},
		{name: "steered prompt wrapped", want: "docs/plans/other.md", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3("docs/plans/other.md")), textBlock(t, runtimeInfo)}
		}},
		{name: "steered prompt unwrapped", want: "stop after this task", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, "stop after this task\n"), textBlock(t, runtimeInfo)}
		}},
		{name: "message quoting the envelope tag keeps it", want: "explain <user_request>\nthis\n</user_request> tag",
			blocks: func(t *testing.T) []json.RawMessage {
				return []json.RawMessage{textBlock(t, wrapT3("explain <user_request>\nthis\n</user_request> tag"))}
			}},
		{name: "unwrapped text ending in a closing tag only", want: "x\n</user_request>", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, "x\n</user_request>")}
		}},
		{name: "unwrapped text with an opening tag only", want: "see <user_request>\nabove", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, "see <user_request>\nabove")}
		}},
		{name: "wrapped empty message", want: "", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{textBlock(t, wrapT3("")), image, textBlock(t, runtimeInfo)}
		}},
		{name: "image before the text", want: "look at this", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{image, textBlock(t, wrapT3("look at this")), textBlock(t, runtimeInfo)}
		}},
		{name: "image only leaves runtime info first", want: "", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{image, textBlock(t, runtimeInfo)}
		}},
		{name: "undecodable block skipped", want: "resume", blocks: func(t *testing.T) []json.RawMessage {
			return []json.RawMessage{json.RawMessage(`{"type":`), json.RawMessage(`"text"`), textBlock(t, "  resume \n")}
		}},
		{name: "no blocks", want: "", blocks: func(t *testing.T) []json.RawMessage { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ParsePrompt(tt.blocks(t)))
		})
	}
}

func TestParsePromptLargeHandoffIsContinuation(t *testing.T) {
	summary := largeHandoffSummary()
	require.Greater(t, len(summary), 64<<10-1, "about 16k tokens")
	text := ParsePrompt([]json.RawMessage{textBlock(t, wrapT3(handoff(summary, ContinuationText))), textBlock(t, runtimeInfo)})
	assert.True(t, IsContinuation(text))
	assert.NotContains(t, text, "old-", "nothing of the handoff history survives")
}

func TestIsContinuation(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "plain", text: ContinuationText, want: true},
		{name: "after the restart note", text: restartNote + "\n\n" + ContinuationText, want: true},
		{name: "trailing blank lines", text: ContinuationText + "\n\n  \n", want: true},
		{name: "surrounding spaces", text: "  " + ContinuationText + "\t", want: true},
		{name: "crlf", text: "note\r\n\r\n" + ContinuationText + "\r\n", want: true},
		{name: "lowercase", text: "continue where you left off.", want: false},
		{name: "extra words on the line", text: ContinuationText + " Then stop.", want: false},
		{name: "not the last line", text: ContinuationText + "\nthanks", want: false},
		{name: "restart note alone", text: restartNote, want: false},
		{name: "resume", text: "resume", want: false},
		{name: "empty", text: "", want: false},
		{name: "blank", text: " \n\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsContinuation(tt.text))
		})
	}
}
