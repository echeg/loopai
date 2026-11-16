# Host integration: foreground runs with `--journal-dir`

A host process (an agent platform, a CI job, a supervisor) that spawns loopai for one plan can follow
the run and read its result as structured files instead of parsing the progress log:

```bash
loopai --journal-dir /var/lib/host/runs/42 docs/plans/feature.md
loopai --journal-dir /var/lib/host/runs/42 --tasks-only docs/plans/feature.md
```

The run writes two files into the directory, in the same schema a detached run (`--detach`) uses for
its run directory:

| File | Content |
|---|---|
| `events.jsonl` | The event journal, one JSON object per line, appended while the run executes. |
| `outcome.json` | How the run ended, written once when execution returns. |

Nothing else is written there. The run is owned by the host: it is not a registered detached run,
so `--runs`, `--attach`, and `--stop` do not see it, and it writes no `run.json`, `run.lock`,
`console.log`, or `report.md` (the report is inside `outcome.json`). The usual progress log is still
written to `.loopai/progress/progress-<plan>.txt`, and the human output still goes to stdout.

## Spawning the run

- Start loopai from the repository root of the checkout the plan should run in. The run executes
  in that checkout, as `loopai <plan>` without `--worktree` does: on the default branch it creates
  and checks out the plan branch; on any other branch it runs there. `use_worktree = true` in the
  configuration is ignored for this run.
- Exactly one plan file, in full mode or with `--tasks-only`. The flag is refused, before any
  provider starts, with `--worktree`, `--commit`, `--review`, `--external-only`, `--plan`,
  `--gen-agents`, `--serve`, `--watch`, `--detach`, `--cmux-workspace`, `--acp`, every standalone
  command (`--init`, `--reset`, `--dump-defaults`, `--clear`, `--merge`, `--pr`, `--report`,
  `--attach`, `--runs`, `--stop`), a plan chain, a missing plan argument, or extra plan arguments.
- The run is non-interactive: a missing plan is an error rather than a selection prompt, a
  repository without commits is an error rather than an offer to create the initial commit, no pause
  handler or manual-break signal (Ctrl+\) is installed, and the cmux and Orca reporters are not
  started (`orca = true` and `--orca` are ignored, and the cmux status pill a previous run left is
  not cleared). An invocation with `--cmux-workspace` is refused rather than handed off, since a
  hand-off would run the plan in another process. Give the child its stdin on the null device.
- The run keeps loopai's other behavior: the configured providers, reviewers, `finalize` mode, and
  report, notifications, and the plan archive. On success the plan is moved to
  `docs/plans/completed/` and committed on the run's branch in this checkout. Pass
  `--skip-finalize` when the host closes the branch out itself.
- It works inside a sandbox that `--detach` refuses (`SANDBOX_RUNTIME`, `CODEX_SANDBOX`, a jail
  started with `--die-with-parent`), because nothing outlives the loopai process. The jail must
  allow writes to the checkout (`.loopai/` included), the journal directory, and the global config
  directory (`~/.config/loopai`): every run installs missing default prompts and agents there,
  rewrites a config file that is missing or holds only comments, and records the launch in
  `launch-history`. To keep a read-only home, point `LOOPAI_CONFIG_DIR` or `--config-dir` at a
  writable directory whose `config` sets at least one key; that also keeps the host's runs out of the
  user's launch history.

## The directory

`--journal-dir <dir>` is checked before configuration is loaded, and a refusal starts no provider:

- A relative path is made absolute against the working directory.
- A missing directory is created with mode `0700`; a path that exists and is not a directory is
  refused.
- A directory that already holds `events.jsonl` or `outcome.json` is refused: both files belong to
  one run. Use a new, empty directory for each run. `events.jsonl` is also created exclusively, so a
  second process given the same directory at the same moment fails instead of appending to it.
- A directory inside the repository work tree is refused unless git ignores both `events.jsonl` and
  `outcome.json` there, because review stalemate detection fingerprints untracked file content and
  journal writes would read as code changes. Prefer a directory outside the checkout. The check is
  repeated once the plan's branch is checked out, before the attempt starts: an existing plan branch
  whose `.gitignore` does not ignore the files ends the run with a `startup:` outcome.

## `events.jsonl`

Every line carries `seq` (1, 2, ... in file order), `time` (RFC 3339, UTC), `attempt` (always `1`
for a foreground run), and `type`, followed by the fields of its type only. Readers must ignore an
event type they do not know; new types may be added without a version change.

