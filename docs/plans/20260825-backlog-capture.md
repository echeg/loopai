# Backlog Capture for Side Findings + Plan Decision Log

## Overview

Two related additions that stop losing knowledge produced as a byproduct of loopai runs:

1. **Backlog capture.** Agents regularly notice real problems that are outside the current plan's scope — during task execution, internal review, external-review evaluation, and plan creation. Today those findings are either fixed as scope creep or dismissed and lost. This plan adds a repository-committed backlog directory (`docs/backlog/` by default, one markdown file per finding) and prompt instructions telling the primary executor to file out-of-scope findings there instead of fixing or dropping them. Backlog entries are plain markdown, so `loopai-adopt` converts them into full plans with zero new consumption code.
2. **Decision Log in plans.** When a plan is revised after critique (`loopai-grill`, plan-creation review), accepted and rejected findings are recorded in a `## Decision Log` section inside the plan itself, so the next critique round does not re-raise already-rejected items. (For code-review iterations this anti-re-raise mechanism already exists via `{{PREVIOUS_REVIEW_CONTEXT}}` and the progress log; the Decision Log covers plan revisions only.)

Key benefits: out-of-scope findings survive the run, live in git history, are shared with the team, and become seeds for future plans; repeated plan-critique rounds converge instead of relitigating rejected suggestions.

## Decisions

- **Context**: side findings from agents need a durable home; plan critiques need memory of rejected findings. Design was brainstormed and approved interactively on 2026-08-25 (inspired by promptadvisers/claudex persona/changelog ideas).
- **Chosen approach**: prompt-convention capture written by the primary executor, one committed markdown file per finding under a configurable `backlog_dir` exposed to prompts as `{{BACKLOG_DIR}}`; `## Decision Log` section convention for plan revisions.
- **Rejected alternatives**:
  - protocol signal `<<<RALPHEX:BACKLOG ...>>>` parsed by Go — works even from read-only reviewers, but needs Go parsing/writing/commit plumbing; kept as a future fallback if the evaluator funnel proves lossy. Do NOT implement it in this plan.
  - single `BACKLOG.md` append file — merge conflicts between parallel worktree branches, whole-file reads for dedup.
  - local-only `.loopai/` storage — lost on worktree removal, not shared, not committable.
- **Verified facts**:
  - external reviewers are read-only (codex pinned to `--sandbox read-only`, claude reviewer without write tools); every external finding already flows through the primary evaluator, which CAN write. The evaluator is therefore the capture funnel for external findings.
  - `{{PLANS_DIR}}` placeholder already exists in `pkg/processor/prompts.go` (expanded in the builders around lines 50, 103, 442); `{{BACKLOG_DIR}}` follows the identical pattern.
  - `.loopai/` is gitignored and worktrees are removed after runs — backlog files must live under a committed path inside the worktree so they ride the normal commit flow and arrive via `--merge`.
  - plan parser only reacts to `### Task N:`/`### Iteration N:` headings and checkboxes; a `## Decision Log` section without checkboxes is inert.
  - Claude plugin manifests are currently at version 0.2.9 in both `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`; any skill change requires bumping both to the same value.

## Context (from discovery)

- Files/components involved:
  - `pkg/config/config.go` — add `BacklogDir` (`backlog_dir`) alongside `PlansDir` (~line 161) and its default wiring (~line 403)
  - `pkg/config/defaults/config` — commented `backlog_dir` entry next to `plans_dir` (~line 279); `--init` picks it up automatically
  - `pkg/processor/prompts.go` — `{{BACKLOG_DIR}}` expansion wherever `{{PLANS_DIR}}` is expanded; update the placeholder list comments
  - `pkg/config/defaults/prompts/` — `task.txt`, `review_first.txt`, `review_second.txt`, `codex.txt`, `external_claude_eval.txt`, `custom_eval.txt`, `make_plan.txt` (header comments list available variables — keep them in sync)
  - `assets/claude/skills/loopai-plan/SKILL.md`, `assets/claude/skills/loopai-brainstorm/SKILL.md` — backlog capture note during discovery; Decision Log convention
  - `assets/claude/skills/loopai-grill/SKILL.md` (+ its bundled prompts/scripts if they carry critique wording) — write Decision Log on apply, critics read it and do not re-raise rejected items
  - `scripts/check-grill-skill_test.sh` — grill contract regression suite must reflect the Decision Log behavior
  - `.claude-plugin/plugin.json`, `.claude-plugin/marketplace.json` — version bump 0.2.9 → 0.3.0 (keep equal)
  - `README.md`, `CLAUDE.md` — document `backlog_dir`, the backlog convention, and the Decision Log convention
