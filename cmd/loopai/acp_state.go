package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// acpStateDir holds one state file per ACP session, relative to the session cwd. It is a loopai
// runtime path, so EnsureLocalGitignore ignores it and the merge gates never see it as untracked.
const acpStateDir = ".loopai/acp"

// acpSessionIDPattern accepts session ids that are one safe path segment: loopai's own
// "loopai-<hex>" ids and the UUID-like ids a client may pass to session/load.
var acpSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// acpSessionState is what an ACP session keeps between prompts and across agent restarts: the chat
// provider's session, resumed by the next chat turn, and the last plan run the chat is about.
type acpSessionState struct {
	Provider          string      `json:"provider,omitempty"`            // provider that owns ProviderSessionID
	ProviderSessionID string      `json:"provider_session_id,omitempty"` // provider session the next chat turn resumes
	LastRun           *acpLastRun `json:"last_run,omitempty"`
	// Actions are the merge and pull request outcomes loopai reported since the chat provider last
	// answered. They happen outside the provider session, so the next chat turn is told about them.
	Actions []string `json:"actions,omitempty"`
}

// acpLastRun records the latest plan run of the session.
type acpLastRun struct {
	Plan         string `json:"plan"`
	Branch       string `json:"branch,omitempty"`
	Base         string `json:"base,omitempty"`
	Tip          string `json:"tip,omitempty"` // commit the run completed on
	Report       string `json:"report,omitempty"`
	MergeSummary string `json:"merge_summary,omitempty"` // auto-merge outcome; empty when none was attempted
	MergeSkipped string `json:"merge_skipped,omitempty"` // why the auto-merge was skipped
	TaskModel    string `json:"task_model,omitempty"`    // --task-model the run was launched with
	ReviewModel  string `json:"review_model,omitempty"`  // --review-model the run was launched with
}

// acpStatePath returns the state file of sessionID under dir, rejecting an id that is not a single
// safe path segment: the id comes from the client, and session/load accepts any saved id.
func acpStatePath(dir, sessionID string) (string, error) {
	if !acpSessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("unsafe ACP session id %q", sessionID)
	}
	return filepath.Join(dir, filepath.FromSlash(acpStateDir), sessionID+".json"), nil
}

// loadACPState reads the state of sessionID under dir. A missing file is an empty state, and so is
// a corrupt one, reported to warnings: losing it costs only the chat's provider session.
func loadACPState(dir, sessionID string, warnings io.Writer) (acpSessionState, error) {
	path, err := acpStatePath(dir, sessionID)
	if err != nil {
		return acpSessionState{}, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is built from a validated single segment
	if errors.Is(err, os.ErrNotExist) {
		return acpSessionState{}, nil
	}
	if err != nil {
		return acpSessionState{}, fmt.Errorf("read ACP session state: %w", err)
	}
	var state acpSessionState
	if err := json.Unmarshal(data, &state); err != nil {
		fmt.Fprintf(warnings, "warning: ignoring corrupt ACP session state %s: %v\n", path, err)
		return acpSessionState{}, nil
	}
	return state, nil
}

// saveACPState writes the state of sessionID under dir atomically with mode 0600.
func saveACPState(dir, sessionID string, state acpSessionState) error {
	path, err := acpStatePath(dir, sessionID)
	if err != nil {
		return err
	}
	return saveJSONState(path, ".acp-state-*.tmp", "ACP session state", state)
}

// newACPLastRun records a plan run launched with o, and its auto-merge outcome, nil when none was
// attempted.
func newACPLastRun(o opts, branch, base string, outcome planExecutionOutcome, merge *acpMergeResult) *acpLastRun {
	run := &acpLastRun{Plan: o.PlanFile, Branch: branch, Base: base, Tip: outcome.branchTip, Report: outcome.report,
		TaskModel: o.TaskModel, ReviewModel: o.ReviewModel}
	if merge != nil {
		run.MergeSummary, run.MergeSkipped = merge.summary(), merge.skipped
	}
	return run
}

// withLastRun replaces the state's last run and drops the provider session and pending action
// outcomes: a chat resumed across a new run would answer about the previous one.
func (s acpSessionState) withLastRun(run *acpLastRun) acpSessionState {
	s.LastRun = run
	s.Provider, s.ProviderSessionID = "", ""
	s.Actions = nil
	return s
}

// acpActionLimit bounds the action outcomes kept for the next chat turn, and acpActionRunes the
// length of each, since a refusal can carry a tool's error output.
const (
	acpActionLimit = 5
	acpActionRunes = 2000
)

// withAction records an action outcome for the next chat turn, keeping the latest acpActionLimit.
func (s acpSessionState) withAction(outcome string) acpSessionState {
	if r := []rune(outcome); len(r) > acpActionRunes {
		outcome = string(r[:acpActionRunes]) + "..."
	}
	s.Actions = append(slices.Clone(s.Actions), outcome)
	if extra := len(s.Actions) - acpActionLimit; extra > 0 {
		s.Actions = s.Actions[extra:]
	}
	return s
}

// sessionOpts layers the last run's model overrides over base, so a chat turn answers through the
// review model the thread's run was launched with rather than the configured one. Like the launch,
// an absent override leaves the configured model; recorded values failing the launch checks are
// ignored with a warning.
func (s acpSessionState) sessionOpts(base opts, warnings io.Writer) opts {
	if s.LastRun == nil {
		return base
	}
	models := opts{TaskModel: s.LastRun.TaskModel, ReviewModel: s.LastRun.ReviewModel}
	if err := validatePassThroughValues("ACP session state", models); err != nil {
		fmt.Fprintf(warnings, "warning: ignoring the recorded model overrides: %v\n", err)
		return base
	}
	o := base
	o.TaskModel, o.ReviewModel = models.TaskModel, models.ReviewModel
	return o
}
