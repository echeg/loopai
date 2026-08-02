# External Reviewers Chain

## Overview

Generalize the external review phase from a single tool (`external_review_tool` + `external_review_model`) to an ordered list of reviewers, each with its own provider and model:

```ini
external_reviewers = codex:gpt-5.5:xhigh, claude:fable:max
```

Reviewers run **sequentially, each until clean**: the first reviewer loops until it reports no findings, then the next one starts. Each reviewer stays read-only and produces findings; the primary executor (with `review_model`, falling back to `task_model`) evaluates and fixes them, exactly as today. The "primary owns all repository writes" invariant is untouched.

Problem it solves: today only one external provider can review a run. Users want a chain like "task with opus, then codex review, then a second claude-model review" without changing the primary provider.

## Context (from discovery)

- files/components involved:
  - `pkg/config/config.go`, `pkg/config/values.go`, `pkg/config/defaults/config` — config keys and parsing
  - `cmd/loopai/main.go` — CLI flag, `externalReviewSelection` resolution, dep checks, run-header/cmux/web labels
  - `pkg/processor/executor_factory.go` — building external executors (`buildExternalClaudeExecutor`, `buildExternalCodexExecutor`, `externalReviewModelEffort`)
  - `pkg/processor/runner.go` — `Executors` struct, `runExternalAndPostReview`
  - `pkg/processor/phase/external_review.go` — the review loop (`runLoop`, `Tool()`, stalemate state)
- related patterns found:
  - `model[:effort]` spec parsing already exists (`parseModelEffort`, `ResolveCodexModelEffort`)
  - the phase loop is already parameterized by tool name and executor — one loop per reviewer reuses it as-is
  - retry policy keys claude-account rotation and session timeout off the per-call `toolName`, so per-reviewer providers need no policy changes
- dependencies identified: `pkg/cmux` (`RunModels.ExternalReview`), `pkg/web` (`FormatRunParams`), notification/run-header strings in `cmd/loopai`

## Development Approach

- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods and modified functions/methods
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change (`go test ./pkg/... ./cmd/...` focused, `make test` + `make lint` at task boundaries)
- maintain backward compatibility: existing `external_review_tool` / `external_review_model` configs must behave exactly as before

## Testing Strategy

- **unit tests**: required for every task; table-driven with `testify`, one `_test.go` per source file, `t.TempDir()` for config files, never touching real `~/.config/loopai/` or `~/.config/ralphex/`
- **e2e tests**: dashboard e2e (`make e2e`) is unaffected — no UI structure changes, only a formatted params string; no new e2e tests required
- smoke test via `make e2e-review` scenario manually (Post-Completion)

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

- New config key `external_reviewers` (config file + `--external-reviewers` CLI flag): comma-separated ordered list of `provider[:model[:effort]]` entries; providers are `claude`, `codex`, `custom`.
- When `external_reviewers` is set (CLI > local > global), it wins over `external_review_tool`/`external_review_model`. When unset, the legacy pair resolves exactly as today and is represented internally as a one-element list — the rest of the pipeline only ever sees a list.
- `auto`/`none` remain legacy-key concepts. The list requires explicit providers; a missing explicit reviewer binary is an error (consistent with current "missing explicit reviewers are errors" rule).
- Duplicated providers with different models are allowed and expected (`claude:opus, claude:fable`), so executors are built per entry, not per provider.
- `custom` entries use `custom_review_script` and must not carry a model spec.
- Run order: sequential-until-clean. Reviewer i loops (find → evaluate → fix) until "no findings" or its iteration cap, then reviewer i+1 starts. Post-external review loop and finalize run once after the whole chain, as today.

## Technical Details

