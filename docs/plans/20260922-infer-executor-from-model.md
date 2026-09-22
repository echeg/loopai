# Infer executor from model name and validate provider consistency

## Overview

loopai picks the primary executor from `--codex` or the `executor` config key alone. A user
whose global config says `executor = codex` and who passes `--task-model fable:high` gets the
Claude model sent to codex, which fails only when the provider API rejects it:

```
ERROR: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'fable' model is not supported when using Codex with a ChatGPT account."}}
```

The same trap exists in the other direction and later in the run: `review_model =
gpt-6-astra:high` inherited from global config under a Claude primary passes startup
validation (`validateModelSpec` checks the effort half only) and fails in the review phase,
after a task phase that can run for hours.

This plan makes loopai:

1. **Infer the executor from the task model** when neither `--codex` nor an explicit `executor`
   key decides it: `task_model = gpt-6-astra:medium` runs codex, `--task-model fable:high` runs
   Claude, with no flag. There is deliberately no `--claude` flag; the model name is the flag.
2. **Reject provider mismatches at startup**: a recognizable `plan_model`, `task_model`, or
   `review_model` from the other provider, or an `external_reviewers` entry such as
   `codex:fable`, is an immediate error naming where the executor came from.
3. **Show the executor's source in the startup banner** so an inferred choice is visible.

An explicit `--codex` or `executor` key still wins over inference; a conflict between it and a
recognizable model name is an error, never a silent override.

## Decisions

- **Context**: `--codex` exists but has no inverse. Claude is `ExecutorClaude = ""`, the default,
  so the only way to override a global `executor = codex` is `executor =` in a local config.
- **Chosen approach**: infer the provider from the model name, explicit setting wins, conflict is
  a startup error. Chosen over "model name always wins" because that would make `executor` and
  `--codex` silently ignored whenever a model name is recognizable.
- **Rejected alternatives**: a `--claude` flag (symmetric but still requires the user to say
  twice what the model name already says); warning-only on conflict (the API error still happens
  hours later); inferring from `codex_model` (that key is commonly set for an external codex
  reviewer under a Claude primary and says nothing about the primary).
- **Verified facts**:
  - `applyCodexOverrides` (`cmd/loopai/main.go:5978`) is the only place `--codex` sets
    `cfg.Executor`; it runs at the end of `applyCLIOverrides`, after the config merge.
  - `Values.ExecutorSet` (`pkg/config/values.go:70`) tracks an explicit `executor` key through
    local/global merge — `executor =` in a local file is an explicit reset, tested in
    `values_test.go` ("local file overrides global through Load") — but it is not copied into
    `Config` in `pkg/config/config.go:373`. `ClaudeArgsSet` is the runtime-only precedent.
  - `validateModelSpecs` (`main.go:2999`) runs at `main.go:468` before
    `resolveExternalReviewSelection` (`main.go:471`); it splits `model[:effort]` with
    `resolveSpec(cliVal, cfgVal)` and never looks at the model half.
  - `resolveExternalReviewSelection` wraps `resolveReviewerChain` and calls
    `validateReviewerEfforts` on `externalReviewSelection.Reviewers` (`[]resolvedReviewer` with
    `Provider`, `Model`, `Effort`); a reviewer-provider check belongs beside it for the same
    reason the effort check does — the chain resolves from several branches.
  - `main.go:3350-3355` already decides "is this the real Claude Code binary" with
    `filepath.Base(strings.TrimSpace(cfg.ClaudeCommand)) == "claude"` (empty means `claude`).
    Under `pi-as-claude`/`opencode-as-claude` wrappers (`docs/custom-providers.md`), model
    names like `gpt-5` or `github-copilot/claude-opus-4.6` are legitimate, so neither inference
    nor the mismatch check may run against a wrapper. `codex_command` can be overridden the
    same way, so the codex side needs the symmetric guard.
  - The banner prints `executor: codex` in `printCodexExecutorInfo` (`main.go:3428`) and
    nothing for Claude; `startupInfo.Executor` (`main.go:248`) carries the resolved executor.
  - `loopai-plan` and `loopai-orca` forward `--task-model`; inference happens inside loopai, so
    neither skill changes and no plugin manifest bump is needed.

## Context (from discovery)

- Files involved: `pkg/config/config.go`, `pkg/config/values.go`, new
  `pkg/config/model_provider.go`, `cmd/loopai/main.go` (`applyCodexOverrides`,
  `validateModelSpecs`, `resolveExternalReviewSelection`, banner), `pkg/config/defaults/config`,
  `README.md`, `CLAUDE.md`.
