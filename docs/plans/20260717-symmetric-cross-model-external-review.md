# Symmetric cross-model external review

## Overview

Make external review symmetric across the first-class Claude and Codex executors. The default external reviewer becomes `auto`: Claude-led runs keep their existing Codex external review, while `--codex` runs automatically add a read-only-intent Claude review using `opus:xhigh`. In the Codex-led pipeline, Claude reports findings only; Codex evaluates every finding, performs all code changes, repeats the external review loop, and owns post-review/finalize.

The feature must also expose explicit `claude`, `codex`, `custom`, and `none` choices plus an executor-aware `external_review_model`. It must preserve existing custom prompts and the legacy `CODEX_REVIEW_DONE` signal, provide provider-neutral progress/status output, and keep Codex-only installations usable when an automatically selected Claude binary is absent.

## Context

The orchestration loop is already role-oriented: `processor.Executors` has `Task`, `Review`, `External`, and `Custom`, and `ExternalReviewPhase` passes external findings to the primary review executor for evaluation and fixes. The current asymmetry is imposed around that reusable loop:

- `cmd/ralphex/main.go` limits `--external-review-tool` to `codex|custom|none`, rejects external modes under `--codex`, and forces `external_review_tool=none` for a Codex executor.
- `pkg/processor/executor_factory.go` only builds external Codex and custom executors; the Codex-primary branch deliberately leaves `Executors.External` nil.
- `pkg/processor/phase/external_review.go`, prompt interfaces, signals, phases, sections, and web/progress parsing use Codex/Claude-specific names even though custom external review already shares the loop.
- `executor.ClaudeExecutor` normally inherits `--dangerously-skip-permissions`; an external Claude reviewer needs a dedicated review-only argument policy.
- External Codex uses `--sandbox read-only` and preserves user Codex configuration. The approved Claude analogue preserves `CLAUDE.md`, skills, hooks, MCP, and Bash access, but strips permission bypass, selects `--permission-mode=plan`, and disallows the built-in `Edit`, `Write`, and `NotebookEdit` tools. This is practical protection rather than an OS-level filesystem sandbox; prompts must also forbid side-effecting commands.

Approved behavior:

- Default `external_review_tool=auto` resolves Claude primary to Codex external review and Codex primary to Claude external review.
- The automatic Claude external model is `opus:xhigh`; explicit `external_review_model` overrides the selected provider's model/effort.
- `codex_enabled=false` continues to disable automatic external review for backward compatibility; an explicitly selected provider still expresses user intent.
- `--codex --external-only` and the deprecated `--codex --codex-only` alias become valid and run external Claude review with Codex evaluation/fixes/finalize.
- Missing automatically selected external binaries produce a visible warning and disable only the external phase. A missing explicitly selected external binary is a startup error. Authentication, quota, and runtime failures after process launch remain normal errors/retries and are never silently skipped.
- Explicit same-provider review is allowed with a weak-signal warning. `custom` combined with an explicit external model is a configuration error.
- New runs emit `<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>`; `<<<RALPHEX:CODEX_REVIEW_DONE>>>` remains recognized for customized and historical prompts/logs.
- No new Go dependencies are required. Completed plans and `CHANGELOG.md` remain untouched.

## Validation Commands

Run targeted commands after the task that names them, then run the full gate in the final task:

```bash
go test ./pkg/config
go test ./cmd/ralphex
go test ./pkg/executor
go test ./pkg/processor/... 
go test ./pkg/status ./pkg/progress ./pkg/web
make fmt
make test
make lint
GOOS=windows GOARCH=amd64 go build ./...
git diff --check
```

Real Claude/Codex execution is deliberately excluded from automatic validation because it consumes account credits. The repository's toy-project E2E is listed under Post-Completion and requires explicit user approval.

## Implementation Steps

### Task 1: Add the external-review configuration surface
- [x] Extend `pkg/config/values.go` and `pkg/config/config.go` with `external_review_model` plus the required raw-value/set sentinel used by global/local merge precedence; update comments and JSON serialization consistently.
- [x] Define and use constants for `auto`, `claude`, `codex`, `custom`, and `none` instead of spreading new string literals through config and processor code.
- [x] Change the embedded `pkg/config/defaults/config` default to `external_review_tool=auto`, document the dynamic provider defaults, and leave `external_review_model` empty so runtime resolution can choose Codex config defaults or Claude `opus:xhigh`.
- [x] Update config loading, merging, default-dump/install expectations, and tests in `pkg/config/values_test.go`, `pkg/config/config_test.go`, and `pkg/config/defaults_test.go`; cover explicit empty/model values, local-over-global precedence, the new tool values, and the fact that embedded defaults are not marked as user-explicit.
- [x] Run `go test ./pkg/config` and fix all failures before continuing.

