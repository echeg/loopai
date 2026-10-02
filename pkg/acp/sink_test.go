package acp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSinkMessage(t *testing.T) {
	var out bytes.Buffer
	s := NewSink(NewConn(strings.NewReader(""), &out), "loopai-1")
	s.Message("")
	assert.Empty(t, out.String(), "empty text sends nothing")

	s.Message("report")
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"loopai-1",`+
		`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"report"}}}}`, out.String())
}

func TestSinkNilIsNoop(t *testing.T) {
	var s *Sink
	assert.NotPanics(t, func() {
		s.Update(map[string]any{"sessionUpdate": "plan"})
		s.Message("text")
	})
	assert.NotPanics(t, func() { (&Sink{}).Message("text") })
}

func TestSinkIgnoresWriteErrors(t *testing.T) {
	s := NewSink(NewConn(strings.NewReader(""), failingWriter{}), "loopai-1")
	assert.NotPanics(t, func() { s.Message("text") })
}