- **Spec type** (`pkg/config`): `ReviewerSpec { Provider, ModelSpec string }` plus `ParseExternalReviewers(s string) ([]ReviewerSpec, error)`. Entry parsing: first `:`-segment is the provider, the remainder is the `model[:effort]` spec passed through untouched (so `codex:gpt-5.5:xhigh` → provider `codex`, spec `gpt-5.5:xhigh`). Validation: known provider, non-empty entries, `custom` has empty spec.
- **Model defaults per entry** reuse the existing `externalReviewModelEffort` logic: claude entry without model → `opus:xhigh`; codex entry without model → `codex_model`/`codex_reasoning_effort` from config.
- **Executors wiring**: `processor.Executors` replaces the single `External Executor` + `Custom *executor.CustomExecutor` pair with `Externals []processor.ExternalReviewer` where `ExternalReviewer { Tool string; Exec Executor }` (custom entries wrap the CustomExecutor). The factory keeps the existing per-provider builders.
- **Phase**: `ExternalReviewPhase` holds the reviewer list. `Run` iterates reviewers, printing the existing per-tool section header and running the existing `runLoop` per reviewer with its own stalemate state and iteration cap. `Tool()` is replaced by an emptiness check plus a display label (used by `runner.runExternalAndPostReview`).
- **CLI resolution**: `externalReviewSelection` becomes a list-shaped resolution (`[]resolvedReviewer{Provider, Model, Effort}` + flags for auto/none provenance in the legacy path). Binary checks in `checkExecutionDeps` run per unique provider. `detectClaudeSwapRecovery` enables recovery when the primary is claude or any reviewer is claude.
- **Labels**: run header, cmux `RunModels.ExternalReview`, and `web.FormatRunParams` display the chain joined as `codex (gpt-5.5:xhigh) → claude (fable:max)`; single-reviewer output stays byte-identical to today.
- Processor `Config.ExternalReviewTool` (string) is superseded by the list on `processor.Config`; the factory fallback for direct `processor.New` callers builds a one-element list from the legacy fields.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): manual smoke runs, release notes

## Implementation Steps

