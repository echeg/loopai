---
name: loopai-orca
description: "Run an existing loopai plan inside an Orca-managed worktree and terminal tab (Orca desktop app, onorca.dev) so the run appears as an Orca card with live status, optionally with executor, model, or external-reviewer overrides. Triggers: loopai-orca, run plan in orca, launch loopai in orca worktree."
metadata:
  short-description: Run a loopai plan in an Orca worktree
---

# loopai-orca - Run a Plan in an Orca Worktree

**SCOPE**: create an Orca-managed worktree, launch `loopai --orca` in a terminal tab there, and report how to monitor it. Do not edit code, commit, or merge. Orca owns the worktree, the branch, and the tab; loopai runs inside them as an ordinary process.

Every `orca` call below must use `--json`; read fields from the JSON, never from prose output. If a flag is rejected, check `orca <command> --help` (or `orca skills get orca-cli`) before improvising - the flag surface changes between Orca releases.

## Step 0: Preflight

Run the checks separately so each failure is distinguishable. **Stop and report the failing check verbatim; do not continue past a failure.**

```bash
command -v loopai                         # missing -> stop: make build && install -m 0755 .bin/loopai ~/.local/bin/loopai
loopai --help | grep -q -- '--orca' || echo "MISSING_ORCA_FLAG"   # printed -> stop: fork too old
orca status --json                        # result.runtime.state must be "ready"; anything else -> stop
git rev-parse --show-toplevel
git branch --show-current
```

- Resolve the Orca executable the way Orca's own `orca-cli` skill does: use `$ORCA_CLI_COMMAND` when set; on Linux outside an Orca terminal use `orca-ide`, never bare `orca` (it is the GNOME screen reader there); otherwise `orca`. Substitute it for `orca` in every command below.
- If HEAD is detached, ask which base branch to use and set `BASE` to the answer.
- Record `ROOT` (repository root) and `BASE` (current branch); the Orca worktree is cut from `BASE`.

## Step 0b: Parse Arguments

Split the request on whitespace. The first token that does not start with `--` is `PLAN_ARG` (may be absent). Every other token must be one of the pass-through flags below; collect them, in order, into `FLAGS` (one space-separated string, empty when none were given):

| Flag | Form | Value |
|------|------|-------|
| `--codex` | bare | none |
| `--task-model` | `--task-model M` or `--task-model=M` | one token |
| `--review-model` | `--review-model M` or `--review-model=M` | one token |
| `--external-reviewers` | `--external-reviewers LIST` or `=LIST` | one token: comma-separated `provider[:model[:effort]]` |

- Every value must match `^[A-Za-z0-9._:,+-]+$`. `FLAGS` is spliced into the `--command` string a shell executes in Step 5, so a value carrying whitespace, quotes, `$`, or `;` is rejected, not escaped.
- Any token outside the table - `--worktree`, `--commit`, `--serve`, `--plan`, `--codex-args`, a second plan path - a flag given twice, or a value-taking flag without a value **stops the run**. Report the offending token verbatim and state that only the four flags above pass through. Never forward it: `--worktree` would nest a second checkout inside Orca's worktree and `--serve` would block the tab after the run.
- Forwarded flags override the matching `.loopai/config` keys carried over in Step 4 (`executor`, `task_model`, `review_model`, `external_reviewers`); a flag not given leaves the config value in force.

## Step 1: Choose the Plan

- With `PLAN_ARG` set, validate that it exists.
- Otherwise list `ls -t docs/plans/*.md` (newest first, `completed/` excluded) and offer up to four as a numbered list, newest marked recommended.
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` relative to `ROOT` (`docs/plans/20260828-feature.md`) and `STEM` as its filename without `.md` - the date stays, since the progress file is named from it.

## Step 2: Derive the Worktree Name

`NAME` is the plan filename without `.md` and without any leading run of digits and dashes (`20260904-20-feature.md` gives `feature`) - the same derivation loopai uses for its own branches (`plan.ExtractBranchName`):

```bash
NAME=$(basename "$PLAN" .md | sed -E 's/^[0-9-]+//')
git branch --all --list "$NAME" "*/$NAME"                    # must print nothing
orca worktree list --repo "path:$ROOT" --json                # no entry with displayName == NAME, archived included
```

An existing branch or Orca worktree with that name means an earlier run needing close-out: stop and report it. If `--repo path:` is rejected, use the `id:<repoId>` fallback from Step 3.

## Step 3: Create the Orca Worktree

```bash
orca worktree create \
  --repo "path:$ROOT" \
  --name "$NAME" \
  --base-branch "$BASE" \
  --no-parent \
  --comment "loopai: $PLAN" \
  --activate \
  --json