- Related patterns: `*Set bool` runtime tracking (`ClaudeArgsSet`), table-driven `testify` tests,
  `validateReviewerEfforts` as the model for a post-resolution chain check, `knownEfforts` as the
  model for a constant list rather than a regex.
- Tests: `cmd/loopai/main_test.go` covers `applyCodexOverrides` and `validateModelSpecs`;
  `pkg/config/values_test.go` covers `executor` merge semantics; `pkg/config/config_test.go`
  covers `Config` assembly.

## Development Approach

- **Testing approach**: TDD — write the table-driven test first, watch it fail, then implement.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run focused tests after each change (`go test ./pkg/config/... ./cmd/loopai/...`).
- Maintain backward compatibility: a config with an explicit `executor` key and unrecognizable
  model names behaves exactly as today.
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real
  `~/.config/loopai/` or `~/.config/ralphex/`.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above).
- **E2E tests**: none — no dashboard or UI change.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, and documentation in this repository.
- **Post-Completion** (no checkboxes): the user's own global config cleanup and a smoke run.
- **Checkbox placement**: checkboxes belong only in Task sections.

## Implementation Steps

### Task 1: Add the model-name provider recognizer

- [x] write `pkg/config/model_provider_test.go`: table-driven test for `ModelProvider(spec string) string`
      covering claude prefixes (`claude-opus-5`, `opus`, `Opus:high`, `sonnet`, `haiku`, `fable:high`),
      codex prefixes (`gpt-6-astra:medium`, `GPT-5`, `codex-mini`, `o3`, `o4-mini`), the effort-only
      form `:high` → `""`, empty → `""`, unknown names (`my-alias`, `github-copilot/claude-opus-4.6`)
      → `""`, and surrounding whitespace; run it and confirm it fails to compile
- [x] create `pkg/config/model_provider.go` with `ModelProvider`: trim, cut at the first `:`,
      lowercase, match against two constant prefix lists (`claudeModelPrefixes`,
      `codexModelPrefixes`); return `ExternalReviewToolClaude`, `ExternalReviewToolCodex`, or `""`.
      `o1`/`o3`/`o4` match only as the whole model or followed by `-` so a future `o`-prefixed
      Claude alias is not misread. Godoc says unknown names return `""` and that the lists are
      deliberately prefixes, not regexes
- [x] run `go test ./pkg/config/...` - must pass before task 2

### Task 2: Propagate the explicit-executor flag and share the real-binary check

- [x] add to `pkg/config/config_test.go` a table-driven `Load` test asserting `Config.ExecutorSet`:
      `executor = codex` in global → true; `executor =` in local over codex global → true with
      empty `Executor`; neither file sets it → false; run it and watch it fail
- [x] add `ExecutorSet bool` to `Config` (runtime-tracking comment beside `ClaudeArgsSet`) and copy
      `values.ExecutorSet` in the `Config` assembly at `pkg/config/config.go:373`
- [x] write tests for `Config.IsRealClaudeCommand()` and `Config.IsRealCodexCommand()`: empty →
      true, `claude`/`codex` → true, `/usr/local/bin/claude` → true, `scripts/pi-as-claude/pi-as-claude.sh`
      → false, surrounding whitespace → true; watch them fail
- [x] implement both methods in `pkg/config/config.go` (base-name check, empty means the default
      binary) and replace the inline check at `cmd/loopai/main.go:3350-3355` with
      `cfg.IsRealClaudeCommand()`; keep its comment about wrappers not sharing Claude Code auth
- [x] run `go test ./pkg/config/... ./cmd/loopai/...` - must pass before task 3

### Task 3: Infer the executor from task_model in applyCodexOverrides

- [ ] add `ExecutorSource string` to `Config` (runtime-only, `json:"-"`) and constants in
      `pkg/config`: `ExecutorSourceFlag` ("--codex"), `ExecutorSourceConfig` ("executor = %s in config"),
      `ExecutorSourceInferred` ("inferred from task_model %q"), `ExecutorSourceDefault` ("default")
- [ ] extend the `applyCodexOverrides` tests in `cmd/loopai/main_test.go` with a table: `--codex`
      wins and source is flag; `ExecutorSet` with `Executor = codex` wins over `task_model = fable`
      (source config, no error here — task 4 rejects the conflict); `ExecutorSet` with empty
      `Executor` stays claude even for `task_model = gpt-6-astra`; unset executor +
      `--task-model gpt-6-astra:medium` → codex, source inferred; unset executor +
      `task_model = fable:high` from config → claude, source inferred; unset executor + unknown
      `task_model = my-alias` → claude, source default; unset executor + `task_model = gpt-5` +
      `claude_command = pi-as-claude.sh` → claude, source default (wrapper blocks inference);
      run and watch them fail
