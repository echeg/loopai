---
name: loopai-merge
description: "Review a loopai completion report, preview merge conflicts, and ask before merging or opening a PR. Triggers: loopai-merge, merge plan, смержи план."
argument-hint: '<plan name or path>'
allowed-tools: [Bash, Read, Glob, Edit, AskUserQuestion]
---

# loopai-merge - Review and Close Out a Completed Plan

SCOPE: This skill reads and narrates a loopai completion report, predicts merge conflicts read-only and narrates them, then runs the close-out action the user explicitly chooses. When the user chooses to resolve conflicts, it merges the base into the plan branch inside that branch's own checkout, resolves the conflicted files there, and hands the clean result to `loopai --merge`. It never merges the plan branch into the base by hand, never deletes branches, and removes only a temporary worktree it created itself.

## Preflight

Run each check separately and stop on the first failure:

```bash
which loopai
git rev-parse --show-toplevel
git status --porcelain
```

- If `loopai` is missing, report that and stop.
- Change to the repository root returned by `git rev-parse --show-toplevel` before subsequent commands.
- The `git status --porcelain` output must be empty. If it is not, show the output and stop; close-out requires a clean working tree.
- Require exactly one non-empty argument. Do not guess a plan when none was supplied.

## Normalize the Plan Argument

Normalize `$ARGUMENTS` to `PLAN_STEM` without changing the repository:

1. Trim surrounding whitespace.
2. Take the final path component, so `docs/plans/foo.md` and `docs/plans/completed/foo.md` become `foo.md`.
3. Remove a trailing `.report.md` or `.md`.
4. Keep a leading date when present. Thus `20260906-foo` remains `20260906-foo`, while `foo` remains `foo`.
5. Reject an empty result or an argument containing more than one whitespace-separated value.

Pass only the normalized stem to loopai:

```bash
loopai --report "$PLAN_STEM"
```

This lets `20260906-foo`, `foo`, `docs/plans/foo.md`, and `docs/plans/completed/foo.md` all resolve through loopai's plan-or-branch lookup.

## Narrate the Completion Report

When `loopai --report` succeeds, preserve its facts and narrate them in the language used by the user in this conversation. Cover these fixed topics in order:

1. Summary
2. Change scope
3. Risk
4. Migrations and operational steps
5. Plan deviation
6. Backlog
7. External review, one reviewer at a time, including each reported finding and outcome

Use only information present in the report. Do not infer missing migrations, risks, findings, reviewer outcomes, validation results, branch names, or base branches. Keep Go-provided facts unchanged, applying the duration display formatting below.

For the narrated report, convert all elapsed durations (including `duration_ms` and phase/total timings) from milliseconds to hours, minutes, and seconds. Round to the nearest whole second before splitting into units; omit leading zero units, display zero as `0 s`, and positive values below one second as `<1 s`. Localize the units and use a readable heading such as `Duration` (`Длительность` in Russian), never `duration_ms`. For example, `3661000 ms` becomes `1 h 1 min 1 s`, and `854733 ms` becomes `14 min 15 s`. Preserve the underlying measurements; this is display formatting only. Other counts and measurements remain unchanged.

Read the feature branch from the command's `branch:` line and the base branch from the report metadata line. If either is absent, state that it is absent instead of inventing it; use the read-only resolution described below before offering close-out.

## Missing Report Fallback

If `loopai --report "$PLAN_STEM"` reports that no completion report exists, say that the run predates completion reports or was archived without one. Then resolve the refs read-only:

- `BASE` is the currently checked-out branch from `git branch --show-current`.
- Prefer the `Branch:` header from the matching `.loopai/progress/*.txt` record. Match its `Plan:` basename to `PLAN_STEM`; use Glob to locate records and Read to inspect only their header blocks.
- Otherwise use an exact local branch named `PLAN_STEM`, if it exists.
- Otherwise, when the matching plan file is available under `docs/plans/` or `docs/plans/completed/`, derive the usual branch by removing its leading run of digits and dashes.

Validate both refs with `git rev-parse --verify` before using them. If either cannot be resolved, report that clearly and stop rather than substituting another ref. When both resolve, show the fallback change summary:

```bash
git log "$BASE".."$BRANCH" --stat
```

