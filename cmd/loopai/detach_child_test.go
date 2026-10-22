package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadDetachSecret(t *testing.T) {
	const token = "t3-secret-token-value"
	tests := []struct {
		name    string
		input   string
		want    detachSecret
		wantErr string
	}{
		{name: "line with a token", input: `{"t3_token":"` + token + `"}` + "\n", want: detachSecret{T3Token: token}},
		{name: "line without a token", input: `{"t3_token":""}` + "\n"},
		{name: "line ended by EOF", input: `{"t3_token":"x"}`, want: detachSecret{T3Token: "x"}},
		{name: "only the first line is read", input: `{"t3_token":"a"}` + "\n" + `{"t3_token":"b"}` + "\n",
			want: detachSecret{T3Token: "a"}},
		{name: "nothing before EOF", input: "", wantErr: "the launcher sent no message"},
		{name: "blank line", input: "\n", wantErr: "the launcher sent no message"},
		{name: "malformed line", input: `{"t3_token":"` + token + "\n", wantErr: "malformed message"},
		{name: "not an object", input: `"` + token + `"` + "\n", wantErr: "malformed message"},
		{name: "line over the cap", input: `{"t3_token":"` + strings.Repeat("x", maxDetachSecretLine) + `"}` + "\n",
			wantErr: "malformed message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readDetachSecret(strings.NewReader(tc.input))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), token, "the secret never reaches an error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("read failure", func(t *testing.T) {
		_, err := readDetachSecret(&failingReader{err: errors.New("broken pipe")})
		require.ErrorContains(t, err, "read handshake secret: broken pipe")
	})
}

// failingReader fails every read with err.
type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// ackRecorder is an ack pipe that records what was written and whether it was closed.
type ackRecorder struct {
	buf      bytes.Buffer
	writeErr error
	closed   int
}

func (r *ackRecorder) Write(p []byte) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	r.buf.Write(p) // a bytes.Buffer write always succeeds
	return len(p), nil
}

func (r *ackRecorder) Close() error {
	r.closed++
	return nil
}

func TestDetachAckSend(t *testing.T) {
	t.Run("ok carries the run id and attempt", func(t *testing.T) {
		rec := &ackRecorder{}
		ack := &detachAck{w: rec}
		require.NoError(t, ack.send(detachAckMessage{OK: true, RunID: "plan-20261022-120000-abcd", Attempt: 2}))
		assert.JSONEq(t, `{"ok":true,"run_id":"plan-20261022-120000-abcd","attempt":2}`, rec.buf.String())
		assert.Equal(t, 1, strings.Count(rec.buf.String(), "\n"), "one line")
		assert.True(t, strings.HasSuffix(rec.buf.String(), "\n"), "newline-terminated")
		assert.Equal(t, 1, rec.closed, "the pipe is closed right after the line")
	})

	t.Run("an error carries only the message", func(t *testing.T) {
		rec := &ackRecorder{}
		ack := &detachAck{w: rec}
		require.NoError(t, ack.send(detachAckMessage{Error: "run x is already running"}))
		assert.JSONEq(t, `{"ok":false,"error":"run x is already running"}`, rec.buf.String())
		assert.True(t, strings.HasSuffix(rec.buf.String(), "\n"), "newline-terminated")
	})

	t.Run("only the first message is sent", func(t *testing.T) {
		rec := &ackRecorder{}
		ack := &detachAck{w: rec}
		require.NoError(t, ack.send(detachAckMessage{OK: true, RunID: "a", Attempt: 1}))
		require.NoError(t, ack.send(detachAckMessage{Error: "later"}))
		assert.Equal(t, 1, strings.Count(rec.buf.String(), "\n"))
		assert.NotContains(t, rec.buf.String(), "later")
		assert.Equal(t, 1, rec.closed)
	})

	t.Run("a failed write is returned and the pipe still closed", func(t *testing.T) {
		rec := &ackRecorder{writeErr: errors.New("broken pipe")}
		ack := &detachAck{w: rec}
		require.ErrorContains(t, ack.send(detachAckMessage{OK: true, RunID: "a", Attempt: 1}), "write handshake ack: broken pipe")
		assert.Equal(t, 1, rec.closed)
		require.NoError(t, ack.send(detachAckMessage{Error: "again"}), "nothing is retried")
		assert.Equal(t, 1, rec.closed)
	})
}
