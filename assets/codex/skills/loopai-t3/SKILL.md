---
name: loopai-t3
description: "Run an existing loopai plan in T3 Code. In an active Codex thread with native subagents, keep execution, Working status, and task/review progress in that thread; otherwise launch a separate T3-managed thread. Supports per-phase model and reviewer overrides. Use for loopai-t3, run plan in t3, or launch loopai in t3 code."
metadata:
  short-description: Run loopai in the current or a new T3 thread
---

# loopai-t3 - Run a Plan in a T3 Code Thread

**SCOPE**: launch an existing plan and report its status. The current-thread mode delegates ordinary CLI execution to one native observer; the new-thread mode uses `loopai --t3-launch` to start a provider session or terminal. loopai does the implementation work. Do not independently edit code, commit, or merge.

## Skill Contract Check

Run this first and read the printed line:

```bash
expected=3
if ! command -v loopai >/dev/null 2>&1; then
  actual=missing
else
  actual=$(loopai --skill-contract 2>/dev/null) || actual=0
  case $actual in ''|*[!0-9]*) actual=0 ;; esac
fi
echo "skill contract: skill=$expected binary=$actual"
```

- `binary=missing`: continue with this skill's existing handling of a missing CLI.
- `binary` lower than `skill`: tell the user "the installed loopai binary is older than these skills; run `$loopai-update`", then continue.
- `binary` higher than `skill`: tell the user "these skills are older than the installed loopai binary; run `$loopai-update`, then restart the Codex session", then continue.
- equal: continue silently.

Never stop on a mismatch.

## Choose the Thread Mode

- **Current thread (default inside T3 Codex):** the injected runtime identifies this as a T3 Code Codex conversation and the current tool inventory exposes native `collaboration.spawn_agent` / `spawn_agent` with live-agent inspection or messaging. Read [references/current-thread.md](references/current-thread.md). For status/resume/recovery, inspect its existing-run record before flag/history questions, plan selection, or any missing-plan rejection; only a new launch needs those shared selections. Before spawning a new observer, resolve the parent thread's identity as the reference describes so loopai can show task/review progress in its title. Then follow that reference instead of the new-thread preflight and Steps 2–4. A runtime file on disk alone does not prove that this conversation is inside T3, and a CLI-spawned model is not a native subagent.
- **New thread:** the user explicitly requests a separate/new thread, or this host lacks the current-thread capability and the user has not required staying here. Follow the existing new-thread steps below. This keeps provider/terminal launching available outside T3 Codex.
- If the user explicitly requires this thread but the necessary native tools or active T3 context are absent, report the missing capability; do not silently create a second thread or invent a tool. If current-thread launch fails, report the failure without switching modes.

## Step 0: Preflight

**New-thread mode only.** Current-thread preflight is in the linked reference.

Run the checks separately so each failure is distinguishable. **Stop and report the failing check verbatim; do not continue past a failure.**

```bash
command -v loopai                                              # missing -> stop: make install in the loopai checkout
loopai --help | grep -qE -- '(--|/)t3-launch' || echo "MISSING_T3_FLAG"   # printed -> stop: fork too old
T3_HOME=${T3CODE_HOME:-$HOME/.t3}; cat "$T3_HOME/userdata/server-runtime.json"   # missing -> stop: T3 Code is not running
git rev-parse --show-toplevel                                   # must equal the current directory
git branch --show-current
```

- A stale runtime file survives a crash; `--t3-launch` reports a refused connection itself, so do not probe the port separately.
- The repository must already be a T3 Code project (added through its sidebar). `--t3-launch` stops with `no T3 Code project for <root>` otherwise; relay that and stop.
- On a detached HEAD the worktree is cut from the current commit; mention that in the report.

## Step 0b: Parse Arguments

Parse flag/value pairs first; the one remaining positional argument is `PLAN_ARG` (may be absent). Preserve a quoted or explicitly supplied plan path with spaces as one argument. Every other argument must be one of the pass-through flags below; collect them in order as an argument array and use `FLAGS` for their space-separated display (empty when none were given):

| Flag | Form | Value |
|------|------|-------|
| `--task-model` | `--task-model SPEC` or `--task-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--review-model` | `--review-model SPEC` or `--review-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--external-reviewers` | `--external-reviewers LIST` or `=LIST` | one token: comma-separated `provider[:model[:effort]]` |

