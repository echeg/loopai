# T3 Code integration

[T3 Code](https://t3.codes) is a control surface for coding agents with desktop, web, and mobile
clients. loopai integrates with it through T3 Code's public, authenticated HTTP and WebSocket
APIs. No T3 Code fork or plugin is involved. loopai reads the server's runtime and settings files,
changes threads through the API, and can invoke the official CLI to issue a settlement token. An
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
config key for it. Launching and title reporting require that token; local-merge settlement can
issue its own short-lived token as described below.

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

`--no-t3` disables this reporting for one invocation, overriding `--t3`, `LOOPAI_T3`, and
`t3 = true`. It also disables run-time PR linking through this reporter. Local-merge settlement
remains independent. It cannot be combined with `--t3-launch`, which creates a thread itself.

## Keep the current Codex thread

Inside T3 Code, the Codex `$loopai-t3` skill defaults to running in the current conversation
when native subagents are available. Discussion, the plan, status checks, the completion report,
and a later merge stay in that conversation. Ask for a new thread explicitly to use the launcher
below. The Claude Code skill and the `--t3-launch` CLI keep their separate-thread behavior.

The skill delegates one ordinary `loopai --worktree --skip-finalize <plan>` run to a
native Codex subagent. loopai owns the isolated Git worktree; creating it creates no T3 thread.
The thread keeps its original Codex provider, while loopai uses its configured task and review
models. The original agent can finish its reply: T3 keeps the thread in **Working** while the
subagent monitors the process. The subagent must stay active until the process exits; starting a
detached shell alone does not keep this status.

The parent resolves its native `CODEX_THREAD_ID` to exactly one active T3 thread **before**
spawning the observer, then passes `--t3-progress-thread ID`. The sidebar keeps its native
Working badge while the thread title shows the actual loopai phase:

| Run state | Current thread title |
|---|---|
| Task 3 of 7 | `[loopai: task 3/7] Original conversation name` |
| Internal review, second pass | `[loopai: review · iteration 2] Original conversation name` |
| External review | `[loopai: external review · iteration 1] Original conversation name` |
| Report | `[loopai: report] Original conversation name` |
| Finished / failed / stopped | `[loopai: done]`, `[loopai: failed]`, or `[loopai: stopped]` followed by the original name |

The prefix comes first so it remains visible in a narrow sidebar. Only a recognized loopai
prefix is replaced on subsequent runs; a manual rename is preserved at the next transition.
This mode changes only the existing thread's title. It never creates a thread, changes its
provider/branch/worktree, pins it, or links a PR. It suppresses ordinary T3 reporting even when
`--t3`, config or environment enables that reporting. It conflicts with `--no-t3`, `--t3-launch`,
ACP and standalone commands that do not execute a plan or review.

Title updates require API credentials. An explicit `LOOPAI_T3_TOKEN` is used as usual; otherwise
loopai issues a 7-day token through the local `t3` CLI (falling back to `npx --yes t3@latest`) and
keeps it in memory. Without an explicit token, a URL override must match the local runtime.
Tokens are never put in observer arguments, state or logs. Authentication or title-update
failures warn once and leave plan execution and native Working unaffected.

T3's public shell API does not expose the native Codex session binding. The skill's bundled
resolver therefore reads only `provider_session_runtime` and active `projection_threads` from
`state.sqlite` beside the selected T3 home's `userdata/server-runtime.json`, using SQLite's
read-only mode. It never writes that database or guesses from a thread title/project. This
binding schema is an internal T3 interface: missing files, schema changes, no match or multiple
matches disable titles, and the skill uses `--no-t3` instead. Every title update also checks the
exact ID through the public shell API. The resolver also rejects a `LOOPAI_T3_URL` for a different
runtime, even with an explicit token. The native observer's own session ID is not the parent's.

Current-thread mode leaves close-out to the user: `--skip-finalize` overrides automatic finalize
and the ordinary CLI does not use ACP auto-merge. Review the report and use `$loopai-merge <plan>`
from the original conversation. `loopai --report <plan>` and `loopai --merge <plan>` can resolve
the feature branch from the original checkout even after the successful run removes its worktree.

The observer saves its log and exit status under the source checkout's `.loopai/progress/`.
Later questions inspect that existing run instead of starting it again. Messages in the original
conversation reach Codex, rather than being ACP prompts that cancel the plan. While loopai runs,
avoid making competing changes in its worktree. A failed run is reported and never retried silently.

When the observer finishes, T3 removes the background Working state; this does not settle the
thread or merge the branch. In the tested Codex harness the child result becomes available on
the next user turn, but does not automatically start a new parent reply. Do not expect an
unsolicited final message. A stopped provider session or server restart can remove the observer;
the durable log does not itself keep Working, and process survival must be checked before
reattaching a watcher. Native subagents are required for this mode; the skill checks their
availability rather than assuming every Codex installation supports them.

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

## Automatic settlement after a local merge

After a successful `loopai --merge`, loopai marks matching T3 Code threads **Settled**. This runs
independently of `--t3` or `t3 = true`: the repository's registered worktree roots identify its
projects, and the merged feature branch identifies the threads. The invoking thread id is not
used. Archived and already settled threads, and threads awaiting approval or input, are skipped.
A failed merge never settles anything. Cleanup must also succeed for `--merge`.

ACP automatic merges and the thread's `merge` command do the same, including an already merged
branch. They wait until the final answer has been sent and T3 marks the completed turn idle,
because T3 rejects settlement during a running turn. The wait is bounded to 15 seconds. A new
message or turn cancels the pending settlement. New commits on the branch, or a base that no
longer contains the merged commit, also prevent it.
T3 may stop the provider session when settling, so loopai saves the report and chat state first.
The thread's branch and worktree remain available, and sending another message reopens the thread.
The CLI `--merge` does not wait for active agent turns: such a thread is left open with a warning.

When `LOOPAI_T3_TOKEN` is absent, settlement discovers the local server runtime and invokes
`t3 auth session issue --token-only --ttl 1h --label loopai-settle`, falling back to
`npx --yes t3@latest` when `t3` is not installed. Issuance has a 15-second timeout, uses the same
T3 home, and never prints or stores the token in loopai configuration. The official CLI records
the session in T3's authentication store. A different `LOOPAI_T3_URL` requires an explicit token;
a locally issued credential is never sent to that override. Without a local runtime, settlement
is silently skipped. Other failures warn and leave the successful Git merge intact.

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
message is read; the runtime instructions T3 Code appends are ignored. A malformed launch fails
the turn and shows the usage line. loopai treats a message as a launch when its first word is an
option, or when its first word ends in `.md` or names an existing file in the thread's working
directory and nothing but options or another plan follows it. Anything else, a message that starts
with a file name and goes on in prose (`README.md is outdated`) included,
such as a question typed after a run or a one-word reply like `thanks`, does not start a run: it is
one of the thread commands or a chat turn, described under [Thread chat](#thread-chat).

The plan runs in place in the thread's working directory, without `--worktree`: T3 Code owns the
thread's worktree, as with `--t3-launch`. On the default branch, loopai creates the plan branch in
that checkout as an ordinary run does. `t3`, `orca`, and `use_worktree` are forced off for the run.
A manually created thread copies nothing into its directory, and a new worktree T3 Code creates
holds committed files only. For manual starts, commit the plan first, and commit `.loopai/config`,
`prompts/`, and `agents/` overrides or move them to the global config when they must apply there.
An agent-mode `--t3-launch` carries those inputs over before sending the turn, so uncommitted plans
and missing local overrides are available without committing them first.
A plan file that is missing is an error rather than an interactive selector. So are an empty
repository and other conditions that would otherwise prompt. Only full plan execution can be
launched: review-only modes, plan creation, and interactive questions are out of scope.

After a successful run, loopai merges the plan branch into the local base branch if the completion
report rates Risk `low` or `medium`. The base is `default_branch` from config, else the detected
`main` or `master`, without any `origin/` prefix, and it must exist as a local branch. The merge
runs in the worktree that already has the base checked out, normally the primary checkout. It is
an ordinary `git merge` of the commit the run completed on, resulting in a fast-forward, a merge
commit named after the plan branch, or `already up to date`, and loopai then verifies that commit is
part of the base. Commits added to the plan branch after the run are never merged with it. Two
threads finishing at once merge one after the other: the merge waits up to two minutes for another
loopai process's repository lock. Nothing is pushed. The plan branch is not deleted and the thread's worktree is not
removed, because T3 Code owns that worktree and the provider session runs inside it. On Windows a
removal would fail for that reason alone. Once the thread is no longer needed, remove both by hand
or through T3 Code.

loopai skips the merge and leaves the repository unchanged when:

- `acp_auto_merge = false` is set in config. The key defaults to `true` and applies only to this mode.
- `finalize` is `pr` or `merge`, because GitHub then owns the close-out. With `finalize = sync` the
  sync runs first and the merge follows only when it succeeded; a `finalize incomplete` sync skips
  the merge, since the base and the branch did not combine or validate cleanly.
- the report rates Risk `high`, states no level, or is the facts-only fallback.
- the thread's checkout has a detached HEAD or is on the base branch, or the base branch does not
  exist locally.
- the thread's checkout or the base worktree has uncommitted changes, untracked files that are not
  ignored, or an unfinished Git operation such as a merge or rebase. Untracked `.loopai/config`,
  `prompts/`, and `agents/` overrides do not count, since `--t3-launch` copies them into the
  thread's checkout uncommitted. If the plan branch committed a file at one of those paths, Git
  refuses to overwrite the base worktree's untracked copy and the merge is skipped.
- no worktree has the base branch checked out. loopai never switches a checkout to the base for you.
- the plan branch moved after the run completed, or another loopai process held the repository lock
  for the whole wait.
- the merge conflicts or fails verification. loopai aborts it and the base stays where it was.

A canceled run never merges.

### Thread chat

The thread stays usable after a run, so questions about the report, small fixes, and the merge
happen where the report and worktree are. loopai routes every message that is not a plan launch:

- `merge` or `мерж`, and `pr` or `пр`: the whole message, trimmed and case-insensitive, runs the
  action directly, with no model.
- Anything else is a chat turn: one session of the `review_model` provider (falling back to
  `task_model`'s, as for the review phase) in the thread's working directory. A `--task-model` or
  `--review-model` the thread's last run was launched with applies to its chat too, so the chat
  answers through the model that reviewed the run. It reads files, answers, edits, and commits on
  the thread's branch, staging only the files it changed.

A chat turn streams the model's output as reasoning and ends with its answer as the final message.
It shows no plan steps, and no activity other than a provider-limit wait and the `loopai` heartbeat
card after 4 silent minutes. The stop button cancels it, and a message sent while it runs
steers it, as with a plan run. The prompt is the embedded `acp_chat.txt`, overridable in
`.loopai/prompts/` or the global prompts directory. The first turn of a provider session carries
the branch, the base, `git log <base>..HEAD --oneline` (at most 50 commits), the last run's report,
and its auto-merge outcome. Later turns send only the message and resume the provider session.
A merge or pull request runs outside the provider session, whether typed as a command or requested
by an answer, so its outcome is kept and prepended to the next chat message: a follow-up such as
"why did that fail?" reaches a model that saw the refusal. Resume works
for Claude through `--resume <id>` after the configured arguments, `claude_args` included, and Codex
through `codex exec resume <id>`. A resume that fails for any reason other than a provider limit,
a session timeout, or cancellation is retried once as a fresh session with the full context. That
includes a configured error pattern such as `API Error: 400`, which is how a provider rejects a
transcript it can no longer continue. Wrapper commands (a `claude_command` or `codex_command` that is not the real CLI) define
their own interface, so they never resume: every message starts a fresh session with the run's
context, and the answer ends with a note saying so. Without a recorded run, chat still works from
the branch, the base, and its commits.

The prompt forbids `loopai --merge` and every other loopai command, `git worktree remove`, branch
deletion, switching to another branch, `git push`, and `git reset --hard`, because T3 Code runs
every later message in this worktree. The model cannot merge or open a pull request itself. When
you ask for one in your message, the answer ends with `<<<RALPHEX:ACP_MERGE>>>` or
`<<<RALPHEX:ACP_PR>>>`; loopai strips the marker, performs the action, and appends its outcome.
Only the prompt guards against a marker you did not ask for: the merge is local, reversible, and
gated like the command.

`merge` merges the thread branch's current head into the local base branch. The base is the last
run's base, else `default_branch` or the detected `main` or `master`. Unlike the auto-merge, it
applies none of the policy gates (`acp_auto_merge`, `finalize`, the report's Risk), because you
asked, and it takes commits added after the run: the `## Merge` section lists them. Every repository
gate of the auto-merge still applies: a non-detached branch other than the base, a local base, both
checkouts clean with no Git operation in progress (untracked local overrides excepted), the base
checked out in some worktree, and the repository lock. A branch the base already contains is
reported as already in the base, and nothing changes. Nothing is pushed, and neither the branch nor
the thread's worktree is removed.

`pr` reports the URL of a pull request already open for the branch. Otherwise it pushes the branch
and opens one against the base through the same code as `loopai --pr`, with the body built from the
last run's report when it was on this branch, otherwise the body `--pr` would build. It needs `gh` and an `origin` that points
at the same GitHub repository. The worktree is untouched.

A refused merge or pull request, such as a dirty checkout, a base checked out nowhere, a conflict
that was aborted, or a missing `gh`, ends the turn normally with the reason. A session that cannot
be opened, such as an invalid config or a working directory that is not a repository, fails the
turn, and so does a chat session that fails: a provider error, a limit without `wait_on_limit`, a
timeout, or a missing provider binary. A failed chat turn keeps the previous provider session.

Each session keeps its state in `.loopai/acp/<session-id>.json` in the thread's working directory,
written atomically with mode `0600`: the chat provider, its session id, and the last plan run (plan,
branch, base, the commit the run completed on, the report, the auto-merge outcome, and the model
overrides the run was launched with), and the outcomes of up to five merge or pull request actions
not yet shown to the chat provider. loopai adds
`acp/` to the generated `.loopai/.gitignore`, so the file never counts as an untracked path for the
merge gates. A new plan run replaces the last run and drops the provider session and pending
action outcomes, so the next chat
starts fresh about the new run. Changing `review_model` to another provider also starts a fresh
session. A canceled or failed chat turn saves nothing and keeps the previous session id. A corrupt state file
is reported on stderr and treated as empty.

### What the thread shows

| loopai event | Thread |
|---|---|
| Run in progress | "Working" from the first event until the turn ends |
| Plan tasks and post-task stages (review, external review, finalize, report) | Plan steps and sidebar progress, each `pending`, `in_progress`, or `completed` |
| Phase change | An activity such as `task 2/5`, `review · iteration 1`, or `external review evaluation`, completed when the next one starts |
| Executor output | Streamed reasoning, at most one update every 500 ms, with oversized chunks truncated |
| Completion report | The final assistant message |
| Auto-merge after a successful run | A `## Merge` section after the report, either ``Merged `<branch>` into `<base>` (<kind>, `<sha>`). Not pushed.`` or ``Not merged into `<base>`: <reason>.`` |
| Skipped merge or failed run | A closing `## Next steps` section: write questions, edits, `merge`, or `pr` in this thread |
| Failed run | A failed turn carrying the failure as its error; a completion report, when one was produced, is sent as the final message first |
| `merge` or `pr` message | A `## Merge` or `## Pull request` section with the outcome or the reason nothing happened |
| Any other message that is not a plan launch | A chat turn: the model's output as streamed reasoning and its answer as the final message, followed by the outcome of a merge or pull request it was asked for |

The stop button cancels the run, and the turn ends as cancelled. The plan stays in place with the
tasks completed so far. One run or chat turn executes at a time per loopai process. A message that reaches loopai
while a run is active steers it: the Grok driver cancels the running turn and sends the new message,
so the current run stops as if canceled and the new message starts its own run once the canceled
one has ended. Each run and chat turn takes its own keep-awake hold when
`keep_awake` is on, released when it ends.

A thread keeps working after its provider session stops, for example when T3 Code restarts or reaps
an idle session. T3 Code then resumes the saved session with `session/load` rather than starting a
new one. loopai accepts any saved session id and records the thread's working directory under it.
It replays no history, but the session state file outlives the process, so the next chat message
resumes the same provider conversation and still knows the last run.

### Watchdog and heartbeat

The Grok driver fails a turn after 10 minutes without content or tool progress, or 30 minutes with
an open activity. A provider-limit wait or a long silent executor step can exceed that. When
nothing has been sent for 4 minutes, loopai updates the open activity's title with the elapsed wait,
such as `task 3/7 · waiting 8m`, and repeats every 4 minutes until output resumes. T3 Code's
provider session reaper skips threads with an active turn, so a long run is not idled out.

### Troubleshooting

Each run writes its full log to `.loopai/progress/progress-<plan>.txt` in the thread's working
directory, and chat turns write theirs to `.loopai/progress/progress-acp-chat.txt`. Human-readable output goes to the agent's stderr, never to stdout, which carries only
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
