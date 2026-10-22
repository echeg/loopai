# loopai and T3 Code: detached runs with a terminal window and a Working thread

Status: draft for approval, 2026-10-22.

## Intent

T3 Code 0.1.x replaced its agent engine (Orchestrator V2, orchestration protocol 2). Every T3 path in
loopai broke: the WebSocket handshake now requires `?orchestrationProtocol=2`, the HTTP dispatch route
is gone, four command shapes changed, the thread shell lost the fields settlement relied on, the state
database moved to `statev2.sqlite`, and ACP prompts arrive wrapped. Porting the old integration is not
the goal. It is replaced by a design built around two facts the old one ignored:

- **A loopai run can last about 40 hours.** Anything the run lives inside (an agent turn, a T3 provider
  process, a T3 terminal shell, the T3 app itself) will die or restart before it ends.
- **What the user wants from T3 is a window onto the run, not a host for it.** Earlier attempts failed
  on exactly this: a thread whose provider was loopai could only accept a plan and not ordinary requests,
  and an empty thread with a terminal inside never showed that work was happening.

So the run becomes an independent, detached process with durable state on disk, and T3 shows it in one
of two ways the user picks per launch.

## Goals

1. **Detached run core, usable without T3.** A run starts detached from whoever launched it, survives
   its launcher, terminal, agent, provider, and T3 restarts, keeps durable state under `.loopai/progress/`,
   and can be listed, followed, stopped, and resumed from any shell.
2. **Mode 1, same-thread terminal.** From an ordinary T3 thread (Claude or Codex agent) the user asks to
   run a plan. The run starts detached in its own loopai worktree. A terminal tab opens in that same
   thread showing the run's live output, and the thread title carries a replaceable progress prefix such
   as `[loopai: task 3/7]`. The thread's agent stays free for normal conversation during the run. After
   a T3 restart the terminal view comes back on its own.
3. **Mode 2, new thread that shows Working.** A new T3 thread is created whose provider is loopai. The
   sidebar shows it as Working for the whole run, with the plan's tasks and the current phase visible in
   the thread. The turn ends with a report summary. The user then switches the thread's model to Claude
   or Codex in the composer and continues an ordinary conversation about the result. A T3 restart in the
   middle of the run brings Working back without user action.
4. **Remove the previous T3 and ACP integration** instead of porting it.

## Non-goals

- Chat, merge, or PR handling inside the loopai provider. Conversation after the run happens by switching
  the thread's model; merging goes through the agent or `loopai --merge`.
- Parallel conversation inside a Mode 2 thread while it is Working. T3 queues messages for an active ACP
  turn; Mode 1 is the mode for working alongside a run.
- T3 merge settlement (`thread.settle` after `--merge`), linking PRs to T3 threads, the in-process `--t3`
  reporter, `--t3-launch`, and the Codex current-thread progress resolver.
- Creating T3 projects. Both modes start from a thread that already belongs to a project.
- Plan chains in a detached run.
- Keeping a laptop awake with its lid closed.
- Detached runs on Windows in this iteration. The code must still build there and `--detach` must fail
  with a clear message.

## Anti-goals

This is a failure even if it technically works when:

- the run dies because a terminal closed, an agent turn ended, a provider process exited, or T3 restarted
  or updated;
- a Mode 2 thread cannot be talked to after the run;
- a Mode 2 thread shows, or stops, a run it did not start;
- the T3 database grows by gigabytes over a long run;
- a T3 token reaches a file, a log, the progress log, an agent transcript, or the environment of the
  model processes the run spawns;
- pressing Stop, or a run failing late, loses committed or uncommitted work;
- a detached run commits into the checkout the user is working in;
- a progress title update overwrites a name the user gave the thread.

## Verified T3 Code 0.1.1 facts this design relies on

These come from reading the 0.1.1 server bundle with adversarial re-verification. They were not
exercised against a live server with a token. Bundle line references are to `apps/server/dist/binCli-*.mjs`.

- **Transport.** `/ws` requires `?orchestrationProtocol=2` before authentication (426 otherwise). A
  `Authorization: Bearer` header on the upgrade still works. Commands go only through WS Effect-RPC
  `orchestration.dispatchCommand`. The orchestration read endpoints require the header
  `x-t3-orchestration-protocol: 2`. `thread.meta.update` is now `thread.metadata.update`.
- **Long ACP turns.** `session/prompt` is awaited with no timeout. There is no turn-duration limit and no
  no-progress watchdog for the `grok` or `acpRegistry` drivers, and a busy session is never idle-reaped.
