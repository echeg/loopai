# loopai

Autonomous plan execution with Claude Code or Codex. See `llms.txt` for compact usage guidance and `README.md` for user documentation.

## Fork policy

The public CLI, configuration directories, progress paths, notifications, and other user-visible strings use `loopai`.

Three upstream-compatible internals intentionally retain the old name:

- `module github.com/umputun/ralphex` in `go.mod`
- Go import paths and internal package identifiers derived from that module
- protocol signals matching `<<<RALPHEX:...>>>`

Do not rename these. Keeping the module path avoids import-line conflicts during upstream merges. Keeping the signals preserves compatibility with embedded/custom prompts and historical progress logs. These are deliberate boundaries, not rebrand omissions.

The fork does not contain upstream packaging/release infrastructure or the upstream documentation website. Distribution is by source build.

## Build commands

```bash
make build      # build .bin/loopai
make test       # asset checks, race-enabled unit tests with coverage, provider-wrapper suites
make test-wrappers # all retained provider-wrapper and wrapper-doc suites
make lint       # golangci-lint
make fmt        # gofmt and goimports
make race       # focused race run
make e2e        # dashboard browser tests
```

Go 1.26 or newer is required.

Dependencies behind the `e2e` build tag must be updated explicitly:

```bash
go get -u ./...
go get -u -tags=e2e github.com/playwright-community/playwright-go
go mod tidy
go mod vendor
```

## Project structure

```text
cmd/loopai/          main package, CLI parsing, startup wiring
pkg/cmux/            best-effort cmux status integration
pkg/config/          configuration loading and embedded defaults
pkg/executor/        Claude-compatible and Codex process execution
pkg/git/             Git CLI operations and worktree management
pkg/input/           interactive input, fzf fallback, draft review
pkg/notify/          Telegram, email, Slack, webhook, custom notifications
pkg/plan/            plan discovery, parsing, and mutation
pkg/processor/       pipeline coordinator, prompts, executor policy
pkg/processor/phase/ task, review, external, finalize, and planning phases
pkg/progress/        timestamped progress logging
pkg/status/          shared phases, sections, and protocol signals
pkg/web/             dashboard, SSE, templates, static assets
e2e/                 Playwright dashboard tests
scripts/             provider wrappers and internal test helpers
scripts/copilot-as-claude/ # GitHub Copilot CLI wrapper for Claude-compatible output
scripts/pi-as-claude/ # pi wrapper for Claude-compatible output
assets/claude/       optional slash-command source assets
docs/                focused operational documentation and plans
```

The top-level `assets/claude/loopai*.md` files are symlinks to the matching
`assets/claude/skills/loopai*/SKILL.md` sources. Keep the command name,
directory name, and link target aligned; `make check-symlinks` rejects broken
links.

## Configuration

- Global directory: `~/.config/loopai/`
- Local directory: `.loopai/`
- Global config: `~/.config/loopai/config`
- Local config: `.loopai/config`
- Global prompts/agents: `~/.config/loopai/{prompts,agents}/`
- Local prompts/agents: `.loopai/{prompts,agents}/`
- Progress: `.loopai/progress/`
- Worktrees: `.loopai/worktrees/`
- Override: `--config-dir` or `LOOPAI_CONFIG_DIR`

Local files override global files, which override embedded defaults. Embedded defaults remain the per-file fallback, so deleting an installed prompt or agent does not disable it. Remove its template reference to disable an agent.

Agent files may carry YAML frontmatter parsed into `config.Options` (`model`,
`agent`, `description`). A non-empty `description` marks the file as a *dynamic
agent*: it is offered to the internal review phase through the
`{{agents:dynamic}}` catalog instead of requiring a hard-wired
`{{agent:<name>}}` reference. The five embedded agents (quality,
implementation, testing, simplification, documentation) carry no description and
always run as the base set. `loopai --gen-agents` writes starter dynamic agents
into `.loopai/agents/`. When `Options.Validate` warns, `buildAgent` drops the
execution overrides but preserves `Description`: catalog membership must not
depend on an unrelated `model` typo. `config.ParseAgentOptions` applies the same
CRLF/whitespace/leading-comment normalization the loader does, so any second
reader of an agent file agrees with the review phase about its description.
`parseOptions` collapses the description to a single space-separated line: a YAML
block or folded scalar parses fine but yields embedded newlines, and the catalog
renders the description as one Markdown list item followed by an indented
invocation snippet, so continuation lines would land unindented between the two.

