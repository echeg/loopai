package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveLines runs a connection over the given input lines and returns every line it wrote.
func serveLines(t *testing.T, input string, register func(c *Conn)) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	c := NewConn(strings.NewReader(input), &out)
	if register != nil {
		register(c)
	}
	require.NoError(t, c.Serve())
	return decodeLines(t, out.String())
}

func decodeLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var msgs []map[string]any
	for line := range strings.SplitSeq(strings.TrimSuffix(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "line %q", line)
		msgs = append(msgs, m)
	}
	return msgs
}

func TestConnRequestRoundTrip(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1},"traceId":"t","spanId":"s"}` + "\n"
	var gotParams json.RawMessage
	msgs := serveLines(t, input, func(c *Conn) {
		c.HandleRequest("initialize", func(id, params json.RawMessage) {
			gotParams = params
			assert.NoError(t, c.Reply(id, map[string]any{"protocolVersion": 1}))
		})
	})
	assert.JSONEq(t, `{"protocolVersion":1}`, string(gotParams))
	require.Len(t, msgs, 1)
	assert.Equal(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      float64(1),
		"result":  map[string]any{"protocolVersion": float64(1)},
	}, msgs[0])
}

func TestConnPreservesIDs(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "above uint32", id: `4294967297`},
		{name: "above float precision", id: `9007199254740993`},
		{name: "max uint64", id: `18446744073709551615`},
		{name: "string", id: `"req-42"`},
		{name: "escaped string", id: `"aéb"`},
		{name: "null", id: `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`+"\n", tt.id)
			var out bytes.Buffer
			c := NewConn(strings.NewReader(input), &out)
			c.HandleRequest("ping", func(id, _ json.RawMessage) {
				assert.Equal(t, tt.id, string(id))
				assert.NoError(t, c.Reply(id, nil))
			})
			require.NoError(t, c.Serve())
			assert.Equal(t, `{"jsonrpc":"2.0","id":`+tt.id+`,"result":null}`+"\n", out.String())
		})
	}
}

func TestConnNotificationDispatch(t *testing.T) {
	input := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"unknown/notification","params":{}}` + "\n"
	var got []string
	msgs := serveLines(t, input, func(c *Conn) {
		c.HandleNotification("session/cancel", func(params json.RawMessage) {
			got = append(got, string(params))
		})
	})
	assert.Equal(t, []string{`{"sessionId":"s1"}`}, got)
	assert.Empty(t, msgs, "notifications are never answered")
}

func TestConnUnknownMethod(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":"x7","method":"session/fork","params":{}}` + "\n"
	msgs := serveLines(t, input, nil)
	require.Len(t, msgs, 1)
	assert.Equal(t, "x7", msgs[0]["id"])
	errObj, ok := msgs[0]["error"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, float64(CodeMethodNotFound), errObj["code"], 0)
	assert.Contains(t, errObj["message"], "session/fork")
	assert.NotContains(t, msgs[0], "result")
}

func TestConnSkipsMalformedLines(t *testing.T) {
	input := strings.Join([]string{
		`not json`,
		``,
		`   `,
		`{"jsonrpc":"2.0","id":3`,
		`[1,2,3]`,
		`{"jsonrpc":"2.0","id":9,"result":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\r",
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`, // unterminated final line is still processed
	}, "\n")
	var ids []string
	msgs := serveLines(t, input, func(c *Conn) {
		c.HandleRequest("ping", func(id, _ json.RawMessage) {
			ids = append(ids, string(id))
			assert.NoError(t, c.Reply(id, "pong"))
		})
	})
	assert.Equal(t, []string{"2", "4"}, ids)
	assert.Len(t, msgs, 2)
}

func TestConnSkipsOversizedLine(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("a", maxLineSize) + `"}`
	valid := `{"jsonrpc":"2.0","id":2,"method":"ping"}`

	t.Run("terminated", func(t *testing.T) {
		var ids []string
		serveLines(t, huge+"\n"+valid+"\n", func(c *Conn) {
			c.HandleRequest("ping", func(id, _ json.RawMessage) { ids = append(ids, string(id)) })
		})
		assert.Equal(t, []string{"2"}, ids)
	})

	t.Run("unterminated at EOF", func(t *testing.T) {
		var ids []string
		serveLines(t, valid+"\n"+huge, func(c *Conn) {
			c.HandleRequest("ping", func(id, _ json.RawMessage) { ids = append(ids, string(id)) })
		})
		assert.Equal(t, []string{"2"}, ids)
	})

	t.Run("exactly at the limit", func(t *testing.T) {
		prefix := `{"jsonrpc":"2.0","id":5,"method":"ping","params":"`
		suffix := `"}`
		line := prefix + strings.Repeat("b", maxLineSize-len(prefix)-len(suffix)) + suffix
		require.Len(t, line, maxLineSize)
		var ids []string
		serveLines(t, line+"\n", func(c *Conn) {
			c.HandleRequest("ping", func(id, _ json.RawMessage) { ids = append(ids, string(id)) })
		})
		assert.Equal(t, []string{"5"}, ids)
	})
}