- **Restart continuation.** On shutdown or crash T3 marks the running turn cancelled and, at the next
  start, sends `message.dispatch` with the text `Continue where you left off.` to the same thread and
  provider instance (`continueThreadsAfterServerUpdate`, default true). It spawns a fresh provider
  process, calls `session/load` with the saved session id, then `session/prompt`. A failed or slow
  `session/load` (90-second timeout) does not fall back: the turn fails with the load error. (Wrong:
  see "Amendments from the T3 v0.1.1 source study", **`session/load` falls back**.) Continuation is skipped after a user Stop, with a pending permission request, when
  the provider instance changed or is disabled, when `settings.json` fails to decode, and after more than
  3 continuations within 5 minutes. (Incomplete: see the source-study amendments, **Continuation
  preconditions** and **Continuation text has three senders**.)
- **Kill scope.** On macOS every T3 kill targets a pid or a process group: desktop to server, server to
  provider group (SIGTERM, no `session/cancel`), the Claude CLI group, terminal shells. A process in its
  own session (`setsid`) with stdio not tied to the provider survives. On Linux under a systemd user
  manager, providers live in a slice that is stopped at the next server start, which `setsid` does not
  escape.
- **Stop and queueing.** For `acpRegistry`, Stop sends one `session/cancel` notification and waits 10
  seconds for a `cancelled` stop reason. Stop disables continuation for that run. The driver reports
  queued messages but no active steering, so ordinary messages typed during a turn are queued.
  (Steering is interrupt-restart: see the source-study amendments, **Stop and steering**.)
- **`acpRegistry` driver.** Enabled by default, no CLI probes. A local instance is `commandPath` plus
  `commandArgs`, spawned with the thread's cwd. The settings UI for a local ACP agent always writes
  `commandArgs: []`. T3 runs a background probe (`initialize` and `session/new`) roughly every 15
  minutes from the server's cwd. (Both wrong: see the source-study amendments, **Settings form and
  instance id** and **Probe cadence**.) The ACP process gets `T3_ACP_MCP_ENDPOINT` and
  `T3_ACP_MCP_AUTHORIZATION` (a thread-scoped MCP credential) in its environment.
- **Prompt envelope.** Every prompt sent before a prompt has completed successfully in the current ACP
  process (the first one, restart continuations, steered prompts) is wrapped as
  `<t3_code_instructions>…</t3_code_instructions>\n\n<user_request>\n…\n</user_request>`, and a separate
  `<runtime_info>` block follows. Inside `user_request` a handoff history or restart note may precede the
  last `\n\nUser message:\n`. Text that starts with `/` arrives unwrapped. (Wrapping is per session and
  steered prompts usually arrive unwrapped: see the source-study amendments, **Prompt wrapping**.)
- **Storage growth.** `turn-item.updated` events are never compacted, and reasoning and message snapshots
  store the whole accumulated segment, so streaming text grows the database quadratically. Every tool
  call retitle or status change writes events, and every plan update writes three. One message or thought
  chunk is truncated at 65,536 UTF-16 code units. Terminal history is capped at 5,000 lines or 8 MiB.
- **Model switch.** After the turn, switching the thread's model and sending a message performs a
  handoff built from whole history items within 16,000 bytes of JSON-escaped UTF-8, prioritising the
  last user message, then the last assistant message. A final message above roughly 12 KB of ASCII
  (about half that in Cyrillic) is dropped from the handoff, leaving only a pointer to `t3_thread_read`.
- **Same-thread terminal.** An agent learns its own thread id from MCP `t3_thread_list`
  (`currentThreadId`), confirmed live including from a subagent. No MCP tool or `t3` subcommand opens or
  writes a thread terminal, so Mode 1 needs a bearer token and WS `terminal.open` / `terminal.write`.
  The terminal manager does not validate the thread and coexists with a live agent. Re-opening an
  existing terminal id with a different cwd, env, or worktree path resets its shell and history. On a
  server restart the shell is killed; the UI later restores scrollback in a fresh idle shell and runs no
  command. (A UI re-attach is not guaranteed: see the source-study amendments, **Mode 1 terminal after a
  restart**.) The sidebar shows "terminal process running" while the shell has a child process.
- **Thread launch from an agent.** MCP `t3_thread_launch` accepts `modelSelection {instanceId, model}`,
  `message`, and `workspaceStrategy` (`root` or `existing_worktree`), and inherits the project from the
  calling thread. MCP `orchestrator_capabilities` lists provider instances and their models from the
  live composer catalog. On this machine no `acpRegistry` instance is registered yet.
- **Tokens.** `t3 auth session issue --token-only` from the CLI matching the server version
  (`<T3 home>/bin/t3`) issues a token whose default scopes include orchestration and terminal operation
  and accepts `--ttl`. The stale `t3` 0.0.45 on `PATH` writes tokens into the frozen V1 database, which
  the server rejects.
- **Auto-settle.** Threads idle for 3 days are auto-settled regardless of terminals, which runs the
  user's settle script on the thread's worktree. (A pin protects from it: see the source-study
  amendments, **Pin and auto-settle**.)

## Design

### 1. Detached run core

`loopai --detach [flags] <plan>` starts a run and returns.

