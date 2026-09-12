---
name: loopai-merge
description: "Read and narrate a loopai completion report, then close out the plan by merging or opening a PR after the user chooses. Use for loopai-merge, merge plan, or смержи план."
---

# Review and close out a completed loopai plan

Read the report and present the close-out choice. Do not edit repository files, run `git merge` directly, or delete branches or worktrees yourself.

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

## Confirm and execute

Keep the selected base as `BASE`. Verify it is a local branch with `git show-ref --verify "refs/heads/$BASE"`, and that it differs from the feature. If report metadata contains a commit, missing ref, or the feature itself, ask which local base branch to use. A `branch: (merged)` report has no live feature to merge; report that and stop.

Ask the user “Merge into <base>?” with choices to merge, open a PR, or cancel. Explain that this skill requires a close-out choice after the report has been presented. Wait for the answer before either mutation.

For Merge:

```bash
loopai --merge="$BASE" "$PLAN_STEM"
```

For Open PR:

```bash
loopai --pr="$BASE" "$PLAN_STEM"
```

Pass the exact confirmed base with the attached option shown above: omitting it selects main/master. Show the command output verbatim. On conflict or other failure, report it and stop; do not resolve conflicts, retry, or attempt a different close-out. On Cancel, stop without changes.