- Related patterns found:
  - `{{PLANS_DIR}}` is the exact precedent for a config-backed prompt placeholder
  - embedded prompt files begin with a comment header listing available variables
  - grill is safety-sensitive: its contracts are enforced by `scripts/check-grill-skill_test.sh` and described in CLAUDE.md — update both together
- Dependencies identified: none new

## Development Approach

- **Testing approach**: Regular (code first, tests in the same task)
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
- Maintain backward compatibility: with `backlog_dir` unset, prompts render the default `docs/backlog` path; no other behavior changes
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real `~/.config/loopai/` or legacy `~/.config/ralphex/`
- Keep comments lowercase except exported godoc; table-driven tests with testify
- The backlog directory has no Go consumer in this plan: loopai never reads, validates, or creates it — the path is only text substituted into prompts. Do not add containment/creation logic.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: dashboard Playwright suite unaffected (no web changes); do not add e2e tests
- Skill and manifest changes are covered by the existing shell suites: `make test-grill-skill`, `make check-plugin`, `make check-symlinks` (all run inside `make test`)

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

### Task 1: Add `backlog_dir` config key

- [x] add `BacklogDir string \`json:"backlog_dir"\`` to the config struct in `pkg/config/config.go`, default `docs/backlog`, wired exactly like `PlansDir` (embedded default, global/local override, empty value falls back the same way `plans_dir` does)
- [x] add commented `backlog_dir` entry with a short description to `pkg/config/defaults/config` next to `plans_dir`
- [x] write table-driven tests in `pkg/config/config_test.go`: default value, local override, global override, empty/unset behavior matching `plans_dir` semantics
- [x] run `go test ./pkg/config/...` - must pass before task 2
- ➕ extracted `Values.mergePathsFrom` from `mergeExtraFrom` in `pkg/config/values.go`: the new key pushed `mergeExtraFrom` to gocyclo 21 (limit 20)

### Task 2: Expand `{{BACKLOG_DIR}}` placeholder in prompts

- [x] in `pkg/processor/prompts.go`, expand `{{BACKLOG_DIR}}` from the configured `BacklogDir` in every builder that expands `{{PLANS_DIR}}` (task, review, evaluation, planning paths)
- [x] update the "supported placeholders" comments in `prompts.go` and the variable-list header comments in every embedded prompt file that gains the placeholder
- [x] write tests in `pkg/processor/prompts_test.go`: placeholder replaced with configured value, default value when unset, no literal `{{BACKLOG_DIR}}` left in any built prompt
- [x] run `go test ./pkg/processor/...` - must pass before task 3
- ➕ expansion added once in `replaceBaseVariables`, the choke point every builder funnels through, so all twelve prompt paths (task, both internal reviews, three external reviews, three evaluations, plan, finalize, gen-agents) are covered by one line plus `getBacklogDir`, mirroring `getPlansDir`
- ⚠️ no embedded prompt file gains the placeholder in this task, so no variable-list header comment changed: the headers document the variables a prompt actually uses (`task.txt` omits `{{PLANS_DIR}}` though it is expanded there). Task 3 adds the usage and its header lines together.

### Task 3: Backlog capture instructions in embedded prompts

- [x] `task.txt`: add a short paragraph — a real issue outside the current plan's scope must NOT be fixed; file it as `{{BACKLOG_DIR}}/<kebab-slug>.md` using the entry format (see Technical Details), checking existing filenames first to avoid duplicates
- [x] `review_first.txt` and `review_second.txt`: in the consolidation step, findings that are real but out of plan scope go to the backlog instead of being fixed
- [x] `codex.txt`, `external_claude_eval.txt`, `custom_eval.txt`: add a third verdict category — "valid but out of scope → file to backlog and say so in the response passed back to the reviewer" (alongside the existing fix/dismiss categories)
- [x] `make_plan.txt`: adjacent problems discovered while researching the plan go to the backlog; also add the `## Decision Log` convention for plan revisions (see Task 4)
- [x] keep every instruction to 3-5 lines; reuse the same wording across prompts
- [x] update or add prompt-content assertions if the test suite checks embedded prompt text; otherwise verify via `go test ./pkg/config/... ./pkg/processor/...`
- [x] run `make test` - must pass before task 4
- ➕ every capture block ends with an explicit commit instruction (`docs: add backlog entry`, or alongside the fixes): `.loopai/` worktrees are removed after a run, so an entry left untracked on a path that commits nothing else would be lost
- ➕ each capture block states that filing is not a fix, so it does not flip the REVIEW_DONE / EXTERNAL_REVIEW_DONE signal logic; out-of-scope findings behave like dismissals and the existing stalemate detection still terminates the loop
- ➕ added `TestPromptBuilder_BacklogCaptureInstructions` in `pkg/processor/prompts_test.go`: the seven capture-path prompts carry the expanded directory, and the three read-only external *review* prompts deliberately do not
- ⚠️ the `## Decision Log` convention now also lives in `make_plan.txt` (template section plus the draft-revision path), which pre-satisfies the second checkbox of Task 4