| `type` | Fields |
|---|---|
| `attempt_start` | `mode` (`full` or `tasks-only`), `plan`, `branch`, `worktree`, `worktree_plan`, `progress_log`, `console_offset`, `tasks` (`[{"n":1,"title":"..."}]`), `task_total`, `stages` (in order, from `review`, `external_review`, `finalize`, `report`; empty for `tasks-only`) |
| `phase` | `phase` (`task`, `review`, `external-review`, `external-eval`, `finalize`, `report`, `limit-wait`), `wait_seconds` for a limit wait |
| `task` | `n` (the plan position of the task section entered), `m` (the plan's task count), `title` (empty when the section names no remaining task) |
| `tasks` | `statuses`: `[{"n":1,"status":"pending|active|done"}]` for every task, re-read from the plan |
| `review` | `kind` (`internal`, `external`, `custom`, `evaluation`), `iteration`, `label` |
| `usage` | `provider`, `model`, `phase`, `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, and `cost_usd` when the provider reported a cost |
| `finalize` | `mode`, `status_note`, `pr_url`, `merged`, `incomplete` |
| `report` | `summary` (the report's Summary section, at most 4 KiB), `risk` |
| `finish` | `status` (`succeeded`, `failed`, `stopped`), `reason`, `exit_code` (`0`, `1`, `2`) |

For a foreground run `attempt_start.plan` and `attempt_start.worktree_plan` are the same absolute
plan path, `worktree` is empty, `console_offset` is `0`, `progress_log` is the absolute path of the
progress log, and `branch` is the branch checked out when the attempt started (after the plan branch
was created). An attempt ends with `finalize` when the close-out ran, `report` when the run produced
a report, and always `finish`, in that order; a run that did not succeed records a last `tasks`
event before them.

A `usage` line looks like this:

```json
{"seq":7,"time":"2026-11-15T10:04:12Z","attempt":1,"type":"usage","provider":"claude","model":"claude-opus-4-1","phase":"task","input_tokens":1520,"cached_input_tokens":48210,"cache_write_input_tokens":6100,"output_tokens":2380,"cost_usd":0.42}
```

One `usage` event is appended per finished provider session that reported usage: every task
iteration, review, evaluation, finalize, and report session, and every retry after a provider limit,
which is a session of its own. `provider` is `claude` or `codex`; `phase` is the phase the session
ran in. A session that hit the session timeout still reports the usage it returned.

### Token convention

`input_tokens` excludes cached tokens for both providers, so the billable input is
`input_tokens + cached_input_tokens + cache_write_input_tokens`, each at its own price:

- Claude: `input_tokens` is the `usage.input_tokens` of the stream's `result` record,
  `cached_input_tokens` its `cache_read_input_tokens`, `cache_write_input_tokens` its
  `cache_creation_input_tokens`, and `cost_usd` its `total_cost_usd`.
- Codex: the counts are the last `token_count` record of the session's rollout file. Codex's own
  `input_tokens` includes both the cache reads and the cache writes, so loopai subtracts
  `cached_input_tokens` and `cache_write_input_tokens` from it.
  Codex reports no cost, so `cost_usd` is absent.

### Limits

- Only the main session is counted: a Claude Task subagent or a Codex child session is not reported
  separately, and whether the provider's totals include them is not verified.
- `cost_usd` is absent for Codex and for Claude-compatible wrappers that report none; price the
  tokens yourself in that case.
- `model` is the single model the Claude result record names, else the configured model, which can
  be an alias such as `opus`, or empty when neither is known. For Codex it is the rollout's
  `turn_context` model, else the configured one.
- A session killed before its result (Claude) or before its first `token_count` record (Codex)
  yields no `usage` event. The custom review script reports none.

## `outcome.json`

Written when execution returns, after the `finish` event. Because it comes after `finish`, it may not
exist yet at the moment a reader sees that event: read it once the loopai process has exited.

| Field | Content |
|---|---|
| `version` | The schema version, currently `1`. |
| `run_id` | Empty for a foreground run. |
| `attempt` | Always `1`. |
| `status` | `succeeded`, `failed`, or `stopped`. |
| `reason` | Why the run did not succeed; empty on success. A run that ended before its attempt started has a reason beginning with `startup:`. |
| `exit_code` | `0` succeeded, `1` failed, `2` stopped. |
| `branch` | The run's branch. |
| `commit` | The commit the branch was left at. |
| `worktree_kept` | Empty for a foreground run. |
| `report` | The completion report (Markdown, at most 256 KiB), when the run produced one. |
| `report_summary` | The report's Summary section, at most 4 KiB. |
| `risk` | The report's risk level. |
| `finalize` | `{"mode","status_note","pr_url","merged","incomplete"}`, the close-out result. |
| `plan_moved` | Whether the plan was archived to `docs/plans/completed/`. |
| `archive_incomplete` | Why the plan archive stopped short, when it did. |
| `notes` | Warnings recorded while completing the run, such as a failed journal append. |
| `finished_at` | When the outcome was written (RFC 3339, UTC). |

## Exit codes and interruption

The loopai process exits `0` on success and `1` otherwise; `--journal-dir` does not change that.
The outcome's `exit_code` is the detached-run classification, which differs only for a stop: a run
stopped by SIGINT or SIGTERM exits `1` while its outcome records `stopped` with `exit_code` `2`.
Read the result from `outcome.json`, not from the exit code alone.

SIGINT or SIGTERM cancels the run. When it returns within the five seconds loopai allows before its
forced exit, the journal ends with `finish` and `outcome.json` records `stopped`. A forced exit, a
SIGKILL, or a crash leaves no `finish` event and no `outcome.json`: treat a non-zero exit without
`outcome.json` as a run that did not complete.

A run that fails after the directory was accepted but before its attempt started, from configuration
loading, a missing provider binary, or the repository checks through plan selection and branch
creation, leaves an empty `events.jsonl` and an `outcome.json` recording `failed` with a `startup:`
reason; a stop in that window is recorded the same way. Only a refusal of the directory itself, or of
the flags, writes nothing into it.

If writing `outcome.json` fails, loopai prints a warning on stderr and keeps its exit code. If
creating the journal or recording `attempt_start` fails, the run stops before any provider starts.
