# Explicit provider specs: provider:model[:effort] everywhere, per-phase providers

## Overview

Today the primary executor is chosen once per run from three sources in order — `--codex`,
an explicit `executor` config key, then inference from the task model's name
(`applyCodexOverrides`, `cmd/loopai/main.go:6056`). The provider is a property of the *run*,
while the model is a property of the *phase*, and the two grammars disagree:
`external_reviewers` takes `provider[:model[:effort]]` while `plan_model`/`task_model`/
`review_model` take `model[:effort]`.

This plan makes the provider part of every model spec and a property of the phase:

1. **One grammar everywhere.** `provider[:model[:effort]]` for `plan_model`, `task_model`,
   `review_model`, and each `external_reviewers` entry, parsed by one shared parser.
2. **Per-phase providers.** `task_model = codex:gpt-6-astra:medium` with
   `review_model = claude:opus:high` runs tasks on codex and the whole review block on claude.
   This is new capability: today it is an immediate startup error.
3. **No implicit provider selection.** `--codex`, the `executor` key, and name-based inference
   are all removed. A spec without a provider prefix is a startup error naming the rewrite.
4. **Removed keys and flags fail loudly.** `--codex`, `--codex-only`, `--external-review-tool`,
   `--external-review-model`, and the config keys `executor`, `external_review_tool`,
   `external_review_model`, `codex_model`, `codex_reasoning_effort` each produce an actionable
   error instead of being ignored or silently honoured.

This is a deliberate breaking change with no deprecation window. Every existing configuration
that sets any of the above must be hand-edited once.

## Decisions

- **Context**: the provider was runtime state (`Config.Executor`) derived from three competing
  sources, so the grammar of a model spec could not express it. Wanting claude to review
  codex-written code was expressible only as an *external* reviewer, which is read-only and has
  its findings filtered and applied by a codex evaluator.

- **Chosen approach**: provider becomes a mandatory leading segment of every model spec, and
  the resolved provider becomes per-phase rather than per-run.

- **Mandatory prefix, no bare form** (user decision). Rejected "optional prefix with inference
  as fallback" because it keeps two ambiguities alive: `codex:high` parses as model `codex`
  with effort `high` under the old grammar and as provider `codex` with model `high` under the
  new one (`codex` and `claude` are both provider names and model-name prefixes —
  `claudeModelPrefixes`/`codexModelPrefixes`, `pkg/config/model_provider.go:5-7`), and
  inference is silently skipped for wrapper commands and unknown model names.

- **Rejected: a deprecation window** (bare form warns for one release). It requires both parse
  branches alive simultaneously and doubles the test matrix for a personal fork with one user.

- **The review block moves as a unit** (user decision). `review_model`'s provider governs
  internal review, external-findings evaluation, finalize, and report — the four consumers of
  the `review` executor built at `pkg/processor/runner.go:235`. Rejected a separate
  `finalize_model` key as YAGNI.

- **An unset `task_model` still means claude.** This is a default, not an inference: nothing is
  read from a model name. It keeps a bare `loopai docs/plans/x.md` working with no config.

- **`--pass-claude-md` is kept, not removed.** Its gate changes from "requires `--codex`" to
  "requires codex as the task or review provider".

- **Removed flags stay in the `opts` struct as hidden fields.** Deleting the field makes
  go-flags answer `--codex` with `unknown flag`, which does not say what to write instead.
  A hidden field lets loopai own the message.

- **`ModelProvider` survives, inference does not.** The function is still needed for two
  checks: the rewrite hint in the missing-prefix error, and the existing mismatch validation
  that rejects `codex:opus` (explicit provider naming a model of the other provider).

