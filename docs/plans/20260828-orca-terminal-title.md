# Orca Terminal Title Status via `--orca`

## Overview

Add a `--orca` flag (with `orca = true` config key and `LOOPAI_ORCA` env) that makes loopai
emit OSC terminal-title escape sequences reflecting the current phase, task progress, and
completion state. Orca (https://github.com/stablyai/orca) runs each agent in a terminal tab and
derives the tab name and agent status (working / permission / idle) from OSC titles, so this gives
a loopai run a meaningful tab name and a correct status without any change on the orca side.

This is the terminal-title analogue of the existing best-effort cmux integration in `pkg/cmux`:
a nil reporter when the flag is off, every method nil-safe, no failure can reach the run.

Title vocabulary, chosen to match orca's detector (`src/shared/agent-title-status.ts` in orca:
a quarter-circle spinner glyph anywhere means *working*; a title starting with `✳ ` means *idle*;
the keyword `waiting` means *permission* but only when the title also carries a known agent name,
which `claude` and `codex` are and `loopai` is not — hence the primary executor name at the end):

| state                     | title                                        | orca reads  |
|---------------------------|----------------------------------------------|-------------|
| task phase                | `◐ loopai · task 3/7 · claude`               | working     |
| task phase, total unknown | `◐ loopai · task 3 · claude`                 | working     |
| internal review           | `◐ loopai · review · iteration 2 · claude`   | working     |
| external review           | `◐ loopai · external review · iteration 1 · claude` | working |
| external evaluation       | `◐ loopai · external eval · claude`          | working     |
| plan creation             | `◐ loopai · plan · iteration 2 · claude`     | working     |
| finalize                  | `◐ loopai · finalize · claude`               | working     |
| waiting for user input    | `loopai · waiting for input · claude`        | permission  |
| provider limit wait       | `loopai · waiting for limit · claude`        | permission  |
| success                   | `✳ loopai · done`                            | idle        |
| failure                   | `✳ loopai · failed`                          | idle        |
| stopped without Finish    | `✳ loopai`                                   | idle        |

The escape sequence is `ESC ] 0 ; <title> BEL` (`"\x1b]0;" + title + "\a"`), written as one
`Write` call so it cannot be torn by a concurrent log line. `✳` is U+2733, `◐` is U+25D0, the
separator is ` · ` (U+00B7). The executor name is `codex` when `cfg.Executor == config.ExecutorCodex`
and `claude` otherwise (`ExecutorClaude` is the empty string).

## Decisions

- **Context**: orca's status detection is title-based and best-effort; loopai already has a
  best-effort sidebar integration for cmux, and the two must not interfere.
- **Chosen approach**: new package `pkg/orca` mirroring `pkg/cmux`'s `Reporter` shape — nil
  reporter when disabled, phase updates through `status.PhaseHolder.OnChange`, iteration and task
  numbers through a thin logger wrapper observing `PrintSection`, waiting-for-input through an
  input-collector decorator, and `Finish`/`Stop` for the final idle title. The wrapper sits
  *below* the cmux wrapper in the logger chain, exactly where `progress.SectionTimer` sits, so the
  outermost logger keeps cmux's optional `LogLimitWait`/`LogLimitRecovery` methods and
  `pkg/cmux` is not modified.