```

- `--base-branch "$BASE"` is load-bearing: Orca's default base for `--no-parent` work is the repo's configured base ref, typically the remote-tracking branch, which can be far behind the local branch the user is looking at.
- If `--repo path:` is rejected, resolve the repo id read-only with `orca worktree list --json` - take `repoId` from the entry whose `path` equals `$ROOT` and `isMainWorktree` is true - and pass `--repo id:<repoId>`. If the repository is not listed at all, tell the user to add it in Orca first and stop.
- From the JSON take `result.worktree.id` (`WT_ID`, the full `<repoId>::<path>` value) and `result.worktree.path` (`WT_PATH`). Every later selector is `id:$WT_ID` verbatim; a bare repo id or a `path:` selector does not reliably resolve a child worktree.
- Orca names the branch itself, typically `<git-user>/<NAME>`. Read it from `result.worktree.branch` and strip `refs/heads/` before showing it; do not assume it equals `NAME`.
- Confirm the base: `git -C "$WT_PATH" rev-parse HEAD` must equal `git rev-parse "$BASE"`. On mismatch remove the worktree (`orca worktree rm --worktree "id:$WT_ID" --json`), report both hashes, and stop - launching on a stale base produces work against the wrong code.
- Repo setup hooks and configured default tabs may open extra terminals; leave them alone.

## Step 4: Carry Over Untracked Inputs

A worktree contains committed files only. Copy what loopai needs but git does not carry:

```bash
mkdir -p "$WT_PATH/$(dirname "$PLAN")" && cp "$ROOT/$PLAN" "$WT_PATH/$PLAN"
for d in config prompts agents; do
  [ -e "$ROOT/.loopai/$d" ] && [ ! -e "$WT_PATH/.loopai/$d" ] && mkdir -p "$WT_PATH/.loopai" && cp -R "$ROOT/.loopai/$d" "$WT_PATH/.loopai/$d"
done
```

- The plan is copied, not committed on `BASE`: loopai's task prompt stages the plan file itself, so it lands on the feature branch exactly as under `loopai --worktree`. An already-tracked plan is overwritten with identical content, harmlessly.
- `.loopai/config`, `prompts/`, and `agents/` are project-local overrides read from the current directory; kept untracked, they would otherwise silently fall back to global defaults inside the new checkout.

## Step 5: Launch loopai in a Tab

`orca worktree create --agent` accepts only Orca's built-in TUI agents, and loopai is not one, so the launch is the documented two-step fallback:

```bash
orca terminal create \
  --worktree "id:$WT_ID" \
  --title "loopai $NAME" \
  --command "$(command -v loopai) --orca $FLAGS '$PLAN'" \
  --json
```

- `$FLAGS` goes between `--orca` and the plan, unquoted; the plan stays last and single-quoted. With no flags the command is `loopai --orca '<plan>'`.
- Use the absolute binary path: the tab runs Orca's own login shell, whose `PATH` may lack `~/.local/bin`.
- No `--worktree` on loopai: it would create a second, nested checkout under `.loopai/worktrees/`. The tab is already on a non-default branch, so loopai runs there directly.
- No pipes or `tee`: `--orca` writes OSC titles only when stdout is a terminal, and that title is what Orca reads for card status.
- Save `result.terminal.handle` as `HANDLE`. Step 3 may also have opened a fallback shell tab; leave it alone unless `orca terminal show` confirms it is idle and unused.

## Step 6: Confirm and Report

```bash
orca terminal wait --terminal "$HANDLE" --for exit --timeout-ms 10000 --json   # a timeout here is GOOD
orca terminal read --terminal "$HANDLE" --screen --json                        # result.terminal.tail
```

If the terminal exited within 10s, loopai failed at startup: read the tail, report the error verbatim, and stop. Otherwise the tail must show loopai's startup header (`Plan:` / `Branch:`) or `--- task iteration 1 ---`.

```
loopai started in Orca.

Worktree: $WT_PATH  (id: $WT_ID)
Branch:   <branch without refs/heads/>   cut from $BASE
Plan:     $PLAN
Flags:    $FLAGS  (empty = executor/models/reviewers from .loopai/config and defaults)
Terminal: $HANDLE  (tab "loopai $NAME")
Progress: $WT_PATH/.loopai/progress/progress-$STEM.txt

Monitoring:
  orca terminal read --terminal $HANDLE --screen --json
  orca terminal wait --terminal $HANDLE --for exit --timeout-ms 7200000 --json
  tail -f <progress file>

The Orca card follows the tab title loopai emits.
Ask "check loopai" for a status update.
```

**Stop here.** Do not monitor automatically.

## Step 7: Status Check (only when asked)

1. `orca terminal show --terminal "$HANDLE" --json` - the title carries the phase.
2. `orca terminal read --terminal "$HANDLE" --screen --json` plus `tail -40` of the progress file.
3. A tail ending in a shell prompt means loopai exited; the last title says how - `done`, `failed`, or a bare `loopai` for a run stopped without finishing.

Then stop.

## Close-out (tell the user, do not run)

Recommend `$loopai-merge <plan>` to narrate the completion report and choose merge, PR, or cancel before closing out.

From the main checkout, `loopai --merge $PLAN` (or `--pr $PLAN`) finds the Orca branch through the progress record loopai wrote in the Orca worktree, merges it, and removes the git worktree. Orca still lists the card; `orca worktree rm --worktree "id:$WT_ID" --json` clears it. After the merge, the untracked plan copy left in `$ROOT/docs/plans/` is shadowed by the merged `completed/` copy and can be deleted.

## Pitfalls

| Symptom | Cause | Fix |
|---------|-------|-----|
| Worktree HEAD behind the local branch | `--base-branch` omitted | Always pass it and verify hashes (Step 3) |
| loopai: plan file not found | Plan uncommitted, absent from the fresh checkout | Step 4 copy |
| `selector_not_found` on a child worktree | `path:` or bare repo id used | Use `id:$WT_ID` verbatim |
| Card stays "working" after loopai finished | Output piped, titles suppressed | Run without `tee` or pipes |
| Two nested worktrees | `loopai --worktree` inside an Orca worktree | Drop the flag |
| Stops on an unknown flag | Only the four flags pass through | Put other settings in `.loopai/config` |
| Unexpected default models or prompts | Untracked `.loopai/` overrides not carried | Step 4 loop |
| `ANTHROPIC_API_KEY` not picked up | Tab inherits the login shell, not this one | Export it in the shell profile |
