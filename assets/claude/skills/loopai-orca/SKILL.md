---
name: loopai-orca
description: "Use when the user wants to run an existing loopai plan inside an Orca-managed worktree and terminal tab (Orca desktop app, onorca.dev), so the run shows up as an Orca card with live status, optionally with explicit per-phase provider/model or external-reviewer overrides. Triggers: loopai-orca, run plan in orca, launch loopai in orca worktree, запусти план в orca."
argument-hint: '[plan file] [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]'
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

## Step 0b: Parse Arguments

Split `$ARGUMENTS` on whitespace. The first token that does not start with `--` is `PLAN_ARG` (may be absent). Every other token must be one of the pass-through flags below; collect them, in the order given, into `FLAGS` (one space-separated string, empty when no flags were given):

| Flag | Form | Value |
|------|------|-------|
| `--task-model` | `--task-model SPEC` or `--task-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--review-model` | `--review-model SPEC` or `--review-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--external-reviewers` | `--external-reviewers LIST` or `--external-reviewers=LIST` | one token: comma-separated `provider[:model[:effort]]` entries |

- `--task-model` and `--review-model` values must start with a provider: `claude` or `codex`, alone or followed by `:model[:effort]` (`codex:gpt-6-astra:medium`, `claude:opus:high`, `codex::medium` for the codex default model). loopai rejects a bare `opus:high` at startup, after the worktree and tab already exist, so stop here instead and suggest the prefixed spelling.
- Every value must match `^[A-Za-z0-9._:,+-]+$`. `FLAGS` is spliced into the `--command` string a shell executes in Step 5, so a value with whitespace, quotes, `$`, or `;` is rejected, not escaped.
- Any token not in the table — `--worktree`, `--commit`, `--cmux-workspace`, `--serve`, `--plan`, `--codex-args`, a second plan path, anything else — a flag given twice, or a value-taking flag without a value **stops the run**. Report the offending token verbatim and state that only the three flags above are passed through. `--codex` was removed from loopai: for it, also say to write `--task-model codex:<model>[:effort]` instead. Do not drop it silently and do not forward it: `--worktree` would nest a second checkout inside Orca's worktree and `--serve` would block the tab after the run.
- Forwarded flags override the matching keys in the `.loopai/config` carried over in Step 4 (`task_model`, `review_model`, `external_reviewers`); a flag not given leaves the config value in force.

**Recent flag combinations.** When `FLAGS` is empty, offer the combinations from earlier launches before launching. Skip this entirely when `FLAGS` is non-empty, and also when the `loopai-plan` skill invoked this skill in the current conversation after its own flag question, since the choice (possibly no flags) is already made there.

loopai itself writes `${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}/launch-history` on every plan-executing launch: one line per launch, newest first, `<UTC time>\t<launcher>\t<flags>`, where the launcher is `orca`, `t3`, or `cli`. The file holds flag strings only, never tokens or paths. Read it only; never write to it.

```bash
HIST="${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}/launch-history"
HISTORY=$(awk -F'\t' '
  function ok(s,   n, t, i, seen) {
    n = split(s, t, " ")
    if (n == 0 || n % 2) return 0
    for (i = 1; i <= n; i += 2) {
      if (t[i] !~ /^--(task-model|review-model|external-reviewers)$/ || seen[t[i]]++) return 0
      if (t[i+1] !~ /^[A-Za-z0-9._:,+-]+$/) return 0
      if (t[i] != "--external-reviewers" && t[i+1] !~ /^(claude|codex)(:|$)/) return 0
    }
    return 1
  }
  $2 ~ /^(orca|t3|cli)$/ && ok($3) && !dup[$3]++ { print $3; if (++k == 3) exit }
' "$HIST" 2>/dev/null)
```

`HISTORY` holds up to three distinct flag strings, newest first, one per line. A line the skill cannot validate is skipped, never offered: an unknown launcher, a token other than the three flags above (or one given twice), a value outside `^[A-Za-z0-9._:,+-]+$`, or a model spec without the `claude`/`codex` provider. A line with no flags is not listed. A missing, unreadable, or empty file yields an empty `HISTORY`; then launch with the empty `FLAGS` as before, without asking.