- **Rejected alternatives**:
  - Adding title emission to `progress.Logger` itself — the logger never sees phases, and it
    mixes output with status reporting.
  - A background poller re-reading the plan every N seconds (cmux's progress-bar model) — it
    writes to stdout concurrently with `progress.Logger` and lags by the poll interval; the
    section events already carry the current task number synchronously.
  - The originally proposed `✳ loopai: task 3/7` format — orca reads a leading `✳ ` as idle, so
    the tab would show the wrong status for the whole run.
  - Placing the orca wrapper outermost and re-forwarding the optional limit methods — works, but
    duplicates the optional-interface contract for no gain: `retryPolicy.enterLimitWait` already
    publishes `status.PhaseLimitWait` through the `PhaseHolder`
    (`pkg/processor/execution_policy.go:164`), so the phase observer covers the limit-wait title.
- **Verified facts**:
  - `Section{Type: SectionTaskIteration, Iteration: N}` already carries the current 1-indexed task
    number (`pkg/processor/phase/task.go:67-71`, via `NextPlanTaskPosition`); the total is not in
    the section and comes from `plan.ParsePlanFile(planFile)`, as `pkg/cmux`'s
    `reportProgressContext` does (`pkg/cmux/cmux.go:575`).
  - The cmux `reportingLogger` does not forward `LogLimitWait` to its inner logger
    (`pkg/cmux/cmux.go:708`), which is why the limit-wait title must come from the phase observer.
  - No code in the repository writes raw escape sequences today; colours go through
    `fatih/color`. `golang.org/x/term` is vendored and used once in
    `cmd/loopai/terminal_unix.go:16`; it also works on Windows, so no build-tag stub is needed.
  - Only two `env:` tags exist on `opts` (`LOOPAI_CONFIG_DIR`, `LOOPAI_WEB_HOST`);
    `TestCmuxEnvOptionsCoversOptionTags` (`cmd/loopai/main_test.go`) fails unless every `env:` tag
    is listed in `cmuxEnvOptions` (`cmd/loopai/main.go:3150`).
  - `progress.Logger` serialises its own file+stdout writes under `writeMu`
    (`pkg/progress/progress.go:321`); a title emitted from the logger wrapper runs on the same
    goroutine as the section it follows, and a single-`Write` escape sequence stays intact.

## Context (from discovery)

- Files/components involved:
  - `pkg/cmux/cmux.go` — the pattern to mirror: `Reporter` (`:187`), `New` (`:225`),
    `OnPhase` (`:766`), `OnSection` (`:810`), `statusText` (`:834`), `WrapLogger` (`:694`),
    `WrapInput` (`:736`), `Finish` (`:461`), `Stop` (`:657`), `Logger` interface (`:681`)
  - `pkg/cmux/cmux_test.go` — test scaffolding to mirror: `fakeLogger`, `fakeCollector`,
    `TestReporterNilReceiver` (`:998`), `TestReporterWrapLoggerForwardsAllMethods` (`:787`),
    `TestReporterOnPhaseAsObserver` (`:811`)
  - `cmd/loopai/main.go` — `opts` struct (`:40-108`), `applyCLIOverrides` (`:4557`),
    `cmuxEnvOptions` (`:3150`), `buildRunnerLogger` (`:772`), execution wiring in `executePlan`
    (`:801-817`, `:864-868`, `:962`, `:977`), `finishCmuxCompletion` (`:655`), plan-creation wiring
    (`:2496-2502`, `:2560`, `:2614`)
  - `pkg/config/values.go` (`:73-74`, `:397-403`, `:623-625`) and `pkg/config/config.go`
    (`:158-159`, `:402-403`) — the `use_worktree` bool-key pattern with its `…Set` twin
  - `pkg/config/defaults/config:217-221` — commented default for `use_worktree`, template for
    the new `orca` key
  - `pkg/status/status.go` (phases), `pkg/status/section.go` (section types),
    `pkg/status/phase_holder.go` (observer contract)
  - `pkg/plan/parse.go` — `ParsePlanFile`, `Plan.Tasks`
  - `README.md` (`## Progress and dashboard`, cmux paragraph at `:864`), `llms.txt` (`:179`,
    `:201`), `CLAUDE.md` (project structure list at `:52`, logger-chain paragraph)
- Related patterns found: nil-receiver no-op reporters; optional logger interfaces discovered by
  type assertion in `pkg/processor/execution_policy.go:132-157`; `go-flags` boolean options with
  `env:` tags; commented defaults in the embedded config
- Dependencies identified: `golang.org/x/term` (already vendored) for TTY detection; no new
  modules

## Development Approach

- **Testing approach**: TDD (write failing tests first, then implement to green)
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
- Maintain backward compatibility: without `--orca` / `orca = true` the binary's stdout must be
  byte-identical to today

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above). Title tests assert the
  exact bytes written to an injected `bytes.Buffer` (`"\x1b]0;◐ loopai · task 3/7 · claude\a"`),
  never substrings.