// lineRecorder records every Write call so tests can check that each one is a complete line.
type lineRecorder struct {
	mu     sync.Mutex
	writes [][]byte
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

func TestConnConcurrentWritesAreLineAtomic(t *testing.T) {
	rec := &lineRecorder{}
	c := NewConn(strings.NewReader(""), rec)
	payload := strings.Repeat("x", 64<<10)

	const senders, perSender = 16, 25
	var wg sync.WaitGroup
	for i := range senders {
		wg.Go(func() {
			for j := range perSender {
				switch j % 3 {
				case 0:
					assert.NoError(t, c.Notify("session/update", map[string]any{"i": i, "j": j, "text": payload}))
				case 1:
					assert.NoError(t, c.Reply(json.RawMessage(strconv.Itoa(i*1000+j)), map[string]any{"text": payload}))
				default:
					assert.NoError(t, c.ReplyError(json.RawMessage(strconv.Itoa(i*1000+j)), CodeInternalError, payload))
				}
			}
		})
	}
	wg.Wait()

	require.Len(t, rec.writes, senders*perSender)
	for _, w := range rec.writes {
		require.Equal(t, 1, bytes.Count(w, []byte("\n")), "each write carries exactly one line")
		require.Equal(t, byte('\n'), w[len(w)-1])
		assert.True(t, json.Valid(w[:len(w)-1]))
	}
}

func TestConnDeferredReplyFromAnotherGoroutine(t *testing.T) {
	clientToAgent, agentIn := io.Pipe()
	agentOut, agentToClient := io.Pipe()
	c := NewConn(clientToAgent, agentToClient)
	release := make(chan struct{})
	c.HandleRequest("session/prompt", func(id, _ json.RawMessage) {
		go func() {
			<-release
			assert.NoError(t, c.Reply(id, map[string]string{"stopReason": "end_turn"}))
		}()
	})
	c.HandleRequest("ping", func(id, _ json.RawMessage) {
		assert.NoError(t, c.Reply(id, "pong"))
	})

	served := make(chan error, 1)
	go func() { served <- c.Serve() }()
	reader := bufio.NewReader(agentOut)

	_, err := io.WriteString(agentIn, `{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{}}`+"\n")
	require.NoError(t, err)
	// the read loop is not blocked by the pending prompt
	_, err = io.WriteString(agentIn, `{"jsonrpc":"2.0","id":11,"method":"ping"}`+"\n")
	require.NoError(t, err)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":11,"result":"pong"}`, line)

	close(release)
	line, err = reader.ReadString('\n')
	require.NoError(t, err)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":10,"result":{"stopReason":"end_turn"}}`, line)

	require.NoError(t, agentIn.Close())
	select {
	case err := <-served:
		require.NoError(t, err, "EOF ends the loop cleanly")
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

func TestConnServeReadError(t *testing.T) {
	boom := errors.New("boom")
	c := NewConn(iotest.ErrReader(boom), io.Discard)
	err := c.Serve()
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "acp: read")
}

func TestConnServeEmptyInput(t *testing.T) {
	assert.Empty(t, serveLines(t, "", nil))
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

func TestConnSendErrors(t *testing.T) {
	c := NewConn(strings.NewReader(""), failingWriter{})
	err := c.Notify("session/update", map[string]any{})
	require.ErrorContains(t, err, "acp: write: closed pipe")

	c = NewConn(strings.NewReader(""), io.Discard)
	err = c.Notify("session/update", map[string]any{"bad": make(chan int)})
	require.ErrorContains(t, err, "acp: marshal")
}

func TestConnNotifyShape(t *testing.T) {
	var out bytes.Buffer
	c := NewConn(strings.NewReader(""), &out)
	require.NoError(t, c.Notify("session/update", map[string]string{"sessionId": "s1"}))
	require.NoError(t, c.Notify("ping", nil))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1"}}`, lines[0])
	assert.JSONEq(t, `{"jsonrpc":"2.0","method":"ping"}`, lines[1], "nil params are omitted")
}