With `HISTORY` non-empty, ask AskUserQuestion — "Which loopai flags should the Orca run use?" — with one option per `HISTORY` entry labelled by its flags, newest first and the newest marked "(Recommended)"; then "No flags (models and reviewers from .loopai/config and loopai defaults)" and "Cancel". AskUserQuestion takes at most four options: when the list is longer, drop the oldest `HISTORY` entries until it fits. A selected entry becomes `FLAGS` exactly as listed, without the "(Recommended)" suffix; "No flags" leaves `FLAGS` empty; "Cancel" stops without launching. "Other" is the manual-entry path: parse the typed text with the rules above and repeat the question naming any rejected token verbatim.

## Step 1: Choose the Plan

- If `PLAN_ARG` is set: validate it exists with Read.
- Otherwise Glob `docs/plans/*.md` (excludes `completed/`), reverse to newest-first, and AskUserQuestion with up to 4 plans, newest marked "(Recommended)".
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` as the path relative to `ROOT` (for example `docs/plans/20260828-feature.md`) and `STEM` as its filename without `.md` (`20260828-feature` — the date stays; the progress file is named from it).

## Step 2: Derive the Worktree Name

`NAME` is the plan filename without `.md` and without any leading run of digits and dashes (`20260904-20-feature.md` → `feature`) — the same derivation loopai uses for its own branches (`plan.ExtractBranchName`):

```bash
NAME=$(basename "$PLAN" .md | sed -E 's/^[0-9-]+//')
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
- Save `result.startupTerminal.handle` as `SHELL_HANDLE` when the response carries it: bare `worktree create` opens a fallback shell in the first tab, and Step 6 closes it once loopai is running.
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
  --command "$(command -v loopai) --orca $FLAGS '$PLAN'" \
  --json
```

- `$FLAGS` from Step 0b goes between `--orca` and the plan, unquoted; the plan stays last and single-quoted. With no flags the command is `loopai --orca '<plan>'`.
- Absolute binary path: the tab runs Orca's own login shell, whose `PATH` may lack `~/.local/bin`.
- No `--worktree` flag on loopai: it would create a second, nested checkout under `.loopai/worktrees/` inside Orca's worktree. Because the tab is already on a non-default branch, loopai runs there directly.
- No pipes or `tee`: `--orca` writes OSC titles only when stdout is a terminal, and the title is what Orca reads for card status.
- Save `result.terminal.handle` as `HANDLE`. The fallback shell tab from Step 3 is closed in Step 6, after loopai is confirmed running; do not close it here.

## Step 6: Confirm and Report

Give loopai a moment to start, then read the tab:

```bash
orca terminal wait --terminal "$HANDLE" --for exit --timeout-ms 10000 --json   # a timeout here is GOOD: loopai is still running
orca terminal read --terminal "$HANDLE" --screen --json                        # result.terminal.tail = rendered lines
```

- If `wait` reports the terminal exited within 10s, loopai failed at startup: read the tail, report the error verbatim, and stop.
- Otherwise the tail must show loopai's startup output (`Plan:` / `Branch:` header lines or a `--- task iteration 1 ---` section).

Then close the fallback shell tab that the bare `worktree create` in Step 3 opened, so the card carries one tab:

```bash
orca terminal list --worktree "id:$WT_ID" --json                  # candidates = every handle except $HANDLE
orca terminal show --terminal "$SHELL_HANDLE" --json              # result.terminal.agentIdentity must be null
orca terminal read --terminal "$SHELL_HANDLE" --screen --json     # last non-empty result.terminal.tail line must be a shell prompt
orca terminal close --terminal "$SHELL_HANDLE" --tab --json
```

- `SHELL_HANDLE` is `result.startupTerminal.handle` from Step 3 when the create response carried it; otherwise it is the single handle other than `$HANDLE` that `terminal list` returns for `id:$WT_ID`. Two or more other handles mean repo setup hooks or configured default tabs ran: close nothing and mention the extra tabs in the report.
- Close only when both checks pass: `agentIdentity` is null (an agent tab carries `claude`, `codex`, or another id) and the screen ends in a shell prompt with no command running above it. A tab that fails either check belongs to the repo's Orca settings; leave it and mention it in the report.
- Run this only after the startup check above passed. A failed launch leaves every tab in place for the user to inspect.
- A `close` error is not a launch failure: report it in one line and continue to the report.

Report:

```
loopai started in Orca.

