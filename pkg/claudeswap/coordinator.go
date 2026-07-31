// Package claudeswap integrates loopai's reactive limit handling with the
// claude-swap CLI. It stores only slot numbers and cooldown timestamps; account
// emails and credentials never enter loopai state or logs.
package claudeswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/umputun/ralphex/pkg/limits"
)

const (
	stateSchemaVersion = 1
	commandTimeout     = 30 * time.Second
	lockPollInterval   = 100 * time.Millisecond
)

// Coordinator serializes account rotation across every loopai process using
// the same state directory.
type Coordinator struct {
	command     string
	stateDir    string
	run         commandRunner
	now         func() time.Time
	settleDelay time.Duration
}

type commandRunner func(ctx context.Context, command string, args ...string) ([]byte, error)

type state struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Generation    uint64               `json:"generation"`
	ActiveAccount int                  `json:"activeAccount,omitempty"`
	LastSwapAt    time.Time            `json:"lastSwapAt,omitzero"`
	LimitedUntil  map[string]time.Time `json:"limitedUntil,omitempty"`
}

type statusResponse struct {
	SchemaVersion int `json:"schemaVersion"`
	Active        *struct {
		Number int `json:"number"`
	} `json:"active"`
	TotalManagedAccounts int `json:"totalManagedAccounts"`
	Error                *struct {
		Type string `json:"type"`
	} `json:"error,omitempty"`
}

type switchResponse struct {
	SchemaVersion int  `json:"schemaVersion"`
	Switched      bool `json:"switched"`
	From          *struct {
		Number int `json:"number"`
	} `json:"from"`
	To *struct {
		Number int `json:"number"`
	} `json:"to"`
	Error *struct {
		Type string `json:"type"`
	} `json:"error,omitempty"`
}

// New constructs a coordinator for an already resolved claude-swap binary.
func New(command, stateDir string) *Coordinator {
	settle := 2 * time.Second
	if runtime.GOOS == "darwin" {
		// Claude Code can retain the previous macOS Keychain item briefly after
		// claude-swap updates it.
		settle = 35 * time.Second
	}
	return &Coordinator{
		command: command, stateDir: stateDir, run: runCommand,
		now: time.Now, settleDelay: settle,
	}
}

// Detect resolves claude-swap from PATH. A missing binary disables the optional
// integration without affecting normal limit retry behavior.
func Detect(stateDir string) (*Coordinator, bool) {
	path, err := exec.LookPath("claude-swap")
	if err != nil {
		return nil, false
	}
	return New(path, stateDir), true
}

// Snapshot records the active slot and shared generation before a Claude call.
// It is deliberately best-effort: failure here must not prevent Claude itself
// from running.
func (c *Coordinator) Snapshot(ctx context.Context) limits.Snapshot {
	st, _ := c.readState()
	snapshot := limits.Snapshot{Generation: st.Generation, Account: st.ActiveAccount}
	if live, err := c.status(ctx); err == nil {
		snapshot.Account = live.Account
		snapshot.Total = live.Total
	}
	return snapshot
}

