# loopai

Autonomous implementation-plan execution with Claude Code or OpenAI Codex.

loopai is a local CLI for running structured engineering plans from the root of a Git repository. Each task runs in a fresh agent session, validation is performed between tasks, and completed work can pass through internal and cross-provider review phases. The result is committed work with a persistent progress log, without requiring an IDE plugin or hosted service.

This repository is a personal fork. It is installed by building from source; no packaged releases are published here.

## Features

- Executes Markdown plans one task at a time with automatic retries
- Creates plans interactively with `--plan`
- Supports Claude Code and Codex as primary executors
- Runs configurable internal and external review phases
- Creates a branch automatically and optionally uses isolated Git worktrees
- Commits after completed tasks and review fixes
- Streams timestamped progress to `.loopai/progress/`
- Serves a real-time web dashboard with `--serve`
- Reports status to the cmux sidebar when available
- Sends optional Telegram, email, Slack, webhook, or custom-script notifications

## Requirements

- Go 1.26 or newer to build loopai
- Git, or a Git-compatible `vcs_command` wrapper
- One primary executor:
  - Claude Code CLI for the default mode
  - Codex CLI 0.130.0 or newer for `--codex`
- Optional: the other executor for automatic cross-provider review
- Optional: `fzf` for interactive selection; a numbered fallback is built in
- Development: Bash and `jq` for the included provider-wrapper test suites
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

### Optional Claude Code commands

The retained command assets provide `/loopai`, `/loopai-plan`, `/loopai-adopt`,
and `/loopai-update`. Install standalone copies with:

```bash
install -d ~/.claude/commands
cp -L assets/claude/loopai*.md ~/.claude/commands/
```

The commands respectively launch/monitor loopai, create a plan, convert an
existing specification into a plan, and merge updated embedded defaults into
customized configuration.

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
adapter, packaged releases (Homebrew, deb, rpm, and release binaries), hosted
documentation site, and Claude plugin marketplace metadata. Source builds and
the optional standalone Claude command assets above are the supported
distribution paths.

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

# execute in an isolated worktree
loopai --worktree docs/plans/feature.md

# continue an interrupted isolated worktree
loopai --resume-worktree docs/plans/feature.md

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
2. First review launches the configured review agents in parallel through the primary executor.
3. External review asks the other provider, or a configured custom reviewer, for findings. The primary executor evaluates findings and owns all fixes.
4. Second review checks the final changes for critical or major regressions.

An optional finalize step can run after review. It is disabled by default and controlled with `finalize_enabled`; `--skip-finalize` disables it for one invocation.

Press Ctrl+\ during a task iteration to pause it, edit the plan, and retry the same task in a fresh session. During external review, Ctrl+\ terminates that review loop. This shortcut is not available on Windows.

## Plan format

Plans are Markdown files, normally stored in `docs/plans/`.

- Task headings must be `### Task N:` or `### Iteration N:`.
- Work items use `- [ ]`; loopai changes completed items to `- [x]`.
- Put actionable checkboxes inside task sections.
- Include validation commands in the plan.
- Keep tasks small enough for one fresh agent context.

By default, a successfully completed plan moves to `docs/plans/completed/`. Set `move_plan_on_completion = false` when another workflow owns plan archival.

## Executors and reviews

Claude Code is the default primary executor. Pass `--codex`, or set `executor = codex`, to use Codex for plan creation, task execution, internal reviews, finding evaluation, and finalize.

With `external_review_tool = auto`, loopai selects the other installed first-class provider:

- Claude primary → Codex external reviewer
- Codex primary → Claude external reviewer

If an automatically selected reviewer is unavailable, loopai warns and skips only that phase. A missing primary or explicitly selected reviewer is an error. Set `external_review_tool = none` to disable external review.

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

`--worktree` creates an isolated checkout under `.loopai/worktrees/<branch>`. This is useful for parallel plans.

```bash
loopai --worktree docs/plans/feature.md
```

If a process was interrupted before its worktree could be removed, resume it explicitly:

```bash
loopai --resume-worktree docs/plans/feature.md
```

`--resume-worktree` implies `--worktree`. It validates that the expected directory is a registered
Git worktree on the plan's feature branch and that the plan exists inside it, then continues from
the first incomplete task. Dirty changes are preserved. On another failure or interruption the
worktree remains available for another resume; after successful completion it is removed normally.
The progress-file lock rejects another live run that resolves to the same progress path. If the
original run used `--branch`, pass the same option when resuming.

To base a plan on a non-default branch, check out that branch first and pass it explicitly:

```bash
git checkout release/13
loopai --worktree --base-ref release/13 docs/plans/hotfix.md
```

In branch-creating modes, a branch-valued `--base-ref` is both the creation and diff base. In review-only modes it is only the diff base.

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

Custom prompts and agents use the same filenames as the embedded defaults. The `{{agent:name}}` template syntax expands configured review agents at runtime.

## Alternative providers

`claude_command` and `claude_args` may point to a Claude stream-json compatible wrapper. Examples live under `scripts/`, including wrappers for Codex, Copilot, Gemini, OpenCode, pi, and agy. See [docs/custom-providers.md](docs/custom-providers.md).

The included Copilot adapter at `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and runs Copilot in native autopilot mode. Task and review phases use `--autopilot --no-ask-user --allow-all`; plan creation uses `--autopilot --allow-all` so clarification can flow through the stable plan signals. Set `COPILOT_MODEL` to choose a model.

The pi adapter at `scripts/pi-as-claude/pi-as-claude.sh` supports `PI_PROVIDER`, `PI_MODEL`, `PI_THINKING`, and `PI_VERBOSE`. The included Codex and Copilot wrappers require `jq` on `PATH` for JSON translation. The included pi wrappers also require `jq` on `PATH`; see each wrapper README for authentication and provider-specific options.

For a custom external reviewer, configure:

```ini
external_review_tool = custom
custom_review_script = /absolute/path/to/reviewer.sh
```

The script receives a prompt-file path as its only argument and writes findings to standard output.

## Progress and dashboard

Progress logs are written to `.loopai/progress/`. Watch the active run with:

```bash
tail -f .loopai/progress/progress-*.txt
```

Start the dashboard:

```bash
loopai --serve
loopai --serve --port=3000
loopai --serve --watch=/path/to/project-a --watch=/path/to/project-b
```

When loopai runs inside cmux, it reports the phase and effective model, review iteration, task count, spinner, and completion notifications through the public cmux CLI. Outside cmux this integration is a no-op.

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
