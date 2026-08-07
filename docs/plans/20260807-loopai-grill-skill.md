# loopai-grill Skill: Plan Critique and Multi-Model Plan-Off

## Overview
- Add a sixth skill `loopai-grill` to the Claude Code plugin with two modes:
  - **Grill mode** (default, `/loopai:loopai-grill [docs/plans/X.md]`): adversarial critique of an existing plan by parallel Claude subagent lenses plus an independent Codex critique via `codex exec`; verified findings are presented for selection and applied to the plan file.
  - **Plan-off mode** (`/loopai:loopai-grill compare <description or plan file>`): Claude and Codex independently produce competing loopai-format plans from the same requirements, each model then judges BOTH plans against fixed criteria, and a synthesis step merges the winner with the loser's best ideas into the final `docs/plans/*.md`.
- Problem it solves: a single-model plan inherits that model's blind spots. Cross-model critique and comparison surface missing tasks, hidden dependencies, and over-engineering before an expensive autonomous run burns hours on a flawed plan.
- Integrates as a pure plugin asset: no Go changes, distributed via the existing marketplace (`claude plugin update loopai@loopai`).

## Status
- Implementation and automated validation are complete.
- Code-review fixes are applied; the plan remains active until the review loop confirms a clean iteration.
- Post-Completion manual checks remain pending and do not block implementation completion.

## Context (from discovery)
- Files/components involved:
  - `assets/claude/skills/loopai-grill/SKILL.md` — new skill source (new file)
  - `assets/claude/skills/loopai-grill/scripts/plan_paths.py` — deterministic active-plan validation, collision checks, and atomic no-clobber final writes
  - `assets/claude/skills/loopai-grill/scripts/snapshot_repository.py` — descriptor-anchored Git-visible snapshot creation without symlink or hard-link traversal
  - `assets/claude/skills/loopai-grill/scripts/run-codex.sh` — isolated Codex invocation with separately captured stdout and stderr
  - `assets/claude/loopai-grill.md` — new top-level symlink to `./skills/loopai-grill/SKILL.md` (relative link, like the existing five)
  - `scripts/check-symlinks.sh:9` — `expected_skills` list must gain `loopai-grill`
  - `scripts/check-plugin.sh` — validates manifests; check whether it pins a skill count or list
  - `scripts/check-grill-skill_test.sh` and `Makefile` — focused metadata/workflow-contract checks wired into `make test`
  - `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` — initial version bump 0.1.3 → 0.2.0 for the new skill, then patch bumps for review hardening (currently 0.2.5)
  - `README.md` ("The plugin provides five skills" list and standalone full-directory installation), `llms.txt`, `CLAUDE.md` (skill inventory notes)
- Related patterns found:
  - Existing SKILL.md frontmatter styles include optional `allowed-tools`, but this safety-sensitive skill deliberately pre-approves no tools; check-symlinks validates frontmatter shape
  - `loopai-plan` SKILL.md carries the canonical plan template the grill modes must reference for format-consistent output
  - `loopai-brainstorm` shows the hand-off pattern between skills of this plugin
- Dependencies identified: Python 3 for deterministic plan-path handling and the `codex` CLI for the cross-model half; the skill must degrade gracefully when codex is absent (Claude-only grill with a warning; plan-off refuses with a clear message)

