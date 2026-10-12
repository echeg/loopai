# Run in the Current T3 Codex Thread

Use this mode only with the active T3 Codex runtime and native collaboration tools described in `SKILL.md`. The native observer's continued activity keeps the parent thread in **Working** after the parent replies. An ordinary background process or a state file alone does not do that. The thread keeps its existing provider; loopai's task/review providers remain independent.

## Preflight and Existing Runs

1. Confirm `loopai` and Python 3 are available, the current directory is the repository root, and `loopai --help` includes `--no-t3`, `--worktree`, and `--skip-finalize`. Missing support is a launch failure: report it and suggest `$loopai-update`. This is a capability check, separate from the non-blocking skill-contract warning. Also check for `--t3-progress-thread`; without it the run can proceed with ordinary Working status, but progress titles are unavailable until loopai is updated. The optional title integration requires a matching local T3 runtime; missing identity/authentication must not block execution.
2. Use the shared flag/history and plan selection in `SKILL.md`. Preserve the selected plan path as one argument, including spaces, and model/reviewer options as an argument array. Do not choose different models for the observer or the run. Check an existing observer before insisting the original plan still exists: loopai may already have moved it into `completed/`.
3. Resolve this skill's `scripts/observe.py` relative to the `SKILL.md` actually loaded. Its read-only `--describe` command prints the deterministic run directory for a repository/plan pair:

   ```bash
   python3 "$OBSERVER_SCRIPT" --repo "$REPO" --plan "$PLAN" --describe
   ```

   Inspect `state.json` there and use native agent listing/messaging to find its `observer_id`. If one observer or CLI run is still active, reuse it; never start a second execution in response to a status question, a new user message, or context loss. The helper refuses an existing run directory rather than overwriting its outcome. Also inspect the matching ordinary progress file and live processes before adopting a plan previously launched without this helper; loopai's worktree/progress locks remain the final guard against competing runs.
4. Ordinary `--worktree` runs from the current committed HEAD and copies an untracked or edited selected plan into the feature worktree. Unrelated source edits are warned about and are not carried into that worktree. Config and prompt overrides are loaded from the source checkout by loopai. Relay worktree/preflight failures; do not add `--commit`, stash changes, or switch branches to bypass them. Git operations already in progress may prevent launch.

## Resolve the Parent Thread for Progress Titles

Before spawning the observer, run the bundled `scripts/resolve-thread.py` **in the parent Codex session**. It uses the parent's `CODEX_THREAD_ID` to find one exact Codex binding in the local T3 database, opened read-only. Resolve the helper relative to the `SKILL.md` actually loaded, just like `observe.py`:

```bash
python3 "$THREAD_RESOLVER_SCRIPT"
```

On success stdout contains only the T3 thread ID. Keep that ID in the parent context and form the progress argument array `["--t3-progress-thread", "<resolved ID>"]`. Pass this array to the observer explicitly: its own `CODEX_THREAD_ID` identifies the child, so it must not repeat the lookup or replace the parent ID.

The resolver reads `${T3CODE_HOME:-$HOME/.t3}/userdata/server-runtime.json` and its sibling `state.sqlite`. `--t3-home <path>` selects a known T3 home, and `--codex-thread-id <id>` allows an explicitly captured parent identity; do not search other databases or select by project, title, recency, or a single visible thread. When using an explicit T3 home, pass the same `T3CODE_HOME` environment value to loopai. An inherited `LOOPAI_T3_URL` must match this runtime, even with an explicit token: a local parent ID must not be sent to another server. A missing, unsupported, or ambiguous binding means titles are unavailable: state this briefly and use `["--no-t3"]` instead. Use that fallback also when `--t3-progress-thread` is unsupported. Never pass both flags or fall back to `--t3`/a new thread.

`--t3-progress-thread` verifies the existing thread through the server API and updates only its title. It suppresses ordinary T3 reporting/thread creation. Authentication uses an existing `LOOPAI_T3_TOKEN`, or loopai issues a local seven-day token through the official `t3` CLI (with `npx` fallback) and retains it in memory. Do not reuse `T3_MCP_BEARER_TOKEN`, print tokens, or put credentials in argv, state files, or observer messages. Token or title-update failure leaves execution and native Working intact.

The Working badge keeps its native label. Progress appears as a prefix on the original title, for example `[loopai: task 3/7] Original title`, then `[loopai: review · iteration 1] Original title`, and finally a done/failed/stopped prefix. A retry replaces the previous loopai prefix rather than stacking it. This does not switch the conversation's provider or start another T3 thread.

## Delegate One Observer

Send **one native subagent** the repository root, exact plan path, validated flag argument array, resolved progress argument array, helper path, this reference, and its ownership instructions below. Use its native task name or returned agent ID as `observer_id`. If the tool only gives an ID after creation, send that ID in a follow-up message before it launches. Keep the returned native agent ID in the parent context as well.