- `--task-model` and `--review-model` values must start with a provider: `claude` or `codex`, alone or followed by `:model[:effort]` (`codex:gpt-6-astra:medium`, `claude:opus:high`, `codex::medium` for the codex default model). `--t3-launch` rejects a bare `opus:high`; stop here instead and suggest the prefixed spelling.
- Every value must match `^[A-Za-z0-9._:,+-]+$`; `--t3-launch` rejects anything else too.
- Any other token (`--worktree`, `--commit`, `--serve`, `--plan`, `--branch`, a second plan path, anything else), a flag given twice, or a value-taking flag without a value **stops the run**. Report the offending token verbatim and state that only the three flags above pass through. `--codex` was removed from loopai: for it, also say to write `--task-model codex:<model>[:effort]` instead.

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

With `HISTORY` non-empty, ask "Which loopai flags should the T3 Code run use?" as a numbered list: one choice per `HISTORY` entry labelled by its flags, newest first and the newest marked "(Recommended)"; then "No flags (models and reviewers from .loopai/config and loopai defaults)" and "Cancel". A picked entry becomes `FLAGS` exactly as listed, without the "(Recommended)" suffix; "No flags" leaves `FLAGS` empty; "Cancel" stops without launching. Typing flags instead of a number is the manual-entry path: parse the typed text with the rules above and repeat the question naming any rejected token verbatim.

## Step 1: Choose the Plan

- For a current-thread status/resume request, inspect the existing run as described in the reference first; a completed run may already have archived the original plan. Do not turn a status question into a new launch.
- If `PLAN_ARG` is set, confirm the file exists.
- Otherwise list `ls -t docs/plans/*.md` (this excludes `completed/`), show up to four as a numbered list with the newest marked recommended, and ask the user to pick one.
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` as the path relative to the repository root.

For current-thread mode, continue with [references/current-thread.md](references/current-thread.md) now. The remaining steps concern a **new thread only**.

## Step 2: Resolve the Token

`--t3-launch` reads the T3 Code bearer token from `LOOPAI_T3_TOKEN`; the launcher does not mint it. After a local merge, loopai can separately issue a short-lived settlement token through the official CLI.

- If `LOOPAI_T3_TOKEN` is already set, use it.
- Otherwise the token is minted inline in Step 3 with the `t3` CLI, so it never appears in the conversation. When `t3` is not on `PATH` (the desktop app does not install it), tell the user the launch will run `npx --yes t3@latest` once to issue the token, and continue only after they agree.

In agent mode the token is used only for the launcher's dispatches; no token or environment is placed in the thread. In terminal mode the token lives in the thread terminal's environment for the whole run; `--ttl 7d` keeps it valid for long plans.

## Step 3: Launch

```bash
T3_CLI=$(command -v t3 >/dev/null 2>&1 && echo t3 || echo "npx --yes t3@latest")
LOOPAI_T3_TOKEN=${LOOPAI_T3_TOKEN:-$($T3_CLI auth session issue --token-only --ttl 7d --label loopai)} \
  loopai --t3-launch $FLAGS "$PLAN"
```

- Run it from the repository root. The bare `--t3-launch` uses `auto`: it reads `<T3 home>/userdata/settings.json` and selects agent mode when `providerInstances` contains an enabled `grok` instance whose `config.binaryPath` names `loopai-acp` or `loopai-acp.exe` (and `config.enabled` is not false). Otherwise it selects terminal mode; unreadable settings produce a warning and a terminal fallback. See `docs/t3-code.md` for setup.
- Agent mode submits the plan and flags as a user turn to the provider session and opens no terminal. Terminal mode types `loopai --t3` into the thread terminal. The launcher exits after submitting the turn or typing the command; it does not wait for the run.
- Agent mode rejects whitespace in the plan's relative path before creating a worktree; use terminal mode for such paths.
- Never echo, log, or print the token.
- Success prints `started loopai in T3 Code thread <id>`, then `mode: agent (...)` or `mode: terminal`, `worktree:`, `branch:`, and the close-out line. Read the effective mode from this output and keep all of these lines for the report.
- A failure after the worktree was created ends with `already created: worktree <path> (branch <name>)[, thread <id>]`. Nothing is rolled back; relay the line verbatim.

## Step 4: Report

Use the `mode:` line from Step 3 to select the report below. Do not infer the mode from local settings.

```
loopai started in T3 Code.

