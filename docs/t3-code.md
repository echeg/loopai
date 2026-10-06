# T3 Code integration

[T3 Code](https://t3.codes) is a control surface for coding agents with desktop, web, and mobile
clients. loopai integrates with it through T3 Code's public, authenticated HTTP and WebSocket
APIs. No T3 Code fork or plugin is involved, and loopai never writes below the T3 home directory:
it only reads the server's runtime and settings files and changes everything else through the API. An
[experimental provider mode](#experimental-loopai-as-a-t3-code-provider) instead lets T3 Code run
loopai as a provider session through its Grok driver.

The integration is best-effort. When the server is not running, the token is missing or rejected,
or a title-reporting request fails or times out, loopai prints one warning and disables reporting
without affecting execution. Pinning and unpinning are best-effort; their failures are ignored silently.

## Setup

1. Run T3 Code (the desktop app or `t3`) and add the repository as a project in its sidebar.
2. Issue a bearer token and export it in the shell that starts loopai:

   ```bash
   export LOOPAI_T3_TOKEN=$(t3 auth session issue --token-only --ttl 7d --label loopai)
   ```

   The desktop app does not install the `t3` command; `npx --yes t3@latest auth session issue ...`
   works too. The token is issued into the T3 home the command resolves, so use the same
   `T3CODE_HOME` as the server.

loopai finds the server through `<T3 home>/userdata/server-runtime.json`, where the T3 home is
`T3CODE_HOME` or `~/.t3`. `LOOPAI_T3_URL` overrides the origin, for example when the server
listens on a non-default address. The token is read only from `LOOPAI_T3_TOKEN`; there is no
config key for it, and loopai never issues one itself.

## Thread status: `--t3`

Pass `--t3`, set `t3 = true` in `.loopai/config`, or set `LOOPAI_T3=1` to report a run as a T3
Code thread. At startup loopai creates a thread in the project whose workspace root is the
repository, named after the plan, and updates its title when the phase changes:

| loopai state | Thread title |
|---|---|
| Task phase | `<plan> · task 3/7` |
| Internal review | `<plan> · review · iteration 2` |
| External review | `<plan> · external review · iteration 1` |
| External evaluation | `<plan> · external eval` |
| Finalize / report | `<plan> · finalize`, `<plan> · report` |
| Waiting for input or a provider limit | `<plan> · waiting for input`, `<plan> · waiting for limit` |
| Finished | `<plan> · done`, `<plan> · failed`; with finalize, `<plan> · done · PR merged` (also `synced`, `PR opened`, `finalize incomplete`) |
| Stopped before completion | `<plan> · stopped` |

`<plan>` is the plan filename without its date prefix, or `loopai` for review-only runs. The thread
records the branch; it also records the checkout path unless loopai runs with `--worktree`, whose
checkout is removed after a successful run. A thread created this way starts no agent session in
T3 Code. Titles change only on phase and task transitions, never per output line.

The thread is pinned while the run is active, so it stays at the top of the sidebar, and unpinned
after the final title. A thread you pinned yourself keeps its pin. Pinning is cosmetic: a server
that rejects it changes nothing else.

`LOOPAI_T3_THREAD_ID` makes the run update an existing thread instead of creating one; the
launcher below sets it.

## Launching a plan in T3 Code: `--t3-launch`

```bash
loopai --t3-launch [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST] docs/plans/<plan>.md
```

`--t3-launch[=auto|agent|terminal]` starts a plan in a T3 Code-managed worktree and thread.
Attach a mode with `=`, since the plan path follows as a positional argument. The bare flag uses
`auto`: it selects agent mode when a usable loopai provider instance is configured, and terminal
mode otherwise.

The launcher reads `providerInstances` from `<T3 home>/userdata/settings.json`, where the home is
`T3CODE_HOME` or `~/.t3`. A usable instance has `driver: "grok"`, neither `enabled` nor
`config.enabled` set to false, and a `config.binaryPath` whose basename is `loopai-acp` or
`loopai-acp.exe` (case-insensitive). Missing enabled fields count as enabled. If several match,
the first settings key in sorted order wins; the output names the selected instance. See
[Adding the provider instance](#adding-the-provider-instance) for setup.

- `--t3-launch` or `--t3-launch=auto` selects agent mode when an instance matches. Missing settings
  or no match selects terminal mode; a settings read or parse error prints a warning and falls
  back to terminal mode.
- `--t3-launch=agent` requires a matching instance. Missing instances or invalid settings fail
  before creating a worktree or thread.
- `--t3-launch=terminal` skips the settings lookup and always starts the terminal run.

Run from the repository root, either mode:

1. asks T3 Code to create a worktree it manages (in `~/.t3/worktrees` by default) on a new branch
   named after the plan, cut from the current branch or, on a detached HEAD, the current commit,
   and verifies that the new worktree checks out the same commit;
2. copies the plan (so uncommitted edits carry over) and any `.loopai/config`, `prompts/`, and
   `agents/` files the worktree lacks;
3. creates a thread bound to that worktree and branch.

Agent mode then dispatches `thread.turn.start` with the selected instance, model `loopai`, and
`<plan relative path> [flags]` as the user message. T3 Code starts `loopai-acp` in the worktree.
The thread reads "Working", shows plan steps and streamed reasoning, and receives the completion
report as the final message. The stop button cancels the run. Agent mode opens no terminal and
places no token or environment in the thread: the bearer token is used only for the launcher's
own API requests. The initial title is the plain run name, matching the turn's `titleSeed` so
T3 Code can replace it with a generated title. If generation does not replace it, the run name
remains. loopai does not update the title during the ACP run.

Terminal mode opens a terminal named `loopai` in the thread and types `loopai --t3 [flags] <plan>`
there, with `LOOPAI_T3_TOKEN`, `LOOPAI_T3_URL`, and `LOOPAI_T3_THREAD_ID` in its environment. The
thread title follows phases and the live output is in the terminal; there is no provider session.

The launcher prints the thread id, effective `mode:`, worktree, and branch and exits without
waiting for the run. The worktree survives for review and close-out in both modes. Only the three
flags above are forwarded, and the two model specs need a `claude` or `codex` prefix; the rest comes
from `.loopai/config`. Agent mode uses forward slashes in the plan path and refuses paths containing
whitespace before creating anything; use `--t3-launch=terminal` for those paths. If a step fails
after the worktree exists, nothing is rolled back and the error lists what was already created.

The `loopai:loopai-t3` Claude Code skill and the `$loopai-t3` Codex skill wrap the bare flag, mint a
token inline when `LOOPAI_T3_TOKEN` is unset, and report the effective mode. `loopai-plan` offers
the launch when a T3 Code runtime file exists.

The launcher itself records nothing. The run it starts does: a terminal-mode `loopai --t3` run and an
agent-mode ACP run both add a line with launcher `t3` to `~/.config/loopai/launch-history` (see
[Claude Code plugin](../README.md#claude-code-plugin)). `loopai-plan` recommends the launcher of the
newest `orca` or `t3` line, so after a T3 Code launch it recommends T3 Code, and `loopai-t3` offers
the recent flag combinations when invoked without flags.

## Pull requests

With `t3` enabled and `LOOPAI_T3_TOKEN` set, `loopai --pr` links the created GitHub pull request to
every non-archived thread of the repository's project whose branch is the feature branch. T3 Code
then tracks the PR and settles the thread when it merges. A linking failure is a warning; the PR is
already created.

`finalize = pr` or `merge` (or `--finalize=pr|merge`) opens the pull request at the end of the run
itself, after merging `origin/<base>` into the branch, and links it the same way; `merge` also waits
for the PR checks and merges it on GitHub, so the thread settles without a separate close-out (see
[Finalize](../README.md#finalize)). Put the key in `.loopai/config`, since `--t3-launch` forwards
only the model and reviewer flags and rejects `--finalize` and `--skip-finalize`. The same applies
to `review_cadence = task`: `--t3-launch` rejects `--review-cadence`, so set the key in
`.loopai/config`. A run started by
`--t3-launch` uses the T3 Code-managed worktree without `--worktree`, and finalize never removes a
worktree loopai did not create, so it stays until it is removed in T3 Code. Terminal mode enables
`t3` reporting and PR linking; agent mode forces `t3` off and passes no launcher token into the run,
so it does not perform that linking. When finalize stops, for example on a conflict that needs a
decision, terminal mode's title ends in `done · finalize incomplete`; agent mode reports the outcome
in its final message. Close out the plan by hand with `/loopai-merge` or `--pr`.

## Project actions in `t3.json`

T3 Code project actions run a command in the thread's terminal. A `t3.json` at the repository root
can offer loopai commands; T3 Code lists them under the project actions menu ("From t3.json"), and
importing one copies it into the project's own actions:

```json
{
  "$schema": "https://t3.codes/schema/t3.json",
  "scripts": [
    { "name": "loopai review", "command": "loopai --t3 --review", "icon": "test" },
    {
      "name": "loopai dashboard",
      "command": "loopai --serve --watch .",
      "icon": "debug",
      "previewUrl": "http://localhost:8080",
      "autoOpenPreview": true
    }
  ]
}
```

The dashboard action opens loopai's web dashboard in T3 Code's preview panel on desktop. T3 Code
detects preview ports automatically on macOS and Linux; on Windows it checks only common
development ports, so keep `previewUrl` explicit.

## Experimental: loopai as a T3 Code provider

`--t3` can only rename a thread. With the provider mode below, loopai instead runs as a T3 Code
provider session, so the thread reads "Working" for the whole run. It also shows plan progress in
the sidebar, phases as activities, executor output as streamed reasoning, and the completion
report as the final assistant message. This works on desktop, web, and mobile.

T3 Code has no plugin API for custom providers. loopai reuses its Grok driver instead, which
talks the Agent Client Protocol (ACP) to whatever binary a provider instance names. `make build`
writes two binaries: `.bin/loopai` and the launcher `.bin/loopai-acp`. On Windows the launcher is
written only as `.bin/loopai-acp.exe`, and loopai additionally as `.bin/loopai.exe`. The launcher answers the Grok CLI's probe commands. For the session command,
`[--permission-mode MODE] agent [--always-approve] stdio`, it starts `loopai --acp` with its own
stdin, stdout, and stderr. `loopai --acp` is a JSON-RPC server on stdin/stdout that runs one plan
per prompt in-process. The launcher looks for the `loopai` binary in this order:
`LOOPAI_ACP_LOOPAI`, then a `loopai` (`loopai.exe` on Windows) beside the launcher, then `PATH`.

### Adding the provider instance

Add an instance of the `grok` driver whose binary path is the launcher. Either use T3 Code's
provider settings, or edit `<T3 home>/userdata/settings.json` while the server runs (T3 Code
reloads `providerInstances` without a restart):

```json
{
  "providerInstances": {
    "loopai": {
      "driver": "grok",
      "displayName": "loopai",
      "accentColor": "#7c3aed",
      "enabled": true,
      "config": { "enabled": true, "binaryPath": "/home/me/src/loopai/.bin/loopai-acp" }
    }
  }
}
```

Keep the other keys of `settings.json`, and use an absolute `binaryPath`; on Windows, point it at
`loopai-acp.exe`. The provider status should then show the instance as ready. Authentication
reads "unknown" because loopai's own providers (Claude Code, Codex) use their own credentials, so
`loopai-acp models` never claims a login. The bearer credential T3 Code sends for its MCP server
is discarded and never logged.

### Running a plan

With the provider instance configured, the normal way to start a provider-session thread is to
run `loopai --t3-launch docs/plans/<plan>.md` from the repository root, or use the `loopai-t3` skill.
The launcher creates the worktree and thread, carries the plan and local overrides over, and sends
the first turn. Use `--t3-launch=agent` to require a provider session instead of allowing fallback.

You can also start a thread manually with the `loopai` instance in a project or worktree that
already holds the plan, then send the plan path as the message:

```text
docs/plans/20261003-feature.md --task-model codex:gpt-5.5:high --external-reviewers claude:opus
```

The grammar is `<plan> [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]`.
Flags take `--flag value` or `--flag=value`. The two model specs need a `claude` or `codex` prefix,
and values follow the same rules as `--t3-launch`. Every flag needs a non-empty value, so a message
cannot disable external review the way `--external-reviewers=` does; set `external_reviewers =`
in config for that. The plan path is resolved against the thread's working directory; paths
containing whitespace and comma-separated plan chains are not supported. Everything else comes
from the normal config layers resolved in that directory: embedded defaults, the global config (or
`LOOPAI_CONFIG_DIR` from the T3 Code server's environment), then `.loopai/config` there. Only the first text block of the
message is read; the runtime instructions T3 Code appends are ignored. A malformed message fails
the turn and shows the usage line.

The plan runs in place in the thread's working directory, without `--worktree`: T3 Code owns the
thread's worktree, as with `--t3-launch`. On the default branch, loopai creates the plan branch in
that checkout as an ordinary run does. `t3`, `orca`, and `use_worktree` are forced off for the run.
A manually created thread copies nothing into its directory, and a new worktree T3 Code creates
holds committed files only. For manual starts, commit the plan first, and commit `.loopai/config`,
`prompts/`, and `agents/` overrides or move them to the global config when they must apply there.
An agent-mode `--t3-launch` carries those inputs over before sending the turn, so uncommitted plans
and missing local overrides are available without committing them first.
A plan file that is missing is an error rather than an interactive selector. So are an empty
repository and other conditions that would otherwise prompt. Only full plan execution is
available: review-only modes, plan creation, and interactive questions are out of scope.

### What the thread shows

| loopai event | Thread |
|---|---|
| Run in progress | "Working" from the first event until the turn ends |
| Plan tasks and post-task stages (review, external review, finalize, report) | Plan steps and sidebar progress, each `pending`, `in_progress`, or `completed` |
| Phase change | An activity such as `task 2/5`, `review · iteration 1`, or `external review evaluation`, completed when the next one starts |
| Executor output | Streamed reasoning, at most one update every 500 ms, with oversized chunks truncated |
| Completion report | The final assistant message |
| Failed run | A failed turn carrying the failure as its error; a completion report, when one was produced, is sent as the final message first |

The stop button cancels the run, and the turn ends as cancelled. The plan stays in place with the
tasks completed so far. One run executes at a time per loopai process. A message that reaches loopai
while a run is active steers it: the Grok driver cancels the running turn and sends the new message,
so the current run stops as if canceled and the new message starts its own run once the canceled
one has ended. Each run takes its own keep-awake hold when
`keep_awake` is on, released when the run ends.

A thread keeps working after its provider session stops, for example when T3 Code restarts or reaps
an idle session. T3 Code then resumes the saved session with `session/load` rather than starting a
new one. loopai accepts any saved session id and records the thread's working directory under it.
There is no history to restore, because every message is a self-contained run.

### Watchdog and heartbeat

The Grok driver fails a turn after 10 minutes without content or tool progress, or 30 minutes with
an open activity. A provider-limit wait or a long silent executor step can exceed that. When
nothing has been sent for 4 minutes, loopai updates the open activity's title with the elapsed wait,
such as `task 3/7 · waiting 8m`, and repeats every 4 minutes until output resumes. T3 Code's
provider session reaper skips threads with an active turn, so a long run is not idled out.

### Troubleshooting

Each run writes its full log to `.loopai/progress/progress-<plan>.txt` in the thread's working
directory. Human-readable output goes to the agent's stderr, never to stdout, which carries only
protocol messages. Config is loaded per message in the thread's working directory, so an invalid
config fails that turn with the error rather than stopping the provider from starting.

To see which protocol requests loopai handled, have the launcher start loopai with `--debug`. On
Linux and macOS, point `LOOPAI_ACP_LOOPAI` in the T3 Code server's environment at an executable
script that runs `exec /path/to/loopai --debug "$@"`. Each `initialize`, `authenticate`,
`session/new`, `session/load`, `session/set_*`, `session/cancel`, and `session/prompt` (started,
completed, failed, or canceled) is written to stderr as one `acp:` line, with MCP header values
redacted. Message payloads, the prompt text, and `session/update` notifications are not traced;
a failure's message is in the turn's error and in the run's progress log under `.loopai/progress/`.

### Caveats and removal

This mode relies on undocumented contracts of T3 Code's Grok driver: the probe commands and their
output, the session argv, the ACP methods it sends (including `session/load` with no capability
check or fallback), and the watchdog timings. These were checked
against the T3 Code build of October 2026. Agent launch also relies on the public dispatch
endpoint accepting the current `thread.turn.start` payload. A T3 Code update can break the mode
without warning, so re-check a launch and provider run after each update. The launcher does not
implement Grok's `inspect` or `update` commands, so the instance offers no skills and cannot update itself.

To remove the provider, delete or disable the instance in T3 Code's provider settings. Otherwise,
remove its entry from `providerInstances` in `settings.json`, or set `enabled` to `false`. Threads
created with it remain as ordinary history.

## Limitations

- `--t3` and terminal-mode `--t3-launch` produce a thread with title and pin status and, for the
  launcher, a live terminal. Without a provider session the thread reads as ready rather than
  working. T3 Code does not read OSC titles from the terminal.
- Agent-mode `--t3-launch` produces a provider-session thread with Working state, plan progress,
  streamed reasoning, and the final report as a message. It uses the experimental Grok-driver
  integration above; loopai forces `t3` reporting off, so it does not update phase titles or link
  PRs during that run.
- Agent mode does not support plan paths containing whitespace. Use `--t3-launch=terminal` for
  those paths.
- Under `review_cadence = task` the first per-task external review enters the External review
  stage, which marks the Review stage completed before the internal review has run. A completed
  stage never reopens, so for the rest of the run the plan view shows Review completed and External
  review in progress while later tasks and the final internal review run. Plan tasks keep their
  own status. Phase activities and thread titles follow every per-task block as usual.
- `--pr` linking understands GitHub pull request URLs only, matching what `--pr` creates.
- The token grants full access to the T3 Code server. In terminal mode it lives in the thread
  terminal's environment for the run. Agent mode uses it only for launcher requests and places no
  token in the thread. Revoke it with `t3 auth session revoke` when no longer needed.
