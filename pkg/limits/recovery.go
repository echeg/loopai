// Package limits defines provider-limit recovery contracts shared by the
// processor and optional recovery integrations.
package limits

import (
	"context"
	"time"
)

// Snapshot identifies the provider state observed immediately before an
// executor invocation. Generation prevents an ABA account rotation from being
// mistaken for an unchanged account.
type Snapshot struct {
	Generation uint64
	Account    int
	Total      int
}

// RecoveryResult describes whether another executor attempt can be made soon.
type RecoveryResult struct {
	Recovered  bool
	Switched   bool
	From       int
	To         int
	RetryAfter time.Duration
	Reason     string
}

// Recovery coordinates an optional provider-specific response to a detected
// limit. Implementations must be safe for multiple loopai processes.
type Recovery interface {
	Snapshot(ctx context.Context) Snapshot
	Recover(ctx context.Context, snapshot Snapshot, cooldown time.Duration) (RecoveryResult, error)
}
