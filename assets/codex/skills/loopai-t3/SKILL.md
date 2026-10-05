---
name: loopai-t3
description: "Run an existing loopai plan inside a T3 Code-managed worktree and thread (T3 Code desktop, web, or mobile app, t3.codes) so the run appears as a T3 Code thread that runs loopai as a provider session when configured, or shows live output in a terminal otherwise, optionally with per-phase model or external-reviewer overrides. Triggers: loopai-t3, run plan in t3, launch loopai in t3 code."
metadata:
  short-description: Run a loopai plan in a T3 Code thread
---

# loopai-t3 - Run a Plan in a T3 Code Thread

**SCOPE**: run `loopai --t3-launch`, which asks the running T3 Code server to create a worktree and a thread for the plan and starts a loopai provider session when a usable instance is configured, or `loopai --t3` in that thread's terminal otherwise, then report how to follow it. Do not edit code, commit, or merge. T3 Code owns the worktree and the thread; loopai runs inside them as a provider session or an ordinary terminal process.

## Step 0: Preflight

Run the checks separately so each failure is distinguishable. **Stop and report the failing check verbatim; do not continue past a failure.**

```bash
command -v loopai                                              # missing -> stop: make build && install -m 0755 .bin/loopai ~/.local/bin/loopai
loopai --help | grep -qE -- '(--|/)t3-launch' || echo "MISSING_T3_FLAG"   # printed -> stop: fork too old
T3_HOME=${T3CODE_HOME:-$HOME/.t3}; cat "$T3_HOME/userdata/server-runtime.json"   # missing -> stop: T3 Code is not running
git rev-parse --show-toplevel                                   # must equal the current directory
git branch --show-current
```

- A stale runtime file survives a crash; `--t3-launch` reports a refused connection itself, so do not probe the port separately.
- The repository must already be a T3 Code project (added through its sidebar). `--t3-launch` stops with `no T3 Code project for <root>` otherwise; relay that and stop.
- On a detached HEAD the worktree is cut from the current commit; mention that in the report.

## Step 0b: Parse Arguments

Split the request on whitespace. The first token that does not start with `--` is `PLAN_ARG` (may be absent). Every other token must be one of the pass-through flags below; collect them, in order, into `FLAGS` (one space-separated string, empty when none were given):

| Flag | Form | Value |
|------|------|-------|
| `--task-model` | `--task-model SPEC` or `--task-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--review-model` | `--review-model SPEC` or `--review-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--external-reviewers` | `--external-reviewers LIST` or `=LIST` | one token: comma-separated `provider[:model[:effort]]` |

- `--task-model` and `--review-model` values must start with a provider: `claude` or `codex`, alone or followed by `:model[:effort]` (`codex:gpt-6-astra:medium`, `claude:opus:high`, `codex::medium` for the codex default model). `--t3-launch` rejects a bare `opus:high`; stop here instead and suggest the prefixed spelling.
- Every value must match `^[A-Za-z0-9._:,+-]+$`; `--t3-launch` rejects anything else too.
- Any other token (`--worktree`, `--commit`, `--serve`, `--plan`, `--branch`, a second plan path, anything else), a flag given twice, or a value-taking flag without a value **stops the run**. Report the offending token verbatim and state that only the three flags above pass through. `--codex` was removed from loopai: for it, also say to write `--task-model codex:<model>[:effort]` instead.

## Step 1: Choose the Plan

- If `PLAN_ARG` is set, confirm the file exists.
- Otherwise list `ls -t docs/plans/*.md` (this excludes `completed/`), show up to four as a numbered list with the newest marked recommended, and ask the user to pick one.
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` as the path relative to the repository root.

## Step 2: Resolve the Token

loopai reads the T3 Code bearer token only from `LOOPAI_T3_TOKEN` and never mints one itself.

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

In agent mode, use the completion report to find the finalize outcome and any pull request URL. The ACP run disables loopai's T3 reporting, so phase/finalize titles, pull request linking, and thread settlement described below apply only to terminal mode. The T3-managed worktree remains in either mode.

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