- **Verified facts** (all confirmed by reading the code during planning):
  - `applyCodexOverrides` (`cmd/loopai/main.go:6056`) is the single decision point for the
    primary; `o.Codex` at `:6059`, `cfg.ExecutorSet` at `:6062`, inference at `:6071-6078`.
  - `primaryProvider` (`cmd/loopai/main.go:2684`) is a binary claude/codex switch on
    `cfg.Executor`; every CLI-layer provider question routes through it.
  - `checkExecutionDeps` (`cmd/loopai/main.go:2814-2820`) checks exactly one primary binary,
    chosen by `primaryProvider`.
  - `config.ExecutorSourceFlag = "--codex"` (`pkg/config/config.go:101`) and the
    `--pass-claude-md requires --codex` error (`cmd/loopai/main.go:6085`) are the two
    user-visible hard-coded flag strings in Go.
  - The `executor` key is validated inline at `pkg/config/values.go:382-388`.
  - `executorFactory.Build` (`pkg/processor/executor_factory.go:19`) branches once on
    `cfg.isCodexExecutor()` and builds task and review executors inside the same branch.
  - `runner.go:235` (`review := execs.Review; if review == nil { review = execs.Task }`) feeds
    four phases: internal review (`:253`), external review/eval (`:265`), finalize (`:272`),
    report (`:275`).
  - `Config.isCodexExecutor()` (`pkg/processor/runner.go:63`) has 27 call sites. The
    prompt-side ones are `prompt_builder.go:63` and `prompts.go:144, 226, 258, 313, 360`.
  - `prompts.go:142` `formatAgentExpansion` emits Task-tool prose for claude and a
    `spawn_agent` block for codex; `spawn_agent` resolves only because `pkg/executor/codex.go`
    registers the agent via `-c agents.reviewer.description=...`. A provider mismatch here
    degrades review silently rather than failing.
  - `markFlagsSet` (`cmd/loopai/main.go:175`) carries an explicit long-name list at
    `:191-198`; `isFlagSet` (`:6090`) looks options up by long name, so any flag change must be
    mirrored there.
  - `--codex-only` (`cmd/loopai/main.go:59`) is a deprecated alias for `--external-only` and
    never implied a codex primary; `determineMode` (`:2899`, case at `:2907`) maps both to
    `ModeCodexOnly`, which stays as an internal constant name.
  - `ModeCodexOnly` bypasses the `codex_enabled` gate at `cmd/loopai/main.go:2752`, mirrored in
    `pkg/processor/executor_factory.go:86`. That behaviour is unchanged.
  - `buildExternalCodexExecutor` (`executor_factory.go:256`) sets `Sandbox = "read-only"` and
    `ForceReadOnly = true`; a review executor that must write has to come from the first-class
    builders, not this one.
  - Documentation blast radius: 31 live `--codex` occurrences in 12 files plus 13 live
    `--pass-claude-md` occurrences, excluding the `docs/plans/` archive. The three shell
    completion files contain no flag names and need no change.
  - Test surface: ~175 matching lines across 17 test files; `ExecutorCodex` alone is 111 lines
    in 12 files, `applyCodexOverrides` 17 lines in 2, `Codex: true` 8 lines in 2.

## Context (from discovery)