- **E2E tests**: the Playwright suite covers the dashboard only; this change does not touch it.
- The repository is not orca, so orca-side detection is verified in Post-Completion, not here.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this codebase - code
  changes, tests, documentation updates
- **Post-Completion** (no checkboxes): items requiring external action - manual testing in orca,
  verification against orca's detector
- **Checkbox placement**: Checkboxes belong only in Task sections. Do not put checkboxes in
  Success criteria, Overview, or Context — they cause extra loop iterations.

## Validation Commands

```bash
make test
make lint
GOOS=windows GOARCH=amd64 go build ./...
```

## Implementation Steps

### Task 1: Title formatting and escape-sequence writer in `pkg/orca`

Pure functions first, so the vocabulary in the Overview table is pinned by tests before any
wiring exists.

- [x] write failing table-driven tests in `pkg/orca/orca_test.go` for `titleFor(state, executor)`
      covering every row of the Overview table: task with total, task without total (0), each
      phase label, review/external-review iterations, plan iteration, waiting-for-input,
      waiting-for-limit, done, failed, stopped; assert exact strings including `◐`, `✳`, ` · `
- [x] write failing tests for `writeTitle(w io.Writer, title string)` asserting the exact bytes
      `"\x1b]0;" + title + "\a"` land in a `bytes.Buffer` in a single `Write` call (use a writer
      double that counts calls), and that a writer error is swallowed and returns nothing
- [x] create `pkg/orca/orca.go` with a package doc mirroring `pkg/cmux/cmux.go:1-5` (best-effort,
      one-directional, nil reporter = no-op), a `state` value type (phase, task, total, iteration,
      waiting kind, final kind), `titleFor`, and `writeTitle`; every title is emitted through
      `writeTitle` and nothing else in the package touches the writer
- [x] make the tests pass; run `go test ./pkg/orca/...`
- [x] run tests - must pass before next task

### Task 2: `Reporter` lifecycle: `New`, TTY gating, `OnPhase`, `Finish`, `Stop`, nil safety

- [x] write failing tests: `New` with `enabled=false` returns nil; `New` with an enabled flag but a
      non-TTY stdout returns nil; a `var r *Reporter` survives every exported method
      (`assert.NotPanics` table, mirror `TestReporterNilReceiver` at `pkg/cmux/cmux_test.go:998`)
- [x] write failing tests: `OnPhase` driven through a real `status.PhaseHolder` writes the
      matching working title for `PhaseTask`, `PhaseReview`, `PhaseExternalReview`,
      `PhaseExternalEval`, `PhasePlan`, `PhaseFinalize`, and the `waiting for limit` title for
      `PhaseLimitWait`; a repeated `Set` of the same phase writes nothing; the executor suffix is
      `codex` when constructed with `config.ExecutorCodex` and `claude` for the empty default
- [x] write failing tests: `Finish(true, …)` writes `✳ loopai · done`, `Finish(false, …)` writes
      `✳ loopai · failed`, and any `OnPhase`/`OnSection` after `Finish` writes nothing; `Stop`
      without a preceding `Finish` writes `✳ loopai` once, `Stop` after `Finish` writes nothing,
      and a second `Stop` writes nothing (`sync.Once`)
- [x] implement `Reporter` with an injectable `io.Writer` and an injectable `isTerminal func() bool`
      (production default `term.IsTerminal(int(os.Stdout.Fd()))` from `golang.org/x/term`,
      checking stdout rather than stdin); guard state with a mutex the way `pkg/cmux` guards
      `statusMu`; keep the `quiesced || stopped || finished` gate so a late phase observer on the
      execution goroutine cannot overwrite the final idle title