`external_reviewers` configures an ordered comma-separated reviewer chain using
`provider[:model[:effort]]` entries. It takes precedence over the legacy
`external_review_tool` and `external_review_model` keys. `custom` entries use
`custom_review_script` and cannot specify a model. An explicitly empty value
clears an inherited chain and disables external review.

`loopai --init` creates project-local commented defaults. `loopai --reset` restores global defaults interactively. `loopai --dump-defaults <dir>` extracts embedded defaults for inspection.

This is a clean configuration break: never add automatic reads or migrations from `~/.config/ralphex/` or `.ralphex/`.

Tests must redirect HOME or config paths to `t.TempDir()` and must never touch either real user configuration directory.

## Architecture and key patterns

- `cmd/loopai/main.go` parses flags, resolves executors, constructs the processor, and owns process-level cleanup.
- `pkg/processor/processor.go` coordinates task and review phases.
- `pkg/processor/phase` contains the individual phase engines.
- `pkg/executor` owns child-process invocation and stream parsing.
- `pkg/status` contains the shared execution model and stable protocol signals.
- `pkg/config/defaults` contains embedded config, prompts, and agents.

Task plans use `### Task N:` or `### Iteration N:` headings and Markdown checkboxes. The task phase handles only the first incomplete section per executor iteration.

`pkg/processor/prompts.go` expands two agent placeholders with the same
per-executor invocation snippet builder (Task tool for Claude, `spawn_agent` for
Codex): `{{agent:<name>}}` inlines one named agent, and `{{agents:dynamic}}`
renders the dynamic-agent catalog sorted by name, or
`(no project-specific agents configured)` when the project defines none. Only
the embedded `review_first.txt` uses the catalog; `review_second.txt` and the
embedded external-reviewer prompts do not, though both placeholders are expanded
on the external path too so customized prompts behave alike. The catalog pass runs
after agent-reference expansion, so `agentBodyText` strips `{{agents:dynamic}}`
from inlined agent bodies — otherwise raw catalog text lands inside an
already-escaped codex `task='...'` literal. The catalog also skips agents the same
prompt already inlines through `{{agent:<name>}}` — `agentRefNames` collects those
names before expansion — so a user copy of a base agent that carries a description
is listed and launched once, not twice. Which catalog entries actually run is decided by the model from
the descriptions — deliberately not by path or glob triggers in code — and the
prompt bounds the selection to roughly 0-3 agents launched in the same parallel
message as the base five. `FirstReviewPrompt` calls `warnMissingDynamicCatalog`,
which warns once per run when the project has dynamic agents but the effective
`review_first.txt` carries no placeholder: a prompt copy installed by an earlier
`--init` predates the catalog and would otherwise drop every dynamic agent
without a trace. The check belongs there and not in `expandDynamicAgentCatalog`,
which also runs for `review_second.txt` and the external prompts that
deliberately omit the placeholder.