Do not turn the fallback log into report facts. Explain that risk, migration, deviation, backlog, and reviewer assessments are unavailable, then continue to the conflict preview.

## Resolve the Base

Store the resolved base branch as `BASE` and verify it names a local branch with `git show-ref --verify "refs/heads/$BASE"`. If it is missing, is a commit rather than a local branch, or equals the feature branch, ask the user which local base branch to use before continuing. Store the feature branch as `FEATURE` and verify it the same way; a `branch: (merged)` report has no live feature to merge, so report that and stop.

## Predict Conflicts

Before offering close-out, predict the merge read-only from the repository root. Nothing in this section changes the repository.

```bash
git merge-tree --write-tree --name-only "$BASE" "$FEATURE"
```

- Exit code 0 means the merge is clean: say so in one line and continue to the confirmation gate.
- Exit code 1 means conflicts. The first output line is the oid of the result tree (`TREE`); the following lines up to the first blank line are the conflicted paths; the `CONFLICT (...)` lines after it name each conflict's kind (`content`, `modify/delete`, `add/add`, `rename/delete`, ...).
- Any other exit code, or `merge-tree` rejecting `--write-tree` (Git older than 2.38), means prediction is unavailable: say so, and continue to the confirmation gate with the plain `Merge` option. `loopai --merge` still aborts a conflicting merge safely.

For each conflicted path, gather read-only evidence and narrate it in the user's language:

```bash
git show "$TREE:$path"                                  # merged content with conflict markers (content conflicts only)
git log --oneline "$BASE".."$FEATURE" -- "$path"       # what the plan branch did to the file
git log --oneline "$FEATURE".."$BASE" -- "$path"       # what the base did to the file since the branch point
```

Report per file: the path, the conflict kind, what each side changed and in which commits, and the conflicting region between the `<<<<<<<` and `>>>>>>>` markers when it is short enough to quote (up to roughly 40 lines; summarize longer regions). Do not guess how a conflict should be resolved at this point, and do not present the prediction as certain: the real merge in the next step is the authority.

## Confirmation Gate

Do not run any mutating command before the answer. Use AskUserQuestion in the conversation language.

**Without conflicts**, ask exactly `Merge into <base>?`, replacing `<base>` with `BASE`, with these options:

- `Merge` - run `loopai --merge="$BASE" "$PLAN_STEM"`
- `Open PR` - run `loopai --pr="$BASE" "$PLAN_STEM"`
- `Cancel` - make no changes and stop

**With conflicts**, ask `Merge into <base>? <N> files conflict`, with these options:

- `Resolve and merge` - follow the Resolve Conflicts section, then run `loopai --merge="$BASE" "$PLAN_STEM"`
- `Open PR` - run `loopai --pr="$BASE" "$PLAN_STEM"`; the PR will show the conflicts on the forge
- `Cancel` - make no changes and stop

For Merge, run:

```bash
loopai --merge="$BASE" "$PLAN_STEM"
```

Show loopai's output verbatim. If it reports a merge conflict that the prediction missed, report that the merge stopped with conflicts and take no further action in this invocation: the user can rerun the skill, and the new prediction will show them.

For Open PR, run:

```bash
loopai --pr="$BASE" "$PLAN_STEM"
```

Show loopai's output verbatim. If the command fails, report the failure and stop without attempting an alternative close-out.

For Cancel, confirm that no close-out action was taken and stop.

## Resolve Conflicts

Only after the user chose `Resolve and merge`. The direction is fixed: `BASE` is merged into `FEATURE`, inside the checkout of `FEATURE`, so that `loopai --merge` afterwards finds a clean merge and still performs its own close-out (ancestry verification, branch and worktree removal). Never merge `FEATURE` into `BASE` by hand.

### 1. Locate the plan branch checkout

```bash
git worktree list --porcelain
```

`WT` is the `worktree` path of the entry whose `branch` line is `refs/heads/$FEATURE`. If no entry has it, the branch is checked out nowhere: ask `Create a temporary worktree for <feature> to resolve the conflicts?` with the options `Create` and `Cancel`. On `Create`, run `WT=$(mktemp -d) && git worktree add "$WT" "$FEATURE"` (an empty directory is a valid target, and no parent directory is left behind when the worktree is removed), and remember that this invocation created it (`TEMP_WT=1`); `loopai --merge` removes it with the branch after a successful close-out. On `Cancel`, stop with no changes.

