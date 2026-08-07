package validation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchesCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		entries []string
		want    bool
	}{
		{name: "exact", command: "make test", entries: []string{"make test"}, want: true},
		{name: "prefix at boundary", command: "go test ./... -count=1", entries: []string{"go test ./..."}, want: true},
		{name: "prefix without boundary", command: "make test-wrappers", entries: []string{"make test"}},
		{name: "normalizes", command: "  go   test ./...  ", entries: []string{"`go test ./...`"}, want: true},
		{name: "empty command", entries: []string{"make test"}},
		{name: "empty entries", command: "make test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchesCommand(tt.command, tt.entries))
		})
	}
}

func TestMatchCommand_ReturnsCanonicalConfiguredEntry(t *testing.T) {
	matched, ok := MatchCommand("make\ttest\n--token secret", []string{"`make   test`"})

	assert.True(t, ok)
	assert.Equal(t, "make test", matched)
}
