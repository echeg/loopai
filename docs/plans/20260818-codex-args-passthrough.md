# Codex Extra Args Pass-Through (--codex-args)

## Overview
- Add a `--codex-args` CLI flag and a `codex_args` config key: extra arguments appended to every codex invocation loopai spawns (first-class `--codex` phases and external codex review under a Claude primary). Primary use case: per-run or per-project control of codex settings loopai deliberately does not manage, e.g. `-c service_tier="default"` to keep long autonomous runs off the priority tier while interactive sessions keep `~/.codex/config.toml`'s `service_tier = "priority"`.
- Unlike `claude_args` (which REPLACES the claude command's argument list), codex args are strictly ADDITIVE: loopai owns the `codex exec` invocation shape (sandbox, timeouts, model overrides) and user extras are appended on top — consistent with the existing "additive `-c` overrides" contract.
- User extras are appended AFTER loopai's own `-c` overrides, so an explicit user setting wins on key collision (power-user semantics, documented as sharp).

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/main.go` — flag definition next to `ClaudeArgs` (~48), runtime override wiring (~4583: `cfg.ClaudeArgs`/`ClaudeArgsSet` pattern)
  - `pkg/config/config.go` — `ClaudeArgs`/`ClaudeArgsSet` fields (~114) to mirror as `CodexArgs`/`CodexArgsSet` (runtime-only Set marker excluded from JSON)
  - `pkg/config/values.go` — ini parsing (~211 `claude_args` pattern), merge precedence (~485: non-empty local overrides global)
  - `pkg/config/defaults/config` — commented `codex_args` example next to `claude_args` (~19-23); `make test` verifies embedded config comments match behavior
  - `pkg/processor/executor_factory.go` — codex executor construction (`buildCodexExecutor` and the external-codex-review builder) gets the extras (~229 shows the claude wiring pattern)
  - `pkg/executor/codex.go` — arg construction (~197-222): append extras after loopai's `-c` overrides, before stdin prompt handling
  - `pkg/executor/executor.go` — `splitArgs` (~120) already handles single/double quotes; reuse for tokenizing the extras string
- Related patterns found:
  - `ClaudeArgsSet` tracks explicit runtime overrides including explicit-empty `--claude-args=` (clears an inherited value) — mirror exactly
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
- [ ] add `CodexArgs string` (json `codex_args`) and runtime-only `CodexArgsSet bool` (json `-`) to `pkg/config/config.go`, documented like `ClaudeArgsSet`
- [ ] parse `codex_args` in `pkg/config/values.go` and include it in the local-over-global merge (non-empty local wins, mirroring `claude_args`)
- [ ] add a commented `codex_args` entry with the service_tier example to `pkg/config/defaults/config` next to `claude_args`, stating additive semantics and user-wins precedence
- [ ] write tests: key parsed from global, local override, absent key → empty, embedded default remains commented-out/empty
- [ ] run tests - must pass before next task

### Task 2: Wire the flag and executor pass-through
- [ ] add `CodexArgs string` option with `long:"codex-args"` and description "extra arguments appended to every codex invocation (additive; explicit -c values override loopai's)" to `cmd/loopai/main.go`, wiring `cfg.CodexArgs`/`CodexArgsSet` exactly like the ClaudeArgs runtime override (explicit empty `--codex-args=` clears an inherited config value)
- [ ] add `ExtraArgs string` to the codex executor and append `splitArgs(e.ExtraArgs)` in `pkg/executor/codex.go` arg construction AFTER loopai's `-c` overrides (model, effort, timeout, project_doc) and sandbox flags
- [ ] pass the config value into BOTH codex executor construction sites in `pkg/processor/executor_factory.go`: first-class `--codex` and external codex review under a Claude primary
- [ ] confirm codex CLI gives later `-c` occurrences precedence (document the verified behavior in a code comment; if earlier-wins, move extras before loopai's overrides and update the flag description)
- [ ] write tests: extras appended in the right position, quoted values survive splitting (`-c service_tier="default"`), empty extras → byte-identical args to today (update existing golden arg tests), both construction sites carry the value, explicit-empty flag clears config value
- [ ] run tests - must pass before next task

### Task 3: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented (flag + config key, additive append, both codex paths, user-wins precedence)
- [ ] verify edge cases: extras containing `=` and quotes, whitespace-only value, config value plus flag override, `--codex-args` without `--codex` (applies to external codex review; harmless no-op when no codex invocation happens)
- [ ] run full test suite (`make test`)
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] verify test coverage meets project standard (80%+) for the new code paths

### Task 4: [Final] Update documentation
- [ ] update README.md flag list and the codex configuration section with the service_tier recipe
- [ ] update CLAUDE.md codex paragraph (additive `-c` overrides now include user extras; user extras win on collision)
- [ ] update llms.txt codex/flags coverage
- [ ] verify embedded config comments match the implemented behavior (checked by make test)

## Technical Details
- Append order in `codex.go`: `exec` → bypass/sandbox flags → loopai `-c` overrides (model, reasoning effort, idle timeout, project_doc, multi-agent) → **user extras** → prompt via stdin. User extras can therefore override any loopai-set `-c` key, deliberately.
- `splitArgs` (executor.go:120) already handles quoting/escaping — no new tokenizer.
- Additive-vs-replace asymmetry with `claude_args` is intentional and must be called out in the flag description and docs: claude args ARE the command's args; codex args extend a command loopai composes.
- The example recipe for the motivating use case, to appear in README and embedded config comments:
  `loopai --codex --codex-args '-c service_tier="default"' ...` or `codex_args = -c service_tier="default"` in `.loopai/config`.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- With `service_tier = "priority"` in `~/.codex/config.toml`: run `loopai --codex --codex-args '-c service_tier="default"' <plan>` and confirm via codex session logs that the run used the default tier while an interactive `codex` session still uses priority
- Run without the flag and diff the spawned codex command line against a pre-change build (byte-identical)

**External system updates**: none