- Files/components involved:
  - `cmd/loopai/main.go` — `opts` struct (`:43`), `markFlagsSet` (`:175`), `primaryProvider`
    (`:2684`), `checkClaudeDep`/`checkCodexDep` (`:2575`/`:2589`), `checkExecutionDeps`
    (`:2814`), `determineMode` (`:2899`), `validateModelSpecs` (`:3016`),
    `validateModelProviders` (`:3033`), `validateModelSpec` (`:3055`), `validateEffort`
    (`:3071`), `validateReviewerEfforts`/`validateReviewerProviders` (`:3082`+),
    `applyCodexOverrides` (`:6056`)
  - `pkg/config/config.go` — `ParseExternalReviewers` (`:46`), executor constants (`:94-104`),
    config fields (`:123-168`), `IsRealClaudeCommand`/`IsRealCodexCommand` (`:517`/`:523`),
    `CodexExecutorSandbox` (`:541`)
  - `pkg/config/values.go` — key parsing (`:218-294`, `:382-399`)
  - `pkg/config/model_provider.go` — `ModelProvider` and the prefix tables
  - `pkg/processor/runner.go` — `isCodexExecutor` (`:63`), `toPhaseConfig`, executor wiring
    (`:235-276`)
  - `pkg/processor/executor_factory.go` — `Build` (`:16`), `buildClaudeExecutors` (`:189`),
    `buildCodexExecutors` (`:291`), external builders (`:240`, `:256`), `parseModelEffort`
    (`:394`)
  - `pkg/processor/prompt_builder.go`, `pkg/processor/prompts.go` — provider-dependent prompt
    rendering
  - `pkg/processor/phase/phase.go` — `isCodexExecutor` (`:39`), `executorName` (`:43`)
  - `pkg/config/defaults/config`, `pkg/config/defaults/prompts/*`
  - `assets/claude/skills/loopai-orca`, `loopai-plan`, `loopai`; `assets/codex/skills/` mirrors
  - `README.md`, `llms.txt`, `CLAUDE.md`, `docs/custom-providers.md`, `Makefile:125`

- Related patterns found: `ParseExternalReviewers` already implements the target grammar and is
  the model for the shared parser. `prependCodexReviewGuidance` and `prependCodexTaskGuidance`
  are already split by phase, so the prompt layer is half-prepared for per-phase providers.

- Dependencies identified: no new third-party dependencies.

## Development Approach

- **Testing approach**: TDD (tests first)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- This change intentionally breaks backward compatibility; every break must be a clear error,
  never a silent behaviour change
- Tests must redirect HOME or config paths to `t.TempDir()` and must never touch either real
  user configuration directory

## Testing Strategy

- **Unit tests**: required for every task, table-driven with `testify`
- **Error-message tests**: every removed flag and key needs a test asserting the error text
  names both the removed thing and its replacement — the whole point of the change is that the
  user is told what to write instead
- **Prompt-rendering tests**: `formatAgentExpansion` under a claude review provider with a
  codex task provider must render Task-tool prose, and the mirror case must render
  `spawn_agent`. This cannot be observed from behaviour, only from the rendered prompt text
- **E2E tests**: the dashboard e2e suite does not exercise flags; no e2e changes expected
- **Shell suites**: `make test` runs skill-asset, manifest, grill-skill, completion, and
  provider-wrapper checks; skill edits must keep those green

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, documentation inside this repository
- **Post-Completion** (no checkboxes): the user's own machine-local config migration

## Implementation Steps

### Task 1: Shared provider spec parser in pkg/config
- [x] write table-driven tests for a new `ParseProviderSpec(string) (ProviderSpec, error)` in `pkg/config/provider_spec_test.go`: `codex:gpt-6-astra:high`, `claude:opus`, `codex` (provider only), `codex::medium` (default model, explicit effort), `custom`, leading/trailing spaces, empty string, four-segment input, unknown provider, empty provider (`:opus:high`)
- [x] write tests asserting the error for a spec with no provider segment names the value and the suggested rewrite derived from `ModelProvider`
- [x] add `ProviderSpec` struct (`Provider`, `Model`, `Effort`) and `ParseProviderSpec` in `pkg/config/provider_spec.go`
- [x] reuse it from `ParseExternalReviewers` (`pkg/config/config.go:46`) so both paths share one grammar, keeping the existing `custom` rule (no model allowed) and the existing per-entry error wording
- [x] run `go test ./pkg/config/...` - must pass before next task

