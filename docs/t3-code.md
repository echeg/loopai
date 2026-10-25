# T3 Code integration

[T3 Code](https://t3.codes) is a control surface for coding agents with desktop, web, and mobile
clients. A loopai run can last about 40 hours, longer than any agent turn, provider process,
terminal shell, or T3 Code session it could live inside. loopai therefore never runs a plan inside
T3 Code. The plan runs as a [detached run](#detached-runs): an independent process with its own
loopai worktree and durable state, usable from any shell. T3 Code is only a window onto it, in one of
two modes:

- [Mode 1, a terminal in this thread](#mode-1-a-terminal-in-this-thread): the thread you are talking
  in gets a terminal tab that follows the run and a `[loopai: task 3/7]` title prefix. The thread's
  agent stays free for conversation.
- [Mode 2, a loopai thread](#mode-2-a-loopai-thread-that-shows-working): a new thread on the loopai
  provider shows Working for the whole run, with the plan's tasks and the current phase, and ends
  with a short report summary.

The `loopai-t3` skill (`/loopai:loopai-t3` in Claude Code, `$loopai-t3` in Codex) asks which mode to
use and starts the run. loopai talks to T3 Code only through its local server API (protocol 2) and
the MCP endpoint T3 Code gives a provider. No T3 Code fork or plugin is involved, and loopai never
edits T3 Code's settings or database files. Every T3 Code update in either mode is best-effort: a failure
never stops or changes the run.

## Detached runs

```bash
loopai --detach [flags] docs/plans/<plan>.md   # start or resume, print the run id, return
loopai --runs                                  # list this checkout's runs
loopai --attach [run]                          # replay and follow a run until it ends
loopai --stop <run>                            # stop a live run gracefully, keeping its worktree
```

Start `--detach` from the repository root; `--runs`, `--attach`, and `--stop` also work from any
subdirectory of the checkout, and refuse outside a repository. `[run]` is a run id, a plan, or a
branch. Without one, `--attach` follows the only live run, else the newest.

`--detach` starts one plan, in full or `--tasks-only` mode, as an independent process. It always runs
in a loopai worktree (`.loopai/worktrees/<branch>`), as `--worktree` would, and accepts the usual
model, reviewer, finalize, and branch flags. It refuses a plan chain, `--commit`, the review-only
modes (`--review`, `--external-only`), plan creation (`--plan`), `--gen-agents`, `--serve`, `--watch`,
`--cmux-workspace`, and the standalone commands. The launcher waits up
to 120 seconds for the run to confirm that its attempt started, prints

```text
run: <run-id>
follow: loopai --attach <run-id>
```

and exits; the run continues on its own. When the run fails before it starts, the launcher prints
the reason and the end of the run's console log.

The run lives in its own session (`setsid`), with stdin on the null device, stdout and stderr in its
console log, and hangups ignored, so closing the terminal, ending the agent turn, or stopping the
T3 Code provider that started it does not end it. On Linux with a systemd user manager the run starts
in a transient user scope (`systemd-run --user --scope`), so a service's cgroup that is stopped
later, such as a T3 Code provider slice, does not take the run with it; when the scope cannot start,
loopai warns and starts the run without it. The run's environment never contains `T3_ACP_MCP_*`,
`LOOPAI_T3_TOKEN`, or `CMUX_*`. It takes its own keep-awake hold when `keep_awake` is on.

`--attach` prints the run's id, attempt, status, phase, and task, replays its console log from the
first byte (every attempt included), and follows it until the run ends. A log longer than 1 MiB is
replayed from the first line start within its last 1 MiB, after a line
`(earlier output omitted; full log: <path>)`. It then prints the status,
reason, branch, commit, report location, pull request, and kept worktree, and exits with `0` for
succeeded, `1` for failed or crashed, and `2` for stopped. Ctrl+C only detaches: the run keeps going.

`--runs` lists every run of the checkout, newest first, with its status (`starting`, `running`,
`succeeded`, `failed`, `stopped`, or `crashed`), attempt, origin (`cli`, `t3-terminal:<thread>`, or
`t3-thread:<thread>`), plan, branch, and start time. A run whose process ended without recording an
outcome is `crashed`.

`--stop` writes a stop marker, sends the run's process SIGTERM, and waits up to 15 seconds for it to
end. The run cancels its current step, records `stopped`, and keeps its worktree. It refuses a run
that is not running, or whose recorded process was replaced by another with the same pid.

### The run directory

Every run records its state in the invoking checkout, under `.loopai/progress/runs/<run-id>/`, where
`<run-id>` is `<plan stem>-<YYYYMMDD-HHMMSS>-<4 hex>`:

| File | Content |
|---|---|
| `run.json` | The descriptor: plan, branch, worktree, progress log, console log, recorded arguments and environment options, origin, attempt, and process. It never holds a token. |
| `run.lock` | The lifetime lock, held by the run's process for its whole life. |
| `events.jsonl` | The event journal: attempt start, phases, tasks, review sections, finalize, report, and finish. Every view of a run reads it. |
| `outcome.json` | How the current attempt ended; earlier attempts are kept as `outcome-attempt-<n>.json`. |
| `report.md` | The completion report, when the run produced one. |
| `console.log` | The run's full output across all attempts, one separator line per attempt. |
| `stop-requested` | The stop marker. |
| `attach.lock` | Held by the loopai thread that follows the run (Mode 2). |
| `source-plans.json` | The invoking checkout's uncommitted plan, as the first attempt found it. |

The run also writes the usual progress log, `.loopai/progress/progress-<plan>.txt`. A new run prunes
older succeeded run directories, keeping the newest 20; failed, stopped, crashed, and live runs, and
a succeeded run whose worktree is still in place, are never pruned.

### Resume

Launching the same plan again resumes its latest run when that run is not live and did not succeed
(failed, stopped, or crashed): same run id, the next attempt, the previous outcome kept as history.
The attempt resumes in the kept worktree from the first incomplete task. There is no resume flag. A
live run is refused with `run <id> of <plan> is already <status>; follow it with loopai --attach <id>`.
A succeeded run is never resumed: the next launch starts a new run, unless the succeeded run's branch
is still checked out in a worktree, which is refused until that worktree is merged or removed.

A resume keeps the origin the run was started with, unless the new launch names a T3 thread itself
(`--t3-thread`, or the provider of a loopai thread). A run started from a loopai thread and resumed
from a plain shell therefore stays that thread's run and can still be stopped from it. A resume from a
plain shell has no T3 token, so it shows no Mode 1 terminal or title.

### Worktree, archive, and source plan

A detached run never commits into the checkout you launched it from. The plan archive and its
completion report sidecar are committed on the run's own branch, and the run never moves the invoking
checkout's HEAD.

After a successful run, the invoking checkout's copy of the plan is made merge-ready the way a
successful worktree chain does it: an unchanged untracked copy is removed and an unchanged tracked copy
is restored to HEAD. A copy you edited after the run started is never overwritten; the outcome notes
it instead.

The worktree is removed only after a clean success. It is kept, and reported as kept, on every other
exit after the attempt started: a failed task, the task phase's iteration cap, Stop, a crash, any other
error, and a success whose finalize close-out or plan archive stopped short, or whose worktree is dirty
or has a Git operation in progress. A review that ends by stalemate or iteration cap is not a failure:
the post-review loop commits what it left on the branch and the run continues. Foreground `--worktree`
runs keep their usual cleanup.

### Close-out while a run is live

`loopai --merge` and `loopai --pr` refuse a branch while a detached run of it is live:

```text
a detached run of <branch> is live: <run-id>; stop it with loopai --stop <run-id>
```

They check the runs directory of every registered worktree of the repository, since a run records in
the checkout it was launched from, and also probe the run lock of the worktree that has the branch
checked out, which catches a foreground `--worktree` run too. Once the run has finished, close out as
usual with `/loopai:loopai-merge`, `loopai --merge <branch>`, or `loopai --pr <branch>`, or read the
report with `loopai --report <branch>`.

## Mode 1: a terminal in this thread

Mode 1 shows the run in the T3 Code thread you are talking in. The skill offers it only when that
thread's access mode is `full-access`, the agent shell is not sandboxed, the system is not Windows, and
the T3 Code tools confirm the conversation is a T3 Code thread. It runs, as one foreground command:

```bash
T3_CLI="${T3CODE_CLI_PATH:-${T3CODE_HOME:-$HOME/.t3}/bin/t3}"
LOOPAI_T3_TOKEN=${LOOPAI_T3_TOKEN:-$("$T3_CLI" auth session issue --scope orchestration:read --scope orchestration:operate --scope terminal:read --scope terminal:operate --ttl 3d --label loopai --token-only)} \
  loopai --detach --t3-thread '<thread-id>' [flags] docs/plans/<plan>.md
```

The token comes from the CLI that ships with T3 Code (`T3CODE_CLI_PATH`, else `<T3 home>/bin/t3`),
never from a `t3` on `PATH`, which may be an old release writing into a frozen database. The four
scopes replace T3 Code's broader default set, which includes settings and provider management. The
token is valid for 3 days and is not revoked at the end. It reaches the run over a pipe between the
launcher and the run, is held in memory only, and never lands in a file, a log, the run's environment,
or the agent's transcript. T3 thread ids are opaque, not always UUIDs (subagent threads carry `:` and
`%`), so `--t3-thread` accepts 1 to 1024 letters, digits, and `_.:%-` characters.

The run finds the T3 Code server through `<T3 home>/userdata/server-runtime.json`, where the T3 home is
`T3CODE_HOME` or `~/.t3`; `LOOPAI_T3_URL` overrides the origin. The connection is re-established, and
the runtime file re-read, whenever it drops.

What the thread shows:

- A terminal tab, `loopai-<run-id>`, opened with the same working directory and environment T3 Code's
  own terminal for the thread uses, running ` cd '<checkout>' && '<loopai>' --attach <run-id>`. The
  leading space keeps the command out of the shell history. When the run ends the shell stays open.
- A title prefix, written on transitions only: `[loopai: task 3/7]`, `[loopai: review]`,
  `[loopai: external review]`, `[loopai: finalize]`, `[loopai: report]`, `[loopai: limit wait]`, and
  finally `[loopai: done]`, `[loopai: failed]`, or `[loopai: stopped]`. The rest of the title is kept,
  including a name you gave the thread after the run started. Only a recognized `[loopai: …]` prefix
  is ever replaced. Before its first write loopai waits up to about 2 minutes for a title T3 Code is
  still generating, since any title write cancels that generation.
- A pin. T3 Code settles a thread that has been idle for 3 days unless it is pinned; the pin protects
  it in T3 Code's code, although T3 Code's user documentation says otherwise. The run pins the thread
  at the start when it is not pinned already and unpins it at the end only when it pinned it itself.
- An unread mark. When the run finishes, succeeded, failed, or stopped, loopai marks the thread unread
  as its last request, after the final title and the unpin, so the sidebar shows T3 Code's green
  "Done" badge until you open the thread. The badge is green for all three outcomes, because T3 Code
  derives a red "Failed" from the status of the thread's own latest agent turn, which loopai cannot
  set; the title prefix carries the outcome. Opening the thread clears the badge, so a thread you have
  open at the finish may never show it. T3 Code refuses the mark while a new agent turn is running in
  the thread, and the thread then stays as it is.

There is no completion message in Mode 1: the final title prefix, the unread mark, and the terminal
show the result.
The thread's agent does not monitor the run. Ask it about the run, and it answers from `loopai --runs`.

A background command or a held Monitor in an ordinary thread never shows Working in T3 Code: a held
Monitor shows a grey "Waiting", and a background command shows nothing and is ended about 4.5 hours
after the provider's last event. This is why Mode 1 uses the token, a title, and a terminal of its own
rather than a background command in the agent's shell, and why Working needs Mode 2.

### Restarts and resume

T3 Code keeps terminals in server memory only, so a restart loses the terminal. When the run
reconnects after a restart and its terminal is gone, it opens it once more with the same launch
context, which T3 Code seeds from the persisted history, waits until it runs, and types the follow
command again. A terminal you closed while connected is never reopened. A terminal left at its prompt
for a minute after the follow command ran, for example after Ctrl+C, gets the command typed again.

An expired or revoked token, or a T3 Code server too old or too new for protocol 2, stops these
updates with one line in the run's progress log; the run itself continues. The pin and the last title
then stay as they were: unpin the thread or remove the prefix in T3 Code if you want to. When T3 Code
refuses only the terminal (for example because the thread's working directory is gone), only the
terminal stops, with one progress-log line, and the title and pin keep following the run. When it
refuses another request, the updates stop the same way, but loopai first removes the progress prefix
(or writes the final state of a finished run), unpins the thread if it pinned it, and marks a finished
run's thread unread. A refused unread mark alone never stops the updates or affects the run.

To resume a failed or stopped run with a fresh token, ask the thread's agent to run the same plan in
this mode again. `loopai --detach <plan>` from a plain shell resumes it too, without the terminal and
title.

## Mode 2: a loopai thread that shows Working

T3 Code shows Working only while a provider turn runs. In Mode 2 loopai is that provider: `loopai
--acp` serves the Agent Client Protocol for T3 Code's `acpRegistry` driver, starts the detached run,
and holds one turn open until the run ends.

### Registering the loopai provider

`make install` installs the argument-free launcher `loopai-acp` beside `loopai`. Register it once in
T3 Code's settings; never edit `settings.json` by hand, because a schema error there makes T3 Code
silently reset its settings:

1. T3 Settings → Providers → Add provider → ACP Registry.
2. Instance ID: keep the default (`acpRegistry_local_acp`); the skill finds the instance by its
   command, never by its id.
3. Display name: `loopai`.
4. ACP source: `Local command`.
5. Executable override: the absolute path of `loopai-acp` (`command -v loopai-acp`). Alternatively,
   the absolute path of `loopai` with the argument `--acp` in the Arguments editor.
6. Add instance.

`loopai-acp` finds `loopai` through `LOOPAI_ACP_LOOPAI`, then a `loopai` beside the launcher, then
`PATH`, and on Linux and macOS execs `loopai --acp` in place. T3 Code starts the provider as the
leader of its own process group and retires it by signalling the whole group, SIGTERM and SIGKILL one
second later; the exec keeps that one stable process. The run it launches is in a session of its own,
so it is not in that group.

T3 Code checks provider readiness about every minute while the provider list is shown, starting
`loopai --acp` briefly each time. loopai recognizes these probes and answers them without reading
config or writing anything. Only a ready result is cached, for 15 minutes; a failed check is repeated
every minute. Before the first check the instance's model reads `default`.

To remove the provider, delete or disable the instance under Settings → Providers. An instance of the
old `grok` driver pointing at `loopai-acp` no longer works; remove it and register the new one.

### Starting a run

The skill launches the thread with the plan as its first message. To start one by hand, open a new
thread on the loopai provider in the project and send:

```text
docs/plans/<plan>.md [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]
```

The first word must be a `.md` path relative to the thread's working directory, followed only by those
three flags, each at most once, as `--flag value` or `--flag=value`. Values match
`^[A-Za-z0-9._:,+-]+$`, and both model specs need a `claude` or `codex` prefix. Paths containing
whitespace are not supported. Everything else comes from the config layers resolved in the thread's
working directory.

The provider runs `loopai --detach` of that plan in the thread's working directory, which must be a
repository root: the project root, or a worktree T3 Code created. A worktree T3 Code creates holds
committed files only, so commit the plan first when the thread uses one. The run gets its own loopai
worktree and records its state in that checkout, and it is recorded as the thread's run (origin
`t3-thread`).

### What the thread shows

| Run event | Thread |
|---|---|
| Run in progress | Working from the start of the turn until the run ends |
| Plan tasks and the stages after them (Review, External review, Finalize, Report) | Plan entries, each pending, in progress, or completed |
| Current phase | One activity, titled `Task 3/7: <task title>`, `Review: <section>`, `External review: <section>`, `Finalize`, or `Report`, completed when the next one starts; a usage-limit wait adds `: waiting for the usage limit` |
| A step without updates for 20 minutes | The activity is retitled with the time the step has taken |
| Run finished | One final message: the status and reason, the report summary, Risk, the branch, and the path of `report.md`; for a failed, stopped, or crashed run, that the worktree is kept and `resume` continues it |

The thread receives updates on transitions only. It shows no reasoning or executor output, because T3
Code stores every update in its database and never compacts them; the full output is in the run's
console log. The final message is capped at 12,000 bytes so that it survives a model switch, whose
handoff carries about 16,000 bytes of history; the full report stays in `report.md` and is reachable
with `loopai --report <branch>` once archived. A failed or crashed run ends the turn normally, with the
status stated in the final message.

The thread only ever shows and stops a run it started. Each message is routed by the first rule that
applies:

1. The thread's own run is live (starting included): the turn follows it, whatever the message says.
   The thread's run ended without success (failed, stopped, or crashed) and the message is `resume`
   or T3 Code's continuation text: the run is resumed from its recorded arguments and environment
   options, and followed. A run that succeeded is never resumed. When another loopai thread or a
   `--t3-thread` launch has resumed the run since, it is no longer this thread's: the thread drops
   the binding, neither follows nor resumes the run, and answers with a status reply.
2. The message names a plan: that plan's own detached run starts (or resumes) and the thread follows
   it. It never adopts another run.
3. The thread has no run, and the message is T3 Code's continuation: it adopts the one live run that a
   loopai thread started from the same directory and that no other thread follows; several candidates
   are listed instead.
4. Anything else gets a status reply: how the current attempt of the thread's run ended (a run that
   ended without recording an outcome reads as crashed or stopped), with the `resume` hint when it did
   not succeed, or how to start one, and that the conversation continues by switching the thread's
   model.

The binding between a thread and its run is stored in `.loopai/acp/` of the thread's working
directory, one file per thread, named by a hash of the thread id, plus one per provider session. The
thread id comes from T3 Code's thread-scoped MCP endpoint; when that lookup fails, the session id is
the fallback key. loopai writes `.loopai/acp/.gitignore` (`*`) before the first binding when the
directory has none, so bindings never show up in Git status or T3 Code's changes panel, even in a
worktree T3 Code created from committed files.

### Stop and resume

Stop ends the turn at once as interrupted. loopai then waits 5 seconds: T3 Code steers a message into
a running turn by cancelling it and sending the new message, and a message within that window keeps
the run, and the new turn follows it again. After the window loopai asks T3 Code whether the cancel
was your Stop, and stops the run gracefully, with the stop marker and SIGTERM, unless T3 Code shows it
was not. When that check fails, the run is stopped. The worktree is kept. A provider that T3 Code shuts
down or restarts during the window never stops the run.

To continue a stopped or failed run, type `resume` in the thread, or press T3 Code's Resume button,
which sends T3 Code's continuation text. Both resume the run in its kept worktree.

Messages typed while the run is in progress are queued by T3 Code by default and reach loopai only
after the run's turn ends, where the rules above answer them.

### After a T3 Code restart

When T3 Code shuts down or restarts during a run, it marks the turn cancelled and stops the provider;
the run continues. At the next start T3 Code sends `Continue where you left off.` to the thread, the
new provider finds the thread's run and follows it, and Working returns without any action. When the
saved session cannot be loaded, T3 Code starts a new session with a summary of the thread, and loopai
still finds the run through the thread id.

T3 Code sends that continuation only when all of these hold:

- the project's continue-after-restart setting (`continueThreadsAfterServerUpdate`) is on, as it is by
  default;
- the run was not stopped by the user, no permission request is pending, the provider instance is
  unchanged and enabled, and `settings.json` decodes;
- the turn was running, the provider session was neither stopped nor errored, and the thread is not
  archived, settled, or snoozed;
- there were no more than 3 continuations within 5 minutes.

Otherwise, and also when the provider process died on its own while T3 Code kept running, which
makes T3 Code mark the turn failed and never continue it, type `resume` in the thread: a live run is
followed again, and an ended one is resumed. Messages that were queued in the thread before the
restart are held afterwards and not sent by the continuation; release them from T3 Code's queue when
you want them sent.

### After the run

The loopai provider only reports status: it does not chat, merge, or open pull requests. To talk about
the result, switch the thread's model to Claude or Codex after the run has ended; the final message
carries the summary, Risk, branch, and report path into that conversation. Switch the model only after
the run: switching during the turn detaches the thread from the run, which keeps going; follow it with
`loopai --attach <run-id>`. Merge with `/loopai:loopai-merge` or `loopai --merge <branch>`.

### Troubleshooting

`loopai --acp` keeps stdout for the protocol and writes its warnings to stderr. The run's own output
is in its console log (`.loopai/progress/runs/<run-id>/console.log` in the thread's working directory)
and its progress log. A launch that fails, such as on a config error, a missing provider binary, a
plan that cannot be read, or a live run of the same plan, is answered with the launcher's message; a
message whose flags cannot start a run is answered with the usage line.

## Project actions in `t3.json`

T3 Code project actions run a command in the thread's terminal. A `t3.json` at the repository root can
offer loopai commands, for example the dashboard in T3 Code's preview panel:

```json
{
  "$schema": "https://t3.codes/schema/t3.json",
  "scripts": [
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

T3 Code detects preview ports automatically on macOS and Linux; on Windows it checks only common
development ports, so keep `previewUrl` explicit.

## Limitations

- Windows: detached runs are not supported, so `--detach`, `--attach`, `--runs`, and `--stop` refuse
  there and neither mode works.
- Sandbox: `--detach` refuses to start inside an agent sandbox (a non-empty `CODEX_SANDBOX`,
  `CODEX_SANDBOX_NETWORK_DISABLED=1`, or `SANDBOX_RUNTIME=1`), because every model process of the run
  would inherit it. Use Mode
  2 from a sandboxed agent, or a thread in `full-access`.
- Linux: T3 Code runs providers in a systemd slice that it stops at its next start. The transient scope
  keeps the run out of that slice; this has not been verified live.
- Sleep: `keep_awake` keeps the machine from idling to sleep while the run is active, but closing a
  laptop lid on battery still sleeps it, pausing the run. There is no special handling for that.
- The Mode 2 run inherits T3 Code's environment: the server's `PATH`, which must find `claude` and
  `codex`, its `HOME` and global config (or the `LOOPAI_CONFIG_DIR` T3 Code was started with), and its
  provider credentials. A resume restores the `LOOPAI_CONFIG_DIR`, `LOOPAI_ORCA`, and
  `LOOPAI_WEB_HOST` the run was first started with.
- The Mode 1 terminal keeps at most 5,000 lines or 8 MiB of history, so after a restart the early
  output of a long run is only in its console log.
- A Mode 1 token expires after 3 days; a longer run stops updating the thread at that point. Resume
  with a fresh token by running the plan in this mode again after the run ends.
- Not yet verified against a live T3 Code: that the provider's MCP endpoint returns the thread id;
  that steering arrives as a cancel followed by a new prompt (the basis of the 5-second Stop window);
  whether Working shows while a saved session loads; whether the composer accepts the model `default`
  before the first probe; the model-switch handoff budget; the Mode 1 terminal reopen after a restart;
  that the Mode 1 unread mark shows the "Done" badge and is not cleared by the final title and unpin
  written just before it; and the Linux scope.
- These contracts come from reading T3 Code 0.1.1, not from a documented API. Re-check both modes after
  a T3 Code update.

## Removed integration

The earlier T3 Code integration hosted the run inside processes that end before a long run does, and
spoke T3 Code's previous protocol. It was removed and replaced by the two modes above:

- `--t3`, `t3 = true`, and `--no-t3` were removed; use Mode 1 (`loopai --detach --t3-thread <thread-id>`) or the skill.
- The `LOOPAI_T3` environment variable was removed and is ignored; `LOOPAI_T3_THREAD_ID` was removed too.
- `--t3-progress-thread` and the Codex current-thread observer were removed; use Mode 1.
- `--t3-launch[=auto|agent|terminal]` was removed; use the `loopai-t3` skill, which offers both modes.
- ACP chat and the in-thread `merge` and `pr` commands were removed, with the `acp_chat.txt` prompt.
- ACP automatic merging and its `acp_auto_merge` key were removed; merge with `loopai --merge` or `/loopai:loopai-merge`.
- Settling threads after a local merge, and linking pull requests to threads, were removed.
- A `t3.json` action that runs `loopai --t3 …` fails since `--t3` was removed; drop the flag.

The removed flags and config keys fail with a message naming the replacement. Old
`.loopai/progress/t3-current-*` directories, `.loopai/acp/<session-id>.json` chat state, and
customized copies of the removed chat prompt are unused and can be deleted.
