# Preserve review worktrees on failure

- found: 2026-09-06, plan: 20260906-review-resume, phase: task
- severity: major
- area: cmd/loopai/main.go

A fresh foreground `--worktree` run still force-removes its worktree after an execution failure
(`runWithWorktree`, `cmd/loopai/main.go:2319`). Once review has begun, this can discard uncommitted
fixes from a reviewer that crashed or was interrupted before it reached its normal commit boundary.
The review checkpoint deliberately does not record a dirty stage, so its clean-tree gate keeps resumed
state honest, but the uncommitted work itself is still lost with the worktree.

Detached runs (`--detach`) no longer have this problem: once their attempt has started they keep the
worktree on every non-success exit, and on a success whose close-out stopped short or whose tree is
dirty, through `executePlanRequest.KeepWorktreeOnFailure` and `keepDetachedWorktree`
(`cmd/loopai/detach_worktree.go`). Foreground `--worktree` runs are unchanged and remain the scope of
this entry.

Consider tracking whether a foreground run entered a review phase and preserving the failed worktree
from that point onward, or reusing the detached-run keep decision. A rerun could then use the existing
worktree lock and resume validation while retaining the dirty reviewer changes for inspection or
continuation. Cleanup should remain unchanged for setup failures and failures before review begins,
and the preserved-worktree path should clearly report its location and required recovery action.