- The launcher re-executes `os.Executable()` in a hidden child mode. The child runs in a new session
  (`setsid`), with stdin from the null device, stdout and stderr appended to a per-run console log, and
  SIGHUP ignored. `T3_ACP_MCP_*` and `LOOPAI_T3_TOKEN` are removed from the child's environment, so the
  model processes it spawns never see them. Secrets the child needs travel over the launcher-to-child
  handshake pipe, never through the environment or a file.
- The launcher refuses to start when it detects that it runs inside an agent sandbox (for example a Codex
  seatbelt profile), because `setsid` does not leave a sandbox and the whole 40-hour run, including its
  model processes, would inherit the restrictions.
- The child takes the non-interactive execution path that ACP uses today (`prepareNonInteractiveRequest`
  and `executePlanRequest.NonInteractive`): a plan is required, no prompts, no pause handler. Only full
  and tasks-only modes are accepted, with exactly one plan; a plan chain is refused with a clear message.
- `--detach` implies worktree mode. A 40-hour run in the user's own checkout would collide with their
  work. Plan carrying and `.loopai/` overrides already work in worktree mode.
- A detached run never commits into the invoking checkout. It archives the completed plan and writes the
  report on the run's own branch, the way chains and non-worktree runs already do, and treats the source
  copy of the plan the way successful worktree chains do: an unchanged untracked copy is removed and an
  unchanged tracked copy is restored to source HEAD. Concurrent edits are never overwritten.
- A detached run keeps its worktree on every exit except success: stop, task failure, iteration cap,
  stalemate, and crash. Only a successful run removes it.
- The launcher prints the run id and the follow command, and exits only after the child holds the run
  lock and has written its descriptor (handshake), so a startup failure is reported by the launcher.
- Per-run state lives under `.loopai/progress/runs/<run-id>/` in the invoking checkout, a path already
  ignored and already excluded from dirty-tree checks:
  - a descriptor: run id, pid, process start time, argv, cwd, mode, plan, branch, progress log path,
    worktree path, and origin (`cli`, `t3-terminal` with its thread id, or `t3-thread` with its thread
    id and ACP session id). It never contains a token;
  - a lock file the run holds for its whole lifetime, acquired blocking by the run and probed
    non-blocking by observers. It is separate from the worktree run lock, which a resuming run acquires
    non-blocking and which an observer probe could make fail as busy;
  - an append-only event journal with sequence numbers;
  - an outcome record written before the lock is released: status (succeeded, failed, stopped), reason,
    exit code, branch and commit, report path, a bounded report summary, finalize or PR result;
  - the console log.
- Classification: lock held is running; lock free with an outcome is finished; lock free without an
  outcome is crashed.
- **Run identity and resume.** A run id names one plan on one branch across all its attempts. Running
  `loopai --detach` again for a plan whose latest run did not succeed and is not live resumes that run:
  same id and directory, a new attempt marker in the journal, the previous outcome kept as attempt
  history, and the existing worktree-resume and review-checkpoint behaviour. A run that succeeded is
  never resumed; a new launch of that plan gets a new id. There is no separate resume flag. Followers of
  a run id see the new attempt.
- Keep-awake is already bound to loopai's own pid and works unchanged. No extra power handling is added;
  a laptop that sleeps with its lid closed pauses the run, and that limitation is accepted.
- On Linux with a systemd user manager the child is started in its own transient user scope so a T3
  slice stop cannot kill it, keeping the handshake pipe. Where that is unavailable, detach works with the
  documented limitation.

### 2. Event journal and observer

- The run writes the journal through the existing `LogDecorator` and `PhaseObserver` hooks. Events cover
  attempt start (plan, branch, worktree plan path, task total), phase changes including limit waits with
  their duration, task position `n/m`, review iterations, finalize result, report, and finish (status,
  reason, exit code).
- One observer package reads the journal from any sequence number, follows it, and reports finish from
  the lock and the outcome record. It is the only source of structured state for the follow view header,
  the Mode 2 provider, and the Mode 1 title bridge. Nothing derives state by parsing the human progress
  log.
- The follow view also prints the run's human progress log verbatim, so the terminal shows the same live
  output a foreground run shows. It does not parse it.

### 3. CLI surface

Flags follow the repository's existing style of standalone commands:

- `loopai --detach [flags] <plan>` starts, or resumes, a detached run.
- `loopai --attach [run|plan|branch]` replays and follows a run's output, then prints the outcome and
  exits with the run's code. With no argument it picks the single live run of this checkout, else the
  newest. Ctrl+C only detaches.
- `loopai --runs` lists this checkout's runs with their status and origin.
- `loopai --stop <run>` stops a live run gracefully after checking that the lock is held and the pid
  matches.
- `loopai --detach --t3-thread <thread-id> [flags] <plan>` additionally shows the run in that T3 thread
  (Mode 1). The launcher reads the token from `LOOPAI_T3_TOKEN` and hands it to the child over the
  handshake pipe.
