package runs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStatStartTime(t *testing.T) {
	// fields 3..52 rendered as f3..f52, so field 22 reads "f22"
	rest := make([]string, 0, 50)
	for n := 3; n <= 52; n++ {
		rest = append(rest, fmt.Sprintf("f%d", n))
	}
	tail := strings.Join(rest, " ")

	tests := []struct {
		name string
		stat string
	}{
		{name: "plain command", stat: "4123 (loopai) " + tail + "\n"},
		{name: "command with spaces and parentheses", stat: "4123 (a) b (c)) " + tail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatStartTime(tc.stat)
			require.NoError(t, err)
			assert.Equal(t, "f22", got)
		})
	}
}

func TestParseStatStartTimeErrors(t *testing.T) {
	_, err := parseStatStartTime("4123 loopai S 1")
	require.ErrorContains(t, err, "no command name")

	_, err = parseStatStartTime("4123 (loopai) S 1 2 3")
	require.ErrorContains(t, err, "4 fields after the command name")
}
