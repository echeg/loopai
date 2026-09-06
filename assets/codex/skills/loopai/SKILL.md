---
name: loopai
description: Launch an autonomous loopai plan run and report its status. Use when the user asks to run, execute, resume, or check a loopai plan.
metadata:
  short-description: Run and monitor loopai
---

# loopai - Autonomous Plan Execution

**SCOPE**: launch loopai, confirm it started, and report status when asked. Do not edit code, commit, open PRs, or make suggestions beyond status reporting. loopai does the implementation work itself; this session only operates the process.

## Step 0: Verify the CLI

```bash
command -v loopai
```

If it is missing, this personal fork installs from source:

```bash
git clone https://github.com/echeg/loopai
cd loopai
make build
install -d ~/.local/bin
install -m 0755 .bin/loopai ~/.local/bin/loopai
```

`~/.local/bin` must be on `PATH`. Do not proceed until `command -v loopai` succeeds.

## Step 1: Choose the Mode

Ask one question at a time and wait for the answer. Present options as a numbered list and accept a number in reply:

1. **Full** (recommended) - tasks, internal reviews, external review chain, finalize.
2. **Review** - skip tasks, run the configured review pipeline (`--review`).
3. **External-only** - start at the external review chain (`--external-only`).

## Step 2: Choose the Plan

```bash
ls -t docs/plans/*.md            # newest first; completed/ is excluded
```

- Full mode **requires** a plan. Offer up to four newest paths, newest marked recommended.
- Review and external-only modes make it optional. Offer the same list plus a "no plan - review the existing diff" choice, searching `docs/plans/completed/` too when the user wants context from finished work.
- When the user passed a plan path in the request, validate it exists and skip the question.

## Step 3: Choose the Iteration Cap

Ask, defaulting to 50: `25` for short plans, `50` for most, `100` for large ones. Passed as `--max-iterations N`.

## Step 4: Launch

Do **not** add `--codex` or model flags on your own. The executor, models, and reviewer chain come from `.loopai/config` and the global config; hardcoding a flag here silently overrides the user's setup. Add a flag only when the user asked for it.

```bash
nohup loopai [--review|--external-only] [--max-iterations N] [plan-file] >/dev/null 2>&1 &
echo $!
```

Record the printed PID. Derive the progress file from mode and plan, where `<stem>` is the plan filename without `.md`:

| Mode | With a plan | Without a plan |
|------|-------------|----------------|
| full | `.loopai/progress/progress-<stem>.txt` | `.loopai/progress/progress.txt` |
| `--review` | `.loopai/progress/progress-<stem>-review.txt` | `.loopai/progress/progress-review.txt` |
| `--external-only` | `.loopai/progress/progress-<stem>-codex.txt` | `.loopai/progress/progress-codex.txt` |

## Step 5: Confirm the Start

Wait 10-15 seconds, then `tail -20 <progress file>` and look for the `Plan:`, `Branch:`, and `Started:` header lines. Report:

```
loopai started. PID: <pid>

Plan:     <from the progress header>
Branch:   <from the progress header>
Mode:     <selected mode>
Progress: <progress file>

Monitoring:
  tail -f <progress file>
  tail -50 <progress file>

The run is autonomous and can take hours; it survives this conversation ending.
Ask "check loopai" for a status update.
```

**Stop here.** Do not poll on your own.

## Step 6: Status Check (only when asked)

Only when the user explicitly asks ("check loopai", "loopai status"):

1. `kill -0 <pid> 2>/dev/null && echo running || echo exited`
2. `tail -40 <progress file>`

While running, name the current phase from the log: `task iteration N` is task execution, `review pass 1/2` is internal review, `codex iteration N` is the external review chain. Show the recent lines.

Once exited, read the final lines for the outcome and report success or failure. Then stop.

## Constraints

- Only launching and monitoring. No code, commits, PRs, or unsolicited advice.
- Never retry a failed run silently; report the failing phase and the error verbatim.
- Preserve a dirty working tree. Without `--worktree`, and when HEAD is already the plan's feature branch, loopai commits in the user's own checkout with no clean-tree gate - warn before launching if the tree holds unrelated work.
