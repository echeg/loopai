# T3 Code integration

[T3 Code](https://t3.codes) is a control surface for coding agents with desktop, web, and mobile
clients. loopai integrates with it through T3 Code's public, authenticated HTTP and WebSocket
APIs. No T3 Code fork or plugin is involved, and loopai never writes below the T3 home directory:
it only reads the server's runtime file and changes everything else through the API.

The integration is best-effort. When the server is not running, the token is missing or rejected,
or a request times out, loopai prints one warning and the run continues exactly as without it.

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
| Finished | `<plan> · done`, `<plan> · failed` |
| Stopped before completion | `<plan> · stopped` |

`<plan>` is the plan filename without its date prefix, or `loopai` for review-only runs. The thread
records the branch; it also records the checkout path unless loopai runs with `--worktree`, whose
checkout is removed after a successful run. A thread created this way starts no agent session in
T3 Code. Titles change only on phase and task transitions, never per output line.

`LOOPAI_T3_THREAD_ID` makes the run update an existing thread instead of creating one; the
launcher below sets it.

## Launching a plan in T3 Code: `--t3-launch`

```bash
loopai --t3-launch [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST] docs/plans/<plan>.md
```

Run from the repository root, `--t3-launch`:

1. asks T3 Code to create a worktree it manages (in `~/.t3/worktrees` by default) on a new branch
   named after the plan, cut from the current branch or, on a detached HEAD, the current commit,
   and verifies that the new worktree checks out the same commit;
2. copies the plan (so uncommitted edits carry over) and any `.loopai/config`, `prompts/`, and
   `agents/` files the worktree lacks;
3. creates a thread bound to that worktree and branch;
4. opens a terminal named `loopai` in the thread and types `loopai --t3 [flags] <plan>` there, with
   `LOOPAI_T3_TOKEN`, `LOOPAI_T3_URL`, and `LOOPAI_T3_THREAD_ID` in the terminal environment.

It then prints the thread id, worktree, and branch and exits; the run continues in the thread
terminal, visible on every T3 Code client. loopai runs there without `--worktree`, so the worktree
survives the run for review and close-out. Only the three flags above are forwarded, and the two model specs need a `claude` or `codex`
prefix; the rest comes from `.loopai/config`. If a step fails after the worktree exists, nothing is rolled back and the
error lists what was already created.

The `loopai:loopai-t3` Claude Code skill and the `$loopai-t3` Codex skill wrap this command, mint a
token inline when `LOOPAI_T3_TOKEN` is unset, and report the result. `loopai-plan` offers the launch
when a T3 Code runtime file exists.

## Pull requests

With `t3` enabled and `LOOPAI_T3_TOKEN` set, `loopai --pr` links the created GitHub pull request to
every non-archived thread of the repository's project whose branch is the feature branch. T3 Code
then tracks the PR and settles the thread when it merges. A linking failure is a warning; the PR is
already created.

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

## Limitations

- Status is title-only. Adding activity rows or messages to a thread, custom MCP tools, or reading
  OSC titles from the terminal would need a T3 Code fork.
- `--pr` linking understands GitHub pull request URLs only, matching what `--pr` creates.
- The token grants full access to the T3 Code server. It lives in the thread terminal's
  environment for the run; revoke it with `t3 auth session revoke` when no longer needed.