### Task 4: Decision Log convention in plan-related prompts and skills

- [x] define the convention (see Technical Details): `## Decision Log` section in the plan, entries `accepted:`/`rejected:` with one-line reasoning, NO checkboxes ever in this section
- [x] `make_plan.txt`: when revising a plan after critique, record accepted and rejected findings in `## Decision Log`
- [x] `assets/claude/skills/loopai-plan/SKILL.md`: plan template gains the optional `## Decision Log` section note; discovery step gains the backlog-capture note
- [x] `assets/claude/skills/loopai-brainstorm/SKILL.md`: same backlog-capture note for issues discovered during design exploration
- [x] run `make check-symlinks` - must pass before task 5
- ➕ added `TestPromptBuilder_DecisionLogConvention` in `pkg/processor/prompts_test.go`: the planning prompt defines the section, records accepted/rejected points, forbids checkboxes, and the template block itself carries none - the plan parser reacts to checkboxes, so one leaking in would become an implementation step
- ➕ `loopai-plan` skill: `Decisions` and `Decision Log` added to the checkbox-placement denylist alongside Success criteria/Overview/Context
- ➕ `loopai-brainstorm` skill: hand-off step also asks `loopai-plan` to record accepted/rejected points from the design dialogue, not only the `## Decisions` context
- ⚠️ the skills are Claude Code skills, not loopai prompts, so `{{BACKLOG_DIR}}` is not expanded there; both name the literal `docs/backlog/` default and point at the `backlog_dir` config key for overrides
- ⚠️ the first two checkboxes were pre-satisfied by Task 3, which landed the convention in `make_plan.txt` (template section at line 136 plus the draft-revision path); this task verified them and added the regression test that was missing

### Task 5: loopai-grill integration

- [x] grill apply step: after applying user-selected findings, add or update `## Decision Log` in the plan — accepted findings with what changed, rejected findings with the user's reason; preserve existing entries
- [x] grill critique prompts (Claude and Codex wrapper prompts): instruct critics to read `## Decision Log` and not re-raise rejected items unless new evidence contradicts the recorded reasoning
- [x] verify the change does not touch grill safety contracts (path helper, snapshot rules, sandbox pins) — behavior additions are prompt/workflow text only
- [x] update `scripts/check-grill-skill_test.sh` to cover the Decision Log contract (present after apply, respected by critique prompts)
- [x] bump version to `0.3.0` in both `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`
- [x] run `make test-grill-skill` and `make check-plugin` - must pass before task 6
- ➕ the apply sequence gained a dedicated numbered step (former steps 6-7 renumbered to 7-8), so the log is written to the temporary draft before `replace-active` publishes it: the skill never edits the plan at its repository path, so recording the round after publication would need a second guarded replacement
- ➕ the re-read verification step now also confirms the `## Decision Log` carries no checkbox, and the suite asserts both that prohibition and the entry formats — a checkbox leaking into the log would be parsed as an implementation step
- ⚠️ the Decision Log is written only on the apply path, which runs when the user selects at least one finding. A round where nothing survives verification, or where the user selects nothing, still leaves the plan untouched as before, so its rejections are not recorded; changing that would mean writing to a plan the user asked to leave unchanged and is out of this plan's scope
- ⚠️ no grill safety contract changed: only `SKILL.md` prose and the assertion suite were touched — `plan_paths.py`, `snapshot_repository.py`, `run-claude.sh`, and `run-codex.sh` are byte-identical

