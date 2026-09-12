---
name: loopai-merge
description: "Review a loopai completion report and ask before merging or opening a PR. Triggers: loopai-merge, merge plan, смержи план."
argument-hint: '<plan name or path>'
allowed-tools: [Bash, Read, Glob, AskUserQuestion]
---

# loopai-merge - Review and Close Out a Completed Plan

SCOPE: This skill reads and narrates a loopai completion report, then runs the close-out action the user explicitly chooses. It never edits code or repository files, runs `git merge` directly, or deletes branches.

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

Do not turn the fallback log into report facts. Explain that risk, migration, deviation, backlog, and reviewer assessments are unavailable, then continue to the confirmation gate.

## Confirmation Gate

Store the resolved base branch as `BASE` and verify it names a local branch with `git show-ref --verify "refs/heads/$BASE"`. If it is missing, is a commit rather than a local branch, or equals the feature branch, ask the user which local base branch to use before offering close-out. Pass that exact confirmed base with the attached `--merge=` or `--pr=` option; omitting it would auto-select main/master.

Use AskUserQuestion with the exact question `Merge into <base>?`, replacing `<base>` with the resolved base branch, and these options:

- `Merge` - run `loopai --merge="$BASE" "$PLAN_STEM"`
- `Open PR` - run `loopai --pr="$BASE" "$PLAN_STEM"`
- `Cancel` - make no changes and stop

Do not run either close-out command before the answer.

For Merge, run:

```bash
loopai --merge="$BASE" "$PLAN_STEM"
```

Show loopai's output verbatim. If it reports a merge conflict, report that the merge stopped with conflicts and take no further action. Do not resolve conflicts, run `git merge` by hand, retry, or delete any branch.

For Open PR, run:

```bash
loopai --pr="$BASE" "$PLAN_STEM"
```

Show loopai's output verbatim. If the command fails, report the failure and stop without attempting an alternative close-out.

For Cancel, confirm that no close-out action was taken and stop.

## Constraints

- Never edit code, plans, reports, configuration, or other repository files.
- Never run `git merge` directly; only `loopai --merge` may merge.
- Never delete branches or worktrees directly.
- Never invent a fact absent from the completion report.
- Never choose Merge or Open PR on the user's behalf.