- `loopai --acp` is the ACP provider for T3's `acpRegistry` driver (Mode 2).
- `loopai-acp` becomes a launcher that only executes `loopai --acp`, so the T3 settings UI, which cannot
  pass arguments, can register it. (The UI can pass arguments; `loopai-acp` stays the documented path:
  see the source-study amendments, **Settings form and instance id**.)

### 4. T3 client

A small new client speaks protocol 2: origin from `${T3CODE_HOME:-~/.t3}/userdata/server-runtime.json`
(re-read on every reconnect, since the port can change after a restart) or `LOOPAI_T3_URL`;
`/ws?orchestrationProtocol=2` with a Bearer header; the RPC methods `terminal.open`, `terminal.write`,
`orchestration.dispatchCommand` (`thread.metadata.update`, `thread.pin`, `thread.unpin`), a thread read,
and terminal metadata subscription. Handshake and RPC errors surface the server's code and message, not
a bare HTTP status. It never writes below the T3 home.

### 5. Mode 1: same-thread terminal

- The skill gets `currentThreadId` from MCP `t3_thread_list`, mints a token with
  `<T3 home>/bin/t3 auth session issue --token-only --ttl 3d` inline so it never reaches the transcript,
  and runs `loopai --detach --t3-thread <id> [flags] <plan>` from the thread's checkout. It reports the
  run id and the follow command, then stays available for conversation. It does not monitor the run.
- When the agent's shell is sandboxed, the skill does not launch Mode 1. It explains why and offers
  Mode 2, whose run is started by the unsandboxed provider process T3 spawns.
- A bridge goroutine inside the detached run, holding the token in memory only:
  - opens one terminal with a run-specific id in the thread and types the follow command into it;
  - sets the replaceable title prefix on task and phase transitions and on finish, re-reading the current
    title before each update so a user rename is kept (see the source-study amendments, **Titles**);
  - pins the thread for the run if it was not pinned, and unpins at the end, to avoid auto-settle;
  - after a reconnect, if the run's terminal exists, its shell is alive, and it has had no child process
    for about a minute, types the follow command again. A terminal the user closed is not recreated.
    (After a server restart the bridge re-opens an absent terminal itself: see the source-study
    amendments, **Mode 1 terminal after a restart**.)
- The bridge is best-effort: an expired or rejected token, or any other T3 failure, disables it with one
  line in the progress log and never affects the run. A resumed Mode 1 run gets a fresh token from the
  skill that resumes it; a resume from a plain shell has no bridge.
- Completion is visible as the final title prefix (`done` or `failed`) and the outcome in the terminal.
  No message is sent into the thread. The final prefix stays until the next run replaces it.

### 6. Mode 2: loopai as the thread's provider

- Registration is a one-time step in T3 settings: add a local ACP provider whose executable is the
  absolute path of `loopai-acp`. The skill and documentation explain it.
- The skill finds the loopai instance through MCP `orchestrator_capabilities` (driver `acpRegistry`
  pointing at loopai) and calls MCP `t3_thread_launch` with that instance, the instance's model, the
  plan path and flags as `message`, and the caller's own checkout as the workspace (`existing_worktree`
  when the caller is in a worktree, else `root`). No token is involved. Without a registered instance it
  stops and explains registration. The user can also start Mode 2 directly in T3 by picking loopai in a
  new thread and typing the plan path.
- **Thread identity.** The provider identifies its thread by the T3 thread id, obtained through the
  thread-scoped MCP endpoint T3 injects (`currentThreadId`), and stores the thread-to-run binding under
  `.loopai/acp/`. The ACP session id is a secondary key, because T3 may replace it after a restart. If
  the thread id cannot be obtained, the session id is the key and the adoption rule below applies.
- **Routing**, in order:
  1. A run bound to this thread (or session) is the thread's run. If it is live, attach, whatever the
     text says, which covers `Continue where you left off.`. If it is crashed and the text is T3's
     continuation, or if it ended without success and the text is `resume`, relaunch it from its
     recorded argv and attach. (Refined because the Resume button sends the continuation text: see
     the source-study amendments, **Continuation text has three senders**.)
  2. A message that names a plan always starts that plan's own detached run and binds it to this
     thread. It never attaches to another run.
  3. With no binding, only T3's continuation text may adopt a run, and only a live run whose origin is
     `t3-thread`, started from this cwd, and not currently attached by another live provider process
     (an attachment lock in the run directory). Exactly one such run is adopted and bound; otherwise the
     reply lists the candidates.
  4. Otherwise the reply is a short status: the bound run's last outcome summary, and that conversation
     happens by switching the thread's model.
- One `session/prompt` stays open for the whole run, so continuation applies after a restart.
- The attach turn sends the plan entries (tasks and review stages) at once, opens one tool call for the
  current phase, retitles it and updates the plan only on transitions, and adds a slow liveness retitle
  about every 20 minutes. It streams no reasoning and no executor output.
