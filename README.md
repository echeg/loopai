# loopai

Autonomous implementation-plan execution with Claude Code or OpenAI Codex.

loopai is a local CLI for running structured engineering plans from the root of a Git repository. Each task runs in a fresh agent session, validation is performed between tasks, and completed work can pass through internal and cross-provider review phases. The result is committed work with a persistent progress log, without requiring an IDE plugin or hosted service.

This repository is a personal fork. The loopai CLI is installed by building
from source; no packaged CLI releases are published here. Optional Claude Code
workflows are distributed through this repository's plugin marketplace.

## Features

- Executes Markdown plans one task at a time with automatic retries
- Creates plans interactively with `--plan`
- Supports Claude Code and Codex as primary executors
- Runs configurable internal and external review phases
- Adds project-specific review agents, drafted for the repository by `--gen-agents`
- Creates a branch automatically and optionally uses isolated Git worktrees
- Commits after completed tasks and review fixes
- Files out-of-scope findings to a committed backlog instead of fixing or dropping them
- Records accepted, rejected, and deferred plan-critique points in the plan's Decision Log
- Streams timestamped progress to `.loopai/progress/`
- Serves a real-time web dashboard with `--serve`
- Reports live and persistent completion status to the cmux sidebar when available
- Optionally hands a run off to its own cmux workspace with `--cmux-workspace[=always|auto]`
- Sends optional Telegram, email, Slack, webhook, or custom-script notifications

## Requirements

- Go 1.26 or newer to build loopai
- Git, or a Git-compatible `vcs_command` wrapper
- One primary executor:
  - Claude Code CLI for the default mode
  - Codex CLI 0.130.0 or newer for `--codex`
- Optional: the other executor for automatic cross-provider review
- Optional: `fzf` for interactive selection; a numbered fallback is built in
- Optional for `--pr`: authenticated GitHub CLI (`gh auth login`) and a GitHub
  repository remote named `origin`
- Development: Bash and `jq` for the full test suite, including plugin manifest
  validation and provider-wrapper tests
- Optional for development: `golangci-lint`

loopai must be run from the repository root. A custom `vcs_command` must accept
the same arguments as Git and return compatible output, including for
`rev-parse --show-toplevel`.

## Installation

Build and install from this checkout:

```bash
git clone https://github.com/echeg/loopai
cd loopai
make build
install -d ~/.local/bin
install -m 0755 .bin/loopai ~/.local/bin/loopai
```

Ensure `~/.local/bin` is in `PATH`, then verify:

```bash
loopai --version
```

For development, `make build` always refreshes `.bin/loopai`.

### Shell completions

The source tree includes completions for Bash, Zsh, and Fish:

```bash
# Bash: add this to ~/.bashrc
source /path/to/loopai/completions/loopai.bash

# Zsh: add this to ~/.zshrc
source /path/to/loopai/completions/loopai.zsh

# Fish
install -d ~/.config/fish/completions
install -m 0644 completions/loopai.fish ~/.config/fish/completions/loopai.fish
```

### Claude Code plugin

Add this repository as a Claude Code marketplace and install its plugin:

```bash
claude plugin marketplace add echeg/loopai
claude plugin install loopai@loopai
```

The plugin provides six skills:

- `loopai:loopai` launches loopai, monitors progress, and resumes active runs
- `loopai:loopai-plan` creates an executable implementation plan
- `loopai:loopai-brainstorm` designs a feature interactively, then hands the
  approved design to `loopai:loopai-plan`
- `loopai:loopai-adopt` converts an existing specification or issue into a plan
- `loopai:loopai-update` merges updated embedded defaults into local
  customizations
- `loopai:loopai-grill` critiques an existing plan with Claude and Codex, or
  runs a plan-off that compares and synthesizes competing plans

Use the namespaced plugin command to review the newest active plan, review a
specific plan, or generate a competing-plan comparison:

```text
/loopai:loopai-grill
/loopai:loopai-grill docs/plans/example.md
/loopai:loopai-grill compare "add a small feature"
/loopai:loopai-grill compare docs/plans/example.md
```

With no path, the skill proposes the newest active plan for confirmation. Grill
mode applies only the verified findings you select, and records the round in the
plan's [Decision Log](#decision-log) — what it accepted, what you rejected, and
what you simply did not select — so a later round does not re-raise a rejected
point without new evidence. A round where you select nothing leaves the plan
untouched. If Codex is unavailable or
fails, it reports that and continues with Claude critics. Compare mode requires
Codex, never edits a source plan, and creates one new plan without overwriting
an existing path. Both modes reject completed plans, symlinked plans or plan
directories, and anything under `.loopai/`; plan-consuming Claude calls run
with read-only tools against an equivalent sanitized snapshot to Codex.
Active-plan edits fail if the reviewed file identity or content
changes, and no-clobber publication preserves concurrent writers. Successful
edits report and retain the displaced plan under a Git-private, non-stageable
recovery path so a late write through a previously opened descriptor remains
recoverable. The skill requires a POSIX environment (Linux, macOS, or Windows
via WSL), Python 3, and Git for descriptor-anchored Claude and Codex snapshots
that exclude ignored files, hard-linked files, every symlink path, and case
aliases of private directories, reject individual files over 64 MiB and
snapshots over 512 MiB, and reject in-worktree alternate Git directories.
Active plans and generated drafts are limited to 8 MiB. Standalone
skill copies use `/loopai-grill` instead of the namespaced plugin command.

