# Dynamic Review Agents

## Overview

Add project-specific dynamic review agents to the internal review phase. Today the first internal review always launches the same 5 built-in agents (quality, implementation, testing, simplification, documentation) hard-wired as `{{agent:<name>}}` references in `review_first.txt`. A project agent dropped into `.loopai/agents/` is loaded by `agentLoader` but never invoked, because nothing references it.

This plan makes such agents first-class:

- an agent file with a non-empty `description:` frontmatter field is a **dynamic agent**
- a new `{{agents:dynamic}}` template variable expands into a catalog (name, description, ready-to-use invocation snippet) of all dynamic agents
- `review_first.txt` gains a step instructing the primary executor to pick 0-3 relevant dynamic agents per diff and launch them in the same parallel message as the 5 base agents
- a new `--gen-agents` standalone mode runs one executor session that analyzes the repository and writes 2-5 project-specific agent files into `.loopai/agents/`

The 5 built-in agents always run unchanged; dynamic agents are additive. Selection of relevant dynamic agents is done by the model from the catalog, not by code. Scope is internal review only (`review_first.txt`); `review_second.txt` and external reviewers are untouched.

## Context (from discovery)

- Files/components involved:
  - `pkg/config/frontmatter.go` — `Options` struct parsed from agent YAML frontmatter (`model`, `agent`); gains `description`
  - `pkg/config/config.go` — `CustomAgent` embeds `Options`, so `Description` flows through automatically
  - `pkg/config/agents.go` — `agentLoader` already unions `.txt` files from local → global → embedded; no changes expected
  - `pkg/processor/prompts.go` — `agentRefPattern` and `expandAgentReferences` implement `{{agent:name}}`; new `{{agents:dynamic}}` expansion lives here
  - `pkg/config/defaults/prompts/review_first.txt` — gains Step 2b and the `{{agents:dynamic}}` reference
  - `pkg/config/defaults/prompts/gen_agents.txt` — new embedded prompt for the generation mode
  - `cmd/loopai/main.go` — new `--gen-agents` flag and standalone-mode wiring (pattern: `--plan` interactive path, `runWithSectionTiming`)
- Related patterns found:
  - per-file fallback loading (local → global → embedded) shared by prompts and agents
  - executor-appropriate agent invocation snippets (Task tool for claude, spawn_agent for codex) generated centrally in `pkg/processor/prompts.go`
  - standalone close-out/init modes resolved in `cmd/loopai` before pipeline construction
- Dependencies identified: none new; reuses existing frontmatter parser (`gopkg.in/yaml.v3`), embedded FS, executor factory

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
- Maintain backward compatibility: with no dynamic agents configured, review behavior must be byte-for-byte identical to today except the catalog placeholder line
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real `~/.config/loopai/` or legacy `~/.config/ralphex/`
- Keep comments lowercase except exported godoc; table-driven tests with testify

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: dashboard Playwright suite is unaffected (no web changes); do not add e2e tests
- Asset-level tests verify the embedded `review_first.txt` references `{{agents:dynamic}}` and that prompt assembly leaves no unexpanded placeholders

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Validation Commands

- `make test`
- `make lint`

## Implementation Steps

### Task 1: Add description field to agent frontmatter

- [x] write failing tests in `pkg/config/frontmatter_test.go`: `description:` parsed into `Options.Description` (present, absent, empty, multi-word); `Options.String()` unchanged for agents without description
- [x] add `Description string \`yaml:"description"\`` to `Options` in `pkg/config/frontmatter.go`
- [x] add test in `pkg/config/agents_test.go`: agent file with description frontmatter loads into `CustomAgent` with `Description` set and body free of frontmatter
- [x] run `go test ./pkg/config/...` - must pass before task 2

### Task 2: Implement {{agents:dynamic}} catalog expansion

- [x] write failing table-driven tests in `pkg/processor/prompts_test.go` for catalog expansion: no dynamic agents → `(no project-specific agents configured)`; one and several dynamic agents → catalog sorted by name with `- <name> — <description>` lines and invocation snippets; agents without description excluded; claude and codex executor snippet variants
- [x] add `agentsCatalogPattern` regexp matching `{{agents:dynamic}}` in `pkg/processor/prompts.go`
- [x] implement expansion that filters `CustomAgent` list to entries with non-empty `Description` and renders the catalog, reusing the existing per-executor invocation snippet builder used by `expandAgentReferences`
- [x] wire the new expansion into the same prompt-processing path that handles `{{agent:name}}`
- [x] run `go test ./pkg/processor/...` - must pass before task 3

### Task 3: Update review_first.txt with dynamic agent step

