---
name: loopai-t3
description: "Use when the user wants to run an existing loopai plan from a T3 Code thread (T3 Code desktop, web, or mobile app, t3.codes): as a detached run this thread follows in a terminal tab with a [loopai: ...] title prefix, or in a new loopai thread that shows Working for the whole run, optionally with explicit per-phase model or external-reviewer overrides. Triggers: loopai-t3, run plan in t3, launch loopai in t3 code, запусти план в t3."
argument-hint: '[plan file] [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]'
allowed-tools: [Bash, Read, Glob, AskUserQuestion, ToolSearch, mcp__t3-code__orchestrator_capabilities, mcp__t3-code__t3_thread_list, mcp__t3-code__t3_thread_launch]
---

# loopai-t3 - Run a Plan from a T3 Code Thread

**SCOPE**: This command ONLY starts one loopai plan run that T3 Code shows, in one of two modes, and reports how to follow it. Mode 1 runs `loopai --detach --t3-thread` from this thread: the run gets its own process and loopai worktree, and this thread follows it in a terminal tab and a `[loopai: ...]` title prefix while its agent stays free for conversation. Mode 2 calls `t3_thread_launch` to open a new thread on the loopai provider, which starts the same detached run and shows Working until it ends. Do NOT edit code, commit, merge, or take any other action; loopai does the work.

## Skill Contract Check

Run this first and read the printed line:

```bash
expected=4
if ! command -v loopai >/dev/null 2>&1; then
  actual=missing
else
  actual=$(loopai --skill-contract 2>/dev/null) || actual=0
  case $actual in ''|*[!0-9]*) actual=0 ;; esac
fi
echo "skill contract: skill=$expected binary=$actual"
```

- `binary=missing`: continue with this skill's existing handling of a missing CLI.
- `binary` lower than `skill`: tell the user "the installed loopai binary is older than these skills; run `/loopai:loopai-update`", then continue.
- `binary` higher than `skill`: tell the user "these skills are older than the installed loopai binary; run `/loopai:loopai-update`, then `/reload-plugins` or restart the session", then continue.
- equal: continue silently.

Never stop on a mismatch.

## Step 0: Preflight

Run the checks separately so each failure is distinguishable. **Stop and report the failing check verbatim; do not continue past a failure.**

```bash
which loopai                                                   # missing -> stop, suggest: make install in the loopai checkout
loopai --help | grep -qE -- '(--|/)detach' || echo "MISSING_DETACH_FLAG"   # printed -> stop: loopai too old, needs the --detach build
git rev-parse --show-toplevel                                   # must equal the current directory
git branch --show-current
```

- On a detached HEAD the run's worktree is cut from the current commit; mention that in the report.

## Step 0b: Parse Arguments

Split `$ARGUMENTS` on whitespace. The first token that does not start with `--` is `PLAN_ARG` (may be absent). Every other token must be one of the pass-through flags below; collect them, in the order given, into `FLAGS` (one space-separated string, empty when no flags were given):

| Flag | Form | Value |
|------|------|-------|
| `--task-model` | `--task-model SPEC` or `--task-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--review-model` | `--review-model SPEC` or `--review-model=SPEC` | one token: `provider[:model[:effort]]` |
| `--external-reviewers` | `--external-reviewers LIST` or `--external-reviewers=LIST` | one token: comma-separated `provider[:model[:effort]]` entries |

- `--task-model` and `--review-model` values must start with a provider: `claude` or `codex`, alone or followed by `:model[:effort]` (`codex:gpt-6-astra:medium`, `claude:opus:high`, `codex::medium` for the codex default model). `loopai --detach` and the loopai provider reject a bare `opus:high`; stop here instead and suggest the prefixed spelling.
- Every value must match `^[A-Za-z0-9._:,+-]+$`; `loopai --detach` and the loopai provider reject anything else too.
- Any token not in the table — `--worktree`, `--commit`, `--serve`, `--plan`, `--branch`, a second plan path, anything else — a flag given twice, or a value-taking flag without a value **stops the run**. Report the offending token verbatim and state that only the three flags above are passed through. `--codex` was removed from loopai: for it, also say to write `--task-model codex:<model>[:effort]` instead.

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
  $(2) ~ /^(orca|t3|cli)$/ && ok($(3)) && !dup[$(3)]++ { print $(3); if (++k == 3) exit }