The CLI remains the execution engine. The plugin adds Claude Code workflows
for planning and operating it. Refresh the marketplace and plugin when a new
version is published, then restart Claude Code to apply the update:

```bash
claude plugin marketplace update loopai
claude plugin update loopai@loopai
```

To remove the workflows:

```bash
claude plugin uninstall loopai@loopai
claude plugin marketplace remove loopai
```

To install standalone skill copies instead:

```bash
install -d ~/.claude/skills
cp -R assets/claude/skills/loopai* ~/.claude/skills/
```

This keeps each skill's bundled resources, including the safety helpers used by
`loopai-grill`. Standalone copies are not managed by Claude Code's plugin
updater. After pulling a newer repository version, rerun the copy command to
refresh them. If replacing older command-file copies, remove only the
`~/.claude/commands/loopai*.md` files that you previously installed.

When migrating from umputun's upstream plugin, remove its plugin and marketplace
after installing this one:

```bash
claude plugin uninstall ralphex
claude plugin marketplace remove ralphex
```

Superpowers can remain installed for debugging, TDD, code review, and its other
orthogonal workflows. `loopai-brainstorm` replaces only its brainstorming-to-
plan-writing path: it sends approved decisions directly to `loopai-plan` and
does not create a separate spec. For projects shared with teammates who keep
Superpowers, add a directive like this to the project's `CLAUDE.md`:

```text
Create implementation plans with loopai:loopai-brainstorm and
loopai:loopai-plan. Do not use superpowers:writing-plans to create plan files.
```

### Migrating from upstream ralphex

loopai intentionally performs no automatic migration and never reads
`~/.config/ralphex/` or project-local `.ralphex/` data. To migrate selected
settings, copy them explicitly and inspect the result:

```bash
install -d ~/.config/loopai .loopai
cp -R ~/.config/ralphex/. ~/.config/loopai/
cp .ralphex/config .loopai/config
# copy .ralphex/prompts/ and .ralphex/agents/ too, if customized
cmux clear-status ralphex
```

The executable is now `loopai`. Replace `RALPHEX_CONFIG_DIR` with
`LOOPAI_CONFIG_DIR` and `RALPHEX_WEB_HOST` with `LOOPAI_WEB_HOST`. Remove an old
`ralphex` binary separately after verifying the new installation. Legacy
`.ralphex/progress/` logs remain on disk but are not discovered by loopai’s
project-root dashboard scan.

This fork also removes the upstream Docker/Bedrock path, built-in Mercurial
adapter, packaged releases (Homebrew, deb, rpm, and release binaries), and
hosted documentation site. Source builds and this repository's Claude Code
plugin marketplace are the supported distribution paths.

## Quick start

Create `docs/plans/my-feature.md`:

```markdown
# Plan: My feature

## Overview

Describe the intended outcome.

## Validation Commands

- `go test ./...`
- `golangci-lint run`

### Task 1: Implement the feature

- [ ] Add the implementation
- [ ] Add or update tests
- [ ] Run validation
```

Validation timing recognizes the exact, case-sensitive H2 heading `## Validation Commands`.
Commands must be plain, non-checkbox Markdown list items before the next heading. Checkbox items,
prose, and fenced examples are ignored. Optional surrounding backticks are stripped and whitespace
is normalized before matching.

Run it:

```bash
loopai docs/plans/my-feature.md
```

loopai creates or selects a feature branch, executes each incomplete task, commits successful iterations, runs the configured reviews, and writes progress under `.loopai/progress/`.

## Common commands

```bash
# choose a plan interactively
loopai

# execute a specific plan
loopai docs/plans/feature.md

# create a plan interactively, then optionally execute it
loopai --plan "add a health-check endpoint"

# execute tasks without review phases
loopai --tasks-only docs/plans/feature.md

# review existing branch changes without executing tasks
loopai --review

# begin at the external-review phase
loopai --external-only

# use Codex for planning, tasks, fixes, and internal reviews
loopai --codex docs/plans/feature.md

# let Codex also discover project-level CLAUDE.md
loopai --codex --pass-claude-md docs/plans/feature.md

# append extra arguments to every codex invocation
loopai --codex '--codex-args=-c service_tier="default"' docs/plans/feature.md

# execute in an isolated worktree
loopai --worktree docs/plans/feature.md

# hand the run off to its own cmux workspace, so it gets its own sidebar card
loopai --cmux-workspace --worktree docs/plans/feature.md

# stay in a free cmux workspace; hand off only when another loopai run is active there
loopai --cmux-workspace=auto --worktree docs/plans/feature.md

# commit local changes, then execute from a new isolated worktree
loopai --worktree --commit docs/plans/feature.md

# continue an interrupted isolated worktree by rerunning the same command
loopai --worktree docs/plans/feature.md

# generate project-specific review agents into .loopai/agents/
loopai --gen-agents

# initialize project-local configuration
loopai --init

# restore global defaults interactively
loopai --reset

# inspect embedded defaults without changing active configuration
loopai --dump-defaults /tmp/loopai-defaults

# run with the live dashboard
loopai --serve docs/plans/feature.md
```

Use `loopai --help` for the complete flag list.

## Execution pipeline

