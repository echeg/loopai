---
name: loopai-merge
description: "Read and narrate a loopai completion report, preview merge conflicts, then close out the plan by merging or opening a PR after the user chooses. Use for loopai-merge, merge plan, or смержи план."
---

# Review and close out a completed loopai plan

Read the report, predict conflicts read-only, and present the close-out choice. When the user chooses to resolve conflicts, merge the base into the plan branch inside that branch's own checkout, resolve the conflicted files there, and hand the clean result to `loopai --merge`. Never merge the plan branch into the base by hand, never delete branches, and remove only a temporary worktree this invocation created.

## Resolve the plan and report

Check `command -v loopai` and `git rev-parse --show-toplevel`, then work from that repository root. Run `git status --porcelain`; if it is nonempty, show the changes and stop because this close-out workflow requires a clean checkout.

Require one plan identifier from the user's request. Normalize a plan path to its final component, remove a trailing `.report.md` or `.md`, and retain any date prefix. Keep the result as `PLAN_STEM`. An absent or ambiguous identifier needs clarification.

Run the following with properly quoted arguments:

```bash
loopai --report "$PLAN_STEM"
```

Read the feature branch from the leading `branch:` line and the base from report metadata. Narrate the report in the conversation language in this order: summary, change scope, risk, migrations and operational steps, plan deviation, backlog, then findings and outcomes for each external reviewer. Preserve supplied counts and measurements, applying the duration display formatting below. State when information is absent; do not invent findings, risks, migrations, or validation results.

For the narrated report, convert all elapsed durations (including `duration_ms` and phase/total timings) from milliseconds to hours, minutes, and seconds. Round to the nearest whole second before splitting into units; omit leading zero units, display zero as `0 s`, and positive values below one second as `<1 s`. Localize the units and use a readable heading such as `Duration` (`Длительность` in Russian), never `duration_ms`. For example, `3661000 ms` becomes `1 h 1 min 1 s`, and `854733 ms` becomes `14 min 15 s`. Preserve the underlying measurements; this is display formatting only. Other counts and measurements remain unchanged.

If the command specifically says no completion report exists, explain that the run predates reporting or archived without one. Resolve the feature read-only using matching progress header records under `.loopai/progress/` (match the plan basename, prefer the newest full/tasks-only record), then an exact local branch, then the branch derived from the matching plan filename by removing its leading digits and dashes. Use the current branch as the proposed base only when it differs from the feature; otherwise ask for the base. Verify refs before showing `git log "$BASE".."$BRANCH" --stat`. Explain that this log provides no risk, migration, deviation, backlog, or reviewer assessment. Other report-command errors must be shown and resolved before continuing.

## Resolve the base

Keep the selected base as `BASE`. Verify it is a local branch with `git show-ref --verify "refs/heads/$BASE"`, and that it differs from the feature. If report metadata contains a commit, missing ref, or the feature itself, ask which local base branch to use. Keep the feature as `FEATURE` and verify it the same way. A `branch: (merged)` report has no live feature to merge; report that and stop.

## Predict conflicts

Before offering close-out, predict the merge read-only from the repository root; nothing here changes the repository:

```bash
git merge-tree --write-tree --name-only "$BASE" "$FEATURE"
```

Exit code 0 means a clean merge: say so and continue. Exit code 1 means conflicts: the first line is the result tree oid (`TREE`), the lines up to the first blank line are the conflicted paths, and the `CONFLICT (...)` lines name each conflict's kind. Any other outcome, including Git older than 2.38 rejecting `--write-tree`, means prediction is unavailable: say so and offer the plain merge choice; `loopai --merge` still aborts a conflicting merge safely.

For each conflicted path, gather `git show "$TREE:$path"` (merged content with markers, content conflicts only), `git log --oneline "$BASE".."$FEATURE" -- "$path"` (what the plan branch did), and `git log --oneline "$FEATURE".."$BASE" -- "$path"` (what the base did). Narrate per file in the conversation language: path, conflict kind, what each side changed and in which commits, and the conflicting region when it is short enough to quote (up to roughly 40 lines). Do not propose resolutions yet and do not present the prediction as certain; the real merge is the authority.

## Confirm and execute

Do not run any mutating command before the answer.

