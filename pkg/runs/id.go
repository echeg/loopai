package runs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxIDLen     = 100
	idTimeLayout = "20060102-150405"
	// idSuffixLen is the length of "-<YYYYMMDD-HHMMSS>-<4 hex>".
	idSuffixLen = 1 + len(idTimeLayout) + 1 + 4
	// threadKeyLen is the number of hex characters ThreadKey keeps.
	threadKeyLen = 32
)

var (
	validIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
	// planDatePrefixRe matches the date-like prefix plan file names carry ("20261022-"), the same
	// prefix plan.ExtractBranchName strips, so the id does not repeat the date it is stamped with.
	planDatePrefixRe = regexp.MustCompile(`^[\d-]+`)
)

// NewID returns a new run id for planPath: "<sanitized plan stem>-<YYYYMMDD-HHMMSS>-<4 hex>",
// with now rendered in UTC and the suffix drawn from crypto/rand. The result satisfies ValidID.
func NewID(planPath string, now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error since Go 1.24
	return idStem(planPath) + "-" + now.UTC().Format(idTimeLayout) + "-" + hex.EncodeToString(b[:])
}

// ValidID reports whether id is one safe path segment usable as a run directory name.
func ValidID(id string) bool {
	return validIDRe.MatchString(id)
}

// ThreadKey returns the first 32 hex characters of sha256(threadID). T3 thread ids can contain
// ':' and '%' and exceed 300 characters, so they never become file names raw.
func ThreadKey(threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	return hex.EncodeToString(sum[:])[:threadKeyLen]
}

// idStem turns the plan file name into the id prefix: the base name without its extension and
// leading date, lowercased, with every other character outside [a-z0-9._-] replaced by '-',
// repeated '-' collapsed, and the result trimmed to start with [a-z0-9] and fit the id length.
func idStem(planPath string) string {
	name := filepath.Base(planPath)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if stripped := strings.TrimLeft(planDatePrefixRe.ReplaceAllString(name, ""), "-"); stripped != "" {
		name = stripped
	}

	var sb strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			sb.WriteRune(r)
			lastDash = false
		case !lastDash:
			sb.WriteByte('-')
			lastDash = true
		}
	}
	stem := strings.TrimLeft(sb.String(), "-._")
	if len(stem) > maxIDLen-idSuffixLen {
		stem = stem[:maxIDLen-idSuffixLen]
	}
	stem = strings.TrimRight(stem, "-")
	if stem == "" {
		return "run"
	}
	return stem
}