### Task 2: Removed config keys fail with actionable errors
- [x] write tests in `pkg/config/values_test.go` asserting a hard error for each of `executor`, `external_review_tool`, `external_review_model`, `codex_model`, `codex_reasoning_effort`, with the error naming the key and the replacement spelling
- [x] write a test that a config with none of those keys still loads unchanged
- [x] write a test that the error fires for a key present in the global config as well as the local one
- [x] replace the `executor` parse block (`pkg/config/values.go:382-388`) and the four other key parse sites with removal errors
- [x] delete the now-unreachable `Config.Executor`, `ExecutorSet`, `ExternalReviewTool*`, `ExternalReviewModel*`, `CodexModel*`, `CodexReasoningEffort*` fields and their `ExecutorSource*` constants (`pkg/config/config.go:94-104`, `:143-146`, `:167`)
  - ⚠️ deleted `ExecutorSet`, `ExternalReviewToolSet`, `ExternalReviewModelSet`, `CodexModel`, `CodexReasoningEffort`, and `ExecutorSourceConfig`, which only a config key could reach. `Executor`/`ExecutorSource` (written by `--codex` and inference) and `ExternalReviewTool`/`ExternalReviewModel` (written by the legacy flags and the resolved single reviewer) are still reachable from the CLI, so they became runtime-only `json:"-"` fields; their deletion moved to Tasks 3 and 5 alongside their writers
  - ➕ removed the five keys from the embedded defaults (`codex_model = gpt-5.5`, `codex_reasoning_effort = xhigh`, `external_review_tool = auto`, `external_review_model =`) and guarded that with a test; a bare codex spec now uses the codex CLI's own default instead of gpt-5.5:xhigh, as the Grammar section specifies, and the automatic reviewer remains the implicit default when `external_reviewers` is unset
  - ➕ `ResolveCodexModelEffort`/`ResolveExternalReviewerModelEffort` lost their codex-default parameters; `resolveExternalReviewSelection`/`resolveReviewerChain` lost their now-unused `opts` parameter
- [x] run `go test ./pkg/config/...` - must pass before next task

### Task 3: Removed CLI flags fail with actionable errors
- [x] write tests in `cmd/loopai/main_test.go` asserting an error for `--codex`, `--codex-only`, `--external-review-tool`, `--external-review-model`, each naming the replacement (`--task-model codex:<model>`, `--external-only`, `--external-reviewers`)
- [x] write a test that the error arrives before config loading and before any dependency check, so a machine without the binary still gets the migration message
- [x] mark those four `opts` fields hidden (`cmd/loopai/main.go:53`, `:54`, `:59`, `:68`) instead of deleting them, so go-flags accepts and loopai explains
- [x] add a `rejectRemovedFlags(o opts) error` called early in the startup path
- [x] update the long-name list in `markFlagsSet` (`cmd/loopai/main.go:191-198`) and the mutual-exclusion lists that carry the literal flag strings (`:3181`, `:3239`, `:3263`, `:3278`, `:5903-5904`)
- [x] ➕ delete the runtime-only `Config.ExternalReviewTool`/`ExternalReviewModel` fields (kept by Task 2 because the legacy flags still wrote them) and the legacy single-reviewer branch of `resolveReviewerChain` that reads them, keeping the automatic reviewer for an unset `external_reviewers`
  - ⚠️ `--codex` no longer reaches `applyCodexOverrides`, so the `ExecutorSourceFlag` constant and the `o.Codex` branch were deleted here; the primary is now chosen by task-model inference alone until Task 5 replaces it. A codex-wrapper primary with a claude-named model is unreachable in the interim and returns with Task 4/5's `codex:<model>` specs
  - ➕ `validateExternalReviewFlags`, `applyExternalReviewCLIOverrides`, `applyEffectiveExternalReview`, and the legacy-flags warning in `printExternalReviewWarnings` were deleted with the legacy branch; the processor factory lost its `AppConfig.ExternalReviewTool/Model` fallbacks
  - ➕ the rewrite hint folds `--external-review-tool`/`--external-review-model` into one ready `--external-reviewers=` entry (`none` → empty chain, `auto` → omit or name a chain, a bare model → provider from `ModelProvider`)
- [x] run `go test ./cmd/loopai/...` - must pass before next task

