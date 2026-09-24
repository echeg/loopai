# Report: 2026 09 24 explicit provider specs
plan: /Users/echeg/orca/workspaces/ralphex/explicit-provider-specs/docs/plans/20260924-explicit-provider-specs.md | branch: explicit-provider-specs | base: master | mode: full | executor: claude | task model: opus:high | review model: opus:xhigh | started: 2026-09-24T08:38:53Z | finished: 2026-09-24T12:24:17Z

## Summary
- task iterations: 13
- failed retries: 0
- external reviewers: 1
- post-review iterations: 1
| phase | duration_ms |
|---|---:|
| evaluation | 653202 |
| external review | 236911 |
| internal review | 4815147 |
| other | 0 |
| tasks | 7818292 |

## Change scope
- commits: 17
- files: 77
- additions: 3624
- deletions: 2849
| status | path |
|---|---|
| M | .claude-plugin/marketplace.json |
| M | .claude-plugin/plugin.json |
| M | .zed/tasks.json |
| M | CLAUDE.md |
| M | Makefile |
| M | README.md |
| M | assets/claude/skills/loopai-grill/SKILL.md |
| M | assets/claude/skills/loopai-orca/SKILL.md |
| M | assets/claude/skills/loopai-plan/SKILL.md |
| M | assets/claude/skills/loopai-update/SKILL.md |
| M | assets/codex/skills/loopai-orca/SKILL.md |
| M | assets/codex/skills/loopai-plan/SKILL.md |
| M | assets/codex/skills/loopai-update/SKILL.md |
| M | assets/codex/skills/loopai/SKILL.md |
| M | cmd/loopai/main.go |
| M | cmd/loopai/main_test.go |
| M | cmd/loopai/model_provider_integration_test.go |
| M | cmd/loopai/model_provider_windows_test.go |
| A | docs/backlog/cmux-handoff-before-config-validation.md |
| M | docs/backlog/review-only-modes-commit-user-edits.md |
| M | docs/custom-providers.md |
| M | docs/plans/20260924-explicit-provider-specs.md |
| M | llms.txt |
| M | pkg/cmux/cmux_test.go |
| M | pkg/config/config.go |
| M | pkg/config/config_test.go |
| M | pkg/config/defaults/config |
| M | pkg/config/defaults/prompts/codex.txt |
| M | pkg/config/defaults/prompts/codex_review.txt |
| M | pkg/config/defaults/prompts/custom_eval.txt |
| M | pkg/config/defaults/prompts/custom_review.txt |
| M | pkg/config/defaults/prompts/external_claude_eval.txt |
| M | pkg/config/defaults/prompts/review_first.txt |
| M | pkg/config/defaults/prompts/review_second.txt |
| M | pkg/config/defaults_test.go |
| A | pkg/config/provider_spec.go |
| A | pkg/config/provider_spec_test.go |
| M | pkg/config/values.go |
| M | pkg/config/values_test.go |
| M | pkg/executor/codex.go |
| M | pkg/executor/codex_test.go |
| M | pkg/orca/orca.go |
| M | pkg/orca/orca_test.go |
| M | pkg/processor/execution_policy.go |
| M | pkg/processor/execution_policy_test.go |
| M | pkg/processor/executor_factory.go |
| M | pkg/processor/executor_factory_test.go |
| M | pkg/processor/phase/external_review.go |
| M | pkg/processor/phase/external_review_test.go |
| M | pkg/processor/phase/finalize.go |
| M | pkg/processor/phase/gen_agents.go |
| M | pkg/processor/phase/phase.go |
| A | pkg/processor/phase/phase_test.go |
| M | pkg/processor/phase/plan_creation.go |
| M | pkg/processor/phase/report.go |
| M | pkg/processor/phase/review.go |
| M | pkg/processor/phase/review_test.go |
| M | pkg/processor/phase/task.go |
| M | pkg/processor/phase/task_test.go |
| M | pkg/processor/prompt_builder.go |
| M | pkg/processor/prompt_builder_test.go |
| M | pkg/processor/prompts.go |
| M | pkg/processor/prompts_test.go |
| M | pkg/processor/run_recorder.go |
| M | pkg/processor/run_recorder_test.go |
| M | pkg/processor/runner.go |
| M | pkg/processor/runner_test.go |
| M | pkg/status/section.go |
| M | pkg/status/status.go |
| M | pkg/web/session_progress_test.go |
| M | scripts/check-codex-skills.sh |
| M | scripts/check-codex-skills_test.sh |
| M | scripts/check-grill-skill_test.sh |
| M | scripts/check-symlinks.sh |
| M | scripts/check-symlinks_test.sh |
| M | scripts/opencode/README.md |
| M | scripts/opencode/opencode-review.sh |