The observer runs the helper as a **foreground command in its own tool session**, using the tool's working-directory argument and properly quoted argv:

```bash
python3 "$OBSERVER_SCRIPT" --repo "$REPO" --plan "$PLAN" --observer-id "$OBSERVER_ID" -- \
  loopai --worktree --skip-finalize [resolved progress arguments] [validated model/reviewer arguments] "$PLAN"
```

The bracketed flags are placeholders for the argument arrays, not literal shell syntax. Progress arguments are exactly `--t3-progress-thread <parent T3 ID>` or the fallback `--no-t3`. Do not construct a command with `eval` or interpolate unescaped user text. The helper calls the command without a shell.

- Both progress choices suppress ordinary T3 reporting even with `t3 = true` or inherited T3 identity/token variables. `--t3-progress-thread` targets only the supplied existing thread; `--no-t3` additionally disables title updates.
- `--skip-finalize` leaves review and merge to the user in this original thread, even if config enables automatic PR creation/merge. Ordinary CLI mode does not perform ACP auto-merge. Do not silently remove this flag; an explicit request for automatic close-out is a different launch choice.
- Do not use `--t3-launch`, `--t3`, ACP, `nohup`, `&`, or a terminal tab for this mode. Do not have the parent launch another copy. The observer operates the CLI; it does not implement the plan itself or spawn additional observers.

The observer's task must say:

> Start the command once and remain active until its actual CLI exit is recorded. Poll the yielded execution session with tool waits of at most 60 seconds, reading only bounded progress/log excerpts as needed. A timeout or a temporarily quiet log is not completion. Do not finish your turn while the CLI is running. Send the parent the native observer ID, state/log paths, CLI PID, and actual progress header once available; only then say that the run has started. Report an early startup failure instead. Do not blindly restart or retry. On exit, inspect state, the final log, and the completion report; send a concise result and then finish. Do not merge, open a PR, or settle/archive the T3 thread.

The helper creates an ignored directory under the source checkout's `.loopai/progress/` containing atomic `state.json` and complete `output.log`. State records the exact argv, repository/plan, observer identity, wrapper PID, CLI PID, timestamps, and actual exit code. loopai's ordinary progress file also stays in the source checkout; read its `Plan:`, `Worktree plan:`, and `Branch:` headers to get actual paths instead of guessing the worktree or branch.

## Parent Handoff

Confirm the observer's start message or the helper's `running` state and progress header using a short status read. A spawned observer alone does not prove that loopai started. If startup is still pending, report that accurately.

Tell the user the plan, native observer ID, progress/state paths, whether title progress is available, and that work stays in this thread with manual merge afterward. Then **finish the parent response**; do not join the whole run with `wait_agent`, poll indefinitely, or keep a parent tool cell alive. The observer's activity supplies background **Working**. Normal follow-up messages go to the original Codex and do not, by themselves, cancel the CLI.

When the observer finishes, background **Working** clears. This is not an automatic `Settled` action. Its result is available to the parent, but the tested harness does **not guarantee an unsolicited parent reply after completion**. Do not promise one. On the next user message, read the saved result and completion report and continue here.

## Status, Completion, and Recovery

- For a status question, read `state.json` and a bounded tail of `output.log`/the progress file, or ask the existing observer. Keep the ongoing run intact.
- Treat `completed` as a zero CLI exit, then inspect the actual completion report and final log before claiming the plan succeeded: some graceful cancellations can return zero. A nonzero exit is failure or interruption, never success. Use `loopai --report "$PLAN_STEM"` from the source checkout for the durable report; it works after loopai removes its temporary worktree. Report absent/failed report retrieval honestly. On successful plan execution, say it is ready for review/merge and use `$loopai-merge` here when requested.
- If the native observer disappears but the wrapper/CLI is still active, create one replacement **watcher only**. It reads the existing state/log and polls the recorded process; it must not rerun the helper or loopai. Keep it active until terminal state is recorded, or until both recorded processes are confirmed gone. If there is no terminal state, report **unknown/interrupted**, not success. A PID by itself can be reused: cross-check the process command and start information with the saved record.
- Stopping is explicit: have the observer send `SIGINT` to the verified helper PID, which forwards it to the CLI process group, then keep observing until exit. If the wrapper is gone, signal only the verified existing CLI process/group. Do not use native agent interruption as a substitute for stopping loopai; otherwise the process can outlive the observer and lose the thread's Working status.
- A terminal record is not permission to run again. Show its result. Only an explicit retry/resume request authorizes another launch; first verify the old observer and processes are finished, archive its whole run directory to a sibling path so the prior log/outcome survives, then invoke one new observer. Never remove or overwrite a live/unknown record to force a retry.

Closing the app or losing the Codex harness can remove the native observer even if the CLI process survives. The state/log aid recovery; they do not promise uninterrupted Working status across host shutdowns.