The full pipeline has four phases:

1. Task execution finds the first incomplete `### Task N:` or `### Iteration N:` section, runs the selected executor, validates the result, marks the task complete, and commits it.
2. First review launches the review agents in parallel through the primary executor: five built-in agents plus any project-specific agents the executor selects (see [Review agents](#review-agents)).
3. External review runs the configured reviewer or reviewer chain for findings. The primary executor evaluates findings and owns all fixes.
4. Second review checks the final changes for critical or major regressions.

An optional finalize step can run after review. It is disabled by default and controlled with `finalize_enabled`; `--skip-finalize` disables it for one invocation.

Press Ctrl+\ during a task iteration to pause it, edit the plan, and retry the same task in a fresh session. During external review, Ctrl+\ terminates the entire reviewer chain and skips all remaining reviewers. This shortcut is not available on Windows.

## Review agents

A review agent is a plain-text file of review instructions. The first internal review always
launches the five built-in agents in parallel: quality, implementation, testing,
simplification, and documentation. These cover problems that apply to any codebase.

Projects can add their own agents for problem classes the built-in five miss — a migration
checker, a schema-compatibility reviewer, an accessibility pass. Drop a `.txt` file into
`.loopai/agents/` (project) or `~/.config/loopai/agents/` (all projects) and give it YAML
frontmatter with a one-line `description`:

```text
---
description: "check SQL migrations for irreversible or lock-heavy operations"
---

Review the changed migration files for:

1. Missing or incorrect down-migrations
2. Operations that take long-lived locks on large tables
...
```

The `description` is what makes the agent active. `review_first.txt` expands the
`{{agents:dynamic}}` variable into a catalog of every described agent, and the primary
executor reads that catalog and launches the ones relevant to the current diff — typically
none to three — in the same parallel message as the built-in five. Selection is the model's
call based on your description, so write it as a precise statement of when the agent applies.
An agent with no description is never offered in the catalog; it runs only where a prompt
references it explicitly as `{{agent:name}}`.

The frontmatter is YAML, so quote the description. An unquoted value containing `: ` makes
the whole block unparsable, which looks exactly like having no frontmatter at all and leaves
the agent inactive; an unquoted ` #` is read as a YAML comment and silently truncates the
description at that point.

The catalog exists only where `review_first.txt` says `{{agents:dynamic}}`. If you have
customized that prompt, add the placeholder to your copy — a customized `review_first.txt`
without it disables every dynamic agent. The first review iteration logs a warning naming the
agents it dropped, but nothing else signals it. `loopai --dump-defaults <dir>` shows the
current default to compare against.

To get started, let loopai draft agents for the repository:

```bash
loopai --gen-agents
```

One executor session inspects the stack, layout, project instructions, and commit history and
writes 2-5 candidate agents into `.loopai/agents/`. It then lists every agent file in that
directory with its description, including ones that already existed. Nothing else changes:
review the files with `git diff`, edit or delete what does not fit, and commit the ones worth
keeping. Files that reuse a built-in agent name are reported with a warning, since they
replace that built-in agent. The listing reports each file as loopai will actually treat it:
a file with no description, unparsable frontmatter, or no prompt body is flagged instead of
being listed as an active agent. The session runs with `task_model` (`--task-model`); `plan_model`
does not apply. The flag is standalone and is rejected together with a plan file, another
standalone mode, or `--serve`/`--watch`.

Agent frontmatter also accepts `model` (`haiku`, `sonnet`, `opus`, or `fable` for that agent
alone) and `agent` (a named subagent type instead of the default `general-purpose`). Both are
optional. An unknown model value is reported as a warning and drops both execution overrides
for that agent; the `description` is kept, so the agent still runs from the catalog with
default settings. Under `--codex` a single shared reviewer agent is used, so `model` and
`agent` are ignored there and logged as a warning — `description` still works.

## Plan format

Plans are Markdown files, normally stored in `docs/plans/`.

- Task headings must be `### Task N:` or `### Iteration N:`.
- Work items use `- [ ]`; loopai changes completed items to `- [x]`.
- Put actionable checkboxes inside task sections.
- Include validation commands in the plan.
- Keep tasks small enough for one fresh agent context.

By default, a successfully completed plan moves to `docs/plans/completed/`. Set `move_plan_on_completion = false` when another workflow owns plan archival.

### Decision Log

A plan may carry an optional `## Decision Log` section recording how earlier critique
rounds were resolved:

```markdown
## Decision Log

- 2026-08-25 grill: **accepted** - split lock acquisition into prepare/commit (tasks 3-4 updated)
- 2026-08-25 grill: **rejected** - "add retry around merge" - merge is already the backstop
- 2026-08-25 grill: **deferred** - "split the git package" - not selected this round
```

`accepted` and `rejected` record decisions you made explicitly; `deferred` records a
finding you were shown and did not pick. Only a `rejected` entry stops a later round from
raising the point again.

`loopai-grill` writes the section when it applies user-selected findings, and plan revision
after critique records accepted and rejected points there. Later critique rounds read it and
do not re-raise a rejected point without new evidence, so repeated rounds converge instead of
relitigating. The section never contains checkboxes: a checkbox outside a task section is not an
implementation step, but it makes the plan read as unfinished work and costs extra loop iterations.

## Backlog capture

Agents regularly notice real problems outside the current plan's scope. Instead of fixing them
as scope creep or dropping them, the primary executor files each one as a separate Markdown
file under `backlog_dir` (`docs/backlog/` by default):

```markdown
# Worktree lock left behind on SIGKILL

- found: 2026-08-25, plan: backlog-capture, phase: task
- severity: minor
- area: pkg/git/worktree.go

`prepareWorktree` (pkg/git/worktree.go:214) ... suggested fix direction ...
```

Capture is a prompt convention, not a code path: loopai never reads, validates, or creates the
directory, and the path is only substituted into prompts as `{{BACKLOG_DIR}}`. Where the entry
gets committed depends on the path that files it, and the three rules are not interchangeable:

- **Task and internal review** write the entry inside the worktree, stage it, and commit it in
  phase, with the fixes or on its own as `docs: add backlog entry`, so it survives worktree
  removal and arrives on the default branch through `--merge`. An entry is always a new untracked
  file, so the stage is what keeps a plain `git commit -m` from silently leaving it behind.
- **External-review evaluation** writes it inside the worktree, stages it, and leaves it
  uncommitted until the reviewer chain finishes: after the first round the external reviewer is
  shown only the uncommitted diff, so a mid-loop commit would hide the accumulated fixes. The
  final `fix: address ... review findings` commit sweeps the staged entry up.
- **Plan creation** runs in the source checkout before the branch and worktree exist, so it stages
  that one file and commits it there with `git commit -m "docs: add backlog entry" -- <entry>` — on
  whatever branch is checked out, normally `main` or `master`. An uncommitted entry would never
  reach the feature branch, and an untracked one blocks branch and worktree creation, which
  tolerate only the plan file itself. Because this is your own checkout rather than a loopai
  worktree, the commit names the entry explicitly, so anything you already had staged stays staged
  instead of being swept into it.

Filing is not a fix: an out-of-scope finding behaves like a dismissal in the completion-signal
logic, so an agent never keeps a review loop alive by claiming it fixed something. The write is
still a repository change, so a round that creates or updates an entry does reset external-review
stalemate detection (`review_patience`). Before creating an entry an agent lists the existing ones
and updates a matching entry rather than duplicating it.

Capture is instructed on four paths: task execution, internal review consolidation, evaluation
of external-review findings (external reviewers are read-only, so their out-of-scope findings are
filed by the primary evaluator), and plan creation. Entries are plain Markdown, so
`/loopai:loopai-adopt docs/backlog/<entry>.md` turns one into a full plan. Nothing prunes the
directory, so delete or archive an entry once it has become a plan, or a later run may file it again.

Capture cannot be switched off with configuration: an empty `backlog_dir` falls back to
`docs/backlog`, the same way `plans_dir` does. To disable it, extract the prompts with
`loopai --dump-defaults`, remove the out-of-scope block from `task.txt`, `review_first.txt`,
`review_second.txt`, `codex.txt`, `external_claude_eval.txt`, `custom_eval.txt`, and
`make_plan.txt`, and install those copies in `.loopai/prompts/`.

## Completion and close-out

Inside cmux, an implementation or review run that reached execution leaves a persistent
status pill in the workspace: a green bolt with `done in <elapsed>` after success, or a
red warning icon with `failed` after an execution error. Startup/preflight failures and
plan-creation failures do not leave a pill. Canceling with `Ctrl+C` performs the usual
cleanup and leaves no completion pill. The next loopai run replaces an existing pill;
it can also be removed explicitly:

```bash
loopai --clear
```

Outside cmux, `--clear` is a successful no-op. After a feature run completes, loopai
can perform either of these standalone close-out actions from the repository root:

```bash
# Merge the current feature branch into main (or master when main does not exist).
loopai --merge

# Use an explicit base branch.
loopai --merge=release/13

# Push the current feature branch and open a GitHub pull request via gh.
loopai --pr
loopai --pr=release/13

# Name the feature explicitly to close it out from the primary checkout.
loopai --merge dynamic-review-agents
loopai --merge 20260806-dynamic-review-agents
loopai --merge docs/plans/20260806-dynamic-review-agents.md
loopai --merge=release/13 dynamic-review-agents
loopai --pr dynamic-review-agents
```

`--merge` requires clean feature and base worktrees, including no untracked files. It
merges the current feature branch in the base worktree, safely removes a linked feature
worktree only when Git confirms its branch and cleanliness, then deletes the merged
branch. The worktree removal also deletes ignored files in that worktree, such as build
output or a local `.env`; preserve any local-only ignored files before running `--merge`.
`--pr` requires authenticated `gh` and a GitHub remote named `origin`; every effective
origin push URL must identify that same GitHub repository. It pushes committed branch
state, builds the title and body from the associated plan and diff
statistics, and keeps the feature branch and worktree. Commit intended changes before
running `--pr`. Each command clears the completion pill only after it succeeds, so a
failed close-out remains visible.

Without an argument both commands close out the current branch and must run from the
feature worktree or checkout. The optional positional argument names the feature instead,
so either command can run from the primary checkout or from any other registered worktree.
Like every close-out invocation it still has to start at the root of a checkout, not in a
subdirectory. It accepts a local branch name, a plan basename with or without `.md`, or a
plan path, and combines with an explicit base such as
`--merge=release/13 dynamic-review-agents`. loopai first looks for a branch of that exact
name, then for a plan file in the plans directory and its `completed/` subdirectory. For a
plan it uses the branch recorded for that plan under `.loopai/progress/`, so a run started
with `--branch` closes out the branch it actually created rather than the one the plan
filename implies; without such a record the branch is derived from the filename. Either
way that branch has to exist locally. The plan lookup uses
the `plans_dir` of the checkout you run from, so a plan that exists only inside the
unmerged feature worktree is not visible from the primary checkout; name the branch in that
case. When the named feature has no registered worktree, `--merge` merges and deletes the
branch without any worktree cleanup; with a worktree the usual cleanliness checks and safe
removal apply. Only the feature worktree and the worktree the merge runs in have to be
clean, so unrelated uncommitted work in an invoking checkout that is neither one does not
block the merge. The merge runs in the base worktree, or in the primary checkout when the
base branch is not checked out anywhere. One arrangement is still refused: when the feature
is checked out in the primary checkout while the base is checked out in a linked worktree,
the merge would have to run in the primary and cannot, so switch the primary off the feature
branch first. `--pr <feature>`
pushes and opens the pull request for a branch that is not checked out anywhere; its title
and body still come from the plan as seen from the invoking checkout, falling back to a
stats-only body when that plan is not present there. Naming the base branch as the feature
is an error, and so is a second positional argument: `--merge release/13 dynamic-review-agents`
is `--merge=release/13 dynamic-review-agents` with the `=` forgotten, which would otherwise
close out `release/13` itself. Apart from this argument, the close-out commands cannot be
combined with a plan file or execution options. On success `--merge` names the worktree it
removed, since with an explicit feature that directory is not the one you ran from.

## Executors and reviews

Claude Code is the default primary executor. Pass `--codex`, or set `executor = codex`, to use Codex for plan creation, task execution, internal reviews, finding evaluation, and finalize.

Codex invocations are composed by loopai and use additive `-c` overrides, so `~/.codex/config.toml`
settings remain available. `--codex-args`, or the `codex_args` config key, appends extra arguments
to every codex invocation loopai spawns — both first-class `--codex` phases and external codex
review under a Claude primary. Unlike `claude_args`, which *is* the claude command's argument list,
codex args are strictly additive and are appended last, so an explicit `-c` value there overrides
the matching override loopai sets. The motivating recipe keeps long autonomous runs off the
priority tier while interactive codex sessions keep `service_tier = "priority"`:

```bash
loopai --codex '--codex-args=-c service_tier="default"' docs/plans/feature.md
```

```ini
codex_args = -c service_tier="default"
```

On the command line the value must be attached with `=`, as above: a value that starts with `-`
is otherwise read as the next option, and loopai exits with `expected argument for flag
'--codex-args'`. Quote the whole `--codex-args=...` token so the shell keeps it together.

An explicit `--codex-args=` clears a value inherited from configuration. Without the flag or key,
codex invocations are unchanged. The wrapper path (`claude_command = .../codex-as-claude.sh`) is
not affected; it is configured through `claude_args` and the wrapper's own `CODEX_*` variables.

The value is tokenized like a shell word list, and quotes group whitespace but are then removed,
so `-c notify="say hi"` reaches codex as the single argument `notify=say hi`. Backslashes follow
the same shell rules: literal inside single quotes, an escape for `"` and `\` inside double quotes,
and an escape for the next character when unquoted, so `-c cwd="C:\work dir"` keeps its
backslash. Codex parses each
`-c` value as TOML and falls back to treating it as a literal string, which is why the
`service_tier` recipe above works — but a value whose *type* depends on its quotes needs the
quotes escaped, or codex fails to load its config and every codex phase dies at startup:

```ini
# wrong: reaches codex as [CLAUDE.md], which is not valid TOML
codex_args = -c project_doc_fallback_filenames=["CLAUDE.md"]
# right
codex_args = -c project_doc_fallback_filenames=[\"CLAUDE.md\"]
```

Three more sharp edges, since loopai appends the extras rather than merging them:

- Last-occurrence-wins applies to `-c key=value` only. Repeating any flag loopai already passes —
  one that takes a value or a bare switch — is a fatal codex parse error, not an override:
  `'--codex-args=--sandbox workspace-write'` aborts every codex session with `the argument
  '--sandbox <SANDBOX_MODE>' cannot be used multiple times`. Set `codex_sandbox` instead. Which
  flags loopai emits depends on the path: first-class `--codex` runs also pass
  `--dangerously-bypass-approvals-and-sandbox` whenever the effective sandbox is
  `danger-full-access` (the default for `--codex`), so repeating that one aborts every phase too.
- Extras land after the `exec` subcommand, so they must be options `codex exec` itself accepts.
  Options that exist only on the top-level `codex` command — `--search`, for example — are a fatal
  `unexpected argument` error rather than a pass-through. Most global options (`-c`, `--model`,
  `--sandbox`, `--cd`, `--profile`) are accepted by `exec` too; set the rest through
  `~/.codex/config.toml` or an equivalent `-c` key.
- Extras must contain no bare positional token. `codex exec` takes an optional positional prompt,
  and loopai sends its own prompt on stdin. When both are present codex uses the positional as the
  prompt and appends the piped stdin as a trailing `<stdin>` block, so a stray word in `codex_args`
  demotes loopai's task instructions into an appendix of the stray word.

Extras are trusted input, like `codex_command`. They also reach the external codex reviewer, which
loopai otherwise pins to a read-only sandbox so it can only report findings. loopai never emits
`--dangerously-bypass-approvals-and-sandbox` on that path, so codex accepts it alongside the
`--sandbox read-only` pin — putting it in `codex_args` gives the reviewer write access to the
repository. A first-class `--codex` run rejects the duplicate outright only when its effective
sandbox is `danger-full-access` (the `--codex` default), since that is when loopai emits the flag
itself; setting `codex_sandbox` to anything else means the primary executor accepts the bypass
silently too.

With `external_review_tool = auto`, loopai selects the other installed first-class provider:

- Claude primary → Codex external reviewer
- Codex primary → Claude external reviewer

If an automatically selected reviewer is unavailable, loopai warns and skips only that phase. A missing primary or explicitly selected reviewer is an error. Set `external_review_tool = none` to disable the legacy single-reviewer path when `external_reviewers` is unset.

For an ordered review chain, set `external_reviewers` to a comma-separated list of
`provider[:model[:effort]]` entries. Providers are `claude`, `codex`, and `custom`:

```ini
external_reviewers = codex:gpt-5.5:xhigh, claude:fable:max
```

Reviewers run in list order. Each reviewer loops until it reports no findings, reaches its own
`max_external_iterations` cap, or triggers its own `review_patience` threshold; then the next
reviewer starts. Reviewers remain read-only: the primary executor evaluates their findings and
applies fixes with `review_model` (falling back to `task_model`). A bare `claude` entry defaults to
`opus:xhigh`; a bare `codex` entry uses `codex_model` and `codex_reasoning_effort`. Codex ignores
the Claude-only `max` effort with a warning. Use `provider::effort` to override only effort.

Providers may be repeated—each entry creates a separate review loop. An explicit
`external_reviewers` value at any config layer takes precedence over `external_review_tool` and
`external_review_model`; use an empty local value or `--external-reviewers=` to clear/disable an
inherited chain. Unlike legacy `auto`, every non-empty chain entry is explicit, so a missing
reviewer binary is an error.

The equivalent per-run flag is:

```bash
loopai \
  --external-reviewers=codex:gpt-5.5:xhigh,claude:fable:max \
  docs/plans/feature.md
```

`--external-reviewers` cannot be combined with `--external-review-tool` or
`--external-review-model`.

Per-phase models can be selected with:

```bash
loopai \
  --task-model=opus:high \
  --review-model=sonnet:medium \
  --external-review-model=:xhigh \
  docs/plans/feature.md
```

The model syntax is `model[:effort]`; either half may be omitted. Provider-specific defaults still apply.

## Worktree isolation

`--worktree` creates an isolated checkout under `.loopai/worktrees/<branch>`. A new plan
branch is cut from the current checkout's `HEAD`, whether it is a branch or a detached
commit. When an existing plan branch does not already contain the source `HEAD` seen before
any `--commit` auto-commit, loopai reuses it and automatically merges the source HEAD into
the fresh worktree. Git 2.38+ predicts conflicts before source mutation. Older Git falls
back to the real merge and removes the worktree if that merge conflicts. An actual conflict
aborts with guidance to merge or rebase the source changes manually. This is useful for
parallel plans and for starting work from any source branch.

```bash
git checkout release/13
loopai --worktree docs/plans/feature.md
```

The source checkout must normally be clean. Pass `-c` or `--commit` to stage all changes
with `git add -A` and commit them in the source checkout before creating a fresh worktree.
This advances the checked-out branch when attached, or the detached `HEAD` otherwise:

```bash
loopai --worktree -c docs/plans/hotfix.md
```

Gitignored files remain uncommitted, and a clean checkout makes `--commit` a no-op. The
flag requires `--worktree`. If the command resumes an existing worktree, loopai warns that
`-c`/`--commit` is ignored and leaves the source checkout untouched.
An uncommitted plan file may be the checkout's only change without `--commit`; loopai
copies and commits it in the feature worktree. With `--commit`, the plan is included in
the source-side all-files commit instead.

Breaking CLI change: the deprecated `-c` alias for `--codex-only` was removed. Use
`--codex-only` explicitly. `-c` now means `--commit` and requires `--worktree`.

For the lifetime of each worktree run, loopai holds an OS advisory lock in the worktree's
private Git directory. A second invocation targeting the same worktree exits immediately
with a busy error that includes the recorded process ID and start time. Those values are
diagnostic only; the OS lock is the source of truth and is released automatically even if
the original process crashes or is killed. Do not delete `loopai-run.lock` manually: deleting
an advisory lock's pathname does not unlock its open file descriptor and can allow another
process to create and independently lock a replacement file.

If a process was interrupted before its worktree could be removed, rerun the same command:

```bash
loopai --worktree docs/plans/feature.md
```

When the expected worktree exists and its run lock is free, `--worktree` automatically validates
that the directory is a registered Git worktree on the plan's feature branch and that the plan
exists inside it, then continues from the first incomplete task. Dirty changes are preserved. On
another failure or interruption the worktree remains available for the next invocation; after
successful completion it is removed normally. The former `--resume-worktree` option has been
removed and is now an unknown option; use `--worktree` for both creation and continuation.
If the original run used `--branch`, pass the same option when continuing it.

Worktree creation does not use or record a base branch. `--base-ref` remains the base for
review diffs and templates; without it, loopai uses `default_branch` configuration or its
normal `main`/`master` detection. Consequently, when a worktree was cut from a non-default
branch, pass that branch explicitly to review against it or merge back into it:

```bash
loopai --review --base-ref release/13
loopai --merge=release/13
```

Non-worktree branch mode is unchanged: a branch-valued `--base-ref` is both its branch-creation
base and its diff base.

## Configuration

Global configuration:

```text
~/.config/loopai/
├── config
├── prompts/
├── agents/
└── scripts/
```

Project-local overrides:

```text
.loopai/
├── config
├── prompts/
└── agents/
```

Run `loopai --init` to create a commented project-local configuration. Local files override global files, which override embedded defaults. Old `~/.config/ralphex/` and `.ralphex/` directories are intentionally not read or migrated.

Useful environment variables include:

- `LOOPAI_CONFIG_DIR` — override the global configuration directory
- `LOOPAI_WEB_HOST` — dashboard listen address

The embedded configuration documents every option. Extract it with:

```bash
loopai --dump-defaults /tmp/loopai-defaults
```

Custom prompts and agents use the same filenames as the embedded defaults. At runtime the `{{agent:name}}` template syntax expands one named review agent, and `{{agents:dynamic}}` expands the catalog of project-specific agents described in [Review agents](#review-agents).

Two directory keys are exposed to prompts as placeholders: `plans_dir` as `{{PLANS_DIR}}` and
`backlog_dir` as `{{BACKLOG_DIR}}` (default `docs/backlog`, see [Backlog capture](#backlog-capture)).
Both are substituted into every prompt loopai builds; a custom prompt can use either.

## Alternative providers

`claude_command` and `claude_args` may point to a Claude stream-json compatible wrapper. Examples live under `scripts/`, including wrappers for Codex, Copilot, Gemini, OpenCode, pi, and agy. See [docs/custom-providers.md](docs/custom-providers.md).

The included Copilot adapter at `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and runs Copilot in native autopilot mode. Task and review phases use `--autopilot --no-ask-user --allow-all`; plan creation uses `--autopilot --allow-all` so clarification can flow through the stable plan signals. Set `COPILOT_MODEL` to choose a model.

The pi adapter at `scripts/pi-as-claude/pi-as-claude.sh` supports `PI_PROVIDER`, `PI_MODEL`, `PI_THINKING`, and `PI_VERBOSE`. The included Codex and Copilot wrappers require `jq` on `PATH` for JSON translation. The included pi wrappers also require `jq` on `PATH`; see each wrapper README for authentication and provider-specific options.

For a custom external reviewer, configure:

```ini
external_reviewers = codex, custom
custom_review_script = /absolute/path/to/reviewer.sh
```

`custom` entries cannot specify a model and require `custom_review_script`. The same script is used
for every custom entry. It receives a prompt-file path as its only argument and writes findings to
standard output. The legacy `external_review_tool = custom` form remains supported.

## Progress and dashboard

Progress logs are written to `.loopai/progress/`. Watch the active run with:

```bash
tail -f .loopai/progress/progress-*.txt
```

Each section is followed by a wall-clock duration line such as `task iteration 1 took 3m38s`
when the next section starts or the run ends. The end-of-run summary groups those durations by
phase, for example `phase durations: tasks 32m26s (8), internal review 2h23m (10)`. The number in
parentheses is the number of section occurrences, so retried iterations count again. Durations
include rate-limit waits. Phase totals can be less than the footer's total elapsed time because
time before the first section is unattributed.

Shell commands that equal an entry in the plan's `## Validation Commands` section, or start with
that entry followed by whitespace, are timed separately. Each completed match is logged as it
finishes, followed by an aggregate after the phase summary:

```text
validation: make test took 1m12s
validation: make lint took 18s
phase durations: tasks 8m41s (2), internal review 2m3s (1)
validation: 1m30s (2 runs)
```

Validation time is already included in the phase durations; it is not additional elapsed time.
The aggregate is the sum of matched command durations, so concurrent commands can overlap and
make it larger than section or run wall-clock time. Plans without validation commands, unmatched
commands, incomplete commands, and provider streams without shell tool events produce no
`validation:` lines. Claude measures stream-event arrival time. Codex prefers native rollout
timestamps and falls back to approximate arrival time when timestamps are absent or invalid.
For current Codex custom-tool records that expose only command output, native timestamps can
prove completion when the call returns before its configured yield threshold. Calls at or beyond
that threshold are paired with a later session continuation only when the association is unique;
ambiguous or abandoned calls produce no timing line. Batched calls use native per-command wall
times when available and are omitted when only the enclosing call's duration is known.

Start the dashboard:

```bash
loopai --serve
loopai --serve --port=3000
loopai --serve --watch=/path/to/project-a --watch=/path/to/project-b
```

When loopai runs inside cmux, it reports the phase and effective model, review iteration, task count, spinner, and completion notifications through the public cmux CLI. Started implementation and review runs retain the completion pill described above after success or non-abort execution failure; startup/preflight failures, plan-creation failures, and aborts do not. Outside cmux this integration is a no-op.

The cmux status pill and progress bar belong to the workspace, not to an individual run, so
several runs started from one workspace overwrite each other's status. Bare `--cmux-workspace`
and `--cmux-workspace=always` avoid that by unconditionally handing the run off: loopai creates a
new cmux workspace named after the branch the run will use, relaunches itself there without the
flag, prints `handed off to cmux workspace <name>`, and exits. The run then owns its own sidebar
card, pill, spinner, and progress bar, which makes parallel runs independent.

```bash
loopai --cmux-workspace --worktree docs/plans/feature.md
```

Use `--cmux-workspace=auto` to keep the first run in the current workspace and give only parallel
runs their own cards. Auto mode reads `cmux list-status`: a `loopai` pill whose text starts with
`done` or `failed` is final, and no `loopai` pill is also free; any other `loopai` pill means a run
is active, so loopai hands off. A run that stays local replaces the free state with a `starting`
pill; the normal reporter takes it over after preflight, so another invocation started during
dependency or worktree setup also sees the workspace as busy. This normally happens before config
loading. An invocation with `--serve` and no plan or description waits for config to distinguish a
watch-only dashboard from a normal run, then reserves if execution will follow; combined
`--reset --serve` reserves before its prompt and releases that reservation if config confirms
watch-only mode. The value is optional but must use the attached `=auto` form, not
`--cmux-workspace auto`.

```bash
loopai --cmux-workspace=auto --worktree docs/plans/feature.md
```

This detection is deliberately best-effort. A run killed before it can write a final pill can
leave a stale phase pill, causing one unnecessary hand-off. Two auto-mode runs started at the same
time can also both observe a free workspace and stay there.

Hand-off is best-effort like the rest of the cmux integration. Outside
cmux, auto mode executes normally in the current terminal without warning; any other auto-mode
status-query failure has the same quiet fallback (and is visible with `--debug`). Unconditional
mode prints a warning and runs locally when cmux refuses to create the workspace. After auto mode
has positively detected a busy workspace, however, any pre-spawn refusal or creation failure stops
with an error instead of starting a conflicting local run. A successful hand-off leaves the
previous run's pill in the workspace it was started from, since the new run reports into its own
card instead.

A creation timeout also stops instead of falling back: cmux may already have created the workspace
and started the run there. loopai exits with an error, and the sidebar shows whether the workspace
exists — close it and re-run, or let it finish.

In unconditional mode, an invocation that could not run anyway is also kept in the current
terminal, so its error appears where it was typed instead of in a new card that closes immediately:
a named plan file that does not exist, and a working directory that is not the repository root.
Both are reported by the local run as usual.

Close-out and configuration commands (`--clear`, `--merge`, `--pr`, `--init`, `--dump-defaults`,
and `--reset` on its own) are never handed off; `--reset` in front of a plan belongs to that run
and is performed once, in the new workspace. With `--plan`, the interactive plan dialog happens in
the new workspace's terminal.

The new workspace starts a fresh shell, so it does not inherit the environment of the terminal the
run was started from. `LOOPAI_CONFIG_DIR` and `LOOPAI_WEB_HOST` are carried over with the command;
anything else the run needs, such as provider credentials, has to come from the shell profile.
The `ANTHROPIC_API_KEY` pass-through travels with the command but the key does not, so loopai warns
at hand-off when `ANTHROPIC_API_KEY` is set in the current terminal: unless the key also comes from
the shell profile, the handed-off run falls back to OAuth or the keychain. The warning covers both
ways of asking for the pass-through, `--preserve-anthropic-api-key` and the
`preserve_anthropic_api_key` config key.

Provider session and rate limits are retried every 10 minutes by default until the provider
recovers or the run is canceled with `Ctrl+C`. During the wait, progress output is red and cmux
shows `rate limited · retry in 10m`. Override the interval with `--wait <duration>` or disable
limit retries for one run with `--wait 0`; the equivalent config key is `wait_on_limit`.

When the native `claude` command is in use and `claude-swap` is available in `PATH`, loopai also
enables reactive account failover automatically. On a real Claude limit match it runs
`claude-swap switch --strategy next-available --model all --json`, waits for the credential change
to settle, and retries the same call. A global lock and generation file under `~/.config/loopai/`
ensure concurrent loopai processes perform only one switch for the same exhausted account; recently
limited slots are skipped even when claude-swap usage data is stale. If switching is unavailable,
the normal 10-minute retry remains active. Disable this integration with `--no-claude-swap` or
`claude_swap_enabled = false`. Custom Claude-compatible wrappers and Codex limits never trigger it,
and loopai stores only slot numbers and timestamps—not account emails or credentials.

## Notifications

Notifications are disabled by default. The configuration supports Telegram, email, Slack, generic webhooks, and custom scripts. See [docs/notifications.md](docs/notifications.md).

## Development

```bash
make build
make test
make lint
make fmt
make e2e
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution rules and [CLAUDE.md](CLAUDE.md) for codebase guidance.

## Fork compatibility policy

The user-facing application is loopai, but the Go module remains `github.com/umputun/ralphex`. Internal package names and the `<<<RALPHEX:...>>>` protocol signals also remain unchanged. This narrow compatibility boundary keeps upstream merges practical and preserves historical progress-log and prompt parsing. They are not unfinished parts of the rebrand.

## License

MIT. See [LICENSE](LICENSE).
