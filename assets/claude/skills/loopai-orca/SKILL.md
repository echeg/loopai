---
name: loopai-orca
description: "Use when the user wants to run an existing loopai plan inside an Orca-managed worktree and terminal tab (Orca desktop app, onorca.dev), so the run shows up as an Orca card with live status. Triggers: loopai-orca, run plan in orca, launch loopai in orca worktree, запусти план в orca."
argument-hint: 'optional plan file path'
allowed-tools: [Bash, Read, Glob, AskUserQuestion]
---

# loopai-orca - Run a Plan in an Orca Worktree

**SCOPE**: This command ONLY creates an Orca-managed worktree, launches `loopai --orca` in a terminal tab there, and reports how to monitor it. Do NOT edit code, commit, merge, or take any other action. Orca owns the worktree, the branch, and the tab; loopai runs inside them as an ordinary process.

Every `orca` call below must use `--json`; read fields from the JSON, never from prose output. If a flag below is rejected, check `orca <command> --help` (or `orca skills get orca-cli` for the full guide) before improvising — the flag surface changes between Orca releases.

## Step 0: Preflight

Run the checks separately so each failure is distinguishable. **Stop and report the failing check verbatim; do not continue past a failure.**

```bash
which loopai                              # missing -> stop, suggest: make build && install -m 0755 .bin/loopai ~/.local/bin/loopai
loopai --help | grep -q -- '--orca' || echo "MISSING_ORCA_FLAG"   # printed -> stop: fork too old, needs the --orca build
orca status --json                        # result.runtime.state must be "ready"; anything else -> stop
git rev-parse --show-toplevel
git branch --show-current
```

- Resolve the Orca executable the way Orca's own `orca-cli` skill does: use `$ORCA_CLI_COMMAND` when set; on Linux outside an Orca terminal use `orca-ide`, never bare `orca` (it is the GNOME screen reader there); otherwise `orca`. Substitute it for `orca` in every command below.
- If HEAD is detached, ask the user which base branch to use and set `BASE` to their answer.
- Record `ROOT` (repository root) and `BASE` (current branch); the Orca worktree is cut from `BASE`.

## Step 1: Choose the Plan

- If `$ARGUMENTS` names a file: validate it exists with Read.
- Otherwise Glob `docs/plans/*.md` (excludes `completed/`), reverse to newest-first, and AskUserQuestion with up to 4 plans, newest marked "(Recommended)".
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` as the path relative to `ROOT` (for example `docs/plans/20260828-feature.md`) and `STEM` as its filename without `.md` (`20260828-feature` — the date stays; the progress file is named from it).

## Step 2: Derive the Worktree Name

`NAME` is the plan filename without the leading `YYYYMMDD-` date and without `.md` — the same derivation loopai uses for its own branches:

```bash
NAME=$(basename "$PLAN" .md | sed -E 's/^[0-9]{8}-//')
git branch --all --list "$NAME" "*/$NAME"                              # must print nothing
orca worktree list --repo "path:$ROOT" --json                          # no entry with displayName == NAME, archived (isArchived: true) included
```

If a branch or an Orca worktree with that name already exists, stop and report it: it means an earlier run that needs close-out first. If `--repo path:` is rejected here, use the same `id:<repoId>` fallback described in Step 3.

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

- `--base-branch "$BASE"` is load-bearing and deliberate: Orca's default base for `--no-parent` work is the repo's configured base ref (typically the remote-tracking branch), which can be far behind the local branch the user is looking at.
- If `--repo path:` is rejected, resolve the repo id read-only — `orca worktree list --json`, take `repoId` of the entry whose `path` equals `$ROOT` and `isMainWorktree` is true — and pass `--repo id:<repoId>`. If the repository is not listed at all, tell the user to add it in Orca first and stop.
- From the JSON take `result.worktree.id` (`WT_ID`, the full `<repoId>::<path>` value) and `result.worktree.path` (`WT_PATH`). Every later selector is `id:$WT_ID` verbatim; a bare repo id or a `path:` selector does not reliably resolve a child worktree.
- Orca names the branch itself, typically `<git-user>/<NAME>` (for example `echeg/feature`). Read it from `result.worktree.branch` and strip the `refs/heads/` prefix before showing it; do not assume it equals `NAME`.
- Confirm the base: `git -C "$WT_PATH" rev-parse HEAD` must equal `git rev-parse "$BASE"`. On mismatch, remove the worktree (`orca worktree rm --worktree "id:$WT_ID" --json`), report both hashes, and stop — launching loopai on a stale base produces work against the wrong code.
- Repo setup hooks and configured default tabs run per the repo's Orca settings and may open extra terminals; leave them alone.

## Step 4: Carry Over Untracked Inputs

A worktree contains committed files only. Copy what loopai needs but git does not carry:

```bash
mkdir -p "$WT_PATH/$(dirname "$PLAN")" && cp "$ROOT/$PLAN" "$WT_PATH/$PLAN"
for d in config prompts agents; do
  [ -e "$ROOT/.loopai/$d" ] && [ ! -e "$WT_PATH/.loopai/$d" ] && mkdir -p "$WT_PATH/.loopai" && cp -R "$ROOT/.loopai/$d" "$WT_PATH/.loopai/$d"