- The final message is one message id, at most about 12 KB: summary, risk, outcome, branch, and the full
  report path. Then `end_turn`.
- **Stop.** `session/cancel` is answered `cancelled` at once. The run is stopped gracefully, as
  `loopai --stop` does, after a short grace period of a few seconds, unless a new `session/prompt`
  arrives in that window, which means the user steered or restarted the turn rather than stopping it.
  Only the thread's own run, of origin `t3-thread`, is ever stopped from a thread. The worktree is kept
  and the run is resumable with `resume`. (A best-effort interrupt-request check now guards the stop:
  see the source-study amendments, **Stop and steering**.)
- SIGTERM or stdin EOF without a cancel means T3 is going away: exit at once and never touch the run.
- The provider never sends permission requests, which would block continuation.

### 7. Skills

- `loopai-t3` (Claude and Codex) is rewritten around the two modes. It asks which mode to use, keeps the
  existing pass-through flag validation (`--task-model`, `--review-model`, `--external-reviewers`) and the
  shared launch-history snippet. Both modes record the launcher label `t3`, so the snippet and its
  pattern stay as they are. Mode 1 uses MCP `t3_thread_list` and the token mint; Mode 2 uses MCP
  `orchestrator_capabilities` and `t3_thread_launch`. The Codex skill checks that the T3 MCP tools are
  reachable, using the bounded direct attempt that T3's orchestration notes describe, before choosing a
  mode.
- `loopai-plan` (both copies) keeps a single T3 option in its launch question, which hands over to
  `loopai-t3` and its mode question, so the four-option cap is unchanged.
- The Codex `loopai` skill and any other skill that routes to `--t3-launch`, `--t3`, or the current-thread
  workflow is updated to the new flow.

### 8. Removal

Deleted rather than ported: `pkg/t3`, the Grok-impersonating `cmd/loopai-acp`, the ACP server, sink, and
chat in `pkg/acp` and `cmd/loopai/acp*.go`, ACP auto-merge and the ACP `merge`/`pr` commands,
`cmd/loopai/t3_settle.go`, the flags `--t3`, `--no-t3`, `--t3-progress-thread`, `--t3-launch`, the config
keys `t3` and `acp_auto_merge`, `processor.ModeACPChat`, the chat phase and `acp_chat.txt`, the ACP chat
signals, executor resume-session support if nothing else uses it, the Codex `resolve-thread.py` and its
tests, and the related docs and tests. A generic JSON-RPC stdio transport may be reused if it fits the new
provider. Removed flags and keys follow the repository's removed-spellings policy and fail with a message
naming the replacement. Launch-history code that keyed the `t3` launcher on the removed `cfg.T3` is
re-keyed on the new origin.

### 9. Documentation and contracts

`docs/t3-code.md` is rewritten for the new design, including registration, both modes, restart
behaviour, and limitations. The T3 and ACP paragraphs in `CLAUDE.md` are replaced. The flag changes alter
the skill contract surface, so `skillContract` and every skill's `expected=` are bumped and the golden is
regenerated. Both plugin manifests are bumped. `CHANGELOG.md` is untouched.

## Decided with the user

- Replace the previous integration instead of porting it, without preserving its behaviour.
- Two modes: a terminal in the same thread with visible progress, and a new thread that shows Working.
- The run must survive about 40 hours, so it is a detached process and T3 is only a window onto it.
- The Mode 2 thread is used for ordinary conversation after the run by switching its model.
- A detached run takes exactly one plan in this iteration; plan chains are out of scope.
- No additional keep-awake or closed-lid handling.

## Calls I made (to check while reading)

- Stop in Mode 2 stops the run gracefully after a short grace period that lets a steered turn through,
  keeps the worktree, and leaves it resumable with `resume`.
- A Mode 2 thread only ever shows or stops its own run. A message naming a plan always starts that
  plan's own run; adoption without a binding is limited to T3's continuation text and unattached runs of
  origin `t3-thread`.
- Mode 1 runs in its own loopai worktree, not in the thread's checkout, and `--detach` always implies
  worktree mode.
- A detached run never commits into the invoking checkout; the plan archive and report land on the run's
  branch.
- A detached run keeps its worktree on every exit except success.
- Resuming means launching the same plan again: same run id, new attempt. No separate resume flag.
- Mode 1 re-attaches its terminal automatically after a T3 restart, which means the detached run holds a
  T3 token, minted with a 3-day lifetime, in memory for its whole life. The token reaches the run over a
  pipe, never through its environment or a file.
- From a sandboxed agent shell, Mode 1 is refused and Mode 2 offered; `--detach` itself refuses to start
  inside a detected sandbox.