### Task 4: Strict validation of plan/task/review specs
- [x] write tests for a rewritten `validateModelSpecs` (`cmd/loopai/main.go:3016`): a bare `opus:high` errors naming `claude:opus:high`; a bare unknown model errors without a suggestion; `custom:...` is rejected for a phase spec; an unknown provider errors; an unknown effort still errors through `validateEffort`
- [x] write tests that `codex:opus` and `claude:gpt-6-astra` are rejected as provider/model mismatches, and that the check is skipped when the corresponding command is a wrapper (`IsRealClaudeCommand`/`IsRealCodexCommand`)
- [x] write a test that an unset spec is valid and that `review_model`/`plan_model` inherit `task_model` including its provider
- [x] rewrite `validateModelSpec` (`:3055`) over `ParseProviderSpec`, dropping the "looks like an external_reviewers entry" branch, which the unified grammar makes obsolete
- [x] fold `validateModelProviders` (`:3033`) into the same pass now that the provider is explicit rather than inferred
  - ⚠️ `validateModelProviders` is gone; the mismatch check now compares each spec's model with its own provider and skips the provider whose command is a wrapper. Because the executors are still built for one provider per run until Tasks 5 and 7, `rejectMixedPhaseProviders` temporarily rejects a plan or review provider that differs from the task provider ("per-phase providers are not supported yet"); Task 5/7 must delete it together with its tests and the integration case that asserts it
  - ➕ `executorModelSpec` strips the provider at the four processor `Config` sites and in `codexBannerForSpec`, because `parseModelEffort` still splits at the first colon; Task 7 must delete it when the factory takes `ParseProviderSpec` directly
  - ➕ `applyCodexOverrides` now selects codex from the explicit `codex:` prefix instead of `ModelProvider`, and no longer skips selection under a claude wrapper command, so `codex:<model>` works with a wrapper in the interim; the `--plan-model`/`--task-model`/`--review-model` help text now documents `provider[:model[:effort]]`
- [x] run `go test ./cmd/loopai/...` - must pass before next task

### Task 5: Per-phase provider resolution replaces applyCodexOverrides
- [x] write tests for a new `resolvePhaseProviders(o opts, cfg *config.Config) (phaseProviders, error)` covering: both phases codex, both claude, task codex with review claude, task claude with review codex, unset `task_model` defaulting to claude, `plan_model` following `task_model` when unset
- [x] write a test that `--pass-claude-md` is accepted when either the task or the review provider is codex and rejected when neither is
- [x] replace `applyCodexOverrides` (`cmd/loopai/main.go:6056`) with the new resolver and delete the inference branch
- [x] replace `primaryProvider` (`:2684`) with an accessor for the task provider and update its call sites
- [x] delete `ExecutorSourceFlag`/`ExecutorSourceConfig`/`ExecutorSourceInferred`/`ExecutorSourceDefault` usage from the startup banner and print the resolved provider per phase instead
- [x] ➕ delete the runtime-only `Config.Executor`/`ExecutorSource` fields and the remaining `ExecutorSource*` constants (kept by Task 2 because `--codex` and inference still wrote them)
- [x] write tests for the startup banner asserting one line per phase in the form `<phase>: <provider> <model>[:effort]` for plan, task, review, and external review, covering a same-provider run and a cross-provider one
- [x] write a test that a claude run prints its task and review models, which today only the codex branch does
- [x] merge `printExecutorInfo` and `printCodexExecutorInfo` (`cmd/loopai/main.go:3466`, `:3499`) into one provider-agnostic renderer, keeping codex-only fields (`sandbox`, CLAUDE.md passthrough) as indented lines under the phase that owns them
  - ⚠️ `Config.Executor`/`ExecutorSource` became runtime-only `PlanProvider`/`TaskProvider`/`ReviewProvider` fields set by `applyPhaseProviders`; the processor (`isCodexExecutor`, `phase.Config.isCodexExecutor`, the run record) still reads `TaskProvider` for every phase, and `rejectMixedPhaseProviders` stays until Task 7 builds executors per phase. Processor tests set both `TaskProvider` and `ReviewProvider` so Tasks 7-9 can switch the review reads without re-editing them. `ExecutorClaude`/`ExecutorCodex` stay as executor labels for the orca title, progress header, and run record
  - ⚠️ the banner prints the phases the mode runs rather than all four: full → task and review, tasks-only and `--gen-agents` → task, `--review`/`--external-only` → review, plan creation → plan. The external-review line now uses the same `<provider> <model>[:effort]` form, comma-joined, with `(auto-selected)` appended; an unset model renders as `default`
  - ➕ `cmuxRunModels` resolves each phase label under its own provider through the same `resolvePhaseBanner`, which strips the provider prefix; `codexModelBanner`/`codexPlanBanner`/`codexBannerInfo`/`configuredModelLabel`/`codexBannerValue` were deleted with their tests. `detectClaudeSwapRecovery` now counts any claude plan, task, or review phase