Thread:   <id>        (open T3 Code)
Mode:     <agent or terminal, from command output>
Worktree: <path>
Branch:   <branch>
Plan:     $PLAN
Flags:    $FLAGS  (empty = phase models and reviewers from .loopai/config and defaults)
Progress: <worktree>/.loopai/progress/progress-<plan stem>.txt

```

For `mode: agent`, append:

```
The thread runs loopai as a provider session. It shows the Working state, plan
steps, streamed reasoning, and the completion report as a final message on
desktop, web, and mobile. The stop button cancels the run. T3 Code controls the
thread title; loopai does not update it during the run.
```

For `mode: terminal`, append:

```
The thread title shows "<plan> · task N/M", "<plan> · review · iteration N",
"<plan> · waiting for input" or "· waiting for limit", and finally
"<plan> · done" (followed by the finalize outcome, such as "· PR merged",
when finalize is on), "· failed", or "· stopped". The thread's "loopai" terminal
shows the live output on desktop, web, and mobile.
```

**Stop after reporting. Do not monitor automatically.**

## Close-out (tell the user, do not run)

In agent mode, use the completion report to find the finalize outcome and any pull request URL. The ACP run disables loopai's T3 reporting, so phase/finalize titles and pull request linking described below apply only to terminal mode. Local merges settle matching threads in both modes: ACP waits until its final response is sent and the turn is idle, while `--merge` settles idle threads after successful cleanup. Settlement is best-effort and can issue its own short-lived token through the local T3 CLI; a new ACP message cancels a pending settlement. The T3-managed worktree remains in either mode. In agent mode, a successful run whose report rates Risk `low` or `medium` is merged into the local base branch by loopai itself, in the worktree where the base is checked out; the final message's `## Merge` section says `Merged ...` or `Not merged into <base>: <reason>`. Nothing is pushed, and the branch and the T3-managed worktree are kept. The thread stays usable after the run: ask about the report or request fixes there, and type `merge` or `pr` to merge the branch's current head into the local base or open a pull request without removing the worktree; an explicit `merge` skips the Risk and `acp_auto_merge` gates but keeps the repository gates. Close out by hand only when that section says `Not merged` and the thread cannot do it, and after a merge remove the branch and worktree through T3 Code rather than `loopai --merge`, which would try to remove the worktree the session runs in.

With `finalize = pr` or `merge` in `.loopai/config` (`--t3-launch` forwards only the model and reviewer flags), the run itself merges `origin/<base>` into the branch, opens the pull request, links it to the thread, and under `merge` merges it once its checks pass; the thread title then ends in `done · PR opened` or `done · PR merged`, and T3 Code settles the thread after the merge. Close-out by hand is needed when finalize is off or `sync` (`done · synced`), or the title ends in `done · finalize incomplete` before a pull request opened; once the run printed `PR: <url>`, a stop leaves that pull request open to fix or merge on GitHub. Then, from this checkout, prefer `$loopai-merge $PLAN`, or run the printed `loopai --merge <branch>` / `loopai --pr <branch>`. With `t3 = true` in `.loopai/config` (or `--t3` on the command) and `LOOPAI_T3_TOKEN` set, `--pr` links the new pull request to the thread, and T3 Code settles the thread once the PR merges. The T3-managed worktree stays until it is removed in T3 Code; finalize never removes it.

## Pitfalls

| Symptom | Cause | Fix |
|---------|-------|-----|
| `no T3 Code project for <root>` | Repository not added to T3 Code | Add it in the T3 Code sidebar, then rerun |
| `LOOPAI_T3_TOKEN is not set` | Token step skipped | Step 2/3 inline mint |
| `HTTP 401` | Token revoked, expired, or minted for another T3 home | Mint a new one with the same `T3CODE_HOME` the server uses |
| Need terminal output or a plan path with whitespace | Agent mode opens no terminal and rejects whitespace paths | Run the Step 3 command with `--t3-launch=terminal` instead of the bare flag; it ignores provider settings |
| Thread title never changes in terminal mode | Run started without `--t3` or the token expired mid-run | Relaunch; titles are best-effort and never stop the run |
| Skill stops on an unknown flag | Only three flags pass through | Put other settings in `.loopai/config` |
