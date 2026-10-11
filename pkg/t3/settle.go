package t3

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const settlementPollInterval = 250 * time.Millisecond

// SettleMergedBranch settles non-archived threads on a successfully merged branch in projects
// rooted at one of the repository's registered worktrees. It never uses the invoking thread id:
// a close-out can run from another thread or checkout. Already settled threads are skipped, and
// a rejected thread does not prevent settling other matches. The count includes only successes.
func SettleMergedBranch(ctx context.Context, api Dispatcher, roots []string, branch string) (int, error) {
	settlement, err := PrepareBranchSettlement(ctx, api, roots, branch)
	if err != nil {
		return 0, err
	}
	settled := 0
	var failures []error
	for _, target := range settlement.targets {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if target.busy {
			failures = append(failures, fmt.Errorf("t3: thread %s still has an active turn", target.id))
			continue
		}
		if _, err := api.Dispatch(ctx, NewThreadSettle(target.id)); err != nil {
			failures = append(failures, fmt.Errorf("t3: settle thread %s: %w", target.id, err))
			continue
		}
		settled++
	}
	return settled, errors.Join(failures...)
}

type settlementTarget struct {
	id        string
	projectID string
	branch    string
	messageAt string
	turnID    string
	busy      bool
}

// BranchSettlement captures the threads and user turns associated with one successful merge.
// Prepare it before returning the ACP response, then call Settle after the response is written.
// It may be used only once and must not run concurrently with itself.
type BranchSettlement struct {
	api     Dispatcher
	targets []settlementTarget
}

// PrepareBranchSettlement captures matching thread identities without changing T3 Code. Pending
// approval or input threads are excluded so settling cannot dismiss a request for the user.
func PrepareBranchSettlement(ctx context.Context, api Dispatcher, roots []string, branch string) (*BranchSettlement, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, errors.New("t3: merged branch is empty")
	}
	shell, err := api.Shell(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // Shell already names the failing call
	}
	projects := map[string]bool{}
	for _, root := range roots {
		if project, ok := shell.FindProject(root); ok {
			projects[project.ID] = true
		}
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("t3: no T3 Code project for %s", strings.Join(roots, ", "))
	}
	settlement := &BranchSettlement{api: api}
	for _, thread := range shell.Threads {
		if !projects[thread.ProjectID] || thread.ArchivedAt != nil || thread.SettledAt != nil ||
			thread.Branch == nil || *thread.Branch != branch || thread.HasPendingApprovals || thread.HasPendingUserInput {
			continue
		}
		settlement.targets = append(settlement.targets, settlementTarget{
			id: thread.ID, projectID: thread.ProjectID, branch: branch,
			messageAt: threadMessageAt(thread), turnID: threadTurnID(thread), busy: threadBusy(thread),
		})
	}
	return settlement, nil
}

// Settle waits for captured turns to finish, then settles them. A new user message or turn,
// changed project or branch, or a pending input request abandons that target. The caller must
// bound ctx and cancel it before accepting another ACP prompt. Settling can stop the ACP process,
// so all report and session-state writes must happen before this method is called.
func (s *BranchSettlement) Settle(ctx context.Context) (int, error) {
	if s == nil || len(s.targets) == 0 {
		return 0, nil
	}
	pending := s.targets
	s.targets = nil
	settled := 0
	var failures []error
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return settled, errors.Join(append(failures, err)...)
		}
		shell, err := s.api.Shell(ctx)
		if err != nil {
			return settled, errors.Join(append(failures, err)...)
		}
		threads := make(map[string]Thread, len(shell.Threads))
		for _, thread := range shell.Threads {
			threads[thread.ID] = thread
		}
		waiting := pending[:0]
		for _, target := range pending {
			thread, ok := threads[target.id]
			if !ok || !target.matches(thread) {
				continue
			}
			if threadBusy(thread) {
				waiting = append(waiting, target)
				continue
			}
			if err := ctx.Err(); err != nil {
				return settled, errors.Join(append(failures, err)...)
			}
			if _, err := s.api.Dispatch(ctx, NewThreadSettle(target.id)); err != nil {
				failures = append(failures, fmt.Errorf("t3: settle thread %s: %w", target.id, err))
				continue
			}
			settled++
		}
		pending = waiting
		if len(pending) > 0 {
			timer := time.NewTimer(settlementPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return settled, errors.Join(append(failures, ctx.Err())...)
			case <-timer.C:
			}
		}
	}
	return settled, errors.Join(failures...)
}

func (t settlementTarget) matches(thread Thread) bool {
	return thread.ProjectID == t.projectID && thread.Branch != nil && *thread.Branch == t.branch &&
		thread.ArchivedAt == nil && thread.SettledAt == nil && !thread.HasPendingApprovals && !thread.HasPendingUserInput &&
		threadMessageAt(thread) == t.messageAt && threadTurnID(thread) == t.turnID &&
		(thread.LatestTurn == nil || (thread.LatestTurn.State != "error" && thread.LatestTurn.State != "interrupted"))
}

func threadBusy(thread Thread) bool {
	return thread.Session != nil && (thread.Session.Status == "starting" || thread.Session.Status == "running")
}

func threadMessageAt(thread Thread) string {
	if thread.LatestUserMessageAt == nil {
		return ""
	}
	return *thread.LatestUserMessageAt
}

func threadTurnID(thread Thread) string {
	if thread.LatestTurn == nil {
		return ""
	}
	return thread.LatestTurn.TurnID
}