- [x] run `go test ./cmd/loopai/...` - must pass before next task

### Task 6: Dependency checks cover every distinct provider
- [x] write tests for `checkExecutionDeps` (`cmd/loopai/main.go:2814`) asserting that a task-codex/review-claude run requires both binaries, that each missing binary produces its own named error, and that a provider appearing in several roles is checked once
- [x] write a test that a missing *reviewer* binary still degrades per the existing explicit/auto rules, unchanged by this task
- [x] rewrite the primary check as a deduplicated loop over the distinct providers of the plan, task, and review phases plus the reviewer chain
- [x] run `go test ./cmd/loopai/...` - must pass before next task

### Task 7: Executor factory builds task and review from their own providers
- [x] write tests in `pkg/processor/executor_factory_test.go` asserting that a codex task provider with a claude review provider yields a `CodexExecutor` in the Task slot and a `ClaudeExecutor` in the Review slot
- [x] write a test that a cross-provider review executor carries write-capable settings and is NOT built through `buildExternalCodexExecutor` — assert `ForceReadOnly` is false and the sandbox is not `read-only`
- [x] write a test that the Review slot stays nil when both provider and resolved model/effort match the task executor, preserving the existing single-executor optimisation
- [x] replace the single `cfg.isCodexExecutor()` branch in `Build` (`pkg/processor/executor_factory.go:19`) with per-phase construction
- [x] replace `parseModelEffort` (`:394`) uses with the shared `ParseProviderSpec`
  - ⚠️ the factory derives each phase's provider from its own spec (`Config.taskSpec()`/`reviewSpec()`: unset task → claude, unset review → task spec whole) rather than from `AppConfig.TaskProvider`/`ReviewProvider`, so processor `Config.TaskModel`/`ReviewModel` now carry full `provider[:model[:effort]]` specs and `executorModelSpec` is deleted. This also makes plan mode build its executor from the plan spec's provider. Tasks 8/9 should switch `isCodexExecutor` and the prompt/phase reads to the same `taskSpec()`/`reviewSpec()` so the processor has one provider source, then drop the `AppConfig.*Provider` reads from the processor
  - ⚠️ `rejectMixedPhaseProviders` is kept (with its tests) until Task 9: executors are now per phase, but prompts and phase naming still read the task provider, and a mixed run would silently render the wrong agent syntax. Task 9 must delete it together with its tests and the integration case that asserts it
  - ➕ `ResolveCodexModelEffort` became provider-aware `ResolveModelEffort(config.ProviderSpec)`, used by the factory and the startup banner; `ResolveExternalReviewerModelEffort` parses through `ParseProviderSpec`. Automatic external-reviewer selection keys off the task spec, the external codex reviewer inherits `idle_timeout` when codex runs either the task or the review phase, and the `--pass-claude-md` setup hint fires for a codex review phase too. The run record's task/review model fields now record the full provider spec
- [x] run `go test ./pkg/processor/...` - must pass before next task