## Risk
_assessment unavailable_

## Migrations and operational steps
_assessment unavailable_

## Plan deviation
_assessment unavailable_
### Added
- ➕ removed the five keys from the embedded defaults (`codex_model = gpt-5.5`, `codex_reasoning_effort = xhigh`, `external_review_tool = auto`, `external_review_model =`) and guarded that with a test; a bare codex spec now uses the codex CLI's own default instead of gpt-5.5:xhigh, as the Grammar section specifies, and the automatic reviewer remains the implicit default when `external_reviewers` is unset
- ➕ `ResolveCodexModelEffort`/`ResolveExternalReviewerModelEffort` lost their codex-default parameters; `resolveExternalReviewSelection`/`resolveReviewerChain` lost their now-unused `opts` parameter
- ➕ delete the runtime-only `Config.ExternalReviewTool`/`ExternalReviewModel` fields (kept by Task 2 because the legacy flags still wrote them) and the legacy single-reviewer branch of `resolveReviewerChain` that reads them, keeping the automatic reviewer for an unset `external_reviewers`
- ➕ `validateExternalReviewFlags`, `applyExternalReviewCLIOverrides`, `applyEffectiveExternalReview`, and the legacy-flags warning in `printExternalReviewWarnings` were deleted with the legacy branch; the processor factory lost its `AppConfig.ExternalReviewTool/Model` fallbacks
- ➕ the rewrite hint folds `--external-review-tool`/`--external-review-model` into one ready `--external-reviewers=` entry (`none` → empty chain, `auto` → omit or name a chain, a bare model → provider from `ModelProvider`)
- ➕ `executorModelSpec` strips the provider at the four processor `Config` sites and in `codexBannerForSpec`, because `parseModelEffort` still splits at the first colon; Task 7 must delete it when the factory takes `ParseProviderSpec` directly
- ➕ `applyCodexOverrides` now selects codex from the explicit `codex:` prefix instead of `ModelProvider`, and no longer skips selection under a claude wrapper command, so `codex:<model>` works with a wrapper in the interim; the `--plan-model`/`--task-model`/`--review-model` help text now documents `provider[:model[:effort]]`
- ➕ delete the runtime-only `Config.Executor`/`ExecutorSource` fields and the remaining `ExecutorSource*` constants (kept by Task 2 because `--codex` and inference still wrote them)
- ➕ `cmuxRunModels` resolves each phase label under its own provider through the same `resolvePhaseBanner`, which strips the provider prefix; `codexModelBanner`/`codexPlanBanner`/`codexBannerInfo`/`configuredModelLabel`/`codexBannerValue` were deleted with their tests. `detectClaudeSwapRecovery` now counts any claude plan, task, or review phase
- ➕ `ResolveCodexModelEffort` became provider-aware `ResolveModelEffort(config.ProviderSpec)`, used by the factory and the startup banner; `ResolveExternalReviewerModelEffort` parses through `ParseProviderSpec`. Automatic external-reviewer selection keys off the task spec, the external codex reviewer inherits `idle_timeout` when codex runs either the task or the review phase, and the `--pass-claude-md` setup hint fires for a codex review phase too. The run record's task/review model fields now record the full provider spec
- ➕ processor `Config.isCodexExecutor()` deleted: the session-timeout gate now uses `runsCodexPhase()` (the same rule as the external codex reviewer's idle timeout), the post-external-review skip message names the review provider, and the run record's `executor` is `taskProvider()`. The processor no longer reads `AppConfig.PlanProvider`/`TaskProvider`/`ReviewProvider`
- ➕ `rejectMixedPhaseProviders`, its tests, and the "per-phase providers are not supported yet" acceptance case are deleted; that case now asserts a claude task runs under an inherited codex review model
- ➕ cmux labels prefix every phase with its provider once plan, task, and review providers differ (same-provider runs keep bare model labels); the startup banner already printed one provider per phase since Task 5
- ➕ the orca title reporter gained a nil-safe `SetPhaseExecutors(plan, review)`, so plan and review-block titles name their own provider instead of the task provider
- ➕ `check-symlinks.sh` and `check-codex-skills.sh` now reject any skill body naming a removed flag or key (bare `--codex`, `--codex-only`, `--external-review-tool/-model`, `external_review_tool/_model`, `codex_model`, `codex_reasoning_effort`, the `executor` key); a line that itself says the spelling was removed is allowed as a migration hint, and `--codex-args` is untouched. Both `_test.sh` suites cover every spelling plus the allowed forms
- ➕ `loopai-grill` told the model never to substitute `loopai --codex --plan`; it now names `loopai --plan` under any `--plan-model`, asserted in `check-grill-skill_test.sh`
- ➕ both orca skills now require a `claude`/`codex` provider prefix on `--task-model`/`--review-model` before creating the worktree, and answer a `--codex` token with the `--task-model codex:<model>[:effort]` rewrite
- ➕ the provider-model comments in `pkg/config/defaults/config` were rewritten for the new grammar: session/idle timeout, `codex_sandbox`, `codex_args`, automatic reviewer, and `pass_claude_md` now describe phase providers instead of a `--codex` primary; `Test_defaultsFS` asserts the new wording and that no live `--codex`/inference text remains
- ➕ CLAUDE.md's `validateModelSpecs` and `ModelProvider`/`applyCodexOverrides` paragraphs were replaced here by the per-phase provider model, validation, removed-spelling, dependency-check, and banner description, so Task 13 only re-checks the fold; `--session-timeout` help text, `scripts/opencode/README.md` (`claude:model:effort` through a wrapper), and the `e2e-codex` Makefile hint (`--task-model codex --external-only`) were updated too
- ➕ verified against a built binary under a scratch HOME: bare `opus:high`/`gpt-6-astra:medium` in flags and in `plan_model`/`review_model` config name the prefixed rewrite, `codex:opus` is a mismatch, `custom` is rejected for a phase, and a task-codex/review-claude config with stub binaries prints the Technical Details banner (`task: codex …`, `sandbox:` indented, `review: claude …`) and fails on a missing claude binary because the review phase needs it
- ➕ the explicit same-provider reviewer warning still said "matches the primary executor", a concept this branch removed; it now says "matches the task provider", which is what it compares against
- ➕ the fold itself landed in Task 11; this pass re-checked it and corrected the close-out paragraph, which still listed `--codex-only` among the modes writing their own progress record (now `--external-only`)
- ➕ stale "first-class --codex" comments in `pkg/executor/codex.go`, `pkg/processor/executor_factory.go`, `pkg/config/config.go`, and `cmd/loopai/main.go`, plus test comments/names in `pkg/executor`, `pkg/processor`, `pkg/web`, and `pkg/cmux`, now speak of codex phase executors and `--review`/`--external-only`; the `CodexExecutorSandbox` comment no longer claims the external reviewer reads the raw `codex_sandbox` (it is pinned read-only); the backlog entry `review-only-modes-commit-user-edits.md` drops `--codex-only`
### Blocked
- ⚠️ deleted `ExecutorSet`, `ExternalReviewToolSet`, `ExternalReviewModelSet`, `CodexModel`, `CodexReasoningEffort`, and `ExecutorSourceConfig`, which only a config key could reach. `Executor`/`ExecutorSource` (written by `--codex` and inference) and `ExternalReviewTool`/`ExternalReviewModel` (written by the legacy flags and the resolved single reviewer) are still reachable from the CLI, so they became runtime-only `json:"-"` fields; their deletion moved to Tasks 3 and 5 alongside their writers
- ⚠️ `--codex` no longer reaches `applyCodexOverrides`, so the `ExecutorSourceFlag` constant and the `o.Codex` branch were deleted here; the primary is now chosen by task-model inference alone until Task 5 replaces it. A codex-wrapper primary with a claude-named model is unreachable in the interim and returns with Task 4/5's `codex:<model>` specs
- ⚠️ `validateModelProviders` is gone; the mismatch check now compares each spec's model with its own provider and skips the provider whose command is a wrapper. Because the executors are still built for one provider per run until Tasks 5 and 7, `rejectMixedPhaseProviders` temporarily rejects a plan or review provider that differs from the task provider ("per-phase providers are not supported yet"); Task 5/7 must delete it together with its tests and the integration case that asserts it
- ⚠️ `Config.Executor`/`ExecutorSource` became runtime-only `PlanProvider`/`TaskProvider`/`ReviewProvider` fields set by `applyPhaseProviders`; the processor (`isCodexExecutor`, `phase.Config.isCodexExecutor`, the run record) still reads `TaskProvider` for every phase, and `rejectMixedPhaseProviders` stays until Task 7 builds executors per phase. Processor tests set both `TaskProvider` and `ReviewProvider` so Tasks 7-9 can switch the review reads without re-editing them. `ExecutorClaude`/`ExecutorCodex` stay as executor labels for the orca title, progress header, and run record
- ⚠️ the banner prints the phases the mode runs rather than all four: full → task and review, tasks-only and `--gen-agents` → task, `--review`/`--external-only` → review, plan creation → plan. The external-review line now uses the same `<provider> <model>[:effort]` form, comma-joined, with `(auto-selected)` appended; an unset model renders as `default`
- ⚠️ the factory derives each phase's provider from its own spec (`Config.taskSpec()`/`reviewSpec()`: unset task → claude, unset review → task spec whole) rather than from `AppConfig.TaskProvider`/`ReviewProvider`, so processor `Config.TaskModel`/`ReviewModel` now carry full `provider[:model[:effort]]` specs and `executorModelSpec` is deleted. This also makes plan mode build its executor from the plan spec's provider. Tasks 8/9 should switch `isCodexExecutor` and the prompt/phase reads to the same `taskSpec()`/`reviewSpec()` so the processor has one provider source, then drop the `AppConfig.*Provider` reads from the processor
- ⚠️ `rejectMixedPhaseProviders` is kept (with its tests) until Task 9: executors are now per phase, but prompts and phase naming still read the task provider, and a mixed run would silently render the wrong agent syntax. Task 9 must delete it together with its tests and the integration case that asserts it
- ⚠️ prompts read the provider from the processor specs (`Config.taskProvider()`/`reviewProvider()` over `taskSpec()`/`reviewSpec()`), not from `AppConfig.TaskProvider`, so the prompt tests now select codex through `Config.TaskModel` or an explicit provider argument. `replacePromptVariables`, `expandAgentReferences`, and `expandDynamicAgentCatalog` take the provider of the phase running the prompt: the task prompt uses the task provider; review, evaluation, finalize, and a customized external-review prompt use the review provider. `isCodexExecutor` remains only for phase naming, session timeouts, and the primary-executor name, which Task 9 switches
- ⚠️ `executorName()` split into `taskExecutorName()` (task, plan creation, `--gen-agents`: the task-slot executor) and `reviewExecutorName()` (review, external evaluation, finalize, report); `phase.Config` gained `TaskProvider`/`ReviewProvider`, filled by `toPhaseConfig` from `Config.taskProvider()`/`reviewProvider()`. The review section label and codex-timeout-is-an-error rule now follow the review provider. `pkg/status` constructors were already provider-parameterised and did not change
- ⚠️ the Claude `loopai` skill carried no `--codex` or model-flag text (`:54` is plan selection); only the codex mirror's launch note changed
- ⚠️ remaining matches for removed spellings are deliberate migration hints (README/llms.txt/custom-providers tables, the embedded config removal note, CLAUDE.md), the skill checkers' pattern lists, the unrelated active plan `20260806-dynamic-review-agents.md`, and the backlog entry `review-only-modes-commit-user-edits.md`, which describe history or other work
- ⚠️ remaining matches are deliberate: removal errors and hidden `opts` fields, their tests, migration tables (README, `docs/custom-providers.md`, embedded config note, CLAUDE.md), the skill checkers' pattern lists, the loopai-orca skills' "was removed" troubleshooting rows, the internal `ModeCodexOnly` identifier whose `codex-only` value progress records persist, the implemented 2026-08-04 design spec under `docs/superpowers/specs/`, and the unrelated active plan `20260806-dynamic-review-agents.md`
### Skipped
- none

## Backlog
| status | path | title |
|---|---|---|
| A | docs/backlog/cmux-handoff-before-config-validation.md | cmux hand-off spawns a workspace for a config the child rejects |

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 3
- duration_ms: 890075
- ended by: done
- had findings: true
#### Iteration 1
- truncated: false
Reviewer output:
```
- **[P2] Preserve explicit models when migrating automatic reviewers** — [Claude skill:137](/Users/echeg/orca/workspaces/ralphex/explicit-provider-specs/assets/claude/skills/loopai-update/SKILL.md:137), mirrored in [Codex skill:87](/Users/echeg/orca/workspaces/ralphex/explicit-provider-specs/assets/codex/skills/loopai-update/SKILL.md:87). The instructions drop `auto` or an unset tool without preserving `external_review_model`. Previously, Codex tasks with `external_review_model = sonnet:high` used Claude Sonnet for external review; following this migration silently switches to `opus:xhigh`. Preserve the model in an explicit reviewer entry, or ask before discarding it.

```
Evaluator response:
```
Using superpowers:receiving-code-review to evaluate the Codex finding rigorously.Now let me verify the old semantics of `external_review_model` with `auto`/unset tool on master.Master confirms the old behavior: with `auto`, the automatically chosen reviewer used `cfg.ExternalReviewModel`. Let me check the plan and how the branch resolves the automatic reviewer's model now.Now let me see how the branch resolves the automatic reviewer and whether an explicit entry differs (codex_enabled gating, missing-binary behavior).I fixed the finding. Nothing is committed and I'm not sending a signal, so the next Codex pass can check the change.

Finding [P2] Preserve explicit models when migrating automatic reviewers: valid and fixed.

- It's a real problem. On master, with the tool set to auto or left unset, the automatically chosen reviewer used cfg.ExternalReviewModel (master cmd/loopai/main.go around line 2769). On this branch, resolveReviewerChain always gives the automatic reviewer an empty model (cmd/loopai/main.go:2787). So following the old skill wording turns "codex tasks plus external_review_model = sonnet:high" into a Claude reviewer at its default model, with no warning. The skill's own bullet says deleting a removed key "silently changes which provider or model a run uses", so this contradicted it.
- It's in scope. This branch wrote both skill bullets, and the plan doesn't say to discard the model. README.md:790 already maps the old key to "the model segment of the matching external_reviewers entry", so only the skills were wrong.
- The fix is in assets/claude/skills/loopai-update/SKILL.md:137 and assets/codex/skills/loopai-update/SKILL.md:87:
  - auto or no tool is still dropped when the old model value is empty.
  - When it isn't empty, the skill now asks whether to pin it as external_reviewers = provider:model[:effort], taken from the old value. The provider is the one other than the migrated task_model's, or codex when task_model is unset.
  - It asks instead of rewriting silently for two reasons. An explicit entry makes a missing reviewer binary a startup error, where the automatic reviewer was skipped with a warning. And an explicit chain ignores codex_enabled.
  - Under codex_enabled = false the old model applied to no reviewer, so the skill says to drop it there.
  - This matches how the codex_model bullet already handles a codex automatic reviewer.
- Each bullet stays on one line containing "were removed". The removed-spelling checkers in check-symlinks.sh and check-codex-skills.sh accept a line naming a removed key only if it says that.
- I didn't bump the manifests again. The branch already raised both from 0.5.8 to 0.5.9, and that version hasn't been released.

Checks: make test exited 0, including the check-symlinks and check-codex-skills suites, the race-enabled Go suite and the wrapper suites. make lint reported 0 issues. Only the two skill files are modified.
```
#### Iteration 2
- truncated: false
Reviewer output:
```
- **[P2] Preserve the model for external-only runs when `codex_enabled = false`** — [Claude skill:137](/Users/echeg/orca/workspaces/ralphex/explicit-provider-specs/assets/claude/skills/loopai-update/SKILL.md:137), mirrored in [Codex skill:87](/Users/echeg/orca/workspaces/ralphex/explicit-provider-specs/assets/codex/skills/loopai-update/SKILL.md:87). The fix correctly handles ordinary runs, but the new instruction to drop the model when `codex_enabled = false` overlooks `--external-only`: both master and this branch bypass that gate in this mode. With Codex tasks and `external_review_model = sonnet:high`, migration still silently replaces Sonnet with the default Claude reviewer. Ask before discarding the model in this case, explaining the tradeoff of pinning an explicit reviewer.

```
Evaluator response:
```
Confirmed on this branch that `--external-only` (ModeCodexOnly) bypasses the `codex_enabled` gate. Now checking whether master applied `external_review_model` in that mode too.The line already says "were removed", so the guard won't flag it. Applying the fix to both skills.The finding is valid, and I fixed it in both skill files. Nothing is committed and there is no completion signal, because a fix was made and Codex needs to review it again.

- What I checked: on master, when no reviewer tool was set and codex_enabled was false, only the --external-only mode (ModeCodexOnly) skipped the codex_enabled gate. In that mode master still ran the reviewer with the old external_review_model value (resolveReviewerChain in master's cmd/loopai/main.go). This branch keeps the same bypass, in resolveReviewerChain at cmd/loopai/main.go:2779 and in externalReviewProvider in pkg/processor/executor_factory.go. The plan says so on line 96. So the skill was wrong to say that under codex_enabled = false the old model reached no reviewer. In the example from the finding (Codex tasks with external_review_model = sonnet:high), --external-only did review with Sonnet, and dropping the key silently switches it to the default Claude model.
- Why it is in scope: the wrong sentence was written in an earlier round of this loop, so it is this branch's own work. Nothing was filed to the backlog.
- The fix, in assets/claude/skills/loopai-update/SKILL.md:137 and assets/codex/skills/loopai-update/SKILL.md:87: "drop it there" is replaced with an instruction to ask under codex_enabled = false as well. The skill now explains that --external-only skips that setting and was the only mode where the old model value was used. It then explains the choice to the user:
  - Pinning the reviewer also turns external review on for every other run, because a pinned entry ignores codex_enabled.
  - Dropping the value keeps those other runs unreviewed. --external-only then uses the default model unless that command passes --external-reviewers=<provider>:<model>[:<effort>].
- Guard and manifests: the bullet is still one line containing "were removed", so the check that rejects removed flags and keys in skill text does not flag it. Both manifests are already at 0.5.9 against master's 0.5.8, and this edit belongs to the same unreleased change, so I did not bump them again.
- Tests: make test passed (exit 0; the lines matching "FAIL" in the log are test names such as "FAILED signal passthrough", not failures). make lint reported 0 issues.

★ Insight
- The config setting codex_enabled only turns off the reviewer loopai picks automatically, and --external-only ignores it on purpose. A migration rule that asks "did this setting ever take effect?" has to consider every mode, not just ordinary runs.
- In this case a value that looks useless in the config file is still used on one command-line path. That is why the skill now asks the user instead of deleting it.
```
#### Iteration 3
- truncated: false
Reviewer output:
```
NO ISSUES FOUND.

The fixes in both skills correctly address the previous findings, including the `--external-only` bypass of `codex_enabled = false` and the tradeoffs of pinning a reviewer.

```
Evaluator response:
```
Codex found nothing, and the skill checks pass. Committing this loop's two skill fixes now.Codex found no issues this round. It confirmed that the earlier fixes to both loopai-update skills cover two things: that --external-only bypasses codex_enabled = false, and the tradeoffs of pinning a reviewer.

What I did:
- Checked git diff HEAD and git status --porcelain. The only uncommitted changes were the two loopai-update SKILL.md files (the Claude and Codex versions) that this review loop edited. There were no untracked files and no backlog entries.
- This branch already bumps both plugin manifests from 0.5.8 to 0.5.9, so these skill edits go out with that bump and no second bump is needed.
- Ran make check-symlinks, check-codex-skills and check-plugin: all passed (exit 0). The changed lines naming external_review_* say "were removed", so the removed-spelling check accepts them.
- Staged only those two files by path and committed them as 8e5d3cd "fix: address codex review findings". The working tree is now clean.

<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>
```

## Validation
- none
- duration_ms: 0
- runs: 0