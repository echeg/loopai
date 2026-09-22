# Report: Infer executor from model name and validate provider consistency
Plan: `/Users/echeg/orca/workspaces/ralphex/infer-executor-from-model/docs/plans/20260922-infer-executor-from-model.md` | Branch: `infer-executor-from-model` | Base: `master` | Mode: `full` | Executor: `codex` | Task model: `gpt-6-astra:medium` | Review model: `gpt-6-astra:high` | Started: `2026-09-22T01:42:41Z` | Finished: `2026-09-22T02:46:33Z`

## Summary

Implemented model-based primary executor selection, startup rejection of recognizable provider mismatches, and executor-source reporting in startup banners. Selection respects `--codex`, then an explicit `executor` key, then the effective task model, with Claude as the fallback. Explicit empty resets, wrapper commands, unknown model names, and Windows executable names receive appropriate handling. Added regression coverage and documentation.

Task iterations: 7; task failed retries: 0. Internal review first ran: true; internal review loop iterations: 1; internal review ended by: `review_done`. Post-review ran: true; post-review iterations: 1.

| Phase | duration_ms |
|---|---:|
| evaluation | 965568 |
| external review | 174706 |
| internal review | 826517 |
| other | 0 |
| tasks | 1865098 |

The finish timestamp and timing snapshot cover work through finalize, before report assessment and archival.

## Change scope

The supplied diff totals are **13 files, 858 additions, and 70 deletions**.

| Status | Files | Change |
|---|---|---|
| M | `cmd/loopai/main.go` | Executor inference, startup validation, reviewer diagnostics, and banner reporting |
| M | `pkg/config/config.go` | Explicit-executor tracking, runtime source metadata, and shared command recognition |
| A | `pkg/config/model_provider.go` | Model-provider recognizer |
| M | `cmd/loopai/main_test.go`, `pkg/config/config_test.go` | Selection, validation, configuration, wrapper, and banner regression coverage |
| A | `cmd/loopai/model_provider_integration_test.go` | Startup acceptance and selected-executor invocation tests |
| A | `cmd/loopai/model_provider_windows_test.go` | Windows executable inference and validation coverage |
| A | `pkg/config/model_provider_test.go` | Provider-recognition tests |
| M | `CLAUDE.md`, `README.md`, `llms.txt`, `pkg/config/defaults/config` | Executor-selection and validation documentation |
| M | `docs/plans/20260922-infer-executor-from-model.md` | Completed checklist and review corrections |

Nine commits:

- `ac2f634ef2f0ca1f4a6abc825302ddfef56c9b50` — fix: address external review findings
- `aace8fa5f8471e9d93c1a20b8e92b685a86540cb` — fix: address code review findings
- `7553ae01ac2bff725a5678c9edca32c253e5ca3b` — feat: document model-based executor selection
- `2b3288a29f59c09e2f50c47c1b131603ab70421c` — feat: verify executor inference acceptance criteria
- `f30d4a2a82ed56e2bdd1a9abbcd8387ef0d6cd1f` — feat: show executor source in startup banner
- `9d3b70ec139c04209a1b296a1570927e0f0e0169` — feat: reject model provider mismatches at startup
- `496a7156763931c65f00cfbd4b4a3ca39bbd4652` — feat: infer primary executor from task model
- `b4970796243314fa51ee3076837c6ef77a6ec769` — feat: preserve explicit executor and share binary checks
- `0ef00ba623ab127004605c32965eda9ce069287a` — feat: recognize model provider from model names

## Risk

**medium** — Configuration behavior changes intentionally: an unset executor can now select Codex from the task model, and previously accepted provider mismatches fail at startup. Explicit choices retain precedence; wrapper exemptions and unknown-name acceptance limit compatibility risk.

Public Go APIs gain helpers, constants, and runtime configuration fields; existing signatures are not removed. The new runtime fields are excluded from JSON, so persisted data schemas remain unchanged. No concurrency mechanisms, database changes, or migrations are introduced. Regression coverage addresses precedence, wrappers, diagnostics, and Windows command recognition.

## Migrations and operational steps

- Database migrations, data migrations, and special deployment actions: none.
- Mandatory configuration migration: none.
- To enable the plan’s intended per-run provider switching, remove `executor = codex` from `~/.config/loopai/config`; otherwise it deliberately continues to override inference. The existing local empty reset remains valid.
- The plan leaves a manual smoke test: confirm an explicit Codex configuration rejects a Claude task model, then remove the executor key and confirm the inferred Claude banner. Ensure inherited plan/review models match the selected primary. Completion of this manual follow-up is not recorded.

## Plan deviation

All seven planned tasks are represented in the implementation: recognition, explicit-setting propagation, inference, mismatch validation, banner reporting, acceptance coverage, and documentation.

Recorded drift: added items: none; blocked items: none; skipped items: none.

Implementation refinements documented in the plan include grouping startup checks to satisfy lint complexity limits, exercising successful acceptance cases through task execution, and normalizing Windows `.exe` names and casing. External review corrected the original instruction to omit reviewer binary guards: reviewer validation now respects each provider’s wrapper command. Diagnostics also identify legacy configuration keys, inherited `codex_model` values, and empty executor resets accurately.

## Backlog

There were no supplied backlog entries or backlog files.

## External review

### claude:opus:high

- label: claude
- iterations: 3
- duration_ms: 1140229
- ended by: done
- had findings: true
- Iteration 1 truncated: false
- Iteration 2 truncated: false
- Iteration 3 truncated: false

Findings and dispositions:

- Reviewer provider checks reject legitimate wrapper configurations -> fixed
- Legacy reviewer errors blame a nonexistent chain entry -> fixed
- Empty `executor =` resets produce double-space diagnostics -> fixed
- Banner inference detection depends on a format-string prefix -> dismissed (the check derives from the same constant, behavior is covered by banner tests, and no defect was demonstrated)
- Model-less Codex chain entries attribute an inherited bad model only to the entry -> fixed

Iteration 2 confirmed the initial fixes and dismissal while identifying the fallback-attribution issue. Iteration 3 confirmed the remaining correction and reported no issues. No findings were filed to backlog.

## Validation

The supplied structured validation record contains:

- Commands: none
- duration_ms: 0
- runs: 0

Separately, the supplied evaluator narratives report successful validation with:

- `go test ./pkg/config/... ./cmd/loopai/...`
- `make test </dev/null`, including race checks and wrapper suites
- `make lint`, with zero issues reported
- `GOOS=windows GOARCH=amd64 go build ./...`
- `git diff --check`

The narratives also report focused and processor-suite checks. The final reviewer reports passing focused tests but did not rerun the full test suite or lint. No individual command timings were supplied; these narrative results do not change the structured validation totals above.