### Task 8: Prompt rendering follows the phase provider
- [ ] write tests in `pkg/processor/prompts_test.go` asserting that with a codex task provider and a claude review provider, `FirstReviewPrompt` renders Task-tool agent prose and carries no codex review guidance, while `TaskPrompt` still carries the codex task guidance
- [ ] write the mirror test (claude task provider, codex review provider) asserting `spawn_agent` blocks appear in the review prompt only
- [ ] write a test that `{{agents:dynamic}}` renders with the review phase's provider syntax
- [ ] write a test that `ExternalEvaluationPrompt`'s evaluator name (`prompt_builder.go:63`) reports the review provider, not the task provider
- [ ] thread a phase provider through `formatAgentExpansion` (`prompts.go:142`), `prependCodexReviewGuidance` (`:225`), `prependCodexTaskGuidance` (`:257`), and the two agent-catalog sites (`:313`, `:360`), replacing `b.cfg.isCodexExecutor()`
- [ ] run `go test ./pkg/processor/...` - must pass before next task

### Task 9: Phase-level naming and status reporting
- [ ] write tests that `executorName()` (`pkg/processor/phase/phase.go:43`) reports the review provider for the review, external-eval, finalize, and report phases and the task provider for the task phase
- [ ] write tests that the cmux reporter and startup banner show both providers when they differ
- [ ] replace `phase.Config.isCodexExecutor()` with per-phase provider fields populated by `toPhaseConfig` (`pkg/processor/runner.go`)
- [ ] update the section labels and `status` strings that name the executor
- [ ] run `go test ./pkg/processor/... ./pkg/status/... ./pkg/cmux/... ./pkg/orca/...` - must pass before next task

### Task 10: Update Claude and Codex skills
- [ ] write or update the assertions in `scripts/check-grill-skill_test.sh` and the skill-asset checks that cover the changed skill text
- [ ] remove `--codex` from the `loopai-orca` passthrough allowlist table and argument hint (`assets/claude/skills/loopai-orca/SKILL.md:4`, `:36`, `:42`, `:192` and the `assets/codex/skills/` mirror)
- [ ] update the `FLAGS` derivation in `loopai-plan` (`assets/claude/skills/loopai-plan/SKILL.md:313`, `:322`, `:325` and the codex mirror at `:178`) to drop the `executor` key mapping
- [ ] update `assets/claude/skills/loopai/SKILL.md:54` and the codex mirror
- [ ] bump the version in `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` to the same value
- [ ] run `make check-symlinks check-codex-skills check-plugin test-grill-skill` - must pass before next task

### Task 11: Update user and developer documentation
- [ ] update `README.md` (14 `--codex` sites incl. `:37`, `:102`, `:321-330`, `:389`, `:509`, `:587`, `:718-724`, `:751-759`, `:794-814`)
- [ ] update `llms.txt` (`:23`, `:45`, `:98-100`, `:161`, `:163`)
- [ ] update `docs/custom-providers.md` (17 `--codex` and 6 `--pass-claude-md` sites, densest at `:5-25`, `:38-41`, `:65-81`)
- [ ] update `CLAUDE.md` (`:100`, `:261`, `:283`, `:308`, `:351`, `:487-491`, `:686`)
- [ ] update the embedded config comments (`pkg/config/defaults/config:56-81`, `:289`, `:299`) and the four prompts naming `--codex-only` (`codex.txt:74`, `custom_eval.txt:74`, `review_first.txt:114`, `review_second.txt:85`)
- [ ] update `Makefile:125`
- [ ] leave `CHANGELOG.md` and everything under `docs/plans/completed/` untouched - the archive is history
- [ ] run `make test` - must pass before next task