- The terminal shows the run's full live output, as a foreground run does. T3 caps terminal history.
- Mode 2 registration uses the settings UI and the argument-free `loopai-acp`, never a hand edit of
  `settings.json`, because a schema error there silently resets T3's settings and disables continuation.
- After the run the loopai provider only reports status. Chat, merge, and PR inside it, ACP auto-merge,
  T3 merge settlement, and T3 PR linking are removed.
- Mode 2's final message is a summary of at most about 12 KB plus the report path, so it survives the
  model-switch handoff.
- Mode 1 sends no completion message into the thread; the title prefix and the terminal show the result.
- Both modes record the launch-history label `t3`; `loopai-t3` asks for the mode.
- Run state lives in the invoking checkout under `.loopai/progress/runs/`, not in a global registry.
- First iteration targets macOS, with Linux using a transient systemd user scope; Windows detach is
  refused.

## Left to the builder

- The run id format, the journal and outcome schemas, retention of old run directories, and the exact
  console log name.
- How the follow view renders phases and colours.
- The exact liveness retitle cadence within the stated bound, the plan entry wording, and the Stop grace
  period within the 10-second acknowledgement window.
- How the sandbox is detected.
- Whether the token is revoked at finish, if the T3 CLI offers a revoke command.
- The order of the work and whether removal comes first.
- Reusing any generic transport code, as long as nothing of the old behaviour is preserved by accident.

## Unverified, check early against a live T3 0.1.1

The source study settled the continuation prompt and the steering shape from source, not live: see
"Amendments from the T3 v0.1.1 source study".

- The exact continuation prompt for an `acpRegistry` agent, and whether Working shows during
  `session/load` or only after `session/prompt`.
- Whether the injected MCP endpoint gives the provider its thread id (`currentThreadId`), and whether
  `<runtime_info>` carries it.
- Whether steering or `t3_queue_promote_to_steer` on an `acpRegistry` turn arrives as `session/cancel`
  followed by `session/prompt`.
- Whether `t3_thread_launch` and the composer accept the `acpRegistry` instance's model before its first
  probe, and which model id the catalog lists for it.
- The live handoff budget after a model switch.
- Whether a token can be minted from a Codex sandbox, and how a sandbox is detectable from a shell.
- Linux slice behaviour for a child in its own transient scope.

## Amendments from plan discovery (2026-10-22)

A codebase and bundle mapping pass before planning corrected or sharpened these points. Where they differ
from the sections above, these win.

- **ACP surface.** `initialize` answers ACP v1 without an `info` key (its presence switches T3 to V2
  methods) and with `loadSession: true`. `session/load` must succeed quickly for any id and restore the
  binding from disk, since the provider is idle-reaped 30 minutes after a turn. Plan entries need
  `priority`; a tool call needs `title`. Updates sent after the `session/prompt` response are dropped, so
  the final message precedes the response. Phase cards use kind `other` or `think`, never `execute`.
  Continuation text may carry a leading note, so it is matched as the last line, not by equality.
- **Background probe.** T3 calls `initialize` and `session/new` from the server's cwd about every 15
  minutes. `session/new` must not load config, open a repository, or return errors containing words such as
  "login", "credential", or "authenticate", which mark the instance unauthenticated. (Cadence and probe
  names corrected: see the source-study amendments, **Probe cadence**.)
- **Thread identity for Mode 2.** The thread-scoped MCP endpoint is streamable HTTP at the URL in the
  `t3-code` entry of `session/new`/`session/load` `mcpServers` (preferred over the spawn-time
  `T3_ACP_MCP_*` environment): `initialize`, then the `mcp-session-id` header, then `tools/call
  t3_thread_list`, whose `currentThreadId` is the ACP thread. The credential is per provider session, so
  the thread id is read right after `session/new`/`session/load` and persisted. Header values are never
  logged. (The `t3-code` entry is a stdio bridge, not a URL entry: see the source-study amendments,
  **MCP entry**.)
- **`loopai-acp` replaces itself.** On Unix it `exec`s `loopai --acp` instead of spawning it: T3 does not
  give the provider its own process group and sends SIGKILL one second after SIGTERM, which would orphan
  a spawned child. (Rationale wrong; `exec` stays: see the source-study amendments, **Process group**.)
- **Finding the loopai instance.** `orchestrator_capabilities` exposes no command path. The skill reads
  `providerInstances` in `<T3 home>/userdata/settings.json` (read-only) for the `acpRegistry` instance
  whose `config.commandPath` basename is `loopai-acp`, and the documentation recommends the instance id
  `loopai`. Its model id is `default`. `t3_thread_launch` requires `title`. (Lookup widened and no
  instance id recommended: see the source-study amendments, **Settings form and instance id**.)
