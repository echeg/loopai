# Preserve review worktrees on failure

- found: 2026-09-06, plan: 20260906-review-resume, phase: task
- severity: major
- area: cmd/loopai/main.go

A fresh `--worktree` run still force-removes its worktree after an execution failure
(`cmd/loopai/main.go:1757`). Once review has begun, this can discard uncommitted fixes from a
reviewer that crashed or was interrupted before it reached its normal commit boundary. The review
checkpoint deliberately does not record a dirty stage, so its clean-tree gate keeps resumed state
honest, but the uncommitted work itself is still lost with the worktree.

Consider tracking whether the run entered a review phase and preserving the failed worktree from
that point onward. A rerun could then use the existing worktree lock and resume validation while
retaining the dirty reviewer changes for inspection or continuation. Cleanup should remain unchanged
for setup failures and failures before review begins, and the preserved-worktree path should clearly
report its location and required recovery action.
