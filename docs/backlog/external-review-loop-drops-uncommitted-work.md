# External review loop drops uncommitted work on cap and stalemate exits

- found: 2026-08-25, plan: 20260825-backlog-capture, phase: internal review
- severity: major
- area: pkg/processor/phase/external_review.go

`ExternalReviewPhase.runLoop` (pkg/processor/phase/external_review.go:143-189) has three exits.
Only `externalReviewStop` follows an evaluator turn that reached the `EXTERNAL_REVIEW_DONE`
block and committed. The stalemate exit (line 167) and the iteration-cap exit (line 188) both
return without running the evaluator again, so everything the evaluator accumulated across the
chain stays uncommitted: the fixes it made in earlier rounds and, since the backlog convention,
the staged backlog entry.

Post-external review runs next, but `review_second.txt` commits only on Path B; Path A emits
`REVIEW_DONE` with no commit. `finalize.txt` runs `git rebase`, which refuses a dirty tree and is
best-effort anyway, and is disabled by default. `runWithWorktree` then removes the worktree
through `git worktree remove --force` (cmd/loopai/main.go, pkg/git/external.go:1207), discarding
staged and untracked content. The run reports success.

This predates the backlog work — the evaluator's fixes were already lost the same way — so it is
not a defect in this branch's diff, but the backlog entry now rides the same path.

Suggested fix direction: commit whatever remains after the reviewer chain finishes, either from a
final evaluator turn on the non-clean exits or from the phase itself before it returns. Do not
move the commit into the loop: `getDiffInstruction` shows the reviewer only the uncommitted diff
after the first round, so a mid-loop sweep hides the accumulated fixes and lets
`EXTERNAL_REVIEW_DONE` fire with them unverified.