## Development Approach
- **Testing approach**: Regular (code first, then tests in the same task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for changes in that task
  - this plan's code is shell/assets: tests are the retained shell suites (`scripts/check-symlinks_test.sh`, `scripts/check-plugin_test.sh`), the focused `scripts/check-grill-skill_test.sh` contract suite, and `make test` asset checks
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility: existing five skills unchanged

## Testing Strategy
- **Unit tests**: shell suites for symlink and plugin manifest validation plus fixture-driven loopai-grill routing, path-safety, Codex isolation/degradation, judging, and no-clobber output-contract checks (they run inside `make test`)
- **E2E tests**: none (no dashboard changes); manual skill invocation checks go to Post-Completion

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Implementation Steps

### Task 1: Author the loopai-grill skill
- [x] create `assets/claude/skills/loopai-grill/SKILL.md` with frontmatter (`name: loopai-grill`, quoted `description` covering triggers: "grill plan", "прогрилить план", "compare plans", "plan-off"; `argument-hint: 'plan file or: compare <description>'`; no `allowed-tools`, so normal permission checks remain in force)
- [x] write **mode routing**: no args → pick newest `docs/plans/*.md` (excluding `completed/`) and confirm; a plan path → grill mode; leading `compare` → plan-off mode
- [x] write **grill mode** instructions: launch parallel critics — three Claude subagent lenses (feasibility against the actual codebase, YAGNI/scope, testability/task-granularity) via the Agent tool, plus one Codex critique via `codex exec` fed the plan content and a "find missing tasks, hidden dependencies, underestimated work, refute weak tasks" prompt; dedupe findings, drop unverified ones after a quick code check, present survivors via AskUserQuestion (multiSelect) and apply accepted ones to the plan file with Edit
- [x] write **plan-off mode** instructions: extract requirements (from the description or an existing plan's Overview); generate plan A with Claude following the `loopai-plan` template; generate plan B via `codex exec` given the same requirements AND the same template (read from the installed skill or embedded in the prompt); cross-judge — Claude scores both, `codex exec` scores both, against fixed criteria (completeness, risk coverage, minimalism, testability), scores summarized in a table; synthesize the final plan from the winner plus the loser's best ideas; write a single final `docs/plans/YYYYMMDD-<name>.md`, discard scratch drafts (scratch files live outside `docs/plans/`, e.g. a temp dir, so loopai never discovers them)
- [x] write **degradation rules**: `codex` binary absent → grill mode continues Claude-only with an explicit warning; plan-off mode stops with an error naming the missing binary; `codex exec` failures reported, never silently swallowed
- [x] add the top-level relative symlink `assets/claude/loopai-grill.md -> ./skills/loopai-grill/SKILL.md`
- [x] run `bash scripts/check-symlinks.sh` — expected to fail on inventory (fixed in Task 2); confirm no other failure mode

### Task 2: Register the skill in validation and manifests
- [x] add `loopai-grill` to `expected_skills` in `scripts/check-symlinks.sh`
- [x] inspect `scripts/check-plugin.sh` and `scripts/check-plugin_test.sh` for any skill-count or skill-list assertions; update if present
- [x] bump `version` to `0.2.0` in `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` (keep both in sync — check-plugin verifies)
- [x] update `scripts/check-symlinks_test.sh` fixtures/expectations for the six-skill inventory
- [x] run `bash scripts/check-symlinks_test.sh` and `bash scripts/check-plugin_test.sh` - must pass
- [x] run `make test` (asset checks + full suite) - must pass before next task

### Task 3: Verify acceptance criteria
- [x] add `scripts/check-grill-skill_test.sh`, wire `make test-grill-skill` into `make test`, and cover routing, real path/symlink fixtures, Codex isolation and degradation, shared-template judging, and no-clobber output contracts
- [x] verify all requirements from Overview are implemented (two modes, mode routing, codex degradation, format-consistent output)
- [x] verify edge cases: no plans in docs/plans/, plan path with wrong case, `compare` with no description, codex absent
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] verify the skill file passes the frontmatter validation in check-symlinks (quoted description, no unquoted `: ` or ` #`)

### Task 4: [Final] Update documentation
- [x] update README.md plugin section: "five skills" → "six skills" with the `loopai:loopai-grill` bullet describing both modes
- [x] update standalone installation to copy complete skill directories so `loopai-grill` retains its bundled helpers
- [x] update CLAUDE.md skill/symlink inventory notes if they enumerate skills
- [x] update llms.txt plugin/skill enumeration if present

## Technical Details
- Codex invocation goes through the bundled `run-codex.sh`, which calls `codex exec` non-interactively from a fresh temporary working directory containing only tracked and non-ignored untracked single-link regular files, copied through descriptor-anchored no-follow reads and excluding `.git/` and `.loopai/`. A restricted permission profile confines reads to that directory and minimal runtime files. This prevents repository Codex configuration, link aliases, and ignored private artifacts from loading; user configuration, rules, MCP servers, skills, apps, hooks, plugins, and external tools are also disabled; credential-like shell variables are removed; approval escalation is denied; and the session is ephemeral. It never uses `loopai --codex --plan`: loopai's plan creation is interactive and writes files, while the grill needs captive text output.
- Plan-off prompts must pin the output format by embedding the loopai plan template (Task N headings, checkbox rules, tests-per-task requirements) so both candidate plans are structurally comparable and the final file is executable by loopai.
- Cross-judging is deliberately symmetric (each model scores both plans) to cancel self-preference bias; criteria are fixed in the skill text so runs are comparable.
- The skill never edits plans under `docs/plans/completed/` and never touches `.loopai/`.
- All plan reads and writes go through the bundled path helper. Active plans are read into an outside-repository snapshot and replaced only when an identity-bound content token confirms the reviewed single-link file is unchanged; publication displaces the reviewed version to a private recovery path and installs the edit with a no-clobber link so a concurrent writer is never overwritten. Plan-off scratch is validated outside the repository. Final plan output is fully written and synced under a hidden name while a shared publication lock serializes alias collision rechecks and atomic no-clobber publication.
- Symlink must be relative (`./skills/...`) like the existing five; `make check-symlinks` rejects broken or absolute links.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Reinstall/update the plugin (`claude plugin marketplace update loopai && claude plugin update loopai@loopai`, restart Claude Code), then:
  - `/loopai:loopai-grill` on a real plan → findings appear, selected ones edit the file
  - `/loopai:loopai-grill compare "small real feature"` → two candidate plans, score table, one synthesized plan lands in docs/plans/
  - temporarily rename `codex` from PATH → grill degrades with warning, plan-off refuses cleanly

**External system updates**: none — plugin is distributed from this repository