// Recover performs a single-flight rotation. A process whose snapshot is stale
// observes the winning process's generation and retries without switching again.
func (c *Coordinator) Recover(ctx context.Context, snapshot limits.Snapshot, cooldown time.Duration) (limits.RecoveryResult, error) {
	if c == nil {
		return limits.RecoveryResult{}, errors.New("claude-swap coordinator is nil")
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return limits.RecoveryResult{}, fmt.Errorf("prepare state directory: %w", err)
	}

	lockPath := filepath.Join(c.stateDir, "claude-swap.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed private state path
	if err != nil {
		return limits.RecoveryResult{}, fmt.Errorf("open coordination lock: %w", err)
	}
	defer lockFile.Close()
	if err = acquireFileLock(ctx, lockFile, lockPollInterval); err != nil {
		return limits.RecoveryResult{}, fmt.Errorf("acquire coordination lock: %w", err)
	}
	defer func() { _ = releaseFileLock(lockFile) }()
	return c.recoverLocked(ctx, snapshot, cooldown)
}

func (c *Coordinator) recoverLocked(ctx context.Context, snapshot limits.Snapshot, cooldown time.Duration) (limits.RecoveryResult, error) {
	now := c.now()
	st, _ := c.readState()
	c.pruneCooldowns(&st, now)
	live, err := c.status(ctx)
	if err != nil {
		return limits.RecoveryResult{}, fmt.Errorf("read active account: %w", err)
	}

	// Generation handles ABA (1 -> 2 -> 1); account handles manual switches
	// performed by tools that do not update loopai's state.
	if st.Generation != snapshot.Generation || (snapshot.Account > 0 && live.Account != snapshot.Account) {
		retryAfter := c.remainingSettle(st.LastSwapAt, now)
		return limits.RecoveryResult{
			Recovered: true, Switched: false, From: snapshot.Account, To: live.Account,
			RetryAfter: retryAfter, Reason: "account already changed",
		}, nil
	}

	failedAccount := snapshot.Account
	if failedAccount <= 0 {
		failedAccount = live.Account
	}
	if st.LimitedUntil == nil {
		st.LimitedUntil = make(map[string]time.Time)
	}
	if failedAccount > 0 {
		st.LimitedUntil[strconv.Itoa(failedAccount)] = now.Add(cooldown)
	}

	total := snapshot.Total
	if live.Total > 0 {
		total = live.Total
	}
	if total > 0 && c.limitedCount(st, now) >= total {
		_ = c.writeState(st)
		return limits.RecoveryResult{From: failedAccount, To: live.Account, Reason: "all accounts cooling down"}, nil
	}

	return c.rotate(ctx, st, live.Account, failedAccount, total, now)
}

// rotate keeps moving past slots that loopai itself recently observed returning
// a real LimitPatternError. This supplements claude-swap when usage data is stale.
func (c *Coordinator) rotate(ctx context.Context, st state, activeAccount, failedAccount, total int,
	now time.Time) (limits.RecoveryResult, error) {
	maxSwitches := total
	if maxSwitches <= 0 {
		maxSwitches = 1
	}
	current := activeAccount
	for range maxSwitches {
		switched, err := c.switchNext(ctx)
		if err != nil {
			_ = c.writeState(st)
			return limits.RecoveryResult{From: failedAccount, To: current, Reason: "switch unavailable"}, err
		}
		if !switched.Switched || switched.To <= 0 {
			_ = c.writeState(st)
			return limits.RecoveryResult{From: failedAccount, To: switched.To, Reason: "no eligible account"}, nil
		}
		current = switched.To
		if st.LimitedUntil[strconv.Itoa(current)].After(now) {
			continue
		}
		st.Generation++
		st.ActiveAccount = current
		st.LastSwapAt = now
		if err = c.writeState(st); err != nil {
			return limits.RecoveryResult{}, fmt.Errorf("persist switch state: %w", err)
		}
		return limits.RecoveryResult{
			Recovered: true, Switched: true, From: activeAccount, To: current,
			RetryAfter: c.settleDelay, Reason: "switched",
		}, nil
	}
	_ = c.writeState(st)
	return limits.RecoveryResult{From: failedAccount, To: current, Reason: "all accounts cooling down"}, nil
}

type liveStatus struct {
	Account int
	Total   int
}

func (c *Coordinator) status(ctx context.Context) (liveStatus, error) {
	out, err := c.runWithTimeout(ctx, "status", "--json")
	if err != nil {
		return liveStatus{}, err
	}
	var response statusResponse
	if err = json.Unmarshal(out, &response); err != nil {
		return liveStatus{}, errors.New("invalid status JSON")
	}
	if response.SchemaVersion != 1 || response.Error != nil || response.Active == nil || response.Active.Number <= 0 {
		return liveStatus{}, errors.New("unsupported or unsuccessful status response")
	}
	return liveStatus{Account: response.Active.Number, Total: response.TotalManagedAccounts}, nil
}

type switchResult struct {
	Switched bool
	From     int
	To       int
}

func (c *Coordinator) switchNext(ctx context.Context) (switchResult, error) {
	out, err := c.runWithTimeout(ctx, "switch", "--strategy", "next-available", "--model", "all", "--json")
	if err != nil {
		return switchResult{}, err
	}
	var response switchResponse
	if err = json.Unmarshal(out, &response); err != nil {
		return switchResult{}, errors.New("invalid switch JSON")
	}
	if response.SchemaVersion != 1 || response.Error != nil {
		return switchResult{}, errors.New("unsupported or unsuccessful switch response")
	}
	result := switchResult{Switched: response.Switched}
	if response.From != nil {
		result.From = response.From.Number
	}
	if response.To != nil {
		result.To = response.To.Number
	}
	return result, nil
}

func (c *Coordinator) runWithTimeout(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	out, err := c.run(ctx, c.command, args...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude-swap command context: %w", ctx.Err())
		}
		// Do not include command output: it may contain account emails or auth
		// diagnostics. The caller logs only this sanitized failure class.
		return nil, errors.New("claude-swap command failed")
	}
	return out, nil
}

func runCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run claude-swap: %w", err)
	}
	return stdout.Bytes(), nil
}

func (c *Coordinator) statePath() string { return filepath.Join(c.stateDir, "claude-swap-state.json") }

func (c *Coordinator) readState() (state, error) {
	data, err := os.ReadFile(c.statePath())
	if err != nil {
		return state{SchemaVersion: stateSchemaVersion}, fmt.Errorf("read state: %w", err)
	}
	var st state
	if err = json.Unmarshal(data, &st); err != nil || st.SchemaVersion != stateSchemaVersion {
		return state{SchemaVersion: stateSchemaVersion}, errors.New("invalid state")
	}
	return st, nil
}

func (c *Coordinator) writeState(st state) error {
	st.SchemaVersion = stateSchemaVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(c.stateDir, ".claude-swap-state-*")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err = os.Rename(tmpPath, c.statePath()); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func (c *Coordinator) pruneCooldowns(st *state, now time.Time) {
	for account, until := range st.LimitedUntil {
		if !until.After(now) {
			delete(st.LimitedUntil, account)
		}
	}
}

func (c *Coordinator) limitedCount(st state, now time.Time) int {
	count := 0
	for _, until := range st.LimitedUntil {
		if until.After(now) {
			count++
		}
	}
	return count
}

func (c *Coordinator) remainingSettle(switchedAt, now time.Time) time.Duration {
	if switchedAt.IsZero() {
		return c.settleDelay
	}
	remaining := switchedAt.Add(c.settleDelay).Sub(now)
	if remaining < 0 {
		return 0
	}
	return remaining
}