`--gen-agents` is a standalone mode backed by `processor.ModeGenAgents` and
`phase.GenAgentsPhase` rather than ad-hoc executor wiring, so retry/limit policy
and section timing match the other phases. It runs one session with
`gen_agents.txt`, logs to `.loopai/progress/progress-gen-agents.txt`, then
reports the agent files present with their descriptions using
`config.ParseAgentOptions`; an unreadable file is reported inline rather than
aborting the listing. `describeAgentFile` reports what the loader will do with
each file rather than its description alone, so it gates emptiness on
`config.AgentFileHasBody` — the same comment-stripping check the loader uses.
A file with no body is ignored (replaced by the embedded default only for a
reserved name, dropped entirely otherwise), and frontmatter that fails to parse —
an unquoted `description` containing `: ` is the likely cause — is
indistinguishable from having none, so both are flagged instead of being listed
as working agents. An unquoted ` #` does not break parsing; YAML reads it as a
comment and truncates the description, so `gen_agents.txt` requires a
double-quoted description for both reasons. That distinction comes from
`config.AgentFrontmatterUnparsable` and not from a `---` prefix on the parsed body:
a working agent may open its body with a markdown rule, and a broken block written
below a `# ...` header never reaches the parsed form at all, so the prefix check
misdiagnoses both. `parseOptionsWithCommentRetry` accepts its comment-stripped retry
whenever that parse consumed a frontmatter block, not only when the block carried a
recognized key: a well-formed block holding only foreign keys must read the same
behind a comment header as it does without one, otherwise its raw `---` lines stay in
the agent body and valid YAML gets reported as broken. The report also runs on the failure path — a session that writes
files and then fails, times out, or is interrupted leaves them in the next run's
catalog, and the reserved-name warning would otherwise be lost with the error.
Overwriting a reserved base name warns instead of failing, because the user
reviews the generated files with `git diff` before committing them, and the
warning fires only for a file that actually has a body: a plain `--init` fills
`.loopai/agents/` with all-commented copies of the five base agents that override
nothing. Non-`.txt` files in the directory are reported as ignored rather than
skipped silently: the agent loader reads `.txt` only, so a session that disregards
the prompt and writes `.md` would otherwise leave files `git status` shows and
nothing ever loads. `clearStaleCmuxStatus` excludes the mode alongside the other
config utilities — it executes no plan and constructs no reporter, so clearing
would drop the previous run's completion pill with nothing to replace it. The
session resolves `task_model` only — `plan_model` does not
apply and no review model is printed. It is routed before branch and worktree
setup but still opens the repository to run `EnsureLocalGitignore`, since it tells
the user to inspect `git status` afterwards. `validateGenAgentsFlags` must reject
`--serve`/`--watch` alongside the other standalone modes: watch-only routing is
decided before the `ModeGenAgents` branch. Like `ModeTasksOnly`, the mode
short-circuits `resolveExternalReviewSelection`: it runs no review phase, so a
configured `external_reviewers` binary missing from `PATH` must not fail it. It is
also routed before `notify.New`, which validates channels eagerly: the mode sends
no notification, so a half-filled `notify_slack_*`/`notify_email_*` block must not
be what stops it. Notification setup covers only the plan-executing paths — the
close-out commands and watch-only mode return before it for the same reason.

The primary executor owns all repository writes. External reviewers produce findings only; the primary evaluates and fixes them using `review_model`, falling back to `task_model`. Reviewer chains run in order, and each reviewer loops until clean, its independent iteration cap, or its independent stalemate threshold before the next reviewer starts. Post-external review and finalize run once after the complete chain.

Claude is the default primary. `--codex` switches planning, tasks, internal reviews, evaluation, and finalize to Codex. `external_reviewers` entries require explicit `claude`, `codex`, or `custom` providers; duplicate providers with different models are supported. In the legacy path, `external_review_tool = auto` selects the other provider when installed. Missing automatic reviewers are skipped with a warning; missing explicit reviewers are errors.

Codex invocations use additive `-c` overrides so user `~/.codex/config.toml` settings remain available. loopai never writes to `~/.codex/`. `--pass-claude-md` lets Codex discover project `CLAUDE.md`; it does not install or link user-level files.