### Task 2: Resolve auto selection, CLI precedence, and dependency policy
- [ ] Add `claude` and `auto` choices to `--external-review-tool`, add `--external-review-model=<model[:effort]>`, and track explicit CLI use in `cmd/ralphex/main.go` using the existing flag-set pattern.
- [ ] Introduce one shared effective-selection resolver used by dependency checks, startup banners, and processor construction so `auto` cannot resolve differently across call sites: Claude primary selects Codex, Codex primary selects Claude, `codex_enabled=false` disables only automatic selection, and `ModeCodexOnly`/external-only still forces the requested external pipeline on.
- [ ] Remove the old `applyCodexOverrides` behavior that forces external review to `none`; retain `--pass-claude-md` validation, allow `--codex` with `--external-only`/`--codex-only`, reject `custom` with an explicitly supplied external model, and warn without blocking when the explicit external provider matches the primary executor.
- [ ] Apply `external_review_model` to the effective provider: empty Claude selection resolves to `opus:xhigh`, empty Codex selection inherits `codex_model`/`codex_reasoning_effort`, and explicit specs use provider-appropriate model/effort parsing including the existing Codex `max` warning behavior.
- [ ] Extend startup output and run-header metadata to show the effective external provider, whether it was auto-selected, and its resolved model/effort without mislabeling primary `review_model`.
- [ ] Check both required providers before execution: primary executor absence always fails; automatically selected external absence warns and resolves the external tool to `none`; explicitly selected external absence fails with the existing actionable install/config message. Do not turn post-launch auth, quota, or runtime errors into skip behavior.
- [ ] Add table-driven tests in `cmd/ralphex/main_test.go` for CLI-over-local-over-global precedence, the executor/tool matrix, `codex_enabled=false`, allowed Codex external-only modes, custom/model rejection, same-provider warnings, dynamic defaults, model overrides, and automatic-versus-explicit missing binary behavior.
- [ ] Run `go test ./cmd/ralphex` and fix all failures before continuing.