- [x] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` to confirm the `x/term` call needs
      no build-tag stub
- [x] run tests - must pass before next task

### Task 3: Section observer and logger wrapper for task and iteration numbers

- [x] write failing tests for `OnSection`: `SectionTaskIteration{Iteration: 3}` with a plan file
      of 7 tasks writes `◐ loopai · task 3/7 · claude`; with `planFile == ""` or an unparsable
      plan writes `◐ loopai · task 3 · claude` (never fails); `SectionInternalReview{Iteration: 2}`
      writes the review iteration title; `SectionExternalReviewIteration` and
      `SectionPlanIteration` likewise; `SectionGeneric`, `SectionExternalEvaluation`, and
      `SectionCustomIteration` write nothing; nothing is written after `Finish`/`Stop`
- [x] write failing tests for `WrapLogger`: a nil reporter returns the inner logger unchanged
      (`assert.Same`); the wrapper forwards every method of the `Logger` interface to the inner
      logger (mirror `TestReporterWrapLoggerForwardsAllMethods` at `pkg/cmux/cmux_test.go:787`);
      `PrintSection` reaches the inner logger *before* the title is written
- [x] declare `orca.Logger` mirroring `cmux.Logger` (`pkg/cmux/cmux.go:681-691`, i.e.
      `progress.SectionLogger`), implement `OnSection`, the `titleLogger` wrapper with an
      embedded inner `Logger` and a `PrintSection` override, and `planTaskTotal` using
      `plan.ParsePlanFile` with every error mapped to total 0
- [x] run tests - must pass before next task

### Task 4: Waiting-for-input decorator (`WrapInput`)

- [x] write failing tests: `WrapInput` on a nil reporter returns the collector unchanged;
      `AskQuestion` writes `loopai · waiting for input · claude` before delegating, returns the
      inner result and error untouched, and restores the previous working title after the inner
      call returns; `AskDraftReview` behaves the same; after `Finish` no title is written
- [x] declare a local `inputCollector` interface mirroring `processor.InputCollector` (as
      `pkg/cmux/cmux.go:729-733` does, to keep `pkg/orca` free of a `pkg/processor` import) and
      implement the decorator with the same `//nolint:wrapcheck` pass-through comment style
- [x] run tests - must pass before next task

### Task 5: `orca` config key with `…Set` twin, embedded default, and merge

- [x] write failing tests in `pkg/config/values_test.go` (or the file that tests `use_worktree`
      parsing): `orca = true` sets `Orca=true, OrcaSet=true`; `orca = false` sets
      `Orca=false, OrcaSet=true`; absent key leaves both false; `orca = maybe` returns
      `invalid orca: …`; `mergeFrom` copies the value only when `OrcaSet`
- [x] write failing test in `pkg/config/config_test.go` that `Load` surfaces `Orca`/`OrcaSet` on
      `config.Config`
- [x] add `Orca bool` and `OrcaSet bool` to `Values` (`pkg/config/values.go:73-74` pattern), parse
      `orca` next to `use_worktree` (`:397-403`), merge next to `WorktreeEnabledSet` (`:623-625`),
      and copy to `Config` (`pkg/config/config.go:158-159`, `:402-403`)
- [x] add a commented `# orca = false` block to `pkg/config/defaults/config` beside `use_worktree`
      (`:217-221`) explaining that it emits OSC terminal titles for orca and is ignored when stdout
      is not a terminal; confirm `make test`'s asset checks and any defaults-config test still pass
- [x] run tests - must pass before next task

### Task 6: `--orca` flag, env tag, CLI override, and hand-off env list

- [x] write failing tests in `cmd/loopai/main_test.go`: `applyCLIOverrides` with `o.Orca=true`
      sets `cfg.Orca=true`; with `o.Orca=false` leaves a config-provided `cfg.Orca=true` intact
      (flag only turns it on, mirroring `--worktree`); `cmuxEnvOptions` contains `LOOPAI_ORCA`
      (`TestCmuxEnvOptionsCoversOptionTags` at `cmd/loopai/main_test.go:10075` fails until it does)
- [x] add `Orca bool \`long:"orca" env:"LOOPAI_ORCA" description:"emit terminal title status for orca"\``
      to `opts` next to `NoColor` (`cmd/loopai/main.go:73`); do **not** add it to
      `executionModeSet` in `markFlagsSet` (`:145-154`) — it is cosmetic and must not turn an
      invocation into an execution run
- [x] extend `applyCLIOverrides` (`:4557`) and `cmuxEnvOptions` (`:3150`)
- [x] run tests - must pass before next task

### Task 7: Wire the reporter into the execution and plan-creation paths

- [x] write failing tests for a new `orcaReporter(cfg *config.Config, planFile string) *orca.Reporter`
      helper in `cmd/loopai`: returns nil when `cfg.Orca` is false; passes `codex` for
      `config.ExecutorCodex` and `claude` otherwise (inject the TTY check so the test does not
      depend on the test runner's stdout)
- [x] write a failing test that `buildRunnerLogger` with a non-nil orca reporter and a nil cmux
      reporter returns a logger whose `PrintSection` writes a title, and that with both reporters
      nil it still returns the section timer unchanged (existing behaviour)
- [x] change `buildRunnerLogger` (`cmd/loopai/main.go:772`) to
      `rep.WrapLogger(titles.WrapLogger(timer))` and update its doc comment: the orca wrapper sits
      below cmux and above the section timer so the outermost logger keeps cmux's optional
      rate-limit methods; update both call sites (`:864`, `:2502`)
- [x] in `executePlan`: construct the reporter beside `cmux.New` (`:801`), register
      `plr.holder.OnChange(titles.OnPhase)` after the cmux observer (`:868`), call `titles.Finish`
      wherever `finishCmuxCompletion` decides success/failure (`:655-666`), and `titles.Stop()`
      on every path that calls `rep.Stop()` (`:812`, `:962`, `:977`)
- [x] in the plan-creation path (`:2496-2614`): construct beside `cmux.New("", …)`, register the
      phase observer next to `:2502`, wrap the collector with `titles.WrapInput` alongside
      `rep.WrapInput` (`:2560`), and `Stop` when the creation reporter is released so the
      execution reporter starts from a clean title
- [x] run tests - must pass before next task

### Task 8: Verify acceptance criteria

- [x] verify every row of the Overview title table has a passing exact-bytes test
- [x] verify a run without `--orca` and without `orca = true` writes no escape sequence
      (test `New` disabled path plus `buildRunnerLogger` with nil reporters)
- [x] verify the non-TTY path writes nothing even with the flag on
- [x] verify `--orca` combined with `--cmux-workspace` is accepted by `validateFlags` and that
      `cmuxHandOffArgv` carries `LOOPAI_ORCA` when set in the environment
- [x] run `make test` (full suite, race-enabled) — must pass
- [x] run `make lint` — all issues fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...` — must compile
- [x] verify `pkg/orca` coverage is at or above the project standard (80%+)

### Task 9: Update documentation

- [ ] `README.md`: add a `--orca` bullet to the feature list near the cmux bullets (`:24-25`) and
      an "Orca terminal titles" paragraph in `## Progress and dashboard` after the cmux paragraph
      (`:864`), including the title table, the TTY rule, and that orca needs no configuration
- [ ] `llms.txt`: add the flag to the option list (`:201`) and a one-paragraph description next
      to the cmux paragraph (`:179`)
- [ ] `CLAUDE.md`: add `pkg/orca/` to the project structure list (`:52`), and extend the
      logger-chain paragraph ("Keep `Reporter.WrapLogger` in the logger chain after dashboard
      setup…") to state that the orca title wrapper sits below the cmux wrapper and above the
      section timer, and that limit-wait titles come from the phase observer, not the logger
- [ ] `docs/custom-providers.md` or a new short `docs/orca.md` only if README grows past a
      screen; otherwise leave README as the single source

## Technical Details

### `pkg/orca` API surface

```go
// New returns a reporter when enabled and stdout is a terminal, otherwise nil.
func New(enabled bool, planFile, executor string) *Reporter

func (r *Reporter) OnPhase(_, cur status.Phase)            // status.PhaseHolder.OnChange
func (r *Reporter) OnSection(section status.Section)        // called by the logger wrapper
func (r *Reporter) WrapLogger(logger Logger) Logger          // nil → logger unchanged
func (r *Reporter) WrapInput(c inputCollector) inputCollector // nil → c unchanged
func (r *Reporter) Finish(success bool)                       // final idle title, then frozen
func (r *Reporter) Stop()                                     // idle title unless finished; once
```

Test constructors inject `io.Writer` and the TTY predicate; production `New` binds
`os.Stdout` and `term.IsTerminal`.

### State → title

```
working(phaseLabel, taskNum, total, iteration, executor)
  "◐ loopai · " + label [+ " N/M" | " N"] [+ " · iteration K"] + " · " + executor
waiting(kind, executor)
  "loopai · waiting for " + kind + " · " + executor      // kind ∈ {input, limit}
final(kind)
  "✳ loopai · " + kind                                     // kind ∈ {done, failed}
stopped
  "✳ loopai"
```

Phase labels: `task`, `review`, `external review`, `external eval`, `plan`, `finalize`. The task
number is taken from `SectionTaskIteration.Iteration`; `OnPhase(PhaseTask)` alone (before the
first section) writes `◐ loopai · task · claude`.

### Processing flow

1. `executePlan` builds `titles := orcaReporter(cfg, planFile)` (nil unless `cfg.Orca` and TTY).
2. Logger chain, innermost → outermost: `progress.Logger` → `web.BroadcastLogger` (optional) →
   `progress.SectionTimer` → `orca.titleLogger` → `cmux.reportingLogger`.
3. Phase changes reach `titles.OnPhase` from the `PhaseHolder`; task/iteration numbers reach
   `titles.OnSection` from `titleLogger.PrintSection`; both write one escape sequence.
4. Interactive waits set the permission title and restore the previous working title afterward.
5. `Finish` writes the idle done/failed title and freezes the reporter; `Stop` writes the bare
   idle title only if `Finish` never ran, so an abort does not leave `◐` in the tab.

### Safety

- All writes are best-effort: write errors are discarded, no method returns an error.
- The reporter never spawns a goroutine and never reads the plan except inside `OnSection`.
- Without the flag, `New` returns nil and every call site is a no-op — the byte stream on stdout
  is unchanged.

## Post-Completion

**Manual verification in orca**:
- In the orca checkout (`/Users/echeg/Projects/Harness/orca`), run each title from the Overview
  table through `detectAgentStatusFromTitle` in `src/shared/agent-title-status.ts` (a small vitest
  or `pnpm tsx` snippet) and confirm working / permission / idle match the table.
- Open a worktree in orca, run `loopai --orca docs/plans/<plan>.md` in a tab, and confirm the tab
  title follows task progress and the tab status turns idle on completion.
- Confirm `loopai --orca … | tee log.txt` writes no escape sequence into `log.txt`.

**External system updates**:
- Add `command: loopai --orca …` (or `orca = true` in `.loopai/config`) to `defaultTabs` in the
  target repository's `orca.yaml` once the flag ships.
- Optionally propose `loopai` for orca's `AGENT_NAMES` upstream so the executor-name suffix
  becomes unnecessary; the format chosen here works without that change.