Alternative Claude-compatible providers live under `scripts/`. `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and uses native autopilot mode; plan creation deliberately uses `--autopilot --allow-all` without `--no-ask-user`. `scripts/pi-as-claude/pi-as-claude.sh` translates pi JSONL output and maps loopai model/effort settings to pi provider options. Detailed setup and wrapper behavior live in `docs/custom-providers.md`.

Worktrees live under `.loopai/worktrees/<branch>`. A new worktree branch is cut from the source checkout's current HEAD, including a non-default branch or detached HEAD; an existing plan branch is reused only if it contains the source HEAD seen before any auto-commit. `--base-ref` remains the review/template diff base and does not control worktree creation. With `-c`/`--commit`, loopai stages all non-ignored changes and commits them in the source checkout, advancing its branch or detached HEAD, before creating a fresh worktree; when reusing a valid existing plan branch, that new source commit is merged into it. Resume never auto-commits. Repository-level locking covers the auto-commit and worktree creation sequence and lock waits honor cancellation. The progress logger is created before changing into a worktree, so logs remain in the main checkout at `.loopai/progress/`.

cmux reporting is best-effort and must never affect execution. The status key and notification title are `loopai`. All calls go through the public `cmux` CLI and failures are ignored. After a completed run, `Reporter.Finish` intentionally leaves the final success or failure pill in place: `Stop` still clears the spinner and progress, but does not clear that pill. Abort paths do not call `Finish`, so `Stop` performs the full cleanup. A later run overwrites the pill, while `--clear`, a successful `--merge`, or a successful `--pr` removes it explicitly.

Standalone close-out routing happens before executor and notification dependencies are constructed. `--clear` exits before config loading; `--merge` and `--pr` load config for `vcs_command` and colors, then open Git directly. Base resolution accepts an explicit local branch or local `main`/`master`. Merge uses the registered base worktree, requires clean feature and base worktrees, overrides branch-level squash/no-commit defaults, verifies the feature commit became an ancestor of the base, never force-removes a close-out worktree, aborts conflicts as `git.ErrMergeConflict`, and deletes the feature branch only after verified cleanup. Ignored files do not make a worktree dirty and are deleted when the linked worktree is removed, so callers must preserve any local-only ignored files before `--merge`. PR creation requires every effective `origin` push URL to identify the same GitHub repository used by `gh`, pushes committed state, and derives metadata from an exact associated plan (including the active-plan fallback used by worktree runs). Tests use local bare remotes, Git push stubs, and `PATH`-injected `gh` stubs.

`cmd/loopai` resolves effective plan, task, review, and external-review models
and passes them to `cmux.Reporter`. Phase labels come from `status.PhaseHolder`;
review iteration labels come from `Reporter.WrapLogger`, which observes
structured `PrintSection` calls while forwarding the complete logger interface.
Keep `Reporter.WrapLogger` in the logger chain after dashboard setup. The
`progress.SectionTimer` sits below that cmux wrapper and above the dashboard
broadcast logger, preserving cmux's outermost rate-limit interfaces while timing
the structured sections. Use `runWithSectionTiming` for both the main runner and
interactive plan-creation paths; it calls `SectionTimer.FinishRun` immediately
after `Runner.Run` returns and before either caller handles the run error. Do not
defer it because dashboard shutdown can close the underlying log first. A nil
reporter must return the timer unchanged.

## Code style

- Use idiomatic Go and keep packages focused.
- Comments are lowercase except exported godoc.
- Wrap errors with context using `fmt.Errorf("context: %w", err)`.
- Define small interfaces at the consumer.
- Prefer existing dependencies; discuss new dependencies first.
- Use table-driven tests with `testify`.
- Test filesystem operations with `t.TempDir()`.
- Put tests in one corresponding `_test.go` file per source file.
- Generated mocks use `moq`; do not hand-edit them.
- Keep behavior cross-platform and use `filepath` for paths.

## Testing

After any code change:

```bash
make test
make lint
```

The full suite is required because configuration and progress paths cross package boundaries.
`make test` first validates Claude command symlinks, then runs the race-enabled
Go suite with coverage, and finally runs every retained provider-wrapper and
wrapper-documentation shell suite. CI runs the same asset and wrapper checks.

Dashboard e2e:

```bash
make e2e-setup
make e2e
make e2e-ui
```

Cross-compile when changing platform-sensitive code:

```bash
GOOS=windows GOARCH=amd64 go build ./...
```

Before tests involving configuration, snapshot or otherwise verify that real `~/.config/loopai/` and legacy `~/.config/ralphex/` content is unchanged.

## Development workflow

1. Read the relevant source, tests, embedded defaults, and plan.
2. Make the smallest cohesive change.
3. Add or update tests with the implementation.
4. Run focused tests while iterating.
5. Run `make test` and `make lint`.
6. Update user and developer documentation when behavior changes.
7. Commit a focused change with a meaningful message.

Do not edit `CHANGELOG.md` during normal development; it belongs to the release process.

Do not weaken tests to make a change pass. Do not skip validation between plan tasks. Preserve unrelated user changes in a dirty worktree.

## End-to-end smoke test

Prepare a toy repository and binary:

```bash
make e2e-prep
cd /tmp/loopai-test
.bin/loopai docs/plans/fix-issues.md
tail -f .loopai/progress/progress-fix-issues.txt
```

Review an existing branch:

```bash
make e2e-review
cd /tmp/loopai-review-test
.bin/loopai --review
tail -f .loopai/progress/progress-review.txt
```

Smoke-test Claude findings with Codex evaluation and fixes:

```bash
make e2e-codex
cd /tmp/loopai-review-test
.bin/loopai --codex --external-only
tail -f .loopai/progress/progress-codex.txt
```

## Pull-request checklist

- New behavior has tests, including error cases.
- `make test` passes.
- `make lint` reports no issues.
- Documentation and embedded config comments match behavior.
- No test touched real user configuration.
- The module path and `<<<RALPHEX:...>>>` signals remain unchanged.
- `CHANGELOG.md` is untouched unless release work explicitly requires it.