done
```

- The plan is copied, not committed on `BASE`: loopai's task prompt stages `{{PLAN_FILE}}` itself, so the plan lands on the feature branch exactly as it does under `loopai --worktree`. (If the plan is already tracked, the copy is a harmless overwrite with the same content.)
- `.loopai/config`, `prompts/`, and `agents/` are project-local overrides that loopai reads from the current directory; if the project keeps them untracked they would otherwise silently fall back to global defaults inside the new checkout.

## Step 5: Launch loopai in a Tab

`orca worktree create --agent` accepts only Orca's built-in TUI agents, and loopai is not one — so the launch is the documented two-step fallback: create the worktree, then create a terminal with an explicit command.

```bash
orca terminal create \
  --worktree "id:$WT_ID" \
  --title "loopai $NAME" \
  --command "$(command -v loopai) --orca '$PLAN'" \
  --json
```

- Absolute binary path: the tab runs Orca's own login shell, whose `PATH` may lack `~/.local/bin`.
- No `--worktree` flag on loopai: it would create a second, nested checkout under `.loopai/worktrees/` inside Orca's worktree. Because the tab is already on a non-default branch, loopai runs there directly.
- No pipes or `tee`: `--orca` writes OSC titles only when stdout is a terminal, and the title is what Orca reads for card status.
- Save `result.terminal.handle` as `HANDLE`. The bare `worktree create` in Step 3 may also have opened a fallback shell tab (find its handle with `orca terminal list --worktree "id:$WT_ID" --json` if needed); leave it alone unless `orca terminal show` confirms it is an idle shell nobody uses.

## Step 6: Confirm and Report

Give loopai a moment to start, then read the tab:

```bash
orca terminal wait --terminal "$HANDLE" --for exit --timeout-ms 10000 --json   # a timeout here is GOOD: loopai is still running
orca terminal read --terminal "$HANDLE" --screen --json                        # result.terminal.tail = rendered lines
```

- If `wait` reports the terminal exited within 10s, loopai failed at startup: read the tail, report the error verbatim, and stop.
- Otherwise the tail must show loopai's startup output (`Plan:` / `Branch:` header lines or a `--- task iteration 1 ---` section).

Report:

```
loopai started in Orca.

Worktree: $WT_PATH  (id: $WT_ID)
Branch:   <branch without refs/heads/>   cut from $BASE
Plan:     $PLAN
Terminal: $HANDLE  (tab "loopai $NAME")
Progress: $WT_PATH/.loopai/progress/progress-$STEM.txt

Monitoring:
  orca terminal read --terminal $HANDLE --screen --json        # what the tab shows now
  orca terminal wait --terminal $HANDLE --for exit --timeout-ms 7200000 --json
  tail -f <progress file>

The Orca card follows the tab title loopai emits: "◐ loopai · task N/M · <executor>"
shows as working, "loopai · waiting for input · <executor>" and
"loopai · waiting for limit · <executor>" show as needs-attention, and
"✳ loopai · done" / "✳ loopai · failed" show as idle; a bare "✳ loopai"
means the run was stopped or interrupted without finishing.
Ask "check loopai" for a status update.
```

**STOP HERE after reporting. Do not monitor automatically.**

## Step 7: Status Check (only on explicit request)

1. `orca terminal show --terminal "$HANDLE" --json` — the current title carries the phase (`◐ …` running, `waiting for …` blocked on input or a provider limit).
2. `orca terminal read --terminal "$HANDLE" --screen --json` and `tail -40` of the progress file for recent activity.
3. If the tail ends in a shell prompt, loopai exited. The last title tells how: `✳ loopai · done` success, `✳ loopai · failed` failure, bare `✳ loopai` means the run was stopped or interrupted without finishing.

After reporting, STOP.

## Close-out (tell the user, do not run)

From the main checkout `loopai --merge $PLAN` (or `--pr $PLAN`) finds the Orca branch through the progress record loopai wrote in the Orca worktree, merges it, and removes the git worktree. Orca still lists the card afterwards; `orca worktree rm --worktree "id:$WT_ID" --json` clears it. Alternatively close out entirely through Orca's own merge and archive flow. After the merge, the untracked plan copy left in `$ROOT/docs/plans/` is shadowed by the merged `completed/` copy and can be deleted.

## Pitfalls

| Symptom | Cause | Fix |
|---------|-------|-----|
| Worktree HEAD is behind the local branch | `--base-branch` omitted, Orca used its configured default base | Always pass `--base-branch "$BASE"` and verify hashes (Step 3) |
| loopai: plan file not found | Plan uncommitted, so absent from the fresh checkout | Step 4 copy |
| `selector_not_found` on a child worktree | `path:` or bare repo id used | Use `id:$WT_ID` verbatim |
| Card stays "working" after loopai finished | Output piped, titles suppressed | Run without `tee`/pipes |
| Two nested worktrees | `loopai --worktree` inside an Orca worktree | Drop the flag |
| Run uses default models/prompts unexpectedly | Untracked `.loopai/` overrides not carried over | Step 4 loop |
| `ANTHROPIC_API_KEY` not picked up | Tab inherits the login shell, not this terminal | Export it in the shell profile |
| Extra tabs appear on create | Repo setup hooks / default tabs ran per Orca settings | Expected; leave them alone |
