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
make test       # race-enabled unit tests with coverage
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

The primary executor owns all repository writes. External reviewers produce findings only; the primary evaluates and fixes them.

Claude is the default primary. `--codex` switches planning, tasks, internal reviews, evaluation, and finalize to Codex. `external_review_tool = auto` selects the other provider when installed. Missing automatic reviewers are skipped with a warning; missing explicit reviewers are errors.

Codex invocations use additive `-c` overrides so user `~/.codex/config.toml` settings remain available. loopai never writes to `~/.codex/`. `--pass-claude-md` lets Codex discover project `CLAUDE.md`; it does not install or link user-level files.

Alternative Claude-compatible providers live under `scripts/`. `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and uses native autopilot mode; plan creation deliberately uses `--autopilot --allow-all` without `--no-ask-user`. `scripts/pi-as-claude/pi-as-claude.sh` translates pi JSONL output and maps loopai model/effort settings to pi provider options. Detailed setup and wrapper behavior live in `docs/custom-providers.md`.

Worktrees live under `.loopai/worktrees/<branch>`. The progress logger is created before changing into a worktree, so logs remain in the main checkout at `.loopai/progress/`.

cmux reporting is best-effort and must never affect execution. The status key and notification title are `loopai`. All calls go through the public `cmux` CLI and failures are ignored.

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
cd /tmp/loopai-review-test
.bin/loopai --review
```

Use `--codex --external-only` to smoke-test Claude findings with Codex evaluation and fixes.

## Pull-request checklist

- New behavior has tests, including error cases.
- `make test` passes.
- `make lint` reports no issues.
- Documentation and embedded config comments match behavior.
- No test touched real user configuration.
- The module path and `<<<RALPHEX:...>>>` signals remain unchanged.
- `CHANGELOG.md` is untouched unless release work explicitly requires it.
