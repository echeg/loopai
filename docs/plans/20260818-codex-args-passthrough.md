# Codex Extra Args Pass-Through (--codex-args)

## Overview
- Add a `--codex-args` CLI flag and a `codex_args` config key: extra arguments appended to every codex invocation loopai spawns (first-class `--codex` phases and external codex review under a Claude primary). Primary use case: per-run or per-project control of codex settings loopai deliberately does not manage, e.g. `-c service_tier="default"` to keep long autonomous runs off the priority tier while interactive sessions keep `~/.codex/config.toml`'s `service_tier = "priority"`.
- Unlike `claude_args` (which REPLACES the claude command's argument list), codex args are strictly ADDITIVE: loopai owns the `codex exec` invocation shape (sandbox, timeouts, model overrides) and user extras are appended on top — consistent with the existing "additive `-c` overrides" contract.
- User extras are appended AFTER loopai's own `-c` overrides, so an explicit user setting wins on key collision (power-user semantics, documented as sharp).

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/main.go` — flag definition next to `ClaudeArgs` (~48), runtime override wiring (~4583: `cfg.ClaudeArgs`/`ClaudeArgsSet` pattern)
  - `pkg/config/config.go` — `ClaudeArgs`/`ClaudeArgsSet` fields (~114) to mirror as `CodexArgs`/`CodexArgsSet` (runtime-only Set marker excluded from JSON) — ⚠️ the `CodexArgsSet` half was dropped during implementation, see Task 1
  - `pkg/config/values.go` — ini parsing (~211 `claude_args` pattern), merge precedence (~485: non-empty local overrides global)
  - `pkg/config/defaults/config` — commented `codex_args` example next to `claude_args` (~19-23); `make test` verifies embedded config comments match behavior
  - `pkg/processor/executor_factory.go` — codex executor construction (`buildCodexExecutor` and the external-codex-review builder) gets the extras (~229 shows the claude wiring pattern)
  - `pkg/executor/codex.go` — arg construction (~197-222): append extras after loopai's `-c` overrides, before stdin prompt handling
  - `pkg/executor/executor.go` — `splitArgs` (~120) already handles single/double quotes; reuse for tokenizing the extras string — ⚠️ it also consumed every backslash, so review added POSIX-shell backslash rules via the new `backslashEscapes` helper, see Task 3
- Related patterns found:
  - `ClaudeArgsSet` tracks explicit runtime overrides including explicit-empty `--claude-args=` (clears an inherited value) — ⚠️ mirrored only for the CLI-side clear; no `Config.CodexArgsSet` field was added, see Task 1
  - Codex `-c` overrides are emitted conditionally to preserve `~/.codex/config.toml` (documented contract in CLAUDE.md/llms.txt) — the new flag extends, not changes, that contract
- Dependencies identified: none new

## Development Approach
- **Testing approach**: Regular (code first, then tests in the same task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility: without the flag/key, codex invocations are byte-identical to today
- Tests must redirect config paths to `t.TempDir()` and never touch real `~/.config/loopai/`, `~/.config/ralphex/`, or `~/.codex/`

## Testing Strategy
- **Unit tests**: table-driven tests across config parsing, merge precedence, factory wiring, and codex arg construction
- **E2E tests**: none (no dashboard changes)

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Implementation Steps

### Task 1: Add codex_args to configuration
- [x] add `CodexArgs string` (json `codex_args`) to `pkg/config/config.go` (⚠️ scope change during review: the planned runtime-only `CodexArgsSet bool` was dropped — `ClaudeArgsSet` exists only so an empty `claude_args` means "no arguments" instead of "use the defaults", and empty codex extras already append nothing, so the explicit-empty `--codex-args=` clear is handled by the `o.codexArgsSet` guard in `applyCLIOverrides` alone)
- [x] parse `codex_args` in `pkg/config/values.go` and include it in the local-over-global merge (non-empty local wins, mirroring `claude_args`)
- [x] add a commented `codex_args` entry with the service_tier example to `pkg/config/defaults/config` (placed in the codex executor section next to `codex_command`, not next to `claude_args`, so the codex block stays coherent), stating additive semantics and user-wins precedence
- [x] write tests: key parsed from global, local override, absent key → empty, embedded default remains commented-out/empty
- [x] run tests - must pass before next task
- ➕ extracted `Values.mergeCodexFrom` from `mergeFrom`: the added merge clause pushed `mergeFrom` past the gocyclo limit of 20

### Task 2: Wire the flag and executor pass-through
- [x] add `CodexArgs string` option with `long:"codex-args"` and description "extra arguments appended to every codex invocation (additive; explicit -c values override loopai's)" to `cmd/loopai/main.go`, wiring `cfg.CodexArgs` like the ClaudeArgs runtime override, with the explicit empty `--codex-args=` clear handled by the local `o.codexArgsSet` guard rather than a `Config.CodexArgsSet` field
- [x] add `ExtraArgs string` to the codex executor and append `splitArgs(e.ExtraArgs)` in `pkg/executor/codex.go` arg construction AFTER loopai's `-c` overrides (model, effort, timeout, project_doc) and sandbox flags
- [x] pass the config value into BOTH codex executor construction sites in `pkg/processor/executor_factory.go`: first-class `--codex` and external codex review under a Claude primary
- [x] confirm codex CLI gives later `-c` occurrences precedence (document the verified behavior in a code comment; if earlier-wins, move extras before loopai's overrides and update the flag description)
- [x] write tests: extras appended in the right position, quoted values survive splitting (`-c service_tier="default"`), empty extras → byte-identical args to today (update existing golden arg tests), both construction sites carry the value, explicit-empty flag clears config value
- [x] run tests - must pass before next task
- ➕ verified against codex-cli 0.147.0 that repeated `-c` keys resolve last-occurrence-wins (`-c model="aaa-first" -c model="zzz-second"` reported `model: zzz-second`), so appending extras last gives the user precedence as designed; recorded in a comment in `pkg/executor/codex.go`
- ➕ `--codex-args` also added to `hasExecutionMode`/`markFlagsSet` so it counts as an execution option like `--claude-args`

### Task 3: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (flag + config key, additive append, both codex paths, user-wins precedence)
- [x] verify edge cases: extras containing `=` and quotes, whitespace-only value, config value plus flag override, `--codex-args` without `--codex` (applies to external codex review; harmless no-op when no codex invocation happens)
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] verify test coverage meets project standard (80%+) for the new code paths
- ➕ added two `TestCodexExecutor_Run_ExtraArgs` rows for the remaining uncovered edge cases: a value containing `=` (`-c shell_environment_policy.set.FOO="a=b"`) and a quoted value containing whitespace (`-c notify="say hi"`), both of which stay a single argument through `splitArgs`
- ➕ both codex construction sites are covered by one assignment in `newBaseCodexExecutor`, which `buildCodexExecutor` and `buildExternalCodexExecutor` share, so `--codex-args` without `--codex` reaches the external codex reviewer (asserted in `TestExecutorFactory_Build_CodexArgs`)
- ➕ coverage after the run: config 90.3%, executor 89.2%, processor 91.5%, cmd/loopai 85.9% — all above the 80% standard; `make lint` reports 0 issues
- ⚠️ `scripts/copilot-as-claude/copilot-as-claude_test.sh` failed sporadically with `echo: write error: Broken pipe` on one `make test` run and passed on repeated standalone and full-suite runs; pre-existing flake in that shell suite, unrelated to codex args — root-caused and fixed during review, see the two entries below
- ➕ review follow-up: fixed that pre-existing flake instead of leaving it documented. Its `echo "$output" | grep -q ...` / `grep ... | head -1` pipelines let the reader exit before `echo`/`grep` finished writing, so the writer took SIGPIPE and the shell reported `write error: Broken pipe`. Replaced with herestrings and `grep -m1`, which have no upstream writer process to kill. The suite now passes deterministically
- ➕ review follow-up: `splitArgs` consumed every backslash regardless of quote context, so `-c cwd="C:\work"` lost its separators. Added `backslashEscapes` to apply POSIX-shell rules (literal inside single quotes, escaping only `"` and `\` inside double quotes, escaping the next rune unquoted, literal when it is the last rune) and covered it with new `TestSplitArgs` rows. The documented `[\"CLAUDE.md\"]` quote-escaping recipe still works

### Task 4: [Final] Update documentation
- [x] update README.md flag list and the codex configuration section with the service_tier recipe
- [x] update CLAUDE.md codex paragraph (additive `-c` overrides now include user extras; user extras win on collision)
- [x] update llms.txt codex/flags coverage
- [x] verify embedded config comments match the implemented behavior (checked by make test)
- ➕ README has no exhaustive flag table, so the "flag list" item became the `## Common commands` example block plus a new paragraph in `## Executors and reviews` carrying the recipe in both flag and `codex_args` form
- ⚠️ `scripts/copilot-as-claude/copilot-as-claude_test.sh` failed 6-7 of 86 assertions with `echo: write error: Broken pipe`; reproduced identically on the pre-plan master checkout at 0c22741, so it was pre-existing and unrelated. Fixed during review (see Task 3) rather than left as a known flake. Go suite, asset/manifest checks, and `make lint` (0 issues) all pass

## Technical Details
- Append order in `codex.go`: `exec` → bypass/sandbox flags → loopai `-c` overrides (model, reasoning effort, idle timeout, project_doc, multi-agent) → **user extras** → prompt via stdin. User extras can therefore override any loopai-set `-c` key, deliberately.
- `splitArgs` (executor.go:120) handles quoting/escaping — no new tokenizer, but review changed the existing one: backslash handling now follows POSIX-shell rules through the added `backslashEscapes` helper (literal in single quotes, escaping only `"` and `\` in double quotes, escaping the next rune unquoted, literal when nothing follows). Before that, every backslash was consumed, so a Windows path in a `-c` value lost its separators.
- Additive-vs-replace asymmetry with `claude_args` is intentional and must be called out in the flag description and docs: claude args ARE the command's args; codex args extend a command loopai composes.
- The example recipe for the motivating use case, to appear in README and embedded config comments:
  `loopai --codex '--codex-args=-c service_tier="default"' ...` or `codex_args = -c service_tier="default"` in `.loopai/config`.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- With `service_tier = "priority"` in `~/.codex/config.toml`: run `loopai --codex '--codex-args=-c service_tier="default"' <plan>` and confirm via codex session logs that the run used the default tier while an interactive `codex` session still uses priority
- Run without the flag and diff the spawned codex command line against a pre-change build (byte-identical)

**External system updates**: none