### Task 6: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented (backlog capture reachable from task, internal review, evaluation, and planning paths; Decision Log written by grill and plan revision)
- [x] verify edge cases: unset `backlog_dir` renders default; custom `backlog_dir` renders everywhere; no literal `{{BACKLOG_DIR}}` leaks into any built prompt; `## Decision Log` never carries checkboxes
- [x] run full test suite: `make test`
- [x] run `make lint` - all issues must be fixed
- [x] cross-compile check not needed (no platform-sensitive code); confirm no test touched real user config directories
- ➕ evidence: `make test` exit 0, 15/15 Go packages ok (processor 91.6%, config 90.4%), 0 FAIL lines, wrapper suite 71/71; `make lint` exit 0 with `0 issues`
- ➕ evidence: capture text reaches all four paths — `task.txt` (task), `review_first.txt`/`review_second.txt` (internal review), `codex.txt`/`external_claude_eval.txt`/`custom_eval.txt` (evaluation), `make_plan.txt` (planning); grill's Decision Log contract is asserted by `scripts/check-grill-skill_test.sh` inside `make test`
- ➕ evidence: `TestPromptBuilder_BacklogDirPlaceholder` covers configured-value and unset-default across all twelve prompt paths, `TestPromptBuilder_BacklogDirNoLiteralLeak` proves no raw placeholder survives, and a section-bounded scan of `make_plan.txt` plus the three skills found no checkbox under any `Decision Log` heading
- ➕ real-config safety verified by hashing `~/.config/loopai/` and `~/.config/ralphex/` before and after the suite: byte-identical (40 tracked files), so no test wrote to either

### Task 7: [Final] Update documentation

- [x] `README.md`: document `backlog_dir`, the backlog entry format, the capture behavior, and the `## Decision Log` convention
- [x] `CLAUDE.md`: add `backlog_dir` to the configuration section; extend the grill contract paragraph with the Decision Log behavior; mention the `{{BACKLOG_DIR}}` placeholder next to the existing prompt-placeholder documentation
- [x] `llms.txt`: update if it enumerates config keys or prompt placeholders
- ➕ README gained a `## Backlog capture` section and a `### Decision Log` subsection under `## Plan format`, plus a `{{PLANS_DIR}}`/`{{BACKLOG_DIR}}` note in `## Configuration`: the config key alone does not explain that capture is a prompt convention with no Go consumer, nor why the directory must be a committed path rather than `.loopai/`
- ➕ llms.txt does enumerate both (plan format and the config/data section), so it gained a compact Decision Log paragraph and a `plans_dir`/`backlog_dir` placeholder paragraph
- ⚠️ CLAUDE.md's grill paragraph also records the two limits Task 5 discovered: the log is written to the draft before `replace-active` publishes it, and a round where the user selects nothing leaves the plan and its rejections unrecorded
- ➕ evidence: `make test` exit 0 (15/15 Go packages ok, wrapper suite 71/71), `make lint` exit 0 with `0 issues`

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details

**Backlog entry format** (plain markdown, no YAML frontmatter — `loopai-adopt` consumes free-form markdown):

```markdown
# <short problem title>

- found: <YYYY-MM-DD>, plan: <plan-name>, phase: <task|internal review|evaluation|planning>
- severity: minor|major
- area: <primary file or package>

<description with file:line references, why it is a problem, suggested fix direction>
```

**Capture instruction skeleton** (adapted per prompt, keep wording consistent):

```
Found a real issue outside this plan's scope? Do NOT fix it here.
File it instead: create {{BACKLOG_DIR}}/<kebab-slug>.md (see format in <ref>).
First list existing files in {{BACKLOG_DIR}}/ — if a similar entry exists,
update it instead of creating a duplicate.
```

**Decision Log convention**:

```markdown
## Decision Log

- 2026-08-25 grill: **accepted** — split lock acquisition into prepare/commit (tasks 3-4 updated)
- 2026-08-25 grill: **rejected** — "add retry around merge" — merge is already the backstop; retry masks real conflicts
```

- section lives in the plan file, never contains checkboxes (plan parser must stay inert)
- critics read it and skip re-raising rejected items absent new evidence

**Processing flow**: external reviewer (read-only) reports finding → primary evaluator classifies: valid-in-scope → fix; invalid → dismiss with reasoning; valid-out-of-scope → write backlog entry inside the worktree → entry is committed by the normal task/finalize commit flow → arrives in master via `--merge` → later converted to a plan with `loopai-adopt`.

## Post-Completion

**Manual verification**:
- run a real plan end-to-end and confirm out-of-scope findings appear as `docs/backlog/*.md` files on the feature branch
- run `loopai-grill` on an active plan twice and confirm the second round does not re-raise findings rejected in the first
- run `loopai-adopt docs/backlog/<entry>.md` and confirm a well-formed plan is produced

**External system updates**:
- installed plugin copies receive the grill/plan skill changes via the marketplace version bump; standalone installs must re-copy `assets/claude/skills/`