- [x] write failing test asserting embedded `pkg/config/defaults/prompts/review_first.txt` contains `{{agents:dynamic}}`
- [x] add Step 2b to `review_first.txt`: from the `{{agents:dynamic}}` catalog select agents relevant to the changed files and the nature of the diff (typically 0-3); launch selected agents in the SAME parallel message as the 5 base agents; skipping all is valid; do not embed diffs into agent prompts
- [x] update the header comment block in `review_first.txt` documenting the new variable (documented as `{{agents:<dynamic>}}` — comment lines are not stripped from prompts, so the literal placeholder in a comment would render the catalog twice)
- [x] verify prompt assembly produces no unexpanded `{{agents:dynamic}}` placeholder for both executors (extend existing prompt assembly tests)
- [x] run `go test ./pkg/config/... ./pkg/processor/...` - must pass before task 4

### Task 4: Add gen_agents.txt embedded prompt

- [x] write failing test asserting the embedded prompt `gen_agents.txt` exists, loads via the prompt loader with per-file fallback, and mentions the reserved base names
- [x] create `pkg/config/defaults/prompts/gen_agents.txt`: analyze stack, structure, CLAUDE.md, and commit history; propose 2-5 narrow agents covering problem classes the 5 base agents do not; write each to `.loopai/agents/<name>.txt` with YAML frontmatter containing a mandatory one-line `description`; never use the reserved names quality, implementation, testing, simplification, documentation; keep agent bodies focused review instructions in the style of embedded agents
- [x] expose the new prompt through the config prompt API (same pattern as `make_plan.txt`)
- [x] run `go test ./pkg/config/...` - must pass before task 5

### Task 5: Add --gen-agents standalone mode

- [ ] write failing tests for the new mode in `cmd/loopai`: flag parsing; mode runs a single executor session with the gen_agents prompt via a stubbed executor in `t.TempDir()`; created agent files are listed in output; a warning is emitted if a reserved base name file was written; missing executor produces the same error class as `--plan`
- [ ] add `--gen-agents` flag to CLI options in `cmd/loopai/main.go`, mutually exclusive with plan-file argument and other standalone modes
- [ ] implement the mode: resolve executor, run one session with the gen_agents prompt through `runWithSectionTiming`, log progress to `.loopai/progress/progress-gen-agents.txt`
- [ ] after the session: scan `.loopai/agents/`, print created/updated agent files with their descriptions, warn on reserved-name overwrites, remind user to review via `git diff` and commit
- [ ] run `go test ./cmd/... ./pkg/...` - must pass before task 6

### Task 6: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: description-marked agents form the catalog, model-side selection instructions in review_first, base 5 unchanged, `--gen-agents` writes reviewable files
- [ ] verify edge cases: no dynamic agents (behavior identical to today), agent with empty description excluded, malformed frontmatter falls back to body-as-prompt, invalid model in dynamic agent still warns via `Options.Validate`
- [ ] run full test suite via `make test`
- [ ] run `make lint` - all issues must be fixed
- [ ] verify test coverage of new code paths meets project standard

### Task 7: Update documentation

- [ ] update `CLAUDE.md`: dynamic agent concept, `description` frontmatter, `{{agents:dynamic}}`, `--gen-agents` mode
- [ ] update `llms.txt`: new flag and agent format notes
- [ ] update `README.md` user documentation for project-specific review agents and `--gen-agents`
- [ ] update embedded config/prompt comments where behavior is described

## Technical Details

- `Options` gains `Description string` (yaml `description`); `CustomAgent` inherits it via the embedded struct, so the loader needs no changes
- Dynamic agent definition: `CustomAgent` with non-empty `Description`; the 5 embedded agents have none and stay base
- Catalog rendering (order: sorted by agent name):

  ```text
  ### Available project-specific agents

  - <name> — <description>
    <executor-appropriate invocation snippet, identical to {{agent:<name>}} expansion>
  ```

  Empty catalog renders exactly `(no project-specific agents configured)`
- Selection is model-owned by design decision: no path/glob triggers in code; the prompt bounds selection to 0-3 agents
- `--gen-agents` is a pre-pipeline standalone mode like `--plan`: config and executor resolution happen, notifications are not required; progress file name `progress-gen-agents.txt`
- Reserved names guard is prompt-level plus post-run code check (warning only, not an error, since the user reviews via git)
- No new dependencies; no changes to `pkg/status` signals (`<<<RALPHEX:...>>>` untouched)

## Post-Completion

**Manual verification**:

- run `loopai --gen-agents` on this repository, review generated agents in `.loopai/agents/`, commit the useful ones
- run a full `loopai` cycle on a toy repo (`make e2e-prep`) with one dynamic agent configured and confirm the review phase launches it alongside the base 5
- run the same cycle with `--codex` to confirm codex-side spawn_agent snippets work

**External system updates**: none — feature is fully local to loopai