### Task 12: Verify acceptance criteria
- [ ] verify all four Overview requirements are implemented
- [ ] verify each removed flag and key produces an error naming its replacement
- [ ] verify a task-codex/review-claude run builds the right executors and renders the right agent syntax in both phases
- [ ] run the full test suite with `make test` (closed stdin: `make test </dev/null`)
- [ ] run `make lint` - all issues must be fixed
- [ ] cross-compile with `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] verify test coverage meets the project standard (80%+)

### Task 13: [Final] Update project knowledge docs
- [ ] fold the new provider model into the CLAUDE.md architecture section, replacing the executor-inference paragraph
- [ ] verify no remaining live reference to `--codex`, `--codex-only`, `--external-review-tool`, `--external-review-model`, `executor =`, `codex_model`, or `codex_reasoning_effort` outside `CHANGELOG.md` and `docs/plans/completed/`

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details

### Grammar

One grammar for every model-bearing setting:

```
spec        := provider [ ":" model [ ":" effort ] ]
provider    := "claude" | "codex" | "custom"
effort      := "low" | "medium" | "high" | "xhigh" | "max"
```

An empty model segment means the provider CLI's own default, so `codex::medium` replaces the
old effort-only form `:medium`. `custom` is valid only inside `external_reviewers` and carries
no model.

### Resolution order

1. `task_model` provider is the task provider; unset means claude.
2. `review_model` provider governs internal review, external-findings evaluation, finalize, and
   report; unset means it inherits `task_model` whole, provider included.
3. `plan_model` provider governs plan creation; unset means it inherits `task_model` whole.
4. Each `external_reviewers` entry keeps its own provider, unchanged.

### Startup banner

The banner prints one line per phase, provider first, so a cross-provider run is readable at a
glance. This replaces the asymmetry where only the codex branch printed its models while a
claude run printed none:

```
plan:            claude opus:high
task:            claude opus:high
review:          claude opus:xhigh
external review: codex gpt-6-astra:high
```

```
task:            codex gpt-6-astra:medium
  sandbox:       danger-full-access
review:          claude opus:xhigh
external review: claude opus:high, codex gpt-6-astra:high
```

Codex-only fields stay indented under the phase that owns them. A phase that inherits its spec
whole from `task_model` still prints its own resolved line rather than being omitted.

### Migration mapping

| removed | replacement |
|---|---|
| `--codex` | `--task-model codex:<model>[:effort]` |
| `executor = codex` | `task_model = codex:<model>[:effort]` |
| `--codex-only` | `--external-only` |
| `--external-review-tool X` | `--external-reviewers X[:model[:effort]]` |
| `--external-review-model M` | the model segment of the matching `external_reviewers` entry |
| `codex_model` / `codex_reasoning_effort` | the model and effort segments of each codex spec |
| `task_model = gpt-6-astra:medium` | `task_model = codex:gpt-6-astra:medium` |

### Error shape

Every removed thing produces one line naming what was found and one naming what to write:

```
task_model "gpt-6-astra:medium" is missing a provider prefix;
write "codex:gpt-6-astra:medium"
```

```
--codex was removed; set the provider in the model spec instead,
e.g. --task-model codex:gpt-6-astra:medium
```

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Local configuration migration** (required before the next run):
- `~/.config/loopai/config` currently holds `task_model = gpt-6-astra:medium`,
  `review_model = gpt-6-astra:high`, empty `codex_model` and `codex_reasoning_effort`, and
  `external_reviewers = claude:opus:high`. After this change it must read
  `task_model = codex:gpt-6-astra:medium`, `review_model = codex:gpt-6-astra:high` (or
  `claude:opus:high` to put the whole review block on claude), with the two `codex_*` lines
  deleted. `external_reviewers` is already in the target grammar and needs no edit.
- Any `.loopai/config` in other checkouts needs the same treatment.
- Any shell alias or script passing `--codex` must be updated; it will now fail loudly.

**Manual verification**:
- Run one real plan with `task_model = codex:...` and `review_model = claude:opus:high` and
  confirm from the progress log that the review phase launched parallel Task agents rather than
  a single pass — this is the one failure mode that produces no error.