### Task 3: Add a review-only Claude execution policy
- [ ] Extend `executor.ClaudeExecutor` in `pkg/executor/executor.go` with a narrowly scoped external-review mode rather than creating a second stream parser or a new executor type.
- [ ] In that mode, sanitize configured/default args by removing both forms of `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, conflicting permission-mode values, and conflicting write-tool deny lists before appending `--permission-mode=plan` and a deny list for `Edit`, `Write`, and `NotebookEdit`; preserve Bash, project/user configuration, `CLAUDE.md`, skills, hooks, MCP, stream-json output, and non-interactive execution.
- [ ] Preserve `claude_command`, explicitly configured wrapper args, model/effort injection, `PreserveAnthropicAPIKey`, error/limit/retry patterns, context cancellation, streaming, and idle-timeout behavior. Document in code only the non-obvious invariant that this is not an OS-level sandbox and Bash remains available.
- [ ] Add table-driven success and conflict tests in `pkg/executor/executor_test.go` for default args, both flag spellings, equals/separate-value forms, configured permission flags, model `opus`, effort `xhigh`, retained read/shell capabilities, and unchanged normal Claude executor arguments; include failure/cancellation coverage where the existing runner test surface supports it.
- [ ] Run `go test ./pkg/executor` and fix all failures before continuing.

### Task 4: Build provider-aware external executors
- [ ] Refactor `pkg/processor/executor_factory.go` to build `Executors.External` from the resolved provider instead of assuming Codex: external Claude uses the review-only `ClaudeExecutor`, external Codex retains `read-only` sandbox and no multi-agent/CLAUDE.md passthrough, custom uses `Custom`, and none leaves the phase disabled.
- [ ] Reuse the configured Claude command/args/auth/error patterns for external Claude and resolve its model/effort independently from task/review models; wire idle timeout for the Codex-primary pipeline consistently with the existing rule that every `--codex` executor call is covered.
- [ ] Apply explicit `external_review_model` to external Codex without changing the primary Codex task/review executors or the existing `codex_model` fallback.
- [ ] Keep `Executors.Review` as the primary evaluator/fixer so Claude findings flow to Codex under `--codex`, while the existing Claude-primary to Codex-primary evaluation direction remains unchanged.
- [ ] Update `pkg/processor/executor_factory_test.go` and `pkg/processor/runner_test.go` with the full primary/external provider matrix, distinct model/effort assertions, no-external behavior, external-only Codex mode, all-writing-by-primary call ordering, and regression cases for the existing Claude-to-Codex pipeline.
- [ ] Run `go test ./pkg/processor/...` and fix all failures before continuing.

### Task 5: Introduce provider-neutral external-review signals and statuses
- [ ] Add `status.ExternalReviewDone = "<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>"` and update executor/phase signal detection so the new signal and legacy `status.CodexDone` both complete an external review loop; keep compatibility wrappers where needed by existing callers and tests.
- [ ] Add provider-neutral external-review and external-evaluation phases/section constructors in `pkg/status` whose labels include the actual reviewer/evaluator names, while retaining legacy Codex/Claude phase parsing for old progress files.
- [ ] Map new phases to the existing external-review/evaluation color configuration rather than renaming user color keys, and update progress/web replay, broadcast events, section boundaries, session phase inference, and frontend non-terminal-signal handling.
- [ ] Add or update tests in `pkg/status`, `pkg/executor`, `pkg/progress`, and `pkg/web` for both signals, old-log replay, `claude external review iteration N`, `codex evaluating claude findings`, phase/color selection, SSE events, and the rule that external completion does not mark the whole run complete.
- [ ] Run `go test ./pkg/status ./pkg/executor ./pkg/progress ./pkg/web` and fix all failures before continuing.

### Task 6: Generalize the external loop and add Claude-specific prompts
- [ ] Add embedded/customizable `pkg/config/defaults/prompts/external_claude_review.txt` and `external_claude_eval.txt`, register them in `pkg/config/config.go` and `pkg/config/prompts.go`, and update defaults/dump/fallback tests. The review prompt must require findings only, permit read-only analysis commands, and explicitly prohibit file changes and side-effecting Bash commands.
- [ ] Make the Claude evaluation prompt instruct the primary Codex executor to verify each Claude finding, fix only valid issues, explain dismissals for the next iteration, leave all writing/commits to Codex, and emit `EXTERNAL_REVIEW_DONE` only after Claude reports no actionable issues.
- [ ] Update embedded Codex/custom evaluation prompts to emit the neutral signal while preserving recognition of legacy customized prompts that still emit `CODEX_REVIEW_DONE`.
- [ ] Refactor `pkg/processor/phase/phase.go`, `pkg/processor/prompt_builder.go`, `pkg/processor/prompts.go`, and `pkg/processor/phase/external_review.go` from hard-coded `claudeResponse`/`runClaudeEvaluation` naming to reviewer/evaluator roles; add Claude prompt routing, provider-correct previous-review context, provider-correct policy/error labels, and neutral phase/section transitions without moving phase behavior into `Runner`.
- [ ] Preserve the existing loop invariants: first-versus-subsequent diff instructions, no-output handling, timeout/retry/wait behavior, manual break, stalemate detection, max external iterations, post-external primary review, and finalize.
- [ ] Add prompt-loader/rendering tests and external-phase/runner tests for Claude findings fixed by Codex, dismissed findings passed back to Claude, no-findings completion, legacy/new signals, timeouts, rate limits/errors, and exact reviewer/evaluator labels; update test prompt mocks to satisfy the consumer-side interface.
- [ ] Run `go test ./pkg/config ./pkg/processor/...` and fix all failures before continuing.

### Task 7: Document the symmetric pipeline and migration behavior
- [ ] Update `README.md` examples, option/config tables, pipeline description, model-selection rules, external-only examples, safety caveat, missing-binary policy, and Codex-mode requirements using the repository's `--flag=value` documentation style.
- [ ] Update `llms.txt` and the living architecture/configuration guidance in `CLAUDE.md` so they no longer claim that `--codex` always skips external review or conflicts with external-only; document `auto`, `external_review_model`, `opus:xhigh`, neutral signals/statuses, and legacy compatibility.
- [ ] Document that external Claude keeps normal user/project customizations, uses plan mode plus built-in write-tool denial, retains Bash, and therefore is not equivalent to Codex's OS/tool `read-only` sandbox.
- [ ] Update embedded default comments and prompt-variable documentation, but do not modify `CHANGELOG.md`, immutable completed plans, or plugin versions because no Claude asset skill changes are in scope.
- [ ] Run `rg -n "external_review_tool|external_review_model|EXTERNAL_REVIEW_DONE|--codex" README.md llms.txt CLAUDE.md pkg/config/defaults` and manually verify that current behavior is described consistently; no additional automated test is required for prose-only changes beyond the final repository gate.

### Task 8: Verify acceptance criteria and repository quality gates
- [ ] Run focused regression tests: `go test ./pkg/config ./pkg/executor ./pkg/processor/... ./pkg/status ./pkg/progress ./pkg/web ./cmd/ralphex`.
- [ ] Run `make fmt`, inspect the resulting diff for unrelated formatting, and run `git diff --check`.
- [ ] Run the full race/coverage suite with `make test`; tests that touch configuration must use `t.TempDir()` and must not read or write the real `~/.config/ralphex` directory.
- [ ] Run `make lint` and resolve every issue without unrelated linter exclusions.
- [ ] Cross-compile with `GOOS=windows GOARCH=amd64 go build ./...` to verify the new flags, executor routing, and signal/status additions remain portable.
- [ ] Re-read the Overview requirements and verify with tests that default Claude-to-Codex behavior is preserved, default Codex-to-Claude uses `opus:xhigh`, explicit overrides/none/custom work, auto/explicit dependency failures differ correctly, Claude cannot use built-in write tools, Codex owns all modifications, both completion signals work, and old progress logs remain readable.

## Post-Completion

After implementation and unit-quality gates pass, ask the user for explicit approval before any real cross-provider E2E because it consumes Claude and Codex credits. With approval, build and prepare the toy project, run a full `--codex` pipeline, and observe that Claude `opus:xhigh` performs external review while Codex alone applies changes and finalizes. Also run an explicit `--external-review-tool=none` smoke case and inspect the progress stream for provider-neutral labels and `EXTERNAL_REVIEW_DONE`.

Do not perform deployment, release-version changes, or changelog updates as part of this plan.