### Task 1: Config key and reviewer-spec parsing

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/values.go`
- Modify: `pkg/config/defaults/config`
- Modify: `pkg/config/config_test.go`
- Modify: `pkg/config/values_test.go`

- [ ] add `ReviewerSpec` type and `ParseExternalReviewers` to `pkg/config` (provider validation, custom-with-model error, empty-entry error, whitespace trimming)
- [ ] add `ExternalReviewers string` + `ExternalReviewersSet bool` fields to `Config`, loaded in `values.go` with the same local-over-global override rules as `external_review_model`
- [ ] document the new key as a commented default in `pkg/config/defaults/config` (syntax, precedence over `external_review_tool`/`external_review_model`, sequential-until-clean semantics)
- [ ] write tests for `ParseExternalReviewers` (valid chains, model-less entries, custom, unknown provider, custom with model, empty string, trailing commas)
- [ ] write tests for config loading/override of `external_reviewers` (global vs local, explicit-set tracking, dump-defaults contains the comment)
- [ ] run tests - must pass before task 2

### Task 2: CLI flag and list-shaped selection resolution

**Files:**
- Modify: `cmd/loopai/main.go`
- Modify: `cmd/loopai/main_test.go`

- [ ] add `--external-reviewers` flag (string) with explicit-set tracking, mutually exclusive with `--external-review-tool`/`--external-review-model` (error if combined)
- [ ] generalize `externalReviewSelection` to carry `[]resolvedReviewer{Provider, Model, Effort}`; legacy tool/model path resolves to a one-element list, `auto`/`none` semantics unchanged
- [ ] resolve per-entry model/effort defaults (claude → `opus:xhigh`, codex → codex config defaults) reusing/relocating `externalReviewModelEffort` logic so cmd and factory share it
- [ ] extend `checkExecutionDeps`: verify binary per unique provider in the chain (explicit ⇒ error, not warning), `custom` entries require `custom_review_script`
- [ ] update `detectClaudeSwapRecovery` to enable recovery when primary is claude or any chain entry is claude
- [ ] write tests for flag parsing, mutual exclusion, chain resolution with defaults, dep-check errors (missing binary, custom without script), recovery detection
- [ ] run tests - must pass before task 3

### Task 3: Executor factory builds per-entry executors

**Files:**
- Modify: `pkg/processor/executor_factory.go`
- Modify: `pkg/processor/runner.go` (Executors struct only)
- Modify: `pkg/processor/executor_factory_test.go`

- [ ] add `ExternalReviewer { Tool string; Exec Executor }` and replace `Executors.External`/`Executors.Custom` with `Externals []ExternalReviewer`
- [ ] factory: build one executor per chain entry via existing `buildExternalClaudeExecutor` / `buildExternalCodexExecutor` / custom builder; duplicate providers with different models produce distinct executors
- [ ] keep the direct-`processor.New` fallback: when no chain is provided, synthesize a one-element chain from legacy `ExternalReviewTool`/`ExternalReviewModel` (including auto-selection and missing-binary downgrade for auto)
- [ ] write tests: chain of two providers, duplicate provider different models, custom entry, legacy fallback parity (same executor settings as before), auto downgrade still works
- [ ] run tests - must pass before task 4

### Task 4: Sequential chain in the external review phase and runner

**Files:**
- Modify: `pkg/processor/phase/external_review.go`
- Modify: `pkg/processor/phase/phase.go` (opts/interfaces as needed)
- Modify: `pkg/processor/runner.go`
- Modify: `pkg/processor/phase/external_review_test.go`
- Modify: `pkg/processor/runner_test.go`

- [ ] change `ExternalReviewPhase` to hold `[]ExternalReviewer`; `Run` iterates entries sequentially, per-entry section header, per-entry `runLoop` with own stalemate state and iteration cap; aggregate `HadFindings`
- [ ] replace `Tool()` usage in `runner.runExternalAndPostReview` with an enabled-check + chain label; keep none/disabled fast path and single post-review + finalize after the whole chain
- [ ] preserve legacy behavior for the one-element chain (log lines and evaluation flow unchanged); evaluation keeps using the review executor
- [ ] handle manual break and context cancellation across the chain (break stops remaining reviewers, same as current single-loop semantics)
- [ ] write tests: two-reviewer chain runs in order and each until clean, findings in reviewer 2 only still trigger post-review, break during reviewer 1 skips reviewer 2, stalemate patience is per reviewer, none-chain skips to finalize
- [ ] run tests - must pass before task 5

### Task 5: Run-header, cmux, and dashboard labels for chains

**Files:**
- Modify: `cmd/loopai/main.go` (runHeaderParams, cmuxRunModels, web run params)
- Modify: `cmd/loopai/main_test.go`
- Modify: `pkg/web/*` (only if FormatRunParams signature needs the joined string)

- [ ] format the chain as `codex (gpt-5.5:xhigh) → claude (fable:max)` for run header, notifications, cmux `RunModels.ExternalReview`, and `web.FormatRunParams`
- [ ] keep single-reviewer output byte-identical to current output (no churn for existing users)
- [ ] write tests for label formatting (single, chain, custom entry, none)
- [ ] run tests - must pass before task 6

### Task 6: Documentation

**Files:**
- Modify: `README.md`
- Modify: `llms.txt`
- Modify: `pkg/config/defaults/config` (final comment wording review)
- Modify: `docs/custom-providers.md` (custom entries in chains)

- [ ] document `external_reviewers` syntax, precedence over legacy keys, sequential-until-clean semantics, and the fix-ownership model (reviewers find, primary fixes with `review_model`)
- [ ] document `--external-reviewers` CLI flag and mutual exclusion with legacy flags
- [ ] note `custom` chain entries and their `custom_review_script` requirement
- [ ] verify embedded config comments match final behavior
- [ ] run `make test` (asset checks included) - must pass before task 7

### Task 7: Verify acceptance criteria

- [ ] verify chain config `external_reviewers = codex, claude:fable` produces two sequential review loops with correct executors (unit-level assertions already exist; re-check end to end via `make e2e-prep` smoke if feasible)
- [ ] verify legacy configs (`external_review_tool` alone, with model, `auto`, `none`, `custom`) behave identically to master
- [ ] verify explicit chain with missing binary fails with a clear error; legacy `auto` with missing binary still downgrades with a warning
- [ ] run full test suite: `make test`
- [ ] run `make lint`
- [ ] cross-compile check: `GOOS=windows GOARCH=amd64 go build ./...`

### Task 8: [Final] Update documentation and close out

- [ ] update `CLAUDE.md` (configuration section: new key; architecture note about reviewer chains) if behavior descriptions there became stale
- [ ] confirm `CHANGELOG.md` untouched
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- `make e2e-review` scenario with `external_reviewers = codex, claude` on a real branch: confirm codex loop reaches clean before the claude reviewer starts, and the primary commits fixes between iterations
- confirm cmux status line and dashboard show the chain label during a live run

**External system updates:**
- none — distribution is by source build; no packaging changes
