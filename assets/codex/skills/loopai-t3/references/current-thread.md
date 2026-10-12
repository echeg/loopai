# Run in the Current T3 Codex Thread

Use this mode only with the active T3 Codex runtime and native collaboration tools described in `SKILL.md`. The native observer's continued activity keeps the parent thread in **Working** after the parent replies. An ordinary background process or a state file alone does not do that. The thread keeps its existing provider; loopai's task/review providers remain independent.

## Preflight and Existing Runs

1. Confirm `loopai` and Python 3 are available, the current directory is the repository root, and `loopai --help` includes `--no-t3`, `--worktree`, and `--skip-finalize`. Missing support is a launch failure: report it and suggest `$loopai-update`. This is a capability check, separate from the non-blocking skill-contract warning. No T3 bearer token, server-runtime file, project registration, or provider configuration is required for this mode.
2. Use the shared flag/history and plan selection in `SKILL.md`. Preserve the selected plan path as one argument, including spaces, and model/reviewer options as an argument array. Do not choose different models for the observer or the run. Check an existing observer before insisting the original plan still exists: loopai may already have moved it into `completed/`.
3. Resolve this skill's `scripts/observe.py` relative to the `SKILL.md` actually loaded. Its read-only `--describe` command prints the deterministic run directory for a repository/plan pair:

   ```bash
   python3 "$OBSERVER_SCRIPT" --repo "$REPO" --plan "$PLAN" --describe
   ```

   Inspect `state.json` there and use native agent listing/messaging to find its `observer_id`. If one observer or CLI run is still active, reuse it; never start a second execution in response to a status question, a new user message, or context loss. The helper refuses an existing run directory rather than overwriting its outcome. Also inspect the matching ordinary progress file and live processes before adopting a plan previously launched without this helper; loopai's worktree/progress locks remain the final guard against competing runs.
4. Ordinary `--worktree` runs from the current committed HEAD and copies an untracked or edited selected plan into the feature worktree. Unrelated source edits are warned about and are not carried into that worktree. Config and prompt overrides are loaded from the source checkout by loopai. Relay worktree/preflight failures; do not add `--commit`, stash changes, or switch branches to bypass them. Git operations already in progress may prevent launch.

## Delegate One Observer

Send **one native subagent** the repository root, exact plan path, validated flag argument array, helper path, this reference, and its ownership instructions below. Use its native task name or returned agent ID as `observer_id`. If the tool only gives an ID after creation, send that ID in a follow-up message before it launches. Keep the returned native agent ID in the parent context as well.

The observer runs the helper as a **foreground command in its own tool session**, using the tool's working-directory argument and properly quoted argv:

```bash
python3 "$OBSERVER_SCRIPT" --repo "$REPO" --plan "$PLAN" --observer-id "$OBSERVER_ID" -- \
  loopai --worktree --no-t3 --skip-finalize [validated model/reviewer arguments] "$PLAN"
```

The bracketed flags are a placeholder for the validated argument array, not literal shell syntax. Do not construct a command with `eval` or interpolate unescaped user text. The helper calls the command without a shell.

- `--no-t3` overrides even `t3 = true` or inherited T3 identity/token variables. It prevents an extra reporting thread, provider switching, and title changes to this conversation.
- `--skip-finalize` leaves review and merge to the user in this original thread, even if config enables automatic PR creation/merge. Ordinary CLI mode does not perform ACP auto-merge. Do not silently remove this flag; an explicit request for automatic close-out is a different launch choice.
- Do not use `--t3-launch`, `--t3`, ACP, `nohup`, `&`, or a terminal tab for this mode. Do not have the parent launch another copy. The observer operates the CLI; it does not implement the plan itself or spawn additional observers.

The observer's task must say:

> Start the command once and remain active until its actual CLI exit is recorded. Poll the yielded execution session with tool waits of at most 60 seconds, reading only bounded progress/log excerpts as needed. A timeout or a temporarily quiet log is not completion. Do not finish your turn while the CLI is running. Send the parent the native observer ID, state/log paths, CLI PID, and actual progress header once available; only then say that the run has started. Report an early startup failure instead. Do not blindly restart or retry. On exit, inspect state, the final log, and the completion report; send a concise result and then finish. Do not merge, open a PR, or settle/archive the T3 thread.

The helper creates an ignored directory under the source checkout's `.loopai/progress/` containing atomic `state.json` and complete `output.log`. State records the exact argv, repository/plan, observer identity, wrapper PID, CLI PID, timestamps, and actual exit code. loopai's ordinary progress file also stays in the source checkout; read its `Plan:`, `Worktree plan:`, and `Branch:` headers to get actual paths instead of guessing the worktree or branch.

## Parent Handoff

Confirm the observer's start message or the helper's `running` state and progress header using a short status read. A spawned observer alone does not prove that loopai started. If startup is still pending, report that accurately.

Tell the user the plan, native observer ID, progress/state paths, and that work stays in this thread with manual merge afterward. Then **finish the parent response**; do not join the whole run with `wait_agent`, poll indefinitely, or keep a parent tool cell alive. The observer's activity supplies background **Working**. Normal follow-up messages go to the original Codex and do not, by themselves, cancel the CLI.

When the observer finishes, background **Working** clears. This is not an automatic `Settled` action. Its result is available to the parent, but the tested harness does **not guarantee an unsolicited parent reply after completion**. Do not promise one. On the next user message, read the saved result and completion report and continue here.

## Status, Completion, and Recovery

- For a status question, read `state.json` and a bounded tail of `output.log`/the progress file, or ask the existing observer. Keep the ongoing run intact.
- Treat `completed` as a zero CLI exit, then inspect the actual completion report and final log before claiming the plan succeeded: some graceful cancellations can return zero. A nonzero exit is failure or interruption, never success. Use `loopai --report "$PLAN_STEM"` from the source checkout for the durable report; it works after loopai removes its temporary worktree. Report absent/failed report retrieval honestly. On successful plan execution, say it is ready for review/merge and use `$loopai-merge` here when requested.
- If the native observer disappears but the wrapper/CLI is still active, create one replacement **watcher only**. It reads the existing state/log and polls the recorded process; it must not rerun the helper or loopai. Keep it active until terminal state is recorded, or until both recorded processes are confirmed gone. If there is no terminal state, report **unknown/interrupted**, not success. A PID by itself can be reused: cross-check the process command and start information with the saved record.
- Stopping is explicit: have the observer send `SIGINT` to the verified helper PID, which forwards it to the CLI process group, then keep observing until exit. If the wrapper is gone, signal only the verified existing CLI process/group. Do not use native agent interruption as a substitute for stopping loopai; otherwise the process can outlive the observer and lose the thread's Working status.
- A terminal record is not permission to run again. Show its result. Only an explicit retry/resume request authorizes another launch; first verify the old observer and processes are finished, archive its whole run directory to a sibling path so the prior log/outcome survives, then invoke one new observer. Never remove or overwrite a live/unknown record to force a retry.

Closing the app or losing the Codex harness can remove the native observer even if the CLI process survives. The state/log aid recovery; they do not promise uninterrupted Working status across host shutdowns.