' "$HIST" 2>/dev/null)
```

`HISTORY` holds up to three distinct flag strings, newest first, one per line. A line the skill cannot validate is skipped, never offered: an unknown launcher, a token other than the three flags above (or one given twice), a value outside `^[A-Za-z0-9._:,+-]+$`, or a model spec without the `claude`/`codex` provider. A line with no flags is not listed. A missing, unreadable, or empty file yields an empty `HISTORY`; then launch with the empty `FLAGS` as before, without asking (the mode question in Step 3 still follows).

With `HISTORY` non-empty, ask AskUserQuestion — "Which loopai flags should the T3 Code run use?" — with one option per `HISTORY` entry labelled by its flags, newest first and the newest marked "(Recommended)"; then "No flags (models and reviewers from .loopai/config and loopai defaults)" and "Cancel". AskUserQuestion takes at most four options: when the list is longer, drop the oldest `HISTORY` entries until it fits. A selected entry becomes `FLAGS` exactly as listed, without the "(Recommended)" suffix; "No flags" leaves `FLAGS` empty; "Cancel" stops without launching. "Other" is the manual-entry path: parse the typed text with the rules above and repeat the question naming any rejected token verbatim.

## Step 1: Choose the Plan

- If `PLAN_ARG` is set: validate it exists with Read.
- Otherwise Glob `docs/plans/*.md` (excludes `completed/`), reverse to newest-first, and AskUserQuestion with up to 4 plans, newest marked "(Recommended)".
- Refuse a plan under `docs/plans/completed/` or under `.loopai/`.

Keep `PLAN` as the path relative to the repository root.

## Step 2: Read the T3 Context

The T3 Code tools are named `mcp__t3-code__<tool>` here and are often deferred: load them with ToolSearch (`select:mcp__t3-code__orchestrator_capabilities,mcp__t3-code__t3_thread_list,mcp__t3-code__t3_thread_launch`), or call them directly, before concluding that T3 Code is absent. When neither works, stop: this skill runs only inside a T3 Code thread.

1. Call `mcp__t3-code__orchestrator_capabilities` with `{}`. Read:
   - `parentThreadId`: null means this conversation is not a T3 thread; stop and say so;
   - `runtimeMode` and `interactionMode` of this thread;
   - `providers[]`: each `providerInstanceId`, `driverKind`, `displayName` (may be null), `models[].id` (read only the id; `label` and `options` are optional), and `constraints[]`.
2. Call `mcp__t3-code__t3_thread_list` with `{"limit": 1}` and read `currentThreadId`.
3. Run the environment check:

```bash
echo "os=$(uname -s)"
sandbox=""
if [ -n "${CODEX_SANDBOX:-}" ]; then sandbox="$sandbox CODEX_SANDBOX=$CODEX_SANDBOX"; fi
if [ "${CODEX_SANDBOX_NETWORK_DISABLED:-}" = 1 ]; then sandbox="$sandbox CODEX_SANDBOX_NETWORK_DISABLED=1"; fi
if [ "${SANDBOX_RUNTIME:-}" = 1 ]; then sandbox="$sandbox SANDBOX_RUNTIME=1"; fi
echo "sandbox=${sandbox# }"
```

**Mode 1 gate.** Mode 1 is offered only when all of these hold; otherwise name the first failed condition when asking the mode question:

- `runtimeMode` is `full-access`: the run, its token mint, and the terminal bridge need an unsandboxed shell;
- `sandbox=` is empty: `loopai --detach` refuses to start inside an agent sandbox, which it detects exactly as this check does (a non-empty `CODEX_SANDBOX`, `CODEX_SANDBOX_NETWORK_DISABLED=1`, or `SANDBOX_RUNTIME=1`), because every model process of a 40-hour run would inherit it;
- `os=` is not Windows (`MINGW*`, `MSYS*`, `CYGWIN*`, `Windows_NT`): `loopai --detach` is refused on Windows;
- `parentThreadId` equals `currentThreadId`, so the terminal goes to this thread and no other;
- the thread id matches `^[A-Za-z0-9_.:%-]{1,1024}$`. T3 thread ids are opaque and not always UUIDs (subagent ids carry `:` and `%`); validate before writing the id into any command, and always single-quote it there.

Mode 2 has no gate here; Step 5 checks the loopai provider.

## Step 3: Choose the Mode

Ask AskUserQuestion — "How should T3 Code show this loopai run?" — with these options, in this order:

- "Terminal in this thread": Mode 1. Only when the Mode 1 gate passed; otherwise leave the option out and name the failed condition in the question text.
- "New loopai thread (shows Working)": Mode 2.
- "Cancel": stop without launching.

Ask this every time, including when the `loopai-plan` skill invoked this skill after its own flag question: the flags were chosen there, the mode was not.

## Step 4: Mode 1 - Terminal in This Thread

The run holds a T3 token in memory for its whole life, so it can re-attach its terminal after a T3 restart. The token is minted inline, so it never reaches this transcript, with the four least-privilege scopes and a 3-day lifetime. It comes from the T3 CLI that ships with T3 Code (`T3CODE_CLI_PATH`, else `<T3 home>/bin/t3`), never from a `t3` on `PATH`, which may be a stale release writing into a frozen database, and never from `npx`. A `LOOPAI_T3_TOKEN` already set in this shell is used as is.

Before running, replace `<THREAD_ID>` with the validated `parentThreadId`, `<FLAGS>` with the Step 0b flag tokens written out literally (nothing when `FLAGS` is empty), and `<PLAN>` with `PLAN`. Refuse a plan path containing a single quote. Run the block as one **foreground** Bash call — never `run_in_background` — with a timeout of at least 180000 ms: the launcher waits up to 120 s for the run to confirm its start, and the default Bash timeout would cut it off at that same moment.

```bash
THREAD_ID='<THREAD_ID>'
T3_CLI="${T3CODE_CLI_PATH:-${T3CODE_HOME:-$HOME/.t3}/bin/t3}"
if [ ! -x "$T3_CLI" ]; then
  echo "T3_CLI_MISSING: $T3_CLI"
else
  LOOPAI_T3_TOKEN=${LOOPAI_T3_TOKEN:-$("$T3_CLI" auth session issue --scope orchestration:read --scope orchestration:operate --scope terminal:read --scope terminal:operate --ttl 3d --label loopai --token-only)} \
    loopai --detach --t3-thread "$THREAD_ID" <FLAGS> '<PLAN>'
fi
```

- Never echo, log, or print the token.
- `T3_CLI_MISSING` -> stop: T3 Code's own CLI is not installed at that path; set `T3CODE_CLI_PATH` to it or offer Mode 2 instead.
- Success prints `run: <run-id>` and `follow: loopai --attach <run-id>`; the launcher then exits while the run continues on its own. Keep the run id for the report.
- Launching a plan whose latest run failed, crashed, or was stopped resumes that run (same id, next attempt) with this fresh token. A live run is refused with `run <id> of <plan> is already <status>; follow it with loopai --attach <id>`: relay it and report that run instead of retrying.
- `--t3-thread needs LOOPAI_T3_TOKEN` means the mint printed nothing: relay the mint's error.
- `refusing to start a detached run inside an agent sandbox` means the gate missed a sandbox: offer Mode 2.
- Any other failure: relay the output verbatim, including the console log path the launcher names.

## Step 5: Mode 2 - New loopai Thread

When `interactionMode` is `plan`, stop and ask the user to leave plan mode first. Refuse a plan path containing whitespace: the new thread receives the plan and flags as one message split on whitespace.

**Find the loopai provider instance.** Read T3's settings read-only; never edit `settings.json` yourself, because a schema error there silently resets T3's settings:

```bash
python3 -I - "${T3CODE_HOME:-$HOME/.t3}/userdata/settings.json" <<'PY'
import json
import sys


def usable(inst):
    if not isinstance(inst, dict) or inst.get("driver") != "acpRegistry" or inst.get("enabled") is False:
        return False
    cfg = inst.get("config")
    if not isinstance(cfg, dict) or cfg.get("source") != "local" or cfg.get("enabled") is False:
        return False
    path, args = cfg.get("commandPath"), cfg.get("commandArgs")
    if not isinstance(path, str):
        return False
    base = path.replace("\\", "/").rsplit("/", 1)[-1]
    if base in ("loopai-acp", "loopai-acp.exe"):
        return True
    return base in ("loopai", "loopai.exe") and isinstance(args, list) and "--acp" in args


try:
    with open(sys.argv[1], encoding="utf-8") as f:
        settings = json.load(f)
except FileNotFoundError:
    settings = {}
except (OSError, ValueError) as err:
    print(f"instance=unreadable ({type(err).__name__})")
    sys.exit(0)
instances = settings.get("providerInstances") if isinstance(settings, dict) else None
found = [key for key in sorted(instances) if usable(instances[key])] if isinstance(instances, dict) else []
print(f"instance={found[0]}" if found else "instance=none")
PY
```

The lookup takes, in sorted key order, the first `providerInstances.<id>` with `driver` `acpRegistry`, `config.source` `local`, neither `enabled` nor `config.enabled` false, and a `config.commandPath` whose basename is `loopai-acp` or `loopai-acp.exe`, or `loopai` or `loopai.exe` with `--acp` in `config.commandArgs`. It never assumes the instance id (T3's default is `acpRegistry_local_acp`) and skips keys and values of unexpected types.

- `instance=<id>`: find the `providers[]` entry whose `providerInstanceId` is `<id>`. It must exist and have empty `constraints`; otherwise relay what T3 reports (for example a provider still being probed or failing its probe) and stop. Take `MODEL` = that entry's `models[0].id`, which is `default` before T3's first probe.
- `instance=none`: the loopai provider is not registered. Explain the registration and stop:
  1. T3 Settings → Providers → Add provider → ACP Registry;
  2. Instance ID: the default is fine (this skill never assumes it);
  3. Display name `loopai`;
  4. ACP source `Local command`;
  5. Executable override: the absolute path of `loopai-acp` (the output of `command -v loopai-acp`), or the absolute path of `loopai` with the argument `--acp` added in the Arguments editor;
  6. Add instance, then run this skill again.
- `instance=unreadable (...)`: relay it and stop; do not try to repair the file.

**Choose the workspace.** The new thread runs where this checkout is:

```bash
TOP=$(git rev-parse --show-toplevel)
GIT_DIR=$(cd "$(git rev-parse --git-dir)" && pwd -P)
COMMON_DIR=$(cd "$(git rev-parse --git-common-dir)" && pwd -P)
if [ "$GIT_DIR" = "$COMMON_DIR" ]; then echo "workspace=root"; else echo "workspace=existing_worktree $TOP"; fi
```

`workspace=root` becomes `{"type": "root"}`; `workspace=existing_worktree <path>` (a linked worktree) becomes `{"type": "existing_worktree", "worktreePath": "<path>"}`.

**Launch.** Call `mcp__t3-code__t3_thread_launch` with:

```json
{
  "title": "loopai: <plan stem>",
  "modelSelection": {"instanceId": "<id>", "model": "<MODEL>"},
  "message": "<PLAN> <FLAGS>",
  "workspaceStrategy": {"type": "root"}
}
```

- `title` is required; `<plan stem>` is the plan's file name without `.md`.
- `message` is the plan path, followed by the Step 0b flag tokens when `FLAGS` is non-empty. The provider starts `loopai --detach` of that plan in the new thread's checkout.
- Omit `runtimeMode` and `interactionMode`: the new thread inherits this thread's modes and may not be broader. T3 refuses the launch from a read-only thread; then relay the refusal and suggest changing this thread's access mode.
- The result carries `threadId`, `projectId`, `modelSelection`, `runId`, and `status`. Keep `threadId` and the title for the report.
- There is no idempotency key. After an error or a lost reply, call `mcp__t3-code__t3_thread_list` with `{"titleContains": "loopai: <plan stem>"}` before retrying, and report a thread this launch already created instead of launching a second one.

## Step 6: Report

Report the block for the mode that ran, with the placeholders filled in, plus any detached-HEAD note from Step 0.

Mode 1:

```text
loopai run started: <run-id>
follow: loopai --attach <run-id>
Progress shows in this thread's terminal tab and as the [loopai: …] title prefix.
If the run fails or is stopped, ask me to run the same plan again in this mode: it resumes the run with a fresh token.
```

Mode 2:

```text
loopai thread started: [<title>](t3-thread://v1/<threadId>)
The thread shows Working until the run ends. After Stop or a failure, type `resume` in that thread.
After the report, switch that thread's model to Claude or Codex to talk about the result.
```

**STOP HERE after reporting. Do not monitor automatically.**

Later questions about the run are answered from `loopai --runs` (status of this checkout's detached runs); `loopai --attach <run-id>` follows a run until it ends, and `loopai --stop <run-id>` stops it and keeps its worktree. A succeeded run's branch is merged with `/loopai:loopai-merge` or `loopai --merge <branch>`.