- [ ] implement the inference in `applyCodexOverrides`: after the `--codex` branch, when
      `!o.Codex && !cfg.ExecutorSet && cfg.IsRealClaudeCommand()`, resolve the effective task spec
      with `resolveSpec(o.TaskModel, cfg.TaskModel)` and set `cfg.Executor` from
      `config.ModelProvider`; set `cfg.ExecutorSource` on every path. Update the function's godoc,
      which currently says it only applies `--codex` / `--pass-claude-md`
- [ ] verify the existing `--pass-claude-md requires --codex` check still fires against the
      inferred value: add a case where `task_model = gpt-6-astra` infers codex and
      `--pass-claude-md` is accepted, and one where `task_model = fable` infers claude and it is
      rejected
- [ ] run `go test ./cmd/loopai/...` - must pass before task 4

### Task 4: Reject provider mismatches at startup

- [ ] write table-driven tests for a new `validateModelProviders(o opts, cfg *config.Config) error`
      in `cmd/loopai/main_test.go`: executor claude (default source) + `review_model = gpt-6-astra:high`
      → error containing `review_model`, `gpt-6-astra:high`, `codex model`, and the source text;
      executor codex from config + `--task-model fable:high` → error naming `--task-model`,
      `claude model`, and `executor = codex in config`; executor codex via `--codex` + `plan_model =
      opus` → error naming `--codex`; matching providers → nil; unknown names → nil; executor claude +
      `claude_command = pi-as-claude.sh` + `review_model = gpt-5` → nil (wrapper skips the check);
      executor codex + `codex_command = my-codex-wrapper` + `task_model = fable` → nil; effort-only
      `:high` → nil; run and watch them fail
- [ ] implement `validateModelProviders` beside `validateModelSpecs`: iterate the same three
      resolved specs (`plan`, `task`, `review`) with their `--flag / key` labels, skip when
      `ModelProvider` is `""` or equals `primaryProvider(cfg)`, skip entirely when the primary's
      binary is not the real one (`IsRealClaudeCommand` for claude, `IsRealCodexCommand` for codex),
      otherwise return an error of the form
      `--review-model / review_model %q is a codex model, but the executor is claude (%s)` where
      `%s` is `cfg.ExecutorSource`
- [ ] wire it at `cmd/loopai/main.go:468` directly after `validateModelSpecs`, before
      `resolveExternalReviewSelection`
- [ ] write tests for `validateReviewerProviders(selection externalReviewSelection) error` beside
      the `validateReviewerEfforts` tests: `codex:fable:high` → error naming entry 1, `codex`, and
      `fable`; `claude:gpt-6-astra` → error; `claude:opus:high,codex:gpt-6-astra:high` → nil;
      `custom` entry → nil; unknown model → nil; empty model → nil; run and watch them fail
- [ ] implement `validateReviewerProviders` and call it from `resolveExternalReviewSelection`
      immediately after `validateReviewerEfforts`, so every branch of `resolveReviewerChain` is
      covered; the reviewer's `Provider` is explicit, so no binary guard is needed here beyond
      skipping `custom`
- [ ] run `go test ./cmd/loopai/...` - must pass before task 5

### Task 5: Show the executor source in the startup banner

- [ ] locate where `startupInfo.Executor` is filled and the banner test(s) that assert on
      `executor: codex`; add `ExecutorSource string` to `startupInfo` and extend the tests: codex
      via config prints `executor: codex (executor = codex in config)`, codex inferred prints
      `executor: codex (inferred from task_model "gpt-6-astra:medium")`, claude inferred prints
      `executor: claude (inferred from task_model "fable:high")`, claude by default prints no
      executor line (banner unchanged for existing users); run and watch them fail
- [ ] implement: `printCodexExecutorInfo` appends ` (%s)` from `info.ExecutorSource`; the Claude
      branch prints `executor: claude (%s)` only when the source is the inferred one
- [ ] run `go test ./cmd/loopai/...` - must pass before task 6

### Task 6: Verify acceptance criteria

- [ ] verify the Overview scenarios end to end with a `t.TempDir()` config through the real
      `run()`/config path used in `main_test.go`: global `executor = codex` + `--task-model fable:high`
      fails at startup with the config source named; global `task_model = gpt-6-astra:medium` and no
      `executor` key runs codex; `--task-model fable:high --review-model fable:high
      --external-reviewers codex:gpt-6-astra:high` over that global runs claude with no error;
      the same without `--review-model` fails naming `review_model`