Without conflicts, ask “Merge into <base>?” with choices to merge, open a PR, or cancel. With conflicts, ask “Merge into <base>? <N> files conflict” with choices to resolve and merge, open a PR (the forge will show the conflicts), or cancel. Explain that this skill requires a close-out choice after the report has been presented. Wait for the answer before either mutation.

For Merge:

```bash
loopai --merge="$BASE" "$PLAN_STEM"
```

For Open PR:

```bash
loopai --pr="$BASE" "$PLAN_STEM"
```

Pass the exact confirmed base with the attached option shown above: omitting it selects main/master. Show the command output verbatim. If Merge reports a conflict the prediction missed, report it and stop; the user can rerun the skill. On other failures, report and stop without attempting a different close-out. On Cancel, stop without changes.

## Resolve and merge

Only after the user chose it. The direction is fixed: `BASE` is merged into `FEATURE` inside the checkout of `FEATURE`, so that `loopai --merge` afterwards finds a clean merge and still performs its own close-out. Never merge `FEATURE` into `BASE` by hand.

1. Locate the checkout: `git worktree list --porcelain`; `WT` is the `worktree` path whose `branch` line is `refs/heads/$FEATURE`. If none, ask whether to create a temporary worktree (`WT=$(mktemp -d) && git worktree add "$WT" "$FEATURE"`; an empty directory is a valid target, and `loopai --merge` removes it after a successful close-out) or cancel, and remember that this invocation created it. Every abort path below ends by removing a worktree this invocation created (`git worktree remove "$WT"`, and say so); a pre-existing checkout is never removed. `git -C "$WT" status --porcelain` must be empty; otherwise show it and stop.
2. Start the merge: `git -C "$WT" merge --no-ff --no-commit "$BASE"`, then `git -C "$WT" diff --name-only --diff-filter=U`. The unmerged paths must equal the predicted set; otherwise `git -C "$WT" merge --abort`, report both lists, and stop. If the merge completes without conflicts, abort it as well and offer the plain merge choice. Marker labels are reversed relative to the prediction: in `WT` the `<<<<<<< HEAD` side is the plan branch and `>>>>>>> $BASE` is the base; read the labels, not the order.
3. Resolve each unmerged path from the file in `WT`, the per-side history, and the plan under `docs/plans/completed/$PLAN_STEM.md` or `docs/plans/$PLAN_STEM.md` in `WT`. Content and add/add conflicts: edit the file to keep both intents, remove every marker, `git -C "$WT" add "$path"`; keep one side wholesale only when the other is genuinely superseded, and say so. Modify/delete and rename/delete: decide from the histories, apply with `git -C "$WT" rm` or `git -C "$WT" add`, and say which side won and why. Edit only unmerged paths. Then verify: `git -C "$WT" diff --cached --check` and `git -C "$WT" grep -n -E '^(<{7}|={7}|>{7})( |$)' -- <staged paths>` must print nothing (`git grep` exit code 1 means no match, the wanted outcome).
4. Validate: run each command from the plan's `## Validation Commands` section in `WT`, in order, and show the results. If the section is absent, say so and continue. On any failure, `git -C "$WT" merge --abort` (which returns `WT` to the untouched plan branch tip), run the same failing command there once more, and report the exit code and output together with which case it is: failing only on the resolution means the resolution is not good enough; failing on the plan branch tip as well means the failure predates the merge. Stop in both cases.
5. Commit `git -C "$WT" commit -m "merge $BASE into $FEATURE: resolve conflicts before close-out"`, show a per-file summary of what the resolution kept, then run `loopai --merge="$BASE" "$PLAN_STEM"` and show its output verbatim. If it still reports a conflict, the base moved in between: report and stop without retrying.

## Constraints

- Never edit repository files except the unmerged paths of the base-into-feature merge the user chose.
- Never run `git merge` directly except `git merge --no-ff --no-commit "$BASE"` inside the plan branch checkout after that choice; only `loopai --merge` merges the plan branch into the base.
- Never resolve a conflict by discarding one side without saying so.
- Never delete branches or worktrees directly, except a temporary worktree this invocation created and only on an abort path; never invent facts; never choose the close-out action on the user's behalf.
