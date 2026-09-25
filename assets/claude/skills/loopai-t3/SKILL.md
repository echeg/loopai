---
name: loopai-t3
description: "Use when the user wants to run an existing loopai plan inside a T3 Code-managed worktree and thread (T3 Code desktop, web, or mobile app, t3.codes), so the run shows up as a T3 Code thread whose title follows the loopai phase and whose terminal shows the live output, optionally with explicit per-phase model or external-reviewer overrides. Triggers: loopai-t3, run plan in t3, launch loopai in t3 code, запусти план в t3."
argument-hint: '[plan file] [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]'
allowed-tools: [Bash, Read, Glob, AskUserQuestion]
---

# loopai-t3 - Run a Plan in a T3 Code Thread

**SCOPE**: This command ONLY runs `loopai --t3-launch`, which asks the running T3 Code server to create a worktree and a thread for the plan and starts `loopai --t3` in that thread's terminal, then reports how to follow it. Do NOT edit code, commit, merge, or take any other action. T3 Code owns the worktree and the thread; loopai runs inside them as an ordinary process.

## Step 0: Preflight

Run the checks separately so each failure is distinguishable. **Stop and report the failing check verbatim; do not continue past a failure.**

```bash
which loopai                                                   # missing -> stop, suggest: make build && install -m 0755 .bin/loopai ~/.local/bin/loopai
loopai --help | grep -qE -- '(--|/)t3-launch' || echo "MISSING_T3_FLAG"   # printed -> stop: fork too old, needs the --t3-launch build
T3_HOME=${T3CODE_HOME:-$HOME/.t3}; cat "$T3_HOME/userdata/server-runtime.json"   # missing -> stop: T3 Code is not running
git rev-parse --show-toplevel                                   # must equal the current directory
git branch --show-current
```

- The runtime file only proves a server started; a stale one is left behind by a crash. `--t3-launch` reports a refused connection itself, so do not probe the port separately.
- The repository must already be a project in T3 Code (added through its sidebar). `--t3-launch` stops with `no T3 Code project for <root>` otherwise; relay that and stop.
- On a detached HEAD the worktree is cut from the current commit; mention that in the report.

## Step 0b: Parse Arguments

Split `$ARGUMENTS` on whitespace. The first token that does not start with `--` is `PLAN_ARG` (may be absent). Every other token must be one of the pass-through flags below; collect them, in the order given, into `FLAGS` (one space-separated string, empty when no flags were given):

| Flag | Form | Value |
|------|------|-------|
| `--task-model` | `--task-model SPEC` or `--task-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--review-model` | `--review-model SPEC` or `--review-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--external-reviewers` | `--external-reviewers LIST` or `--external-reviewers=LIST` | one token: comma-separated `provider[:model[:effort]]` entries |

- `--task-model` and `--review-model` values must start with a provider: `claude` or `codex`, alone or followed by `:model[:effort]` (`codex:gpt-6-astra:medium`, `claude:opus:high`, `codex::medium` for the codex default model). `--t3-launch` rejects a bare `opus:high`; stop here instead and suggest the prefixed spelling.
- Every value must match `^[A-Za-z0-9._:,+-]+$`; `--t3-launch` rejects anything else too.
- Any token not in the table — `--worktree`, `--commit`, `--serve`, `--plan`, `--branch`, a second plan path, anything else — a flag given twice, or a value-taking flag without a value **stops the run**. Report the offending token verbatim and state that only the three flags above are passed through. `--codex` was removed from loopai: for it, also say to write `--task-model codex:<model>[:effort]` instead.

## Step 1: Choose the Plan

- If `PLAN_ARG` is set: validate it exists with Read.
- Otherwise Glob `docs/plans/*.md` (excludes `completed/`), reverse to newest-first, and AskUserQuestion with up to 4 plans, newest marked "(Recommended)".
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` as the path relative to the repository root.

## Step 2: Resolve the Token

loopai reads the T3 Code bearer token only from `LOOPAI_T3_TOKEN` and never mints one itself.

- If `LOOPAI_T3_TOKEN` is already set in this shell, use it.
- Otherwise the token is minted inline in Step 3 with the `t3` CLI, so it never appears in this transcript. When `t3` is not on `PATH` (the desktop app does not install it), tell the user the launch will run `npx --yes t3@latest` once to issue the token, and continue only after they agree.

The token lives in the new thread terminal's environment for the whole run; `--ttl 7d` keeps it valid for long plans.

## Step 3: Launch

```bash
T3_CLI=$(command -v t3 >/dev/null 2>&1 && echo t3 || echo "npx --yes t3@latest")
LOOPAI_T3_TOKEN=${LOOPAI_T3_TOKEN:-$($T3_CLI auth session issue --token-only --ttl 7d --label loopai)} \
  loopai --t3-launch $FLAGS "$PLAN"
```

- Run it from the repository root. `--t3-launch` exits after typing the command into the thread terminal; it does not wait for the run.
- Never echo, log, or print the token.
- Success prints `started loopai in T3 Code thread <id>`, then `worktree:`, `branch:`, and the close-out line. Keep all of them for the report.
- A failure after the worktree was created ends with `already created: worktree <path> (branch <name>)[, thread <id>]`. Nothing is rolled back; relay the line verbatim so the user can remove the worktree in T3 Code or retry after fixing the cause.

## Step 4: Report

```
loopai started in T3 Code.

Thread:   <id>        (open T3 Code; the thread title follows the run)
Worktree: <path>
Branch:   <branch>
Plan:     $PLAN
Flags:    $FLAGS  (empty = phase models and reviewers from .loopai/config and defaults)
Progress: <worktree>/.loopai/progress/progress-<plan stem>.txt

The thread title shows "<plan> · task N/M", "<plan> · review · iteration N",
"<plan> · waiting for input" or "· waiting for limit", and finally
"<plan> · done", "· failed", or "· stopped". The thread's "loopai" terminal
shows the live output on desktop, web, and mobile.
```

**STOP HERE after reporting. Do not monitor automatically.**

## Close-out (tell the user, do not run)

From this checkout, prefer `/loopai-merge $PLAN`, or run the printed `loopai --merge <branch>` / `loopai --pr <branch>`. With `t3 = true` in `.loopai/config` (or `--t3` on the command) and `LOOPAI_T3_TOKEN` set, `--pr` links the new pull request to the thread, and T3 Code settles the thread once the PR merges. The T3-managed worktree stays until it is removed in T3 Code.

## Pitfalls

| Symptom | Cause | Fix |
|---------|-------|-----|
| `no T3 Code project for <root>` | Repository not added to T3 Code | Add it in the T3 Code sidebar, then rerun |
| `LOOPAI_T3_TOKEN is not set` | Token step skipped | Step 2/3 inline mint |
| `HTTP 401` | Token revoked, expired, or minted for another T3 home | Mint a new one with the same `T3CODE_HOME` the server uses |
| Thread title never changes | Run started without `--t3` or the token expired mid-run | Relaunch; titles are best-effort and never stop the run |
| loopai: plan file not found | Plan outside the repository | Move it under the repository |
| Skill stops on an unknown flag | Only three flags pass through | Put other settings in `.loopai/config` |