- [ ] verify edge cases: `executor =` local reset still forces claude with `task_model = gpt-…`
      and then fails the mismatch check (explicit wins, conflict reported); `--codex` with a wrapper
      `codex_command` and `task_model = fable` passes
- [ ] run `make test </dev/null` - the full suite (asset checks, race-enabled Go tests, wrapper
      suites) must pass
- [ ] run `make lint` - all issues must be fixed
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...` (base-name checks use `filepath`)

### Task 7: Update documentation

- [ ] `pkg/config/defaults/config`: extend the `executor` comment — unset means inferred from
      `task_model` when the name is recognizable (`gpt-*`, `codex*`, `o1`/`o3`/`o4` → codex;
      `claude*`, `opus`, `sonnet`, `haiku`, `fable` → claude), otherwise Claude Code; an explicit
      value always wins and a recognizable model of the other provider is a startup error;
      inference and the check are skipped for a custom `claude_command`/`codex_command`
- [ ] `README.md` (around line 717, "Claude Code is the default primary executor"): describe
      inference, the mismatch error, and that `executor =` in a local config is the explicit
      override; add the `--task-model fable:high --external-reviewers codex:gpt-6-astra:high`
      example as the flag-free way to write with Claude and review with codex
- [ ] `CLAUDE.md`: add a paragraph after the `validateModelSpecs` one describing
      `ModelProvider`, inference in `applyCodexOverrides` (explicit wins, wrapper guard, why
      `codex_model` is not consulted), `validateModelProviders`/`validateReviewerProviders`
      placement, and `ExecutorSource` in the banner
- [ ] `llms.txt`: one line on executor inference if it lists executor selection

## Technical Details

**Recognizer** — `pkg/config/model_provider.go`:

```go
var claudeModelPrefixes = []string{"claude", "opus", "sonnet", "haiku", "fable"}
var codexModelPrefixes  = []string{"gpt", "codex"}
var codexModelExact     = []string{"o1", "o3", "o4"} // whole model or followed by "-"

// ModelProvider returns ExternalReviewToolClaude, ExternalReviewToolCodex, or ""
// for a model[:effort] spec whose model half it does not recognize.
func ModelProvider(spec string) string
```

**Executor resolution order** in `applyCodexOverrides`:

1. `o.Codex` → codex, source `--codex`
2. `cfg.ExecutorSet` → `cfg.Executor` as configured (empty = claude), source `executor = … in config`
3. `cfg.IsRealClaudeCommand()` and `ModelProvider(resolveSpec(o.TaskModel, cfg.TaskModel)) != ""`
   → that provider, source `inferred from task_model "…"`
4. otherwise claude, source `default`

**Mismatch check** — `validateModelProviders`, called right after `validateModelSpecs`:

```
for each (label, spec) in plan/task/review:
    p := ModelProvider(spec); if p == "" || p == primaryProvider(cfg): continue
    if primary is claude && !cfg.IsRealClaudeCommand(): return nil   // wrapper-defined names
    if primary is codex  && !cfg.IsRealCodexCommand():  return nil
    return fmt.Errorf("%s %q is a %s model, but the executor is %s (%s)", label, spec, p, primary, cfg.ExecutorSource)
```

`validateReviewerProviders` applies `ModelProvider` to each `resolvedReviewer.Model` and rejects
`Provider != ModelProvider(Model)` when both are non-empty and the provider is not `custom`.

**Error text examples**:

```
--task-model / task_model "fable:high" is a claude model, but the executor is codex (executor = codex in config)
--review-model / review_model "gpt-6-astra:high" is a codex model, but the executor is claude (inferred from task_model "fable:high")
external reviewer entry 1 (codex) names a claude model "fable:high"
```

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

**User config cleanup**: remove `executor = codex` from `~/.config/loopai/config`. With
`task_model = gpt-6-astra:medium` still there, codex is inferred for ordinary runs, and
`--task-model fable:high --review-model fable:high --external-reviewers codex:gpt-6-astra:high`
switches a single run to a Claude primary with codex as the external reviewer. The
`wall-defense-battle/.loopai/config` written during planning keeps working unchanged; its
`executor =` line becomes redundant once the global key is gone.

**Smoke test**: from a built binary, run `loopai --task-model fable:high <plan>` against a config
that still has `executor = codex` and confirm the startup error names
`executor = codex in config`; then remove the key and confirm the banner reads
`executor: claude (inferred from task_model "fable:high")`.