Worktree: $WT_PATH  (id: $WT_ID)
Branch:   <branch without refs/heads/>   cut from $BASE
Plan:     $PLAN
Flags:    $FLAGS  (empty = providers/models/reviewers from .loopai/config and defaults)
Terminal: $HANDLE  (tab "loopai $NAME")
Progress: $WT_PATH/.loopai/progress/progress-$STEM.txt

Monitoring:
  orca terminal read --terminal $HANDLE --screen --json        # what the tab shows now
  orca terminal wait --terminal $HANDLE --for exit --timeout-ms 7200000 --json
  tail -f <progress file>

The Orca card follows the tab title loopai emits: "◐ loopai · task N/M · <executor>"
shows as working, "loopai · waiting for input · <executor>" and
"loopai · waiting for limit · <executor>" show as needs-attention, and
"✳ loopai · done" / "✳ loopai · failed" show as idle (with finalize on, "done" is
followed by its outcome, such as "· PR merged"); a bare "✳ loopai"
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

With `finalize = pr` or `merge` in `.loopai/config` (the skill forwards only the model and reviewer flags), the run itself merges `origin/<base>` into the branch, opens the pull request, and under `merge` merges it once its checks pass; the title then ends in `done · PR opened` or `done · PR merged`. Close-out by hand is needed when finalize is off or `sync` (`done · synced`), or the title ends in `done · finalize incomplete` before a pull request opened; once the run printed `PR: <url>`, a stop leaves that pull request open to fix or merge on GitHub. Finalize never removes the Orca worktree, so `orca worktree rm --worktree "id:$WT_ID" --json` still clears the card.

Otherwise, from the main checkout, prefer `/loopai-merge $PLAN`: it reads and narrates the completion report, previews merge conflicts, then asks whether to merge (resolving predicted conflicts on the plan branch first), open a PR, or cancel. Its chosen `loopai --merge $PLAN` (or `--pr $PLAN`) command finds the Orca branch through the progress record loopai wrote in the Orca worktree; merge also removes the git worktree. Orca still lists the card afterwards; `orca worktree rm --worktree "id:$WT_ID" --json` clears it. Alternatively close out entirely through Orca's own merge and archive flow. After the merge, the untracked plan copy left in `$ROOT/docs/plans/` is shadowed by the merged `completed/` copy and can be deleted.

## Pitfalls

| Symptom | Cause | Fix |
|---------|-------|-----|
| Worktree HEAD is behind the local branch | `--base-branch` omitted, Orca used its configured default base | Always pass `--base-branch "$BASE"` and verify hashes (Step 3) |
| loopai: plan file not found | Plan uncommitted, so absent from the fresh checkout | Step 4 copy |
| `selector_not_found` on a child worktree | `path:` or bare repo id used | Use `id:$WT_ID` verbatim |
| Card stays "working" after loopai finished | Output piped, titles suppressed | Run without `tee`/pipes |
| Two nested worktrees | `loopai --worktree` inside an Orca worktree | Drop the flag |
| Skill stops on an unknown flag | only `--task-model`, `--review-model`, `--external-reviewers` pass through; `--codex` was removed, use `--task-model codex:<model>` | Put other settings in `.loopai/config`; never forward the token |
| Run uses default models/prompts unexpectedly | Untracked `.loopai/` overrides not carried over | Step 4 loop |
| `ANTHROPIC_API_KEY` not picked up | Tab inherits the login shell, not this terminal | Export it in the shell profile |
| Card shows "Terminal 1" beside the loopai tab | Fallback shell from bare `worktree create` was not closed | Step 6 close; only an idle shell with null `agentIdentity` qualifies |
| Extra tabs appear on create | Repo setup hooks / default tabs ran per Orca settings | Expected; leave them alone and mention them |
