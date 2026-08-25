# Review-only modes can commit the user's pre-existing edits in a dirty checkout

- found: 2026-08-25, plan: 20260825-backlog-capture, phase: evaluation
- severity: major
- area: pkg/config/defaults/prompts, pkg/processor/runner.go

`--review`, `--external-only`, and `--codex-only` map to `ModeReview` and `ModeCodexOnly`, which
`modeRequiresBranch` (cmd/loopai/main.go:1937) excludes, so they create no branch and no worktree
and run in the user's own checkout. A dirty tree there is allowed and never gated, by design.

Every commit instruction on those paths stages by file path: review_first.txt:110,
review_second.txt:81, codex.txt:66, custom_eval.txt:66, external_claude_eval.txt:38, and the
Go-side `commitPrefix` at pkg/processor/runner.go:431, which is the widest of the six because it
says only "stage and commit them" without naming paths at all. `git add <file>` is
file-granular, so whenever a review fix lands in a file the user was already editing, the user's
unrelated hunks in that same file are staged with it and land in loopai's
`fix: address code review findings` commit, on whatever branch they had checked out.

Avoiding `git add -A` bounds the blast radius to files the reviewer itself touched but does not
close this: the two sets overlap exactly when the user is mid-edit in code worth reviewing.
Nothing is lost — the commit is recoverable — but the user's work in progress is mixed into and
mislabelled by an automated commit they did not author.

Not a defect in this branch's diff: the pre-existing prompts said only
`git commit -m "fix: address code review findings"`, which models satisfy with `git add -A` or
`git commit -a` and which therefore swept every dirty file, not just the touched ones. The
current wording strictly narrows the exposure.

Suggested fix direction, in increasing cost: warn at startup when a review-only mode begins in a
dirty checkout, so the outcome is at least predicted; or gate those modes on a clean tree behind
an opt-out flag; or run them in a worktree like `ModeFull` does. Baseline-aware hunk staging
(`git stash push --keep-index` around the fix, or `git apply` of a diff computed against the
pre-run baseline) is the only approach that preserves today's dirty-tree tolerance, and is by
far the most fragile. Whichever is chosen, all six instruction sites must move together.