- **Caller facts from one call.** `orchestrator_capabilities` returns `parentThreadId` (the caller's
  thread, null outside T3) and the caller's `runtimeMode`. Mode 1 uses `parentThreadId` and is offered
  only when `runtimeMode` is `full-access`; any other mode means a sandboxed shell. Launch-time sandbox
  signals are `SANDBOX_RUNTIME=1` (Claude Code) and `CODEX_SANDBOX` (Codex). Claude names the tools
  `mcp__t3-code__*`, Codex `mcp__t3_code__*`.
- **Token.** Minted with explicit `--scope` flags limited to what the bridge uses (terminal and
  orchestration read/operate), not the default scope set, which includes settings and provider
  management. The mint uses `T3CODE_CLI_PATH` when set, else `<T3 home>/bin/t3`.
- **Protocol framing.** Every WS Request carries `headers: []`. Stream RPCs need a client `Ack` after
  every `Chunk`, or the stream stalls after its snapshot. Each logical command gets a fresh `commandId`.
- **Mode 1 terminal after a restart.** Terminal sessions live in server memory. After a restart the
  bridge's terminal is absent from terminal metadata until the user views the thread and the UI
  re-attaches it with a fresh shell. The bridge never re-opens it: it waits for the terminal to reappear,
  then types the follow command if the shell has no child. (Replaced: the bridge re-opens its own
  terminal once after a restart; see the source-study amendments, **Mode 1 terminal after a restart**.) The first `terminal.open` uses the cwd and env
  the UI would use (`thread.worktreePath ?? project.workspaceRoot`, env `T3CODE_PROJECT_ROOT` and
  `T3CODE_WORKTREE_PATH`), with the project root from `GET /api/projects`, so a UI re-attach never resets
  the shell. The follow command uses the absolute path of the loopai binary, because the terminal shell has
  T3's `PATH`.
- **Follow view source.** The follow view prints the run's console log, which carries everything a
  foreground run prints (banner, live output, stats, finalize and PR lines) and is never truncated,
  instead of the progress log, which is archived and truncated when the plan runs again.
- **Outcome record.** It carries the report text itself, since a report sidecar may not exist (tasks-only,
  `move_plan_on_completion = false`) and the outcome must survive later runs.
- **Child process hygiene.** The handshake pipe is close-on-exec in the child, so model processes never
  inherit it. SIGHUP is handled with `signal.Notify`, not ignored, because an ignored disposition is
  inherited across exec. `CMUX_*` variables are removed so a detached run never touches cmux. The child
  receives the raw argument list and the environment-tag options, so explicit empty values survive.
- **Concurrency and stop.** A bounded launch lock per plan and branch stops two launchers from running
  the same plan. `--stop` writes a stop marker first, so a run killed by the five-second force exit is
  classified as stopped, not crashed. `--merge` and `--pr` refuse while a detached run of that branch is
  live.
- **Limit waits.** The journal derives a limit wait's duration from `wait_on_limit` on the limit-wait
  phase, because the duration label never reaches the logger chain below section timing.
- **Windows.** `GOOS=windows GOARCH=amd64 go build ./...` is part of validation for every task that
  adds build-tagged files.

## Amendments from the T3 v0.1.1 source study (2026-10-22)

A read of the T3 Code source at tag `v0.1.1` corrected these points. It replaces facts derived from the
bundle but was not exercised against a live server either. Where these differ from the sections above,
including "Amendments from plan discovery", these win.

- **`session/load` falls back.** A failed or slow `session/load` does not fail the turn. T3 starts a new
  provider process with `session/new` (a new session id) and prepends a rendered `Context handoff
  (full_thread_summary):` block of up to 16k tokens before `\n\nUser message:\nContinue where you left
  off.`. Without `loadSession` advertised, the continuation fails. The MCP thread id is therefore the
  required binding key; the session id is only a fallback.
- **Continuation text has three senders.** Besides a restart, the web Resume button after Stop (latest
  run `interrupted`) and usage-limit auto-resume send `Continue where you left off.`. T3 never sends a
  restart continuation after a Stop. Routing rule 1 therefore relaunches a bound run that is not live
  and did not succeed (crashed, stopped, or failed) on continuation text or `resume`; a live run,
  `starting` included, is attached whatever the text says, and a succeeded run gets the status reply.
