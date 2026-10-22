package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewID(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		plan string
		stem string
	}{
		{name: "dated plan", plan: "docs/plans/20261022-feature-x.md", stem: "feature-x"},
		{name: "undated plan", plan: "docs/plans/fix-issues.md", stem: "fix-issues"},
		{name: "date only", plan: "docs/plans/20261022.md", stem: "20261022"},
		{name: "mixed case and punctuation", plan: "Fix Bug #12 (urgent).md", stem: "fix-bug-12-urgent"},
		{name: "dots and underscores kept", plan: "v1.2_release.md", stem: "v1.2_release"},
		{name: "leading separators trimmed", plan: "_hidden.md", stem: "hidden"},
		{name: "non-ascii only", plan: "docs/plans/план.md", stem: "run"},
		{name: "no extension", plan: "plans/feature", stem: "feature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := NewID(tc.plan, now)
			assert.True(t, ValidID(id), "id %q must be valid", id)
			assert.Regexp(t, "^"+regexp.QuoteMeta(tc.stem)+`-20261022-153000-[0-9a-f]{4}$`, id)
		})
	}
}

func TestNewIDRendersUTC(t *testing.T) {
	now := time.Date(2026, 10, 22, 18, 30, 0, 0, time.FixedZone("x", 3*3600))
	assert.Regexp(t, `^feature-20261022-153000-[0-9a-f]{4}$`, NewID("feature.md", now))
}

func TestNewIDLongPlanNameFitsLimit(t *testing.T) {
	plan := "docs/plans/" + strings.Repeat("very-long-plan-name-", 10) + ".md"
	id := NewID(plan, time.Now())
	assert.True(t, ValidID(id), "id %q must be valid", id)
	assert.LessOrEqual(t, len(id), maxIDLen)
	assert.NotContains(t, id, "--")
}

func TestNewIDUniquePerCall(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	assert.NotEqual(t, NewID("plan.md", now), NewID("plan.md", now.Add(time.Second)),
		"ids of different seconds never collide")

	// the 4-hex suffix is random: same-instant calls collide only with probability 1/65536,
	// so a run of calls that all agree would mean the suffix is not random at all
	seen := map[string]bool{}
	for range 32 {
		id := NewID("plan.md", now)
		require.True(t, ValidID(id), "id %q must be valid", id)
		seen[id] = true
	}
	assert.Greater(t, len(seen), 1)
}

func TestValidID(t *testing.T) {
	tests := []struct {
		id    string
		valid bool
	}{
		{id: "feature-x-20261022-153000-a1b2", valid: true},
		{id: "a", valid: true},
		{id: "v1.2_release-20261022-153000-0000", valid: true},
		{id: strings.Repeat("a", 100), valid: true},
		{id: "", valid: false},
		{id: ".", valid: false},
		{id: "..", valid: false},
		{id: "../x", valid: false},
		{id: "a/b", valid: false},
		{id: "/abs", valid: false},
		{id: `a\b`, valid: false},
		{id: ".hidden", valid: false},
		{id: "-dash", valid: false},
		{id: "Upper", valid: false},
		{id: "has space", valid: false},
		{id: "colon:id", valid: false},
		{id: strings.Repeat("a", 101), valid: false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.valid, ValidID(tc.id), "ValidID(%q)", tc.id)
	}
}

func TestThreadKey(t *testing.T) {
	native := "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3A" + strings.Repeat("x", 250) + "%3Aagent%3A3"
	other := "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3A" + strings.Repeat("x", 250) + "%3Aagent%3A4"
	require.Greater(t, len(native), 300)

	key := ThreadKey(native)
	assert.Regexp(t, `^[0-9a-f]{32}$`, key)
	assert.Equal(t, key, ThreadKey(native), "the key is stable")
	assert.NotEqual(t, key, ThreadKey(other), "distinct thread ids get distinct keys")

	sum := sha256.Sum256([]byte(native))
	assert.Equal(t, hex.EncodeToString(sum[:])[:32], key)
	assert.Regexp(t, `^[0-9a-f]{32}$`, ThreadKey("8e8b69fc-3359-47c8-a5a3-6dd91c245a86"))
}
