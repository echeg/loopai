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

The primary executor owns all repository writes. External reviewers produce findings only; the primary evaluates and fixes them using `review_model`, falling back to `task_model`. Reviewer chains run in order, and each reviewer loops until clean, its independent iteration cap, or its independent stalemate threshold before the next reviewer starts. Post-external review and finalize run once after the complete chain.

Claude is the default primary. `--codex` switches planning, tasks, internal reviews, evaluation, and finalize to Codex. `external_reviewers` entries require explicit `claude`, `codex`, or `custom` providers; duplicate providers with different models are supported. In the legacy path, `external_review_tool = auto` selects the other provider when installed. Missing automatic reviewers are skipped with a warning; missing explicit reviewers are errors.

Codex invocations use additive `-c` overrides so user `~/.codex/config.toml` settings remain available. loopai never writes to `~/.codex/`. `--pass-claude-md` lets Codex discover project `CLAUDE.md`; it does not install or link user-level files.

Alternative Claude-compatible providers live under `scripts/`. `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and uses native autopilot mode; plan creation deliberately uses `--autopilot --allow-all` without `--no-ask-user`. `scripts/pi-as-claude/pi-as-claude.sh` translates pi JSONL output and maps loopai model/effort settings to pi provider options. Detailed setup and wrapper behavior live in `docs/custom-providers.md`.

Worktrees live under `.loopai/worktrees/<branch>`. A new worktree branch is cut from the source checkout's current HEAD, including a non-default branch or detached HEAD; an existing plan branch is reused only if it contains the source HEAD seen before any auto-commit. `--base-ref` remains the review/template diff base and does not control worktree creation. With `-c`/`--commit`, loopai stages all non-ignored changes and commits them in the source checkout, advancing its branch or detached HEAD, before creating a fresh worktree; when reusing a valid existing plan branch, that new source commit is merged into it. Resume never auto-commits. Repository-level locking covers the auto-commit and worktree creation sequence and lock waits honor cancellation. The progress logger is created before changing into a worktree, so logs remain in the main checkout at `.loopai/progress/`.

cmux reporting is best-effort and must never affect execution. The status key and notification title are `loopai`. All calls go through the public `cmux` CLI and failures are ignored. After a completed run, `Reporter.Finish` intentionally leaves the final success or failure pill in place: `Stop` still clears the spinner and progress, but does not clear that pill. Abort paths do not call `Finish`, so `Stop` performs the full cleanup. A later run overwrites the pill, while `--clear`, a successful `--merge`, or a successful `--pr` removes it explicitly.

`--cmux-workspace` is part of that best-effort contract and must never affect execution either. `cmux.SpawnWorkspace` is the one cmux call that propagates its error instead of swallowing it, because the caller has to choose between exiting after hand-off and running locally; it returns `cmux.ErrNotInCmux` when `CMUX_WORKSPACE_ID` is unset or the `cmux` binary is absent. `handOffToCmuxWorkspace` is routed from `handleEarlyFlags`, so hand-off happens before config loading and executor resolution, and keeps `run` under the gocyclo limit. It covers plan execution and interactive `--plan` creation alike, but never `isStandaloneCommand` invocations (`--clear`, close-out, `--init`, `--dump-defaults`, reset-only), which own no sidebar state; that predicate is shared with `clearStaleCmuxStatus`. The workspace name comes from `cmuxWorkspaceName`: a `--branch` override wins, then `plan.ExtractBranchName`, then `loopai`, so cards line up with `.loopai/worktrees/<branch>`. The relaunch argv comes from `cmuxHandOffArgv`: `os.Executable()` plus `stripCmuxWorkspaceArg(os.Args[1:])`, which removes the bare and `=value` forms and is the recursion guard, prefixed with `env` and the currently-set variables in `cmuxEnvOptions` when there are any. That prefix exists because the new workspace runs a shell of cmux's own, which inherits cmux's environment and not loopai's, so an option provided through the environment would silently revert to its default after hand-off; `env` is used instead of shell assignment prefixes because the target shell is unknown. `cmuxEnvOptions` must list every `env:` tag in `opts`, and `TestCmuxEnvOptionsCoversOptionTags` enforces that by reflection. Every argv element is POSIX single-quote escaped into the `--command` string, since cmux sends it to the new workspace's shell as text. `cmux new-workspace` is bounded by `spawnTimeout` rather than the 2s `execTimeout`: it starts a terminal instead of updating a label, and a premature kill is ambiguous rather than cosmetic, because cmux may already have created the workspace while the caller reads the error as a failure and runs the plan locally too. Isolation is only achievable at the workspace boundary: `cmux set-progress` has no key and the pill key is the fixed constant `loopai`. Any failure, including an unresolvable executable or working directory, prints a warning and continues the run in the current terminal. A non-empty `o.PlanFile` that does not stat is refused before spawning, for the same reason: the child would otherwise be the first to notice, long after the workspace was created and focused, leaving this terminal claiming success with exit 0 and the user an orphan card to close by hand. That check is the same stat `plan.Selector` performs on a non-empty plan file, resolved against the same working directory, so it never refuses a hand-off that would have worked; an empty plan file, as under `--plan` or `--review`, skips it. A working directory that is not a repository root is refused before spawning for that same reason, since running from a subdirectory is the likelier mistake and the child's `must run from repository root` would arrive after the card exists. The guard is `fileExists(".git")`, the marker the run itself checks for, so hand-off stays config-independent wherever that marker exists; only its absence consults `config.LoadReadOnly` through `customVcsConfigured`, to tell a non-git `vcs_command`, which legitimately has no `.git`, apart from a subdirectory. An unreadable config is not one of those, because the child would fail to load it too. A possibly watch-only `--serve` is exempt via `mayBeWatchOnlyMode`, since it never reaches the repository-root check and runs from any directory. A successful hand-off leaves the originating workspace's completion pill alone, since the run reports into another card: `prepareStaleCmuxStatus` defers the stale-pill clear for `--cmux-workspace` exactly as it does for a possibly-watch-only `--serve`, and `handOffSucceeded` derives the preserve verdict from `handleEarlyFlags` returning an early exit with no error. Under `--cmux-workspace` the other early flags reach an early exit only after a failed hand-off, and with an error except for the standalone commands whose clear is a no-op anyway.

Standalone close-out routing happens before executor and notification dependencies are constructed. `--clear` exits before config loading; `--merge` and `--pr` load config for `vcs_command` and colors, then open Git directly. Base resolution accepts an explicit local branch or local `main`/`master`. Both commands take an optional positional argument naming the feature, so they can run from the root of the primary checkout or any other registered worktree instead of the feature checkout; `runCloseoutCommand` still requires a checkout root, not an arbitrary subdirectory. Without the argument the feature is the current branch and behavior is unchanged, including the invoking checkout's own clean-tree requirement; with it that top-level `IsDirtyAll` check is skipped and cleanliness is enforced per worktree inside `prepareMergeWorktrees`. `resolveFeatureBranch` resolves the identifier deterministically: an exact local branch first, then a plan file in the plans directory and its `completed/` subdirectory. For a located plan, the branch recorded for it in `.loopai/progress/` wins over the name derived from the filename, because a `--branch` override makes the filename imply a branch the run never created and an unrelated branch carrying that derived name would otherwise be merged and deleted; the newest record wins and an unrecorded plan falls back to derivation. Only records whose `Mode` creates a branch are consulted: `--review`, `--codex-only`, and plan creation write their own record for the same plan, under a distinct progress filename and with a later mtime, but their `Branch` header names whatever was checked out at the time, so `recordedBranchIsFeature` skips them rather than resolving the close-out to an unrelated branch. That check is a denylist, so an absent or future `Mode` still supplies the branch, since dropping a valid association silently falls back to the derivation it exists to override. `findRecordedPRPlan` keeps consulting every mode: its branch-to-plan direction is correct for review runs too. Either way the branch must exist locally. `readProgressAssociations` is the shared scan behind both this plan-to-branch lookup and `findRecordedPRPlan`'s branch-to-plan lookup. The plan-to-branch lookup scans every root `progressRecordRoots` returns, which is every registered worktree: the primary first, the invoking checkout second, then the rest, deduplicated through `sameProgressRoot`. The progress logger resolves `.loopai/progress` against the working directory before loopai changes into a worktree, so a run started from the primary records there even though it executed in a linked worktree, while a run started inside any linked worktree records in that worktree. Restricting the scan to the primary and the invoking checkout misses a run started in a third worktree, and a miss falls back to the filename derivation the record exists to override. The newest matching record across all roots wins. The branch-to-plan lookup behind PR metadata stays anchored at the invoking checkout, since its result feeds `readPRPlan`'s containment check and a record naming a plan in another worktree would turn a stats-only body into a hard failure. `recordedBranchForPlan` matches a record to a located plan by case-folded filename via `planAssociationKey`, not by absolute path, so the association survives a case-insensitive filesystem returning the caller's spelling, a record naming the `completed/` copy of a plan still present in the active directory, and a lookup running in a different worktree than the record. It matches the path the record names rather than that path's repo-contained resolution, so an out-of-tree `plans_dir` or a checkout moved since the run still supplies the branch; resolution inside the repository is required only by the PR-metadata consumer, which skips associations whose plan resolves nowhere. Missing the association is the dangerous direction, since it silently falls back to the filename derivation the record exists to override. `recordedPlanInRepo` re-anchors a recorded path at the root when it resolves inside the repository only through symlinks, which a purely lexical containment test rejects wherever the checkout sits behind one; it still refuses symlinked plans and paths genuinely outside the repository. Plan lookup, for both resolution and PR metadata, anchors `cfg.PlansDir` at the invoking checkout's root through `plansDirPath`, so a plan present only inside the unmerged feature worktree is invisible from the primary checkout. Filename derivation goes through `git.Service.EffectiveBranchName`, which resolves the plan's real on-disk filename case, so the derived name matches the one worktree creation produced on case-insensitive filesystems. A feature equal to the base is an error. So is a surplus positional: `main` records `args[1:]` through `applyPositionalArgs` and `validateCloseoutFlags` rejects them, because `--merge <base> <feature>` is the `--merge=<base> <feature>` form with the `=` forgotten, go-flags hands both tokens back as positionals, and silently taking the first would merge and delete the intended base branch. Merge uses the registered base worktree, falling back to the primary worktree when the base branch is checked out nowhere, requires clean feature and merge worktrees, overrides branch-level squash/no-commit defaults, verifies the feature commit became an ancestor of the base, never force-removes a close-out worktree, aborts conflicts as `git.ErrMergeConflict`, and deletes the feature branch only after verified cleanup. `prepareMergeWorktrees` returns a `mergeTargets` value covering three cases: the feature is the current checkout, it lives in another registered worktree, or it is checked out nowhere and the merge runs in the base worktree with worktree cleanup skipped entirely. A registered worktree whose directory was deleted by hand fails with prune guidance instead of a raw chdir error, since Git still treats its branch as checked out until `git worktree prune`. Feature-worktree cleanliness is validated before the merge, not only before cleanup, and `git.Service.BranchHash` reads the feature head without a checkout. Ignored files do not make a worktree dirty and are deleted when the linked worktree is removed, so callers must preserve any local-only ignored files before `--merge`. The success line names the removed worktree, resolved through `mergeTargets.removableWorktree`, because with an explicit feature the removal target comes from the worktree list rather than being the caller's own directory. PR creation requires every effective `origin` push URL to identify the same GitHub repository used by `gh`, pushes committed state, and derives metadata from an exact associated plan (including the active-plan fallback used by worktree runs). With an explicit feature it needs no checkout: `git.Service.BranchDiffStats` measures the branch tip against the base through `refs/heads/<branch>` so a same-named tag cannot shadow it, and `gh pr create --head <branch>` targets the resolved branch. Plan discovery never returns a path outside the repository root, so an out-of-tree `plans_dir` or recorded plan degrades to a stats-only PR body instead of failing `readPRPlan`'s containment check. Tests use local bare remotes, Git push stubs, and `PATH`-injected `gh` stubs.

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