- **Stop and steering.** Stop is one `session/cancel`, acknowledged within 10 seconds; the same process
  and session serve the next prompt. Steer, promote-to-steer, and MCP `t3_thread_send` in auto mode are
  interrupt-restart: `session/cancel`, a wait for the prompt to end, then a new `session/prompt`,
  usually unwrapped. A steer records no `run_interrupt_request`; a Stop does. v0.1.2 previews also send
  `session/cancel` when T3's own event ingestion fails a run. The Stop grace is measured from loopai's
  `cancelled` answer. Before stopping, the provider reads the thread's latest run through MCP
  `t3_thread_read` (best-effort, isolated, at most 5 seconds, with the session's credential) and stops
  only when a user interrupt request is recorded; if the read fails or the item kind is unavailable, it
  stops. A steered prompt that names a plan while the bound run is live re-attaches it. Every finished
  run ends with `end_turn`, the failed or crashed status stated in the final message, never a JSON-RPC
  error.
- **Probe cadence.** T3 refreshes provider readiness about every minute while provider status is in
  demand. Only `ready` is cached (15 minutes); failures retry every minute. The probe clientInfo names are
  `t3-code-provider-test`, `t3-code-session-manager`, and `t3-code-provider-auth`. The last sends
  `initialize` only, then EOF, and must cause no side effect and no error output.
- **Process group.** On macOS the `acpRegistry` provider leads its own process group (Effect's default
  `detached` on non-Windows); T3 signals the group with SIGTERM, then SIGKILL after 1 second. The "no
  process group" rationale was wrong. `loopai-acp` still `exec`s `loopai --acp`: it is harmless and keeps
  one stable process. `loopai --acp` never exits by itself during a held turn: it recovers handler panics
  and keeps serving, and exits promptly on SIGTERM or stdin EOF.
- **Settings form and instance id.** The local-ACP settings form has an Arguments editor, one literal
  argument per row; only the add dialog seeds `commandArgs: []`. The default instance id is
  `acpRegistry_local_acp`, not `loopai`, and it is fine to keep. Registration still documents the
  argument-free `loopai-acp`, or `loopai` with the argument `--acp`. The skill's read-only lookup accepts
  either shape, never assumes the instance id, tolerates unknown keys and value types, and reads only
  `models[].id` from `orchestrator_capabilities`.
- **MCP entry.** The `t3-code` `mcpServers` entry is a stdio `acp-mcp-bridge` (the T3 executable plus that
  argument), never a streamable-HTTP URL entry. Its `env` array carries `T3_ACP_MCP_ENDPOINT` and
  `T3_ACP_MCP_AUTHORIZATION`, which loopai prefers over the spawn-time environment; it speaks streamable
  HTTP to that endpoint directly, in the sequence of T3's own bridge client. Advertising
  `mcpCapabilities.acp` would make T3 send an entry with no URL, so loopai never advertises it. The
  credential is touched hourly during a running run, and issuing a new one for the thread revokes the old.
- **Prompt wrapping.** Wrapping follows per-session instruction state, recorded after any non-error
  prompt response, a cancelled one included. A steered prompt after a cancel therefore usually arrives
  unwrapped.
- **Continuation preconditions.** Besides the skips listed above, a restart continuation needs a running
  provider turn, an active provider thread, a session that is not stopped or errored, and a thread that
  is not archived, settled, or snoozed. If `loopai-acp` dies by itself while T3 is up, the turn is
  finalized `failed` and never continued; the user types `resume`.
- **Mode 1 terminal after a restart.** Terminals live only in server memory. `terminal.open` of an
  unknown id creates a fresh shell seeded from the persisted history, while a UI re-attach after a
  restart is not guaranteed: it needs that client's localStorage and the thread being viewed. Re-opening
  a live id with an identical launch context only resizes it. `terminal.write` fails with
  `TerminalNotRunningError` while the terminal is `starting` or `error`, and drops data once it has
  `exited`. After a reconnect where the server restarted (runtime pid or `startedAt` changed, or the
  connection was lost), the bridge therefore calls `terminal.open` once with the identical UI launch
  context when its terminal is absent and no `remove` for it was seen during a live connection, waits
  until it runs, then types the follow command. A terminal the user closed is still never recreated. The
  follow command is never run through `exec` and never ends in `; exit`.
- **Titles.** There is no compare-and-set, and any title write clears a pending `titleRegeneration`. The
  bridge waits, bounded at about 2 minutes, until `titleRegeneration` is null before its first prefix
  write; a later regeneration is overwritten at the next transition. A 426 or an undecodable reply is a
  typed "protocol unsupported" error that disables the bridge.
- **Pin and auto-settle.** In code a pin protects a thread from auto-settle, contrary to T3's user docs;
  `thread.auto-settle.set` also exists. Mode 1 keeps its pin.
- **Background work never shows Working.** In an ordinary thread a held Monitor shows a grey "Waiting",
  and a background Bash shows nothing and dies about 4.5 hours after the last provider event. Mode 1
  keeps its bearer-token bridge; no Monitor or scheduler follower is added.

## Testing

Unit tests follow the repository rules: a fake protocol-2 server that enforces the query parameter and
the header; scripted JSON-RPC sessions for the provider, including the envelope, continuation, cancel
with and without a following prompt, SIGTERM, the session-id fallback, and the routing rules (a plan
message never adopts another run; a thread never stops another thread's run); detach tests with a stub
child covering the handshake, lock, outcome, crash classification, attempts on resume, and token
handling; and journal and observer tests. A live checklist on T3 0.1.1 covers both modes end to end, a T3
restart mid-run in each, Stop and resume in Mode 2, and a model switch after the report.