Every abort path below (`git merge --abort` followed by stop) ends with cleaning up a worktree this invocation created: when `TEMP_WT` is set, run `git worktree remove "$WT"` and say so. A pre-existing checkout of the plan branch is never removed.

`git -C "$WT" status --porcelain` must be empty. If it is not, show the output and stop: the plan branch checkout carries uncommitted work that a merge would entangle.

### 2. Start the merge

```bash
git -C "$WT" merge --no-ff --no-commit "$BASE"
git -C "$WT" diff --name-only --diff-filter=U
```

The merge is expected to stop with conflicts. The unmerged paths must be exactly the predicted set; if they differ, run `git -C "$WT" merge --abort`, report both lists, and stop. If the merge unexpectedly completes without conflicts, run `git -C "$WT" merge --abort` too and offer the plain `Merge` option: the prediction was stale.

Marker labels are reversed relative to the prediction: in `WT` the `<<<<<<< HEAD` side is the plan branch and the `>>>>>>> $BASE` side is the base, while `merge-tree` labelled them `$BASE` first and `$FEATURE` second. Read the labels, not the order.

### 3. Resolve each file

For every unmerged path, read the working-tree file in `WT` (it carries the markers), the per-side history from the prediction, and the plan under `docs/plans/completed/$PLAN_STEM.md` or `docs/plans/$PLAN_STEM.md` in `WT` for the intent of the plan branch. Then:

- Content conflicts: edit the file in `WT` so that it keeps the intent of both sides, remove every marker, and `git -C "$WT" add "$path"`. Combining both changes is the default; keep one side wholesale only when the other side's change is genuinely superseded, and say so in the summary.
- Modify/delete and rename/delete conflicts: decide from the histories whether the deletion or the modification stands, apply it with `git -C "$WT" rm "$path"` or `git -C "$WT" add "$path"`, and say which side won and why.
- Add/add conflicts: merge the two contents the same way as a content conflict.

Edit only the unmerged paths. Do not touch other files, reformat, or fix unrelated issues you notice. Before continuing, verify no markers survived:

```bash
git -C "$WT" diff --cached --check
git -C "$WT" grep -n -E '^(<{7}|={7}|>{7})( |$)' -- $(git -C "$WT" diff --cached --name-only)
```

The `grep` must print nothing; its exit code 1 means no match and is the wanted outcome.

### 4. Validate

Read the `## Validation Commands` section of the plan file in `WT`. Run each command from `WT`, in order, and show the results. If the section is absent, say that the plan carries no validation commands and continue.

If any command fails, run `git -C "$WT" merge --abort`, which returns `WT` to the untouched plan branch tip, then run the same failing command there once more. Report the exit code and whatever output there was, and say which case it is: the command fails only on the resolution, so the resolution is not good enough to hand to `loopai --merge`; or it fails on the plan branch tip as well, so the failure predates the merge and is not caused by the resolution. Stop in both cases; the second one is for the user to decide about.

### 5. Commit and hand off

```bash
git -C "$WT" commit -m "merge $BASE into $FEATURE: resolve conflicts before close-out"
```

Show a per-file summary of what the resolution kept, then run:

```bash
loopai --merge="$BASE" "$PLAN_STEM"
```

Show its output verbatim. If it still reports a conflict, the base moved between the resolution and the merge: report that and stop without retrying; the user can rerun the skill.

## Constraints

- Never edit code, plans, reports, configuration, or other repository files, except the unmerged paths of the base-into-feature merge the user chose.
- Never run `git merge` directly, except `git merge --no-ff --no-commit "$BASE"` inside the plan branch checkout after the user chose `Resolve and merge`; only `loopai --merge` may merge the plan branch into the base.
- Never resolve a conflict by discarding one side without saying so in the summary.
- Never delete branches or worktrees directly, except a temporary worktree this invocation created, and only on an abort path.
- Never invent a fact absent from the completion report or the git history.
- Never choose Merge, Resolve and merge, or Open PR on the user's behalf.
