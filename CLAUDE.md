# loopai

Autonomous plan execution with Claude Code or Codex. See `llms.txt` for compact usage guidance and `README.md` for user documentation.

## Fork policy

The public CLI, configuration directories, progress paths, notifications, and other user-visible strings use `loopai`.

Three upstream-compatible internals intentionally retain the old name:

- `module github.com/umputun/ralphex` in `go.mod`
- Go import paths and internal package identifiers derived from that module
- protocol signals matching `<<<RALPHEX:...>>>`

Do not rename these. Keeping the module path avoids import-line conflicts during upstream merges. Keeping the signals preserves compatibility with embedded/custom prompts and historical progress logs. These are deliberate boundaries, not rebrand omissions.

The fork does not contain upstream packaging/release infrastructure or the upstream documentation website. Distribution is by source build and the repository's Claude Code plugin marketplace.

## Build commands

```bash
make build      # build .bin/loopai and the .bin/loopai-acp launcher (on Windows the launcher is .bin/loopai-acp.exe only, plus .bin/loopai.exe)
make test       # asset checks, race-enabled unit tests with coverage, provider-wrapper suites
make check-symlinks # validate the ten Claude skill assets and links
make test-symlinks  # regression tests for Claude skill asset validation
make check-codex-skills # validate the Codex skill tree against the Claude inventory
make test-codex-skills  # regression tests for Codex skill validation and installation
make install-codex-skills # copy the Codex skills into ${CODEX_HOME:-~/.codex}/skills
make check-plugin   # validate Claude plugin and marketplace manifests
make test-plugin    # regression tests for manifest validation
make test-grill-skill # validate loopai-grill metadata and workflow contracts
make test-report-docs # check skill inventories and report contracts in documentation
make test-launch-history-skills # launch-history snippet and contract in the six launch skills
make test-skill-contract # execute the shared skill contract snippet against stub binaries
make install    # build, install binaries, update the Claude plugin and Codex skills, verify the contract
make test-install # regression tests for scripts/install.sh against stub tools in a temp HOME
make test-wrappers # all retained provider-wrapper and wrapper-doc suites
make lint       # golangci-lint
make fmt        # gofmt and goimports
make race       # focused race run
make e2e        # dashboard browser tests
```

Go 1.26 or newer is required.

Dependencies behind the `e2e` build tag must be updated explicitly:

```bash
go get -u ./...
go get -u -tags=e2e github.com/playwright-community/playwright-go
go mod tidy
go mod vendor
```

## Project structure

```text
cmd/loopai/          main package, CLI parsing, startup wiring
cmd/loopai-acp/      argument-free launcher of the T3 Code loopai provider: execs `loopai --acp`
internal/validation/ shared validation-command matching without package cycles
pkg/acp/             JSON-RPC stdio transport, ACP v1 provider protocol, prompt parser, update encoders
pkg/awake/           best-effort keep-awake sleep inhibitor renewed by run activity
pkg/cmux/            best-effort cmux status integration
pkg/config/          configuration loading and embedded defaults
pkg/executor/        Claude-compatible and Codex process execution
pkg/git/             Git CLI operations and worktree management
pkg/input/           interactive input, fzf fallback, draft review
pkg/notify/          Telegram, email, Slack, webhook, custom notifications
pkg/orca/            best-effort OSC terminal-title status integration
pkg/plan/            plan discovery, parsing, and mutation
pkg/processor/       pipeline coordinator, prompts, executor policy
pkg/processor/phase/ task, review, external, finalize, report, planning, and agent-generation phases
pkg/progress/        timestamped progress logging
pkg/runs/            detached-run registry, locks, event journal, and journal observer
pkg/status/          shared phases, sections, and protocol signals
pkg/t3/              T3 Code orchestration-protocol-2 and thread MCP clients; pkg/t3/t3test fake server
pkg/web/             dashboard, SSE, templates, static assets
e2e/                 Playwright dashboard tests
scripts/             provider wrappers and internal test helpers
scripts/copilot-as-claude/ # GitHub Copilot CLI wrapper for Claude-compatible output
scripts/pi-as-claude/ # pi wrapper for Claude-compatible output
assets/claude/skills/ canonical Claude Code plugin skill sources
assets/claude/loopai*.md legacy standalone-command compatibility symlinks
assets/codex/skills/  hand-written Codex CLI skill sources
.claude-plugin/      Claude Code plugin and marketplace manifests
plugins/<name>/      further marketplace plugins (codex-imagegen), each with its own manifest
docs/                focused operational documentation and plans
```

The top-level `assets/claude/loopai*.md` files are symlinks to the matching
`assets/claude/skills/loopai*/SKILL.md` sources. Keep the command name,
directory name, and link target aligned; `make check-symlinks` rejects broken,
missing, incorrect, and orphan links, requires skill descriptions, and verifies
the exact skill inventory. It and `check-codex-skills.sh` also reject a skill body
line naming a removed loopai spelling (`--codex`, `--codex-only`, the
`--external-review-*` flags, `--t3`, `--no-t3`, the `executor`, `codex_model`, `codex_reasoning_effort`,
`external_review_*`, and `finalize_enabled` keys, and the T3 Code spellings that
`--t3-launch`, `--t3-progress-thread`, and `acp_auto_merge` were removed with) unless that line says
it "was removed" or "were removed"; `--codex-args` and `--t3-thread` do not match, and the two
`removed_spellings` lists must stay identical. The current set is `loopai`, `loopai-merge`,
`loopai-plan`, `loopai-brainstorm`, `loopai-adopt`, `loopai-update`,
`loopai-grill`, `loopai-orca`, `loopai-t3`, and `loopai-retro`; every added skill needs the matching
top-level symlink.

`loopai-retro` is a read-only retrospective over bounded progress-log excerpts, completion
reports, backlog, steering files, and check definitions, invoked manually as
`/loopai:loopai-retro` or explicitly as `$loopai-retro`. It ranks environment improvements
with `path:line` evidence. User-selected `docs/backlog/` entries, committed by pathspec, are
its only writes; it never edits prompts, agents, config, steering files, plans, or reports.

The `loopai-t3` skill (Claude and Codex) starts one plan run that T3 Code shows and asks for the
mode every time. It reads `orchestrator_capabilities` (a null `parentThreadId` stops it) and
`currentThreadId` from `t3_thread_list`. Mode 1 is offered only when `runtimeMode` is `full-access`,
the environment check finds no sandbox the way `detectSandbox` does (a non-empty `CODEX_SANDBOX`,
`CODEX_SANDBOX_NETWORK_DISABLED=1`, or `SANDBOX_RUNTIME=1`; `check-loopai-t3-skill_test.sh` executes it),
the OS is not Windows, `parentThreadId` equals `currentThreadId`, and the id matches
`^[A-Za-z0-9_.:%-]{1,1024}$`. It mints the token inline through
`${T3CODE_CLI_PATH:-<T3 home>/bin/t3} auth session issue` with the four `orchestration:` and
`terminal:` read/operate scopes and `--ttl 3d`, never through a `t3` on `PATH` or `npx`, so the token
never reaches the transcript, and runs `loopai --detach --t3-thread` as one foreground Bash call with a
timeout of at least 180 s, above the launcher's 120 s ack wait. Mode 2 reads
`<T3 home>/userdata/settings.json` read-only for an enabled local `acpRegistry` instance whose
`commandPath` basename is `loopai-acp`, or `loopai` with `--acp` in `commandArgs`, never assuming the
instance id (T3 Code's default is `acpRegistry_local_acp`), requires it in `providers[]` with empty
`constraints`, and calls `t3_thread_launch` with the required `title`, `models[0].id` as the model,
the plan and flags as the message, and a `root` or `existing_worktree` workspace; after an error or a
lost reply it inspects `t3_thread_list` before retrying. It never edits `settings.json`, whose schema
errors make T3 Code silently reset its settings, and explains registration through the settings UI
instead. Both modes' report blocks are byte-identical across the two hosts, and the skill stops after
reporting. `loopai-plan` offers T3 Code only when the T3 Code tools are present and hands its flag
choice to `loopai-t3`, which then asks only the mode. `scripts/check-loopai-t3-skill_test.sh` pins
these contracts; the runtime side is described below and in `docs/t3-code.md`.
`loopai-orca` drives the Orca desktop app's `orca` CLI and executes nothing
itself: it creates an Orca-managed worktree cut from the current branch (Orca's
default base is the remote-tracking ref, so `--base-branch` is always passed and
Orca names the branch `<git-user>/<name>`), copies the plan and untracked
`.loopai/` overrides into that checkout because a worktree carries committed
files only, and launches `loopai --orca [flags] <plan>` without `--worktree` in an Orca
terminal tab, where the OSC titles from `pkg/orca` give the card its status. A bare
`orca worktree create` opens a fallback shell in the card's first tab, because `--agent`
accepts only Orca's built-in TUI agents; once loopai is confirmed running the skill closes
that tab through `orca terminal close --tab`, but only an idle shell with a null
`agentIdentity` and a prompt as its last screen line, since a configured default tab or a
setup-hook terminal looks like an extra tab too and is not the skill's to remove. It
forwards only `--task-model`, `--review-model`, and `--external-reviewers` from
its own arguments, requires a `claude`/`codex` prefix on the two model flags, and stops on any other token,
because the flags are spliced into a shell command string and `--worktree` or
`--serve` would break the Orca flow. `loopai-plan` ends by printing that
invocation when an `orca` binary is on `PATH`, deriving the flags from the
effective `task_model`, `review_model`, and `external_reviewers` config keys, and offers to launch it. Its launch
question (Claude only; Codex has no `loopai-grill`) also offers "Grill the plan first", which hands the plan to
`loopai-grill` and then asks again without that option; under the four-option cap, with Orca and T3 Code both
available, the grill option takes the place of "Not now" until the grill round has run. When adding or removing a
skill, update `expected_skills` in `scripts/check-symlinks.sh` and the valid
fixture inventory in `scripts/check-symlinks_test.sh`, then bump both manifest
versions.

The marketplace lists `loopai` first, at source `./`; every further plugin lives
in `plugins/<name>/` with its own `.claude-plugin/plugin.json`, a description,
and a version equal to its marketplace entry. `make check-plugin` enforces this,
so loopai's skill inventory checks never see those plugins. `codex-imagegen`
is the only one today: its skill drives `codex exec` through
`scripts/codex_image.py`, whose Python unit tests (`make test-codex-imagegen`,
part of `make test`) replay a fake Codex CLI and never call a model.

Standalone installation must copy the complete directories under
`assets/claude/skills/`, not dereference only the top-level Markdown symlinks;
`loopai-grill` depends on bundled scripts addressed through
`${CLAUDE_SKILL_DIR}` and requires Python 3 in a POSIX environment (Linux,
macOS, or Windows via WSL) for its path helper.

`assets/codex/skills/` is the Codex CLI counterpart, installed by
`scripts/install-codex-skills.sh` into `${CODEX_HOME:-$HOME/.codex}/skills`,
which is the only place Codex looks. Those skills are written by hand and are
deliberately not generated from `assets/claude/skills/`: the two hosts expose
different tools. Claude Code has `AskUserQuestion`, `Task` subagents, and named
file tools, which cannot be copied into Codex skills. Native Codex subagent tools are
runtime-dependent, so no Codex skill assumes they exist. loopai's executor also registers a reviewer agent
through `-c agents.reviewer.description=...` for its own Codex runs.
A mechanical conversion would produce a skill that stalls at its first
interactive step. `scripts/check-codex-skills.sh` is what keeps the split honest:
it derives the expected Codex inventory from the Claude inventory minus an
explicit `exempt_skills` list, requires a `name` matching the directory (the
Claude checker treats `name` as optional, Codex does not) plus
`agents/openai.yaml` carrying `display_name`, `short_description`, and
`default_prompt`, rejects any symlink under `assets/codex`, and greps every body
for Claude-only constructs so an unadapted copy-paste fails the build rather
than the user's first `$loopai-plan`. `loopai-grill` is the sole exemption: its
safety model is the read-only Claude tool pin plus scripts addressed through
`${CLAUDE_SKILL_DIR}`, and a half-ported grill is worse than an absent one.
Adding a Claude skill therefore fails `make test` until the Codex counterpart
exists or the name joins `exempt_skills`. The installer removes a pre-rename
`ralphex-*` skill only when its file set is still exactly `SKILL.md` and
optionally `agents/openai.yaml` — the shape the old hand-install produced —
because anything else means the user extended it, and there is no baseline to
diff against.

`skillContract` in `cmd/loopai/skill_contract.go` is a manually bumped integer naming the binary
surface the skills depend on, so a stale binary and newer skills (or the reverse) cannot disagree
silently — a binary built before the report gained Evidence and Merge danger once produced reports
the skills described differently, with no warning. `loopai --skill-contract` prints it alone on
stdout and exits 0 before config loading; it is a standalone command, and `versionBannerWriter`
discards the pre-parse banner when the flag appears before `--`, because stdout must carry only the
integer. An old binary answers `unknown flag` with exit 1, which the skills read as contract 0.
`--version` and the banner are deliberately unchanged. All 19 skills (10 Claude, 9 Codex) open with
one byte-identical fenced snippet whose only variable line is `expected=<N>`, followed by an
instruction to warn and continue — never stop — when `binary` differs from `skill`, and to fall back
to the skill's own missing-CLI handling for `binary=missing`; `loopai-update` reruns the snippet after
`make install` and reports that line in its summary instead. The canonical text lives only in `scripts/lib/skill-contract.sh`, which both
`check-symlinks.sh` and `check-codex-skills.sh` source: they read the constant from the Go file, fail
when it cannot be found, and reject a missing or modified snippet, one that is not the skill's first fenced block, and a stale `expected=`.
`scripts/check-skill-contract-snippet_test.sh` executes the snippet against stub binaries. The number
detects incompatibility only: an all-old but consistent install stays silent, and there is
deliberately no lag-behind-checkout detection. `TestSkillContract` fingerprints the surface —
visible long flags of `opts` with their sorted `choice:` values (hidden removed spellings excluded), keys of the embedded `config`
(commented `# key =` lines included), and the `##` headings of the embedded `report.txt` in file
order — against `cmd/loopai/testdata/skill-contract.golden`, whose first line is `contract: N`.
Protocol signals and the launch-history format stay out of it. When the surface changes, bump
`skillContract` and `expected=` in every skill if a skill depends on the change, then run
`go test ./cmd/loopai -run TestSkillContract -update`; `-update` without a bump is a deliberate,
diff-visible acceptance that no skill is affected. A bump is a skill change, so it bumps both
manifests too.

`make install` runs `scripts/install.sh`, which updates every channel from the checkout: build
(failure stops before anything is installed), `loopai` and `loopai-acp` into
`${LOOPAI_BIN_DIR:-$HOME/.local/bin}`, `claude plugin marketplace update loopai` plus
`claude plugin update` for each installed `@loopai` plugin, the Codex skills when
`${CODEX_HOME:-$HOME/.codex}` exists, then a contract check of the installed binary, a plugin
version check against `plugin.json` (from `claude plugin list --json`, `folderVersion` first because a
directory-sourced plugin is read live from the checkout), and a warning when `loopai` on `PATH`
resolves elsewhere. Only the build, the binary install, and a contract mismatch (an unreadable `skillContract` in the checkout included) exit non-zero.
Standalone `~/.claude/skills/loopai*` copies are reported with an `rm -rf` line and never deleted:
they are foreign directories with no baseline. `scripts/install_test.sh` runs it from a copied repo
tree with stub `make` and `claude` on a `PATH` of stubs plus `/usr/bin:/bin` — not the real `PATH`,
where an installed `claude` or `~/.local/bin/loopai` would leak into what the installer inspects.
`loopai-update` (both copies) starts with an installation step: it locates the checkout from
`$LOOPAI_SRC`, else the `directory` source of the `loopai` marketplace in
`~/.claude/plugins/known_marketplaces.json`, else asks; fetches, offers only `git pull --ff-only` and
only when upstream is ahead, runs `make -C <checkout> install`, and only then merges prompts.

`.claude-plugin/marketplace.json` exposes this repository as the `loopai`
marketplace, and `.claude-plugin/plugin.json` points Claude Code at the skill
directory. Whenever any skill changes, bump the version in both manifests so
installed copies receive the update. Keep the marketplace entry version equal
to the plugin version.

`loopai-grill` has two safety-sensitive routes. Grill mode critiques an active
plan and applies only user-selected verified findings; plan-off creates one new
plan and never edits its source. Both reject completed, symlinked, nested, and
`.loopai/` plans. The skill pre-approves no Claude tools. Its bundled path
helper rejects symlinked plan and `.loopai` roots, rejects hard-linked plans,
validates outside-repository scratch directories, identity-and-content-guards
active-plan replacements without overwriting concurrent writers, retains and
reports each displaced inode in Git-private non-stageable storage so late
pre-opened-descriptor writes remain recoverable, and performs
locked atomic no-clobber final creation. Plan and draft reads are capped at
8 MiB. Its Claude and Codex wrappers snapshot
only tracked and non-ignored untracked single-link regular files through
descriptor-anchored no-follow reads while excluding `.git/`, `.loopai/`,
recovery paths, and their case aliases, reject files over 64 MiB and snapshots
over 512 MiB, confine model reads to isolated temporary directories, and reject
in-worktree alternate Git directories. The Claude wrapper exposes only
read-only repository tools and disables user/project customizations; the Codex
wrapper requires strict-config and permission-profile support, disables
user/project config, rules, MCP and external tools, strips
credential-like shell variables, and starts an ephemeral session without
approval escalation. Candidate and judging scratch files live outside the
repository and are removed on every exit. Grill mode reports Codex failure
and degrades to Claude-only; plan-off requires Codex and fails closed. Grill
mode's apply path also records the round in the draft's `## Decision Log` —
accepted findings with what changed, findings the user declined with a stated
reason as rejected, and findings merely not selected as deferred — before
`replace-active` publishes it, since the skill never edits the plan at
its repository path and a post-publication write would need a second guarded
replacement. The section is created immediately before `## Implementation
Steps` when absent, existing entries are preserved, a finding already recorded
as deferred has that entry's date updated in place instead of a second
near-identical line appended, and it must never carry a checkbox. `ParsePlan` ignores a checkbox above `## Implementation Steps`,
since the H2 closes the current task, but `FileHasUncompletedCheckbox` — the
fallback used when a plan has no task headings — counts it, so such a plan
never reads as complete; the executor also reads the section as text and can
treat the line as outstanding work. Every critic and the verification pass are told
to read the log and not re-raise a recorded rejection absent contradicting
evidence, while a deferred entry may be raised again: `AskUserQuestion` with
`multiSelect` captures no reason, so recording an unexplained non-selection as
rejected would silently suppress the finding in every later round. The log is
written only when the user selects at least one finding;
a round where nothing is selected leaves the plan untouched, so its rejections
go unrecorded rather than loopai writing to a plan the user asked to leave
alone. When these contracts change, update and run `scripts/check-grill-skill_test.sh`.

## Configuration

- Global directory: `~/.config/loopai/`
- Local directory: `.loopai/`
- Global config: `~/.config/loopai/config`
- Local config: `.loopai/config`
- Global prompts/agents: `~/.config/loopai/{prompts,agents}/`
- Local prompts/agents: `.loopai/{prompts,agents}/`
- Progress: `.loopai/progress/`
- Worktrees: `.loopai/worktrees/`
- Detached runs: `.loopai/progress/runs/<run-id>/`
- Mode 2 thread bindings: `.loopai/acp/binding-*.json` (runtime path with its own `.gitignore`)
- Override: `--config-dir` or `LOOPAI_CONFIG_DIR`

Local files override global files, which override embedded defaults. Embedded defaults remain the per-file fallback, so deleting an installed prompt or agent does not disable it. Remove its template reference to disable an agent.

`backlog_dir` (default `docs/backlog`) names the directory where agents file
out-of-scope findings, one markdown file per entry. Capture remains agent-owned
through the `{{BACKLOG_DIR}}` placeholder. Completion reporting reads added entries
under this directory to collect titles; startup does not validate or create it. It is
deliberately a committed repository path and not `.loopai/`, which is gitignored and
whose worktrees are removed after a run, so an entry written there would be lost
before `--merge`. Capture is instructed on the four paths that can write: task,
internal review, external-review evaluation, and plan creation. The three external
*review* prompts deliberately omit it, since external reviewers are read-only and
their findings reach the backlog through the review-provider evaluator. Each path states its
own commit rule, and they are not interchangeable. Task and internal review commit the
entry in phase, and all three prompts stage it with `git add` first, since it is untracked and
no commit picks it up on its own. No capture path sweeps with `git add -A`, `task.txt` included:
`modeRequiresBranch` being true for the two modes that run it does not make the tree clean, because
`prepareBranchPlan` returns early — "already on feature branch, caller should skip" — the moment
`matchesDefaultBranch` is false, which is *before* its uncommitted-changes gate. `use_worktree`
defaults to false, so a resumed run whose HEAD is already the plan's feature branch commits in the
user's own checkout with no clean-tree check anywhere on the path, and a sweep there would commit
their unrelated work in progress. `task.txt` therefore stages `git add <paths>` over the files the
model created, modified, or deleted, plus `{{PLAN_FILE}}` and any backlog entry. It also carries the
same bound the review prompts do — a defect in code this branch changed is never out of scope —
because it owns `ALL_TASKS_DONE` and filing is dismissal-equivalent, so an unbounded category would
let the executor file its own defect and tick the checkboxes. That bound names the branch and not the
current task: the task phase runs one task per iteration on the same branch, so a justification scoped to
"this task's own work" would leave an earlier task's defect filable and `ALL_TASKS_DONE` still reachable.
`task.txt` is also the one capture path whose in-phase commit sits behind a success gate — STEP 3 runs only
after validation passes — so its `TASK_FAILED` branch commits any filed entry on its own through the
pathspec form first: a fresh `--worktree` run is removed with `--force` on failure, and a staged but
uncommitted entry dies with it while the failed task's own work is meant to. Every path that stages a *set* of
files it has to discover is preceded by a `git status --porcelain` enumeration, including the Go-side
`commitPrefix` in `Runner.runExternalAndPostReview`: enumerating from `git diff HEAD` alone, or from memory,
drops a file the model created while working, which under `--worktree` then dies with the worktree. The
entry-only stages in `make_plan.txt` and the two internal-review prompts name one path the model just
created and deliberately carry no enumeration of their own. Where a prompt does enumerate, it must also say
what the enumeration covers: each external evaluation runs as a fresh session, so a final commit scoped to
the files *that session* created names an empty set — the round that reaches `EXTERNAL_REVIEW_DONE` is by
definition the one that fixed nothing — and would commit only the already-staged backlog entry while
dropping every accumulated fix. The three evaluation prompts therefore bound staging by *the loop's* output
rather than the session's, naming earlier iterations of the loop explicitly instead of the files the model
itself wrote. The enumeration is deliberately not the staging set: `git status --porcelain` lists the whole
dirty tree, so "stage every path those commands list" is `git add -A` spelled out and would commit the user's
unrelated work on the three modes that run in their own checkout. Those prompts say the two commands enumerate
the whole tree, stage only what this loop produced, and leave unstaged any dirty path the model cannot
attribute to the loop. For the same reason, no prompt may justify staging an entry as what saves it from worktree removal:
the index lives in the worktree's own Git metadata and is removed with it, so only the commit does. That
`commitPrefix` carries the same pathspec bound and the same `git add -A` prohibition as the seven
prompt sites, because it is prepended to `review_second.txt` — which forbids the sweep inline — and
runs on the `runReviewOnly` and `runCodexOnly` paths that create no worktree — and, like the two
internal-review prompts, on a `runFull` run without `--worktree` whose `prepareBranchPlan` short-circuits,
so all three sites name that case too rather than resting on the three no-worktree modes alone. Do not restore a bare
"stage and commit them" there.
`review_first.txt` and `review_second.txt` deliberately do not sweep and say so
inline: `ModeReview` and `ModeCodexOnly` create no branch and no worktree, so `--review`
and `--external-only` commit in the user's own checkout, where a dirty tree is
allowed and never gated, and a sweep there would commit their unrelated work in progress. Those
two prompts stage the files the model itself created or modified, and their entry-only
`docs: add backlog entry` commit uses the pathspec form
`git commit -m "docs: add backlog entry" -- <entry>` for the same reason `make_plan.txt` does.
Do not restore a bare `git commit -m` at either site: with the entry staged it succeeds and
commits only the entry. The three evaluation prompts must leave it uncommitted: after the first
round `getDiffInstruction` shows the external reviewer only the uncommitted diff, so a
mid-loop commit hides the accumulated fixes, the next round reports clean, and
`EXTERNAL_REVIEW_DONE` fires with the fixes unverified — the entry is swept into the
final commit instead. Those three prompts do say to `git add` the entry alone: the final
sweep is described as reviewing `git diff`, which never shows a new untracked file, and
staging keeps the entry out of the unstaged diff the reviewer is shown while guaranteeing
the commit picks it up. Because that leaves the index deliberately non-empty, the same three
prompts spell the final commit as `git diff HEAD` plus an explicit `git add <paths>` over every
file the model created, modified, or deleted: a bare `git commit -m` used to fail loudly with
`no changes added to commit`, and with the entry staged it would instead succeed, commit only the
entry, and drop every accumulated fix right before `EXTERNAL_REVIEW_DONE`. Those three prompts pair
`git diff HEAD` with `git status --porcelain`, because the diff never lists an untracked file:
enumerating the commit from the diff alone drops a new test or helper the model wrote while fixing
findings, which under `--worktree` then dies with the worktree. That explicit stage is deliberately
not `git add -A`, for the same reason the internal-review prompts avoid one: these prompts also
run under `--review` and `--external-only`, which create no worktree. Staging does not
weaken the stalemate reset either, since `diffFingerprint` runs `git diff HEAD`. Plan creation runs in the source checkout before
the branch and worktree exist, so it stages that one file and commits it through the
pathspec form `git commit -m "docs: add backlog entry" -- <entry>`; an uncommitted
entry there never reaches the branch, and an untracked one fails branch and worktree
creation outright, because `hasChangesOtherThan` counts untracked files and exempts only
the plan file. The pathspec is what keeps that commit honest: this is the user's own
checkout rather than a loopai worktree, so a bare `git commit -m` would sweep whatever they
had already staged into a commit labelled as a backlog entry, on whatever branch is checked
out — normally `main` or `master`, and the same happens on a session the user later cancels
at draft review. The `loopai-plan` and `loopai-brainstorm` skills file entries too and carry
that same commit rule for the same reason: they run before loopai does, and loopai commits
nothing but the plan. Filing is dismissal-equivalent only for the completion signal:
the write is a real repository change, so the round that makes it resets
`review_patience` stalemate detection. A pre-existing linter error or failing test is
never out of scope on any path that can fix — all six such capture blocks say so
explicitly, because the category is otherwise an escape hatch from the pre-existing-issues
rule that sits beside it in the same prompts, and `task.txt` and `external_claude_eval.txt`
carry no such rule of their own. `make_plan.txt` is the deliberate exception: plan creation
fixes nothing, so filing is the only thing it can do with such a finding. The review and
evaluation prompts additionally bound the category unconditionally: a defect in code the branch
itself wrote is never out of scope, whether or not `{{PLAN_FILE}}` names a plan. Filing is
dismissal-equivalent for the signal, so without that bound "outside this plan's scope" is a new
way to end a review green with a defect this branch introduced. The bound must not be written as
a consequence of the no-plan fallback alone: under `--review` and `--external-only` there is
additionally no plan for any finding to be out of scope of, but the
dangerous case is the ordinary one where a plan is present.

Agent files may carry YAML frontmatter parsed into `config.Options` (`model`,
`agent`, `description`). A non-empty `description` marks the file as a *dynamic
agent*: it is offered to the internal review phase through the
`{{agents:dynamic}}` catalog instead of requiring a hard-wired
`{{agent:<name>}}` reference. The five embedded agents (quality,
implementation, testing, simplification, documentation) carry no description and
always run as the base set. `loopai --gen-agents` writes starter dynamic agents
into `.loopai/agents/`. When `Options.Validate` warns, `buildAgent` drops the
execution overrides but preserves `Description`: catalog membership must not
depend on an unrelated `model` typo. `config.ParseAgentOptions` applies the same
CRLF/whitespace/leading-comment normalization the loader does, so any second
reader of an agent file agrees with the review phase about its description.
`parseOptions` collapses the description to a single space-separated line: a YAML
block or folded scalar parses fine but yields embedded newlines, and the catalog
renders the description as one Markdown list item followed by an indented
invocation snippet, so continuation lines would land unindented between the two.

Every model-bearing setting shares one grammar, `provider[:model[:effort]]`, parsed by
`config.ParseProviderSpec`: `plan_model`, `task_model`, `review_model`, and each
`external_reviewers` entry (`ParseExternalReviewers` reuses the parser and keeps its own
`custom`-carries-no-model rule). The provider prefix is mandatory; an empty model segment
means the provider CLI's default, so `codex::medium` sets effort alone. There is no
implicit provider selection: `--codex`, the `executor` key, and model-name inference are
gone. The provider is a property of the phase, not the run. `task_model`'s provider runs
the task phase and an unset `task_model` means Claude — a default, not an inference; the embedded
config sets `task_model = claude:opus:high`, and bare `claude` keeps the Claude CLI's own model;
`review_model`'s provider runs internal review, external-findings evaluation, finalize,
and report (the four consumers of the runner's review executor); `plan_model`'s runs plan
creation; the latter two inherit `task_model` whole, provider included, when unset.
`resolvePhaseProviders` resolves the three providers into the runtime-only
`Config.PlanProvider`/`TaskProvider`/`ReviewProvider` fields and rejects
`--pass-claude-md` unless Codex runs the task or review phase. It runs inside
`applyCLIOverrides`, before startup spec validation, and resolves an invalid spec to claude,
so when that trips the gate it returns `validateModelSpecs`' error instead: an unmigrated
`task_model = gpt-6-astra` beside `pass_claude_md` must get the prefixed rewrite. The
processor does not read
those fields: `processor.Config.taskSpec()`/`reviewSpec()` derive each phase's provider
from its own spec, `executorFactory.Build` constructs the task and review executors from
them separately (the review slot stays nil when provider, model, and effort all match the
task), and prompt rendering takes the provider of the phase running the prompt, so a
Claude review block under a Codex task phase launches Task-tool agents while the task
prompt keeps its Codex guidance. A provider mismatch there degrades review silently rather
than failing, which is why agent syntax follows the phase and not the run.

`run` calls `validateModelSpecs` at startup, before external-review
resolution, on the resolved plan, task, and review specs, so a CLI flag overriding a bad
config value passes exactly as the executors would see it. `validateModelSpec` checks
grammar, provider, effort, and provider/model consistency in one pass. A spec without a
provider segment reports the value and a rewrite whose provider comes from
`config.ModelProvider` (`opus:high` → `claude:opus:high`); `missingProviderHint` rewords two
cases where that rewrite would mislead: the old effort-only `:high` form gets both
`claude::high` and `codex::high`, and under a wrapper `claude_command` any unprefixed spec of
at most two segments gets `claude:<spec>`, since that wrapper ran every unprefixed spec before.
`effortInModelSegment` rejects an effort written where the model goes (`codex:high` parses as
model `high`), for phase specs and reviewer entries alike. The rewrite hint is
`config.ModelProvider`'s surviving role besides the mismatch check: it recognizes case-insensitive model prefixes and rejects
`codex:opus`, unless the named provider's command is a wrapper
(`IsRealClaudeCommand`/`IsRealCodexCommand`) that defines its own model names. `custom` is
rejected for a phase spec. An effort outside `low|medium|high|xhigh|max` is reported as
unknown; `max` stays accepted for both providers, since codex already drops it through a
dedicated warning and re-reporting it would turn a deliberate downgrade into a hard failure.
`resolveExternalReviewSelection` applies the same effort list to the chain it resolves: it
wraps `resolveReviewerChain` and calls `validateReviewerEfforts` and then
`validateReviewerProviders` on the result, because that resolver returns from several
branches — the chain, the automatic reviewer, the modes that disable review — and a check
placed inside one of them leaves the others unguarded. `config.ParseExternalReviewers`
checks the separator count and the provider name but never the effort value, so a typo would
otherwise reach the reviewer process and fail there — after the task phase, and only for that
one reviewer in the chain. Explicit reviewer providers must match known models, with custom
reviewers, wrapper commands, and unknown names skipped.

Removed spellings fail loudly rather than being ignored. `rejectRemovedFlags` runs through
`validateFlags` before config loading and dependency checks, so a machine without the codex
binary still gets the migration message; `--codex`, `--codex-only`,
`--external-review-tool`, `--external-review-model`, `--t3`, and `--no-t3` stay declared as hidden
`opts` fields because deleting them makes go-flags answer with a bare `unknown flag`, and the two
external-review flags fold into one ready `--external-reviewers=` entry. So do the flags
`--t3-progress-thread` and `--t3-launch`, which were removed with the old T3 Code integration and
whose errors name `loopai --detach --t3-thread <thread-id>` and the `loopai-t3` skill. The config keys
`executor`, `external_review_tool`, `external_review_model`, `codex_model`,
`codex_reasoning_effort`, `finalize_enabled`, and `t3` are rejected by `checkRemovedKeys` at every
config layer, each error naming the replacement spelling; `finalize_enabled` was a boolean whose
replacement is the `finalize` mode, so its error names `finalize = sync|pr|merge`. The key
`acp_auto_merge` was removed too and names `loopai --merge` or the `loopai-merge` skill. The old
environment toggle `LOOPAI_T3` was removed: it lost its `env:` tag and is inert, so an exported value
never fails a run, while `LOOPAI_T3_TOKEN` and `LOOPAI_T3_URL` stay live inputs read directly. `checkExecutionDeps` checks every distinct provider
of the phases the mode runs, each missing binary with its own error, then the reviewer chain
under the existing explicit/automatic rules. Full mode does not check the plan provider, since
most full runs never plan; `tryAutoPlanMode` checks it before prompting for a description
instead. `modePhaseProviders` is also what makes the progress header's and run record's
`executor` name the first phase the mode runs (the review provider under `--review` and
`--external-only`) and what `detectClaudeSwapRecovery` counts, plus the plan provider in full
mode. The startup banner prints one
`<phase>: <provider> <model>[:effort]` line per phase the mode runs, with Codex-only
settings indented under the first Codex phase; cmux labels prefix each phase with its
provider once the phases' providers differ, and Orca plan and review-block titles name their
own provider.

`external_reviewers` configures an ordered comma-separated reviewer chain using
`provider[:model[:effort]]` entries. When it is unset, the provider other than
`task_model`'s reviews automatically, gated by `codex_enabled`. `--review` and
`--external-only` run no task phase, so `reviewerBaseProvider` (and the processor's
`externalReviewProvider` fallback) key them off `review_model`'s provider instead: keyed off
an unused task spec, a split config would make the review provider both reviewer and
evaluator. The explicit same-provider warning compares against that same provider. `custom` entries use
`custom_review_script` and cannot specify a model. An explicitly empty value
clears an inherited chain and disables external review. `createRunner` hands an empty
selection to the processor as `none` through `externalReviewSelection.processorTool`: the
processor reads an empty tool as auto and `--external-only` bypasses `codex_enabled`, so an
empty tool there would build a reviewer for a chain the user explicitly emptied.

`loopai --init` creates project-local commented defaults. `loopai --reset` restores global defaults interactively. `loopai --dump-defaults <dir>` extracts embedded defaults for inspection.

This is a clean configuration break: never add automatic reads or migrations from `~/.config/ralphex/` or `.ralphex/`.

Tests must redirect HOME or config paths to `t.TempDir()` and must never touch either real user configuration directory. Redirect `HOME` and `USERPROFILE` together: on Windows `os.UserHomeDir` reads only `USERPROFILE`, so a `HOME`-only redirect still resolves the real `~/.config/loopai`.

## Architecture and key patterns

- `cmd/loopai/main.go` parses flags, resolves executors, constructs the processor, and owns process-level cleanup.
- `pkg/processor/processor.go` coordinates task and review phases.
- `pkg/processor/phase` contains the individual phase engines.
- `pkg/executor` owns child-process invocation and stream parsing.
- `pkg/status` contains the shared execution model and stable protocol signals.
- `pkg/config/defaults` contains embedded config, prompts, and agents.

Task plans use `### Task N:` or `### Iteration N:` headings and Markdown checkboxes. The task phase handles only the first incomplete section per executor iteration.

Review-only startup guards live in `cmd/loopai`: `worktreeIgnoredWarning` reports that an explicit
`--worktree` has no effect, and `checkReviewDiffRange` calls
`git.Service.DiffRangeEmptyContext` before executor dependency checks, progress logging, or reporter
creation. The Git helper validates the literal base and runs the exact context-aware equivalent of
`git diff --quiet <base>...HEAD --`, so empty commits and net-reverted branches are rejected while
missing merge bases remain errors. It never normalizes the base to a different ref because review
prompts receive the literal value. It does not reuse `DiffStats`, because that helper treats an
unresolvable base like an empty diff instead of preserving the base-ref error.

`pkg/processor/prompts.go` expands `{{BACKLOG_DIR}}` alongside `{{PLANS_DIR}}` in
`replaceBaseVariables`, the choke point every builder funnels through, so every
prompt path gets it from one line plus `getBacklogDir`, which mirrors `getPlansDir`
and falls back to `docs/backlog` when `BacklogDir` is unset. A prompt file's header
comment lists the variables that prompt actually uses, not every variable expanded in
it, so adding the placeholder to a prompt and its header happen together.

`pkg/processor/prompts.go` expands two agent placeholders with the same
per-provider invocation snippet builder (Task tool for Claude, `spawn_agent` for
Codex): `{{agent:<name>}}` inlines one named agent, and `{{agents:dynamic}}`
renders the dynamic-agent catalog sorted by name, or
`(no project-specific agents configured)` when the project defines none. Only
the embedded `review_first.txt` uses the catalog; `review_second.txt` and the
embedded external-reviewer prompts do not, though both placeholders are expanded
on the external path too so customized prompts behave alike. There they render in
the syntax of the reviewer that runs the prompt, not the review block's: a Claude
reviewer keeps the Task tool, while a Codex reviewer has no `spawn_agent` because
`MultiAgent` stays off on the reviewer path, and a custom script keeps the review
block's syntax. The catalog pass runs
after agent-reference expansion, so `agentBodyText` strips `{{agents:dynamic}}`
from inlined agent bodies — otherwise raw catalog text lands inside an
already-escaped codex `task='...'` literal. The catalog also skips agents the same
prompt already inlines through `{{agent:<name>}}` — `agentRefNames` collects those
names before expansion — so a user copy of a base agent that carries a description
is listed and launched once, not twice. Which catalog entries actually run is decided by the model from
the descriptions — deliberately not by path or glob triggers in code — and the
prompt bounds the selection to roughly 0-3 agents launched in the same parallel
message as the base five. `FirstReviewPrompt` calls `warnMissingDynamicCatalog`,
which warns once per run when the project has dynamic agents but the effective
`review_first.txt` carries no placeholder: a prompt copy installed by an earlier
`--init` predates the catalog and would otherwise drop every dynamic agent
without a trace. The check belongs there and not in `expandDynamicAgentCatalog`,
which also runs for `review_second.txt` and the external prompts that
deliberately omit the placeholder.

`--gen-agents` is a standalone mode backed by `processor.ModeGenAgents` and
`phase.GenAgentsPhase` rather than ad-hoc executor wiring, so retry/limit policy
and section timing match the other phases. It runs one session with
`gen_agents.txt`, logs to `.loopai/progress/progress-gen-agents.txt`, then
reports the agent files present with their descriptions using
`config.ParseAgentOptions`; an unreadable file is reported inline rather than
aborting the listing. `describeAgentFile` reports what the loader will do with
each file rather than its description alone, so it gates emptiness on
`config.AgentFileHasBody` — the same comment-stripping check the loader uses.
A file with no body is ignored (replaced by the embedded default only for a
reserved name, dropped entirely otherwise), and frontmatter that fails to parse —
an unquoted `description` containing `: ` is the likely cause — is
indistinguishable from having none, so both are flagged instead of being listed
as working agents. An unquoted ` #` does not break parsing; YAML reads it as a
comment and truncates the description, so `gen_agents.txt` requires a
double-quoted description for both reasons. That distinction comes from
`config.AgentFrontmatterUnparsable` and not from a `---` prefix on the parsed body:
a working agent may open its body with a markdown rule, and a broken block written
below a `# ...` header never reaches the parsed form at all, so the prefix check
misdiagnoses both. It requires a *complete* `---` block — opening delimiter plus a
closing one on its own line — before calling anything unparsable, because a file with
no frontmatter whose body simply starts with a markdown rule matches the opening
delimiter alone and its real gap is a missing `description`, not YAML quoting.
`parseOptionsWithCommentRetry` accepts its comment-stripped retry
whenever that parse consumed a frontmatter block, not only when the block carried a
recognized key: a well-formed block holding only foreign keys must read the same
behind a comment header as it does without one, otherwise its raw `---` lines stay in
the agent body and valid YAML gets reported as broken. The report also runs on the failure path — a session that writes
files and then fails, times out, or is interrupted leaves them in the next run's
catalog, and the reserved-name warning would otherwise be lost with the error.
Overwriting a reserved base name warns instead of failing, because the user
reviews the generated files with `git diff` before committing them, and the
warning fires only for a file that actually has a body: a plain `--init` fills
`.loopai/agents/` with all-commented copies of the five base agents that override
nothing. Non-`.txt` files in the directory are reported as ignored rather than
skipped silently: the agent loader reads `.txt` only, so a session that disregards
the prompt and writes `.md` would otherwise leave files `git status` shows and
nothing ever loads. `clearStaleCmuxStatus` excludes the mode alongside the other
config utilities — it executes no plan and constructs no reporter, so clearing
would drop the previous run's completion pill with nothing to replace it. The
session resolves `task_model` only — `plan_model` does not
apply and no review model is printed. It is routed before branch and worktree
setup but still opens the repository to run `EnsureLocalGitignore`, since it tells
the user to inspect `git status` afterwards. `validateGenAgentsFlags` must reject
`--serve`/`--watch` alongside the other standalone modes: watch-only routing is
decided before the `ModeGenAgents` branch. Like `ModeTasksOnly`, the mode
short-circuits `resolveExternalReviewSelection`: it runs no review phase, so a
configured `external_reviewers` binary missing from `PATH` must not fail it. It is
also routed before `notify.New`, which validates channels eagerly: the mode sends
no notification, so a half-filled `notify_slack_*`/`notify_email_*` block must not
be what stops it. Notification setup covers only the plan-executing paths — the
close-out commands and watch-only mode return before it for the same reason.

`cmd/loopai/launch_history.go` records every plan-executing launch in
`<config dir>/launch-history`, where the directory is `o.ConfigDir` or `config.DefaultConfigDir()` —
never `DefaultConfigDir()` alone, which would ignore `--config-dir`. It is best-effort like the cmux
and Orca reporters: `recordLaunchHistory` returns nothing and turns a failure into one
`warning: launch history not recorded` line on the caller's stderr writer, never `os.Stdout`. There
are exactly three hook points: `run()` after `resolveExecutionDeps` and the gen-agents return, gated
on `modeRequiresBranch` (so `--review`, `--external-only`, and every standalone command, the
`--detach` launcher and `--acp` included, record nothing, and a plan chain records once);
`runPlanMode` where plan creation continues into a `ModeFull` run; and the detached child after
`prepareNonInteractiveRequest`, once per attempt, which passes `launcherForOrigin` of the recorded
origin: `t3` for both T3 origins (`t3-terminal` and `t3-thread`), `cli` otherwise, since
`loadDetachedConfig` forces `cfg.Orca` off. `launcherFor` returns `orca` when the post-override
config enables it and `cli` otherwise. `launchFlags` renders the
review spec only when set and the reviewer chain only when `sel.Explicit`, because an inherited
review spec or an automatic reviewer follows from the task spec and would be frozen into an explicit
choice on replay; any value outside `^[A-Za-z0-9._:,+-]+$`, the charset the three skills accept,
drops that flag, so a skill never offers a string it would reject. `externalReviewSelection.flagValue`
renders each reviewer from `resolvedReviewer.Spec`, the `model[:effort]` remainder as configured, and
never from the resolved `Model`/`Effort`: `ResolveExternalReviewerModelEffort` fills Claude's
`opus`/`xhigh` and Codex's `gpt-6-astra`/`high` defaults and drops a Codex `max`, so a resolved rendering would freeze today's default
into every replay. A chain containing a `custom` reviewer renders empty, since `custom_review_script`
cannot travel through the flag, and an explicitly empty chain is omitted for the same reason; a replay
of either selects the automatic reviewer. The `run()` hook sits before plan selection, so a full-mode
run that falls into auto-plan records there even when the description prompt is canceled, and records
again, deduplicated, if plan creation continues. The file is
tab-separated `<RFC3339 UTC>`, `<launcher>`, and `<flags>`, newest first, deduplicated by the flags string alone (a
repeated combination moves to the top and takes the new launcher), capped at
`launchHistoryLimit` (10), and rewritten whole through `writeFileAtomic` with mode `0600`; an empty
flags string is a valid entry. There is no lock: two simultaneous launches lose at most one line.
`loopai-plan`, `loopai-orca`, `loopai-t3`, and their Codex copies read it with one shared snippet
that `scripts/check-launch-history-skills_test.sh` requires to be identical across all six skills
and executes against fixtures; change the format and the snippet together. The skills read only
`${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}`, so a launch made with `--config-dir` is recorded where
no skill looks. Without pass-through flags, `loopai-orca` and `loopai-t3` offer up to three history
entries before launching, but never when `loopai-plan` already chose the flags in the same
conversation, an empty choice included; `loopai-plan` marks the launcher of the newest `orca`/`t3`
line "(Recommended)", and within `AskUserQuestion`'s four-option cap the oldest history entries are
dropped first.

The task and review providers own all repository writes. External reviewers produce findings only; the `review_model` provider (falling back to `task_model`'s) evaluates and fixes them. Reviewer chains run in order, and each reviewer loops until clean, its independent iteration cap, or its independent stalemate threshold before the next reviewer starts. Post-external review and finalize run once after the complete chain.

`review_cadence = end|task` (`config.ReviewCadence*`, read through `EffectiveReviewCadence`;
`--review-cadence` sets it) adds per-task external review blocks to full mode only.
`phase.TaskPhase` carries an `AfterTask(ctx, taskNum, headBefore)` hook and calls it after an
iteration that did not end in `SignalFailed` and either advanced `NextPlanTaskPosition` or left no
uncompleted task, whatever its signal. That includes a timed-out session and one resumed after a
manual break, when either ticked its task before stopping: the next iteration starts at another
position, so skipping them would silently drop that task's review. `task.txt` ticks the plan before it commits, so such a session can
stop in between: `taskStart` also records the `DiffFingerprint` at the position's first attempt, and an
interrupted iteration whose fingerprint changed skips the hook with a log line, since a reviewer diffing
against HEAD would miss the uncommitted work, and reports the task through `TaskPhaseOpts.UncommittedTask`, which sets the same pending flag described below; a break the user answers with abort, and an executor error or cancellation such as Ctrl+C, runs no review but reports such a task too, since the next invocation starts at the following position. `headBefore` is kept from the
first attempt at a position, so a retried attempt does not move the diff base past the task's own
commits. `taskNum` is the plan position, and one session can tick several tasks, so
`reviewScopeForTask` names where the reviewed work starts rather than a single task. A hook error is wrapped as `after task N: ...`,
while `context.Canceled` and `ErrUserAborted` propagate unchanged. The runner installs the hook only
when the mode is `ModeFull`, the cadence is `task`, and `externalPhase.Enabled()`; `runTasksOnly`
keeps it nil. `Runner.afterTaskReview` skips the block with a log line when `headBefore` is empty,
otherwise calls `promptBuilder.SetReviewScope(headBefore, reviewScopeForTask(...))` — the builder is
one instance shared by every phase, so the override is a mutable field — which makes
`getDefaultBranch`, and with it `{{DIFF_INSTRUCTION}}` and `{{DEFAULT_BRANCH}}`, name the task's start
commit and fills `{{REVIEW_SCOPE}}` in the six external review and evaluation prompts;
`{{FINALIZE_BASE}}` is untouched and an unset scope renders empty, keeping `end` byte-identical.
It runs the external phase with `SetResume(0, false)` under `PhaseExternalReview`, and its deferred
restore returns the holder to `PhaseTask`, clears the scope, and re-applies the checkpoint resume
state for the final block. The internal review and `review_second.txt` loop run only in the final
block. The evaluation prompts commit only on `EXTERNAL_REVIEW_DONE`, so a block ending by stalemate,
iteration cap, or break leaves its fixes uncommitted and nothing on the task path stages them;
`afterTaskReview` compares `DiffFingerprint` before and after the block, on an error or cancellation exit as well, and sets
`perTaskLeftovers` through `Runner.setPendingReviewFixes`. While it is set, `runInternalReview` prepends a `leftoverCommitPrefix` to the final block's first review, because every final reviewer reads `<base>...HEAD` and would otherwise miss the work, and `runExternalAndPostReview` runs the post-review loop as a backstop, whose
`commitPrefix` commits anything left, even when the final chain finds nothing; once that loop returns, or was checkpointed as done, the flag is cleared and saved, so a later invocation on the branch does not owe it again. The flag is persisted as
`RunRecord.PendingReviewFixes` through `runRecorder.SetPendingReviewFixes`, read back by `startRunRecord` in
full mode, carried across `resetRunRecord` (which saves at once when it is set) and `adoptLoadedRunRecord`,
so a run stopped between that block and the final one still commits the fixes after a resume. A fingerprint rather than
`IsDirtyAll` is what keeps a user's unrelated dirty files from triggering it. `warnMissingReviewScope`
warns once when a scoped render meets a customized external prompt without `{{REVIEW_SCOPE}}`. `perTaskReview` is set for the block's duration: `onReviewerDone` saves no checkpoint stage
while it is, because a lost per-task review is repeated by the final whole-branch block, and
`runRecorder.updateExternal` mirrors the block's reviewer records into `Runner.currentExternal`,
which `resetRunRecord` and `adoptLoadedRunRecord` restore — the task-phase commit that follows a
block otherwise resets the record and discards every per-task review before the report.
`ExternalDone` aggregates rather than overwrites per reviewer key: durations add, `HadFindings` is
OR-ed, `EndedBy` keeps the latest completion, and `ExternalReviewerRecord.Blocks` counts loops, which
`run_facts.go` renders as `(N review blocks)` when above one. Each iteration carries its 1-based `Block`, rendered as `#### Block N, iteration M` once a reviewer has more than one, because indexes restart per block; `boundExternalReviewText` truncates nothing while the total fits and otherwise guarantees the reviewers' latest blocks at least half the text budget, plus whatever the earlier blocks leave unused, so the final whole-branch review is not truncated to an even share with the per-task blocks it re-reviews. `reviewCadenceStartupWarning` warns
when `task` meets `--review`, `--external-only`, `--tasks-only`, or an empty chain.
`cmd/loopai`'s `reviewCadenceFor` already turns the cadence into `end` outside `ModeFull` or with an
empty chain, so `processor.Config.ReviewCadence` and the banner's `review cadence:` line carry `task`
only when it will run. The journal `Snapshot` treats an external review block entered from the task
phase as a per-task block that leaves the stages alone, so the Mode 2 plan view does not complete the
Review stage early.

Completion reporting is split between durable fact collection and model assessment. `processor.RunRecord` is the persisted run-level model; `phase.RunRecorder`, supplied through `phase.Deps`, lets task, internal-review, external-review, and post-review phases record events without depending on the processor store. `Runner` owns the concrete recorder and saves `.loopai/progress/<progress-log-stem>.run.json` atomically with mode `0600` after each event. The record captures phase/task counts and timings plus bounded reviewer/evaluator text; repository facts such as commits, name-status, diff totals, backlog entries, validation commands, and plan drift are collected separately at report time. `Runner.SetRunTimingsSource` reads non-finalizing `SectionTimer` and `ValidationTimer` snapshots at recorded events and before reporting; resumed records add prior measurements once. The report uses a finish timestamp captured before model assessment, while the final persisted record timestamp includes the report phase. Sidecar creation uses exclusive creation so existing files and symlink targets are never overwritten.

Every plan-archive commit is pathspec-restricted: `MovePlanToCompleted` commits through
`commitFiles` over the destination plus, when `git mv` staged it, the source deletion, and the
report variant goes through `commitReportPlanMove`. Never use the bare `commit` there: a single-plan
worktree run archives through `MainGitSvc` in the user's own checkout, so anything they staged during
the run would otherwise land under the `move completed plan` message. A detached run is the exception:
`ArchiveOnRunBranch` archives on its own branch through the run's Git service, so its pushed branch,
and a `finalize = pr` PR, carry the archive commit and the report sidecar.
A rejected single-plan archive (a `commit-msg` hook is the reported case) keeps the run green but is
not silent: `moveCompletedPlan` warns through the progress logger and returns it as `incomplete`,
which `displayStats` repeats after the finalize summary lines and before any `finalize incomplete`
line as `plan archive incomplete`, since the move may be left staged. A
chain archive failure stays fatal.
The archive's `os.Rename` fallback (reached when `git mv` refuses, an existing destination included)
first `Lstat`s the destination and refuses to overwrite it, and `resolvePlanMoveTargets` returns
`done=true` only when the source is gone and a `completed/` copy exists, so a collision reaches that
guard instead of passing for an archived plan. This is what makes a dirty source checkout safe for a
single-plan `--worktree` run: `prepareWorktreePlan` only warns about unrelated dirty files, while
`inspectWorktreePlanChanges` refuses an unfinished Git operation through `operationInProgress`, the
same marker scan `--commit`'s `validateAutoCommitState` uses. Worktree chains keep the strict
dirty-tree check.

Finalize is a close-out of a successful run selected by `finalize = none|sync|pr|merge`
(`config.Finalize*` constants, read through `EffectiveFinalize`); `--finalize` sets it and
`applyFinalizeOverride` applies `--skip-finalize` afterwards so skipping wins. It is split across the
processor and `cmd/loopai`. The base sync is `phase.FinalizePhase`, run by
`Runner.runFinalize` in the old finalize slot of `runExternalAndPostReview` — after post-review,
before the report — through the review executor. It reaches Git only through the consumer
interface `phase.FinalizeGit`, wired by `Runner.SetFinalizeGit(req.GitSvc)` (the worktree service
in worktree mode). It requires a clean tree with no Git operation in progress (the operation is checked first, because a pending merge with staged changes is dirty too and one whose index matches HEAD passes the dirty check, where the failed-merge cleanup would otherwise abort it; either way the operation is left as found and the outcome is marked `Unrestored` so the plan archive is not staged into it, while a plain dirty tree is not, since the pathspec archive commit tolerates it) and refuses a checkout that is on the base branch itself
(a `--review` of the base would otherwise merge `origin/<base>` into the user's local base), records
the pre-merge HEAD, runs
`FetchContext` with an explicit `+refs/heads/<base>:refs/remotes/origin/<base>` refspec and
`MergeRemoteNoCommitContext` on the full `refs/remotes/origin/<base>` (so a local branch or tag named `origin/<base>` cannot shadow the fetched commit), which leaves a conflicted merge in place instead of aborting like
`mergeRevision`, and fails rather than reporting up to date when a merge of a non-ancestor
records no `MERGE_HEAD`, since verify would otherwise accept the unchanged HEAD as synced. Up to date runs a validation session only; a clean merge is committed by Go
through `CommitMergeContext` and then validated; a conflicted one is snapshotted with
`StageZeroSnapshot` and handed to the resolution session. `finalize.txt` names the base through
`{{FINALIZE_BASE}}` (`Config.FinalizeBase`, not `{{DEFAULT_BRANCH}}`, which is the diff base and can
be a commit hash or `origin/main`; `report.txt` uses it the same way), the conflicted paths
through `{{FINALIZE_CONFLICTS}}` and the plan's `## Validation Commands` through
`{{VALIDATION_COMMANDS}}`. `FinalizePrompt` warns once when the effective template lacks the
`FINALIZE_DONE` signal, since a customized copy of the old rebase prompt otherwise blocks every sync
without a trace. Acceptance needs all of `<<<RALPHEX:FINALIZE_DONE>>>`, a clean tree with
no operation in progress, the checkout still on the branch the sync started on, an unchanged HEAD
when up to date or otherwise HEAD's parents exactly
`[pre-merge HEAD, fetched base]`, and an empty `ChangedOutside(snapshot, conflicts, HEAD)`. The
signal check alone is not enough because the model owns the commit, and the snapshot comparison is
what proves it edited only the conflicted paths; the branch check is what keeps a merge committed on
another branch from passing and becoming the PR head. Everything else becomes
`FinalizeOutcome{Status: blocked}` after `restore`: `MergeAbortContext(snapshot)` while the merge
is uncommitted, because a bare `git merge --abort` silently drops staged changes whose worktree
file matches the index, then `RestoreHeadContext` (`git reset --keep`, which requires the target to
be an ancestor). Restore refuses to reset at all once the checkout has left the plan branch, since
it would move the wrong branch. Restore runs under `context.WithoutCancel` with a one-minute bound, so a canceled
run, the one error the phase returns, never leaves a half-done merge for its resume. A failed
`StageZeroSnapshot` leaves nothing to compare against, so that one case aborts through
`AbortCleanMergeContext`, a plain `git merge --abort` that is lossless only because the merge just
started on the required clean tree. `reset --keep` keeps local changes, so edits a rejected session
left uncommitted stay in the tree; the blocked reason says so rather than discarding them. A failed
restore, like a preserved pre-existing operation, sets `FinalizeOutcome.Unrestored`, and `archiveAfterFinalize` then leaves the plan in place as
`plan archive incomplete`, since the archive commit would otherwise land inside the merge or on the
branch the session switched to.
`FINALIZE_BLOCKED` is checked before `FINALIZE_DONE` in `detectSignal`, and because a later Claude
text block overwrites the stream's signal, `runSession` also treats `FINALIZE_BLOCKED` anywhere in the
output as blocked, so output carrying both reads as blocked, and `ParseFinalizeBlockedReason` takes the bounded line after it. Both signals are
in `knownSignals`, without which they are never detected. The outcome lands in `RunRecord.Finalize`
and is exposed as `Runner.FinalizeOutcome()`; after any successful sync (`up_to_date`, `merged`, or
`resolved`), when the diff base names the same branch, `Runner.DiffBase()`
measures against `origin/<base>` for both the report facts and the completion summary, so neither
counts base changes already on the branch as the run's work; a commit-hash diff base is kept.

The `cmd/loopai` side picks the base with `finalizeBaseBranch` (the local branch `--base-ref` names, or its
`origin/<branch>` remote-tracking ref when no local branch exists — a remote-only base must not
fall back to merging the default branch — else the configured or detected default branch, without
`origin/`) and the mode with
`finalizeModeFor`: `pr|merge` degrade to `sync` under `--review` and `--external-only`, which
review a branch they did not create, and everything to `none` under `--tasks-only` and for a chain
member flagged `ChainNotLast`, since an earlier member's branch is the next one's start ref.
`finalizeStartupWarning` reports the mode degradations; a `ChainNotLast` member degrades silently. `executePlan` calls `runFinalizeCloseout` after
`moveCompletedPlan`, so a non-worktree run pushes its archive commit too, and before worktree
cleanup, because it pushes from the run's own checkout. A PR opens only after a successful sync
(`up_to_date|merged|resolved`), through `createPullRequest` — the body of `--pr`, factored out so
`runPRCommand` keeps its own error wording and output; `closeoutTarget.statsBase` measures the
body against `origin/<base>`. `executePlan` passes `Runner.Report()` through
`runFinalizeCloseout` and `openFinalizePR` into `closeoutTarget.report`. This in-memory report
reaches the PR even under single-plan `--worktree`, where archival writes the sidecar through
`MainGitSvc` in the source checkout rather than on the pushed branch; a detached run's sidecar is on
the pushed branch as well. `merge` runs
`gh pr checks <url> --watch --fail-fast` under `finalize_checks_timeout`, retrying a fresh PR's
"no checks reported" for `finalizeNoChecksGrace` before treating it as having none — the wait
emits no output, so `awake.Holder.Pin` holds the keep-awake inhibitor through it — then
`gh pr merge <url> --<finalize_merge_method> --match-head-commit <pushed HEAD>` so a PR head that
moved is refused, then `gh pr view <url> --json state` confirms `MERGED`, since gh exits 0 when a
merge queue only enqueued the PR; no branch is deleted and the local base is never touched. Every failure — no
`gh`, origin mismatch, rejected push, failed or timed-out checks, refused or merely queued merge — becomes
`finalizeResult.incomplete` rather than a run error: the plan's work succeeded, so like a rejected
plan archive it keeps the run green and is surfaced, printed last by `displayStats` as
`finalize incomplete: <reason>`, in `notify.Result.Finalize`/`PRURL`, and through `SetFinishNote`
on the cmux and Orca reporters (`synced`, `PR opened`, `PR merged`, `finalize incomplete`).
Finalize removes no worktree: loopai's own worktree (`--worktree`, or a detached run's after a clean
success) is removed by the usual teardown, and a T3 Code-managed checkout a detached run was launched
from is never touched.

`phase.ReportPhase` runs after finalize and before the successful review-checkpoint clear on every review pipeline when `report_enabled` is true. It receives rendered deterministic facts and returns ordinary assistant Markdown; the model must not write the repository file because a single-plan worktree run archives through `MainGitSvc` in the main checkout, outside the executor's worktree. `Runner.Report()` exposes the extracted report, or an eleven-section facts-only fallback when model assessment is unavailable. `MovePlanToCompletedWithReport` writes `docs/plans/completed/<stem>.report.md` beside the archived plan in the same commit; tasks-only never generates a report, and review-only modes may populate `Runner.Report()` but do not archive a sidecar because `shouldMovePlan` is false. A non-empty review-checkpoint invalidation reason resets and removes stale run-record state together with the checkpoint. Current-invocation task counts and start time survive post-task invalidation so newly completed work remains in the report. The success clear intentionally removes only the checkpoint, preserving the record through report generation and archival; successful archival then removes the `.run.json` file.

The report contract is eleven sections (the title and ten level-two headings), in order:
`# Report: <plan title>`, `Summary`, `Change scope`, `Evidence`, `Risk`, `Merge danger`,
`Migrations and operational steps`, `Plan deviation`, `Backlog`, `External review`, and
`Validation`. Evidence requires before/after proof or `none`; Merge danger records
`Door: one-way | two-way` and a one-word `Blast radius`. `extractReport` still validates only
the report title, so older reports and customized prompts remain supported.

`buildReportPRTitleBody` preserves the plan-derived title and legacy body, then prefers
`closeoutTarget.report`; otherwise `locateCompletionReport` searches the associated sidecar
on `refs/heads/<branch>` via `ShowFile`, then the working tree through `readPRPlan`.
`findReportPlanForBranch` resolves report identity from progress records across registered
worktrees, including custom branch names. Both
PR reads are capped at `maxPRPlanSize`; `--report` retains unrestricted file reads
for configured external or symlinked plans directories. Not-found keeps
the legacy body, and other lookup errors warn on stderr without failing PR creation.
`reportPRBody` requires Summary, emits its body without a heading, then Evidence, Merge danger,
Risk, Migrations and operational steps, and Plan deviation, omitting absent sections. External
review and Validation go in collapsed `<details>` blocks before `## Changes` stats.
`fitPRBody` checks `maxPRBodyRunes` (65,536): use the full body, then drop both `<details>`
blocks, then fall back to the legacy plan-overview and diff-statistics body. Missing Summary
or a facts-only Summary also uses the legacy body; existing PR metadata validation
still applies to that fallback.
Section parsing preserves headings inside backtick or tilde code fences as section content.

Claude runs every phase by default. `task_model = codex:<model>[:effort]` moves tasks to Codex, and with them planning and the review block unless `plan_model` or `review_model` names another provider. `external_reviewers` entries require explicit `claude`, `codex`, or `custom` providers; duplicate providers with different models are supported. With the chain unset, the provider other than `task_model`'s (`review_model`'s under `--review` and `--external-only`) is selected when installed. A reviewer spec missing its model or effort gets the reviewer default segment by segment — `opus:xhigh` for Claude, `gpt-6-astra:high` for Codex, a dropped Codex `max` counting as missing — while a bare `codex` phase spec keeps the codex CLI's own defaults. The embedded config cannot pin the automatic reviewer through `external_reviewers`: embedded values reset `ExternalReviewersSet`, and automatic selection never reads the key. Missing automatic reviewers are skipped with a warning; missing explicit reviewers are errors.

Codex invocations use additive `-c` overrides so user `~/.codex/config.toml` settings remain available. loopai never writes to `~/.codex/`. `--pass-claude-md` lets Codex discover project `CLAUDE.md`; it does not install or link user-level files.

`--codex-args` and the `codex_args` config key extend that additive contract: their value is tokenized by the shared `splitArgs`, which separates tokens on any unquoted, unescaped ASCII whitespace rather than the space alone — a tab or newline surviving the ini loader would otherwise fold into the neighboring token and turn the remainder into the bare positional `codex exec` takes as its prompt. The set is deliberately ASCII and not `unicode.IsSpace`, since no shell splits on U+00A0 and a non-breaking or thin space pasted from rendered documentation into an unquoted value would otherwise produce that same stray positional — and applies POSIX-shell backslash rules (literal inside single quotes, escaping only `"` and `\` inside double quotes, escaping the next rune when unquoted) so literal backslashes in Windows paths survive while the documented `[\"CLAUDE.md\"]` quote-escaping recipe still works, and appended to every codex invocation loopai composes, at both construction sites in `pkg/processor/executor_factory.go` — codex plan/task/review phases and external codex reviewers, which share `newBaseCodexExecutor`. The extras go *after* loopai's own `-c` overrides and sandbox flags, because codex resolves repeated `-c` keys last-occurrence-wins (verified against codex-cli 0.147.0), so a user value deliberately overrides the matching loopai one. This is the intentional asymmetry with `claude_args`, which replaces the claude command's argument list rather than extending it: loopai owns the `codex exec` invocation shape. An explicit empty `--codex-args=` clears an inherited config value through the `o.codexArgsSet` guard in `applyCLIOverrides` alone; there is deliberately no `Config.CodexArgsSet` mirroring `ClaudeArgsSet`, because that flag exists only so an empty `claude_args` means "no arguments" instead of "use the defaults", and empty codex extras already append nothing — the invocation stays byte-identical to a build without the flag. Four consequences of appending rather than merging are documented for users and deliberately not policed in code, since extras are trusted input like `codex_command`: `splitArgs` consumes unescaped quotes, so a `-c` value whose TOML type depends on them must escape them or codex fails to load its config and every phase dies at startup; last-occurrence-wins covers `-c key=value` only, so repeating any flag loopai already emits, valued or bare, is a fatal codex parse error rather than an override — `--sandbox` on every path, and `--dangerously-bypass-approvals-and-sandbox` on codex plan/task/review phases, where `codex.go` emits it whenever the effective sandbox is `danger-full-access`, which is the codex phase default; the extras land after the `exec` subcommand, so an option only the top-level `codex` command accepts (`--search`) is a fatal unexpected-argument error rather than a pass-through, while the global options that matter here (`-c`, `--model`, `--sandbox`, `--cd`, `--profile`) are accepted by `exec` too; and a bare positional token becomes `codex exec`'s prompt, demoting the one loopai sends on stdin to the trailing `<stdin>` block codex appends when both are present. Because the extras are appended on the shared base, they also reach the external reviewer whose `ForceReadOnly` pin exists so it cannot write — that pin holds against `codex_sandbox` but not against `--dangerously-bypass-approvals-and-sandbox` in `codex_args`, which codex accepts alongside `--sandbox read-only` precisely because loopai emits no bypass flag on the reviewer path. The CLI spelling is `--codex-args=<value>`: the values that matter start with `-`, so the detached form is rejected by go-flags before the run starts, and the documented examples use the attached form.

On Windows, `CodexExecutor.ForceReadOnly` adds `-c windows.sandbox="unelevated"`
before user extras, retaining the external reviewer's `--sandbox read-only` pin without
requiring elevated sandbox setup. `codex_args = -c windows.sandbox="elevated"` restores
a working elevated setup. This default does not change Codex phases or Linux/macOS invocations.

Every Codex invocation tails its main session rollout when the session ID is available,
even with nil display handlers. The first `event_msg` whose payload type is `task_complete`
starts a 60-second exit grace period; duplicate events do not extend it. If the process
still has not exited, loopai logs `codex did not exit after task_complete; terminating`,
terminates the process tree, and returns a successful `Result` unless the stderr scan
captured a CLI limit/error diagnostic; those retain their typed errors for retry/error
handling. Review text is not scanned for patterns on this path. Non-empty stdout supplies
the result; otherwise `last_agent_message` does, with signals detected from the selected
text. Normal exits retain their stdout result without an extra delay. Parent cancellation
still returns the context error, and existing idle/session timeouts remain in force.
Without an observed completion event (including a missing rollout file), there is no
grace kill. This mechanism applies independently of whether reviewer idle timeout is enabled.

Claude and Codex runners on Windows start the process suspended, assign it to a Job Object
with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, then resume its initial thread. This prevents
fast launcher descendants from escaping assignment. Cancellation, idle/session timeout, and Codex
completion-grace expiry terminate the job, including launcher descendants such as
`cmd.exe -> node -> codex.exe`. Job creation, assignment, or termination failure falls
back to a bounded `taskkill /T /F /PID <pid>` invocation. An exit watcher closes the job handle
as soon as the launcher exits, killing remaining descendants even while callers drain output.
`Wait` joins that watcher after reaping the command. Cancellation also closes stdout
and stderr read pipes so escaped descendants cannot keep the runner blocked on output.
Unix runners retain process-group termination.

Alternative Claude-compatible providers live under `scripts/`. `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and uses native autopilot mode; plan creation deliberately uses `--autopilot --allow-all` without `--no-ask-user`. `scripts/pi-as-claude/pi-as-claude.sh` translates pi JSONL output and maps loopai model/effort settings to pi provider options. Detailed setup and wrapper behavior live in `docs/custom-providers.md`.

Worktrees live under `.loopai/worktrees/<branch>`. A new worktree branch is cut from the source checkout's current HEAD, including a non-default branch or detached HEAD. Plan-chain orchestration lives in `runSelectedPlans`/`runPlanChain` in `cmd/loopai/main.go`, around `selectAndExecutePlan`: `PlanFiles` holds every normalized input while `PlanFile` remains the current/first compatibility field. The coordinator recreates each plan's reporter and progress-log lifecycle, waits for cleanup and cwd restoration, and captures the completed branch SHA as the successor's immutable worktree start ref. Durable chain checkpoints in `.loopai/progress/` record the completed prefix, immutable tip, and initial source-plan state; before plan one's branch is prepared, retries require the saved source HEAD and plan inputs to remain unchanged, while later retries verify the completed/prepared branch tip and skip directly to the first pending plan. Recovery from a crash during archival removes a stale finalized worktree under the preparation and run locks only when it is clean or contains exactly the interrupted plan move; unrelated edits leave the checkpoint and worktree intact for manual recovery. `CreateWorktreeForPlanChainContext` in `pkg/git/service.go` carries initially dirty chain plans into plan one and preserves predecessor edits; resume validation requires the predecessor SHA to remain an ancestor. After a successful worktree chain, unchanged tracked source plans are restored to source `HEAD` and unchanged untracked plans are removed so close-out can merge cleanly; concurrent edits are never overwritten. Non-worktree chains force plan one onto its own branch even from a feature checkout, then create each successor from the captured predecessor tip after verifying a clean hand-off; failed post-checkout synchronization restores the predecessor branch. Only full and tasks-only modes accept chains; explicit `Outcome.succeeded` advances the loop because abort returns nil, and `--commit` is cleared after plan one. Successful intermediate cmux reporters quiesce without publishing a final/free pill, retaining busy ownership through successor setup until the next reporter starts; only chain completion publishes `done`. When an existing plan branch does not already contain its applicable start commit, loopai reuses the branch and automatically merges that start commit—not the current source state—into the fresh worktree. Git 2.38+ predicts conflicts during preflight before source mutation; older Git skips prediction, and prediction races remain possible, so the real merge is always the backstop and removes the fresh worktree on failure. `--base-ref` remains the review/template diff base and does not control worktree creation. With `-c`/`--commit`, loopai stages all non-ignored changes and commits them in the source checkout, advancing its branch or detached HEAD, before creating a fresh worktree; branch reuse merges that new source commit through the same synchronization path. Every worktree run holds a non-blocking OS advisory lock at `$(git -C <worktree> rev-parse --git-dir)/loopai-run.lock` for its lifetime. The lock, not its diagnostic pid and start time, is the liveness authority and the OS releases it on process death. Worktree preparation holds the shared repository lock while classifying or creating the expected path and acquiring its run lock. A durable per-target preparation marker in shared Git metadata distinguishes a crash during fresh initialization from an interrupted executable run; the next invocation removes and recreates a marked partial checkout instead of resuming it. Existence of the expected path
is not what selects resume: `prepareWorktreeTargetLocked` asks `worktreePathRegistered`
whether Git actually knows the path as a linked worktree, because a process outliving a
run can recreate loopai's own directory after the worktree was removed — a Unity or Burst
daemon writing its cache is the observed case — and classifying that as a resume target
failed validation with the plan branch intact but unreachable. An unregistered path is
discarded by `discardLeftoverWorktreePath` and the worktree is recreated from the existing
branch, so a checkpointed review resumes where it stopped. `.loopai/worktrees/<branch>` is
loopai's own gitignored namespace, so an unregistered directory there cannot hold tracked
work; the three shapes that are not loopai's to delete are still refused with the source
checkout unmutated, which is what keeps `--commit` from advancing before the failure: a
symlink, a plain file, and a directory carrying its own `.git`. A held run lock produces a precise busy error before mutable resume validation; an acquired lock routes through existing-worktree validation and automatically resumes from the first incomplete task without source synchronization or auto-commit. Any supplied `-c`/`--commit` is ignored with a warning on this path. Teardown takes the same shared lock before releasing the run lock and removing the worktree, preventing another process from acquiring ownership in the Windows-required release-before-delete interval. The lock file is advisory: deleting its directory entry while held does not release the open file lock and can allow a replacement file to be locked independently, so users must not delete it manually. The former `--resume-worktree` flag was removed and is an unknown option. Shared lock waits and run-lock Git-directory lookup honor cancellation and the configured `vcs_command`. The progress logger is created before changing into a worktree, so logs and chain checkpoints remain in the invoking checkout at `.loopai/progress/`.

Reusing a progress log whose last line is a `Completed:` footer archives it first: the locked
canonical file is copied to `.loopai/progress/history/<stem>/archive-<YYYYMMDD-HHMMSS>-<token>.txt`,
then truncated with a fresh header, then older archives are pruned to keep the live file plus nine.
The timestamp comes from the archived footer. Archive names deliberately lack the `progress-`
prefix: the dashboard's recursive discovery matches `progress-*.txt` and skips neither `.loopai` nor
`history`, so a prefixed archive would replay as a session of its own. `readProgressAssociations`
reads only the top level of each progress directory, so archives never supply a plan-to-branch
association. Failed or unfinished logs keep appending after a restart separator.

Review checkpoint architecture is split across `pkg/processor/review_checkpoint.go` (the versioned
model and resume resolution), `pkg/processor/review_resume.go` (runner load/save/invalidation), and
`cmd/loopai/review_checkpoint_state.go` (atomic file storage beside the progress log). The ordered
stage keys are `internal_review`, `external_review`, and `post_review`. A stage is saved only after
its fixes are committed and the tree is clean, and it is resumed only when the current tree is clean,
its plan identity matches, and its recorded HEAD is an ancestor of the current branch tip. New full
or tasks-only task-phase commits invalidate the checkpoint, as do unverifiable task HEADs. A durable
pre-task HEAD marker makes that invalidation survive a process death inside task execution. Finalize
is not checkpointed: it runs once, never fails the run, and a rerun either merges the base again or
finds the branch up to date.

Keep-awake is best-effort in the same way and must never affect execution. `awake.New` returns
nil when `keep_awake` is off or the platform has no inhibitor, and every `*awake.Holder` method is
nil-safe. `run()` creates one holder after config loading and defers its `Stop`, so one hold spans
a plan chain; it is carried in `executePlanRequest.KeepAwake`, wrapped into the logger chain by
`buildRunnerLogger` between the orca wrapper and `SectionTimer`, and subscribed to each
`status.PhaseHolder` beside the cmux and orca observers. Activity is executor output and section
and phase changes; `LogQuestion`/`LogAnswer`/`LogDraftReview` deliberately do not renew, so a run
waiting for a human expires like a hung one. The idle window is `awake.DefaultIdle` (one hour) and
is deliberately not a config key. `PhaseLimitWait` pins the hold until the phase changes because a
scheduled `wait_on_limit` sleep emits no output and can exceed the window. Both Unix inhibitors are
bound to loopai's pid (`caffeinate -w`, `tail --pid` under `systemd-inhibit`) so process death
releases them without cleanup; the Windows backend drives `SetThreadExecutionState` from one
locked OS thread because the continuous flag is per thread. A failed `Acquire` disables the holder
for the run instead of retrying on every output line. Watch-only mode, close-out, and the other
standalone commands return before the holder is created. `--acp` and the `--detach` launcher take
none; the detached child creates its own holder from its loaded `keep_awake` in `execute`, so the hold
lives in the run's own process. `cmd/loopai` tests replace
`newAwakeHolder` in `TestMain` so `run()` never starts a real inhibitor.

cmux reporting is best-effort and must never affect execution. The status key and notification title are `loopai`. All calls go through the public `cmux` CLI and failures are ignored. After a completed run, `Reporter.Finish` intentionally leaves the final success or failure pill in place: `Stop` still clears the spinner and progress, but does not clear that pill. Abort paths do not call `Finish`, so `Stop` performs the full cleanup. A later run overwrites the pill, while `--clear`, a successful `--merge`, or a successful `--pr` removes it explicitly.

Auto-mode status ownership begins only after a successful local `starting` reservation or normal reporter startup. Validation and mandatory hand-off failures never clear a pre-existing pill that may belong to the active run. The reservation is installed before a combined `--reset` can block for input, including ambiguous bare-`--serve` invocations; if loaded configuration proves the run is dashboard-only, that temporary reservation is cleared. Interactive plan creation quiesces its reporter but retains the non-final pill through branch/worktree setup, then releases it only after the execution reporter starts. At completion, the reporter is quiesced before plan archival, progress-log closure, cwd restoration, and any worktree removal required before final status; `done` or `failed` is published afterward as the final sidebar command and marks the execution inactive for auto-mode routing. A successful `--worktree --serve` run is the deliberate exception to immediate worktree removal: its final pill is published after cwd restoration and log closure, while the plan-bearing worktree remains until the idle dashboard exits. A failed final status write does not become persistent: normal stop cleanup clears the preceding active pill.

`--cmux-workspace[=always|auto]` is part of that best-effort contract. The bare flag and `=always` retain unconditional hand-off. With `=auto` (the optional value must be attached with `=`), `cmux.WorkspaceBusy` runs `cmux list-status` and examines only the `loopai` pill: no pill, or pill text beginning with the final prefixes `done` or `failed`, means free; any other text means busy and triggers hand-off. The final prefixes are shared with `Reporter.Finish`, so producer and query cannot drift. A local auto run whose execution intent is known before config immediately uses `Reporter.Reserve` to install a non-final `starting` pill before config, dependency, or worktree setup; the run-level `cmuxStop` holder clears it on every startup failure and transfers cleanup ownership when the normal reporter starts. A missing cmux environment or binary and any list-status failure make auto mode continue locally without warning unless debug logging is enabled; reservation is attempted independently so a transient query failure does not reopen the startup window when status writes still work. Detection remains intentionally best-effort: a force-killed run can leave a stale phase pill and cause one extra workspace, while simultaneous auto starts can both observe free before either reservation lands. The output query stays separate from discard-only reporter commands, and its `cmd.Output` path must retain `WaitDelay` so a descendant inheriting stdout cannot hold the pipe past the query deadline. `cmux.SpawnWorkspace` is the one cmux call that propagates its error instead of swallowing it, because the caller has to choose between exiting after hand-off and running locally; it returns `cmux.ErrNotInCmux` when `CMUX_WORKSPACE_ID` is unset or the `cmux` binary is absent. `prepareRunBeforeConfig` routes `handOffToCmuxWorkspace` before taking a local reservation and before `handleEarlyFlags`, so hand-off happens before config loading and executor resolution while a combined `--reset` can reserve before blocking for input. It covers plan execution and interactive `--plan` creation alike, but never `isStandaloneCommand` invocations (`--clear`, close-out, `--init`, `--dump-defaults`, reset-only), which own no sidebar state; that predicate is shared with `clearStaleCmuxStatus`. The workspace name comes from `cmuxWorkspaceName`: a single-plan `--branch` override wins, then the first plan-derived name (for a chain), then `loopai`; every chain entry is validated before spawn and successor reporters reuse the same card. The relaunch argv comes from `cmuxHandOffArgv`: `os.Executable()` plus `stripCmuxWorkspaceArg(os.Args[1:])`, which removes the bare and `=value` forms and is the recursion guard, prefixed with `env` and the currently-set variables in `cmuxEnvOptions` when there are any. That prefix exists because the new workspace runs a shell of cmux's own, which inherits cmux's environment and not loopai's, so an option provided through the environment would silently revert to its default after hand-off; `env` is used instead of shell assignment prefixes because the target shell is unknown. `cmuxEnvOptions` must list every `env:` tag in `opts`, and `TestCmuxEnvOptionsCoversOptionTags` enforces that by reflection. Only those options cross; the rest of the environment does not, and `warnAPIKeyNotCarried` reports the one case where that silently changes the run's outcome rather than failing it: the pass-through request travels in argv or config while `ANTHROPIC_API_KEY` does not, so a key exported only in the originating terminal leaves the handed-off run falling back to OAuth or the keychain and billing an account the user did not pick. The key is deliberately not forwarded, because the command reaches the new workspace as text typed into its shell. The warning is emitted only after a successful spawn and only when the variable is actually set here, since otherwise there is nothing to preserve in this terminal either. That test comes first so the quiet case costs no config read, because `preserveAPIKeyRequested` answers the request from both of its sources: `--preserve-anthropic-api-key` and the `preserve_anthropic_api_key` config key, which `applyCLIOverrides` ORs together. Reading argv alone would stay silent for exactly the users who set it once and never type it again. Config is read through `config.LoadReadOnly` as in `handOffAllowedOutsideRepo`, and an unreadable config leaves the flag as the only answer since the child would fail to load it too. Every argv element is POSIX single-quote escaped into the `--command` string, since cmux sends it to the new workspace's shell as text. `cmux new-workspace` is bounded by `spawnTimeout` rather than the 2s `execTimeout`: it starts a terminal instead of updating a label, and a premature kill is ambiguous rather than cosmetic, because cmux may already have created the workspace while the caller reads the error as a failure and runs the plan locally too. That deadline is a mitigation, not a fix, so the ambiguity is also reported: `spawnWorkspace` wraps a `context.DeadlineExceeded` outcome in `cmux.ErrSpawnAmbiguous`, and `handOffSpawnFailure` maps it to a stop with an error. A positive auto busy verdict likewise makes every later refusal or spawn failure stop: falling back into a workspace known to be occupied would defeat isolation. Unconditional mode retains the local fallback for clean failures. Because that error is the one cmux failure a user sees, `SpawnWorkspace` uses `spawnRunner` instead of the output-discarding `execRunner`: it captures the child's stderr into a temp file and folds a bounded excerpt into the wrapped error, so a refusal arrives with cmux's own reason rather than a bare exit status. The spawn sets `CMUX_QUIET=1` in the child environment for that excerpt's sake, because `new-workspace` is a legacy alias for `cmux workspace create` and cmux prints a ~150-character deprecation hint ahead of everything else on every call, which on its own is most of `stderrDetailLimit` and would truncate the reason away; the variable silences advisory output only, so cmux's own `Error:` line still arrives, and the rest of the inherited environment is kept since the client needs it to find the socket. The capture is an `*os.File` and not an `io.Writer`, which is what keeps `execRunner`'s pipe-and-copy-goroutine hazard from applying — `os/exec` hands the descriptor to the child directly. Isolation is only achievable at the workspace boundary: `cmux set-progress` has no key and the pill key is the fixed constant `loopai`. In unconditional mode, any other failure, including an unresolvable working directory, prints a warning and continues the run in the current terminal. `executableHandOffRefusal` covers the relaunch binary itself: resolution failing is one half, but `os.Executable` also succeeds while naming a path the new workspace's shell cannot run, since Linux reads `/proc/self/exe` and keeps naming an unlinked binary with a ` (deleted)` suffix. Presence is all it can test, so a binary that disappears afterwards is out of reach: `go run` unlinks its temporary binary only once the successful hand-off has exited 0, so that invocation still hands off and fails in the new workspace, and the flag has to be smoke-tested from a built binary. A non-empty `o.PlanFile` the child could not read is refused before spawning, for the same reason: the child would otherwise be the first to notice, long after the workspace was created and focused, leaving this terminal claiming success with exit 0 and the user an orphan card to close by hand. `planFileRefusal` resolves the path against the same working directory `plan.Selector` does and goes past its bare existence test, because `os.Stat` also succeeds for a directory, so `--cmux-workspace docs/plans` with the filename forgotten would hand off and only fail in the child at `plan.ParsePlanFile`, in full mode after branch creation. A non-regular path and an unreadable file are both refused; neither can be read by the child either, so no hand-off that would have worked is refused, and a symlink to a readable regular file still passes. The regular-file test runs before the open because opening a fifo blocks until a writer appears. An empty plan file, as under `--plan` or `--review`, skips the whole check. A working directory that is not a repository root is refused before spawning for that same reason, since running from a subdirectory is the likelier mistake and the child's `must run from repository root` would arrive after the card exists. The guard is `fileExists(".git")`, the marker the run itself checks for, so hand-off stays config-independent wherever that marker exists; only its absence consults `config.LoadReadOnly`, through `handOffAllowedOutsideRepo`, to tell the two invocations that legitimately have no marker apart from a subdirectory: a non-git `vcs_command`, and a watch-only `--serve`, which never reaches the repository-root check and runs from any directory. That second case is decided by `isWatchOnlyMode` and not the config-free `mayBeWatchOnlyMode`, because a bare `--serve` with no `--watch` and no `watch_dirs` is a normal run that does reach the check; exempting it would create and focus the orphan card the guard exists to prevent. An unreadable config is neither, because the child would fail to load it too. A successful hand-off leaves the originating workspace's completion pill alone, since the run reports into another card: `prepareStaleCmuxStatus` defers the stale-pill clear for `--cmux-workspace` exactly as it does for a possibly-watch-only `--serve`, and `handOffSucceeded` derives the preserve verdict from `prepareRunBeforeConfig` stopping after a hand-off with no error. Under `--cmux-workspace` the other early flags reach an early exit only after a failed hand-off, and with an error except for the standalone commands whose clear is a no-op anyway.

A worktree run's progress header records two plan paths: `Plan:` is the main-checkout path the run
was launched with, and `Worktree plan:` is the copy the run actually ticks, taken from `wt.planFile`
before the logger is built because the logger is created before the chdir. `handleSessionPlan`
(`pkg/web/server.go`) serves the worktree copy while it exists and falls back to `Plan:` once the
worktree is removed, so a watch-mode dashboard shows live checkbox state instead of the untouched
main copy. The line is written only when the mapped path differs from `Plan:`.

The immediate pre-config auto reservation described above has one exception: an invocation with `--serve` and no plan or description must wait for loaded `watch_dirs` to distinguish a watch-only dashboard from a normal execution run. It reserves after config only when execution will follow. Combined `--reset --serve` still reserves before its prompt, then releases that temporary ownership if config confirms watch-only mode. On successful worktree execution with `--serve`, finalization restores cwd and closes the progress log but leaves the worktree for the dashboard's `/api/plan`; the normal `runWithWorktree` defer removes it when the dashboard exits.

Standalone close-out routing happens before executor and notification dependencies are constructed. `--clear` exits before config loading; `--merge`, `--pr`, and read-only `--report` load config for `vcs_command` and colors, then open Git directly. `--report` resolves the same optional feature identifier, prefers `ShowFile` on the live feature branch, and falls back to the working-tree sidecar with `branch: (merged)` after branch deletion; it never constructs executors or notifications. Base resolution for mutating close-out accepts an explicit local branch or local `main`/`master`. All three commands take an optional positional argument naming the feature, so they can run from the root of the primary checkout or any other registered worktree instead of the feature checkout; `runCloseoutCommand` still requires a checkout root, not an arbitrary subdirectory. Without the argument the feature is the current branch and behavior is unchanged, including the invoking checkout's own clean-tree requirement for mutating close-out; with it that top-level `IsDirtyAll` check is skipped and cleanliness is enforced per worktree inside `prepareMergeWorktrees`. `resolveFeatureBranch` resolves the identifier deterministically: an exact local branch first, then a plan file in the plans directory and its `completed/` subdirectory. For a located plan, the branch recorded for that plan under `.loopai/progress/` wins over the name derived from the filename, because a `--branch` override makes the filename imply a branch the run never created and an unrelated branch carrying that derived name would otherwise be merged and deleted; the newest record wins and an unrecorded plan falls back to derivation. Only records whose `Mode` creates a branch are consulted: `--review`, `--external-only`, and plan creation write their own record for the same plan, under a distinct progress filename and with a later mtime, but their `Branch` header names whatever was checked out at the time, so `recordedBranchIsFeature` skips them rather than resolving the close-out to an unrelated branch. That check is a denylist, so an absent or future `Mode` still supplies the branch, since dropping a valid association silently falls back to the derivation it exists to override. `findRecordedPRPlan` keeps consulting every mode: its branch-to-plan direction is correct for review runs too. Either way the branch must exist locally. `readProgressAssociations` is the shared scan behind both this plan-to-branch lookup and `findRecordedPRPlan`'s branch-to-plan lookup. The plan-to-branch lookup scans every root `progressRecordRoots` returns, which is every registered worktree: the primary first, the invoking checkout second, then the rest, deduplicated through `sameProgressRoot`. The progress logger resolves `.loopai/progress` against the working directory before loopai changes into a worktree, so a run started from the primary records there even though it executed in a linked worktree, while a run started inside any linked worktree records in that worktree. Restricting the scan to the primary and the invoking checkout misses a run started in a third worktree, and a miss falls back to the filename derivation the record exists to override. The newest matching record across all roots wins. The branch-to-plan lookup behind PR metadata stays anchored at the invoking checkout, since its result feeds `readPRPlan`'s containment check and a record naming a plan in another worktree would turn a stats-only body into a hard failure. `recordedBranchForPlan` matches a record to a located plan by case-folded filename via `planAssociationKey`, not by absolute path, so the association survives a case-insensitive filesystem returning the caller's spelling, a record naming the `completed/` copy of a plan still present in the active directory, and a lookup running in a different worktree than the record. It matches the path the record names rather than that path's repo-contained resolution, so an out-of-tree `plans_dir` or a checkout moved since the run still supplies the branch; resolution inside the repository is required only by the PR-metadata consumer, which skips associations whose plan resolves nowhere. Missing the association is the dangerous direction, since it silently falls back to the filename derivation the record exists to override. `recordedPlanInRepo` re-anchors a recorded path at the root when it resolves inside the repository only through symlinks, which a purely lexical containment test rejects wherever the checkout sits behind one; it still refuses symlinked plans and paths genuinely outside the repository. Plan lookup, for both resolution and PR metadata, anchors `cfg.PlansDir` at the invoking checkout's root through `plansDirPath`, so a plan present only inside the unmerged feature worktree is invisible from the primary checkout. Filename derivation goes through `git.Service.EffectiveBranchName`, which resolves the plan's real on-disk filename case, so the derived name matches the one worktree creation produced on case-insensitive filesystems. A feature equal to the base is an error. So is a surplus positional: `main` records `args[1:]` through `applyPositionalArgs` and `validateCloseoutFlags` rejects them, because `--merge <base> <feature>` is the `--merge=<base> <feature>` form with the `=` forgotten, go-flags hands both tokens back as positionals, and silently taking the first would merge and delete the intended base branch. Merge uses the registered base worktree, falling back to the primary worktree when the base branch is checked out nowhere, requires clean feature and merge worktrees, overrides branch-level squash/no-commit defaults, verifies the feature commit became an ancestor of the base, never force-removes a close-out worktree, aborts conflicts as `git.ErrMergeConflict`, and deletes the feature branch only after verified cleanup. `prepareMergeWorktrees` returns a `mergeTargets` value covering three cases: the feature is the current checkout, it lives in another registered worktree, or it is checked out nowhere and the merge runs in the base worktree with worktree cleanup skipped entirely. A registered worktree whose directory was deleted by hand fails with prune guidance instead of a raw chdir error, since Git still treats its branch as checked out until `git worktree prune`. Feature-worktree cleanliness is validated before the merge, not only before cleanup, and `git.Service.BranchHash` reads the feature head without a checkout. Ignored files do not make a worktree dirty and are deleted when the linked worktree is removed, so callers must preserve any local-only ignored files before `--merge`. The success line names the removed worktree, resolved through `mergeTargets.removableWorktree`, because with an explicit feature the removal target comes from the worktree list rather than being the caller's own directory. PR creation requires every effective `origin` push URL to identify the same GitHub repository used by `gh`, pushes committed state, and derives metadata from an exact associated plan (including the active-plan fallback used by worktree runs). With an explicit feature it needs no checkout: `git.Service.BranchDiffStats` measures the branch tip against the base through `refs/heads/<branch>` so a same-named tag cannot shadow it, and `gh pr create --head <branch>` targets the resolved branch. Plan discovery never returns a path outside the repository root, so an out-of-tree `plans_dir` or recorded plan degrades to a stats-only PR body instead of failing `readPRPlan`'s containment check. Tests use local bare remotes, Git push stubs, and `PATH`-injected `gh` stubs.

`cmd/loopai` resolves effective plan, task, review, and external-review models
and passes them to `cmux.Reporter`. Phase labels come from `status.PhaseHolder`;
review iteration labels come from `cmux.Reporter.WrapLogger`, which observes
structured `PrintSection` calls while forwarding the complete logger interface.
Normal execution paths construct a setup `orca.Reporter` after config loading and retain it
through dependency checks, startup prompts, plan selection, branch/worktree setup, and progress
logger creation. Review-only modes deliberately defer that reporter until after optional plan
selection and the empty-range preflight, so repository validation, base resolution, and any initial
commit prompt before the guard have no Orca title reporter. Interactive plan creation and
execution/review then construct phase-specific reporters, publish the replacement's working title,
and silently quiesce the setup predecessor;
watch-only,
agent-generation, and standalone utility paths do not construct reporters. `orca.New` is gated by
both the resolved config and a stdout TTY check and returns a nil-safe no-op when inactive. Phase
changes come from `status.PhaseHolder`, task and iteration detail from `WrapLogger`, and
human waits from `WrapInput` or `WithInputWait`. `Finish` freezes a persistent done/failed
title, while `Stop` emits the bare idle title for aborts, declined continuation, and other
non-completions. OSC writes are best-effort single writes to stdout and never fail the run.

Detached runs (`cmd/loopai/detach_*.go`, `runs_cmd*.go`, `pkg/runs`) are how a plan outlives the
terminal, agent turn, T3 Code provider, or T3 Code session that started it; T3 Code is only a window
onto them. `validateDetachFlags` keeps `--detach` to exactly one plan in full or tasks-only mode (no
chain, `--commit`, review-only or plan-creation mode, `--serve`/`--watch`, `--cmux-workspace`, or
standalone command), always executed in worktree mode. `runPreConfigCommand` refuses it inside an
agent sandbox (`detectSandbox`: `SANDBOX_RUNTIME`, `CODEX_SANDBOX`, `CODEX_SANDBOX_NETWORK_DISABLED`)
before config loading, whose defaults installer would otherwise fail first with an unrelated write
error; the launcher itself is routed after config through `runConfiguredStandaloneCommand`.
`launchDetached` checks the plan and executable, requires the repository root, takes the launch lock
`<runs dir>/.launch-<hash(plan, branch)>.lock` for at most `detachLaunchLockTimeout`, and decides
through `runs.LatestFor`, which ignores that lock's own tier so a crashed run reads `crashed`: a live
latest run is refused; one that did not succeed is resumed (outcome archived as
`outcome-attempt-<n>.json`, stop marker cleared, attempt incremented, recorded origin kept unless the
launch names a thread); otherwise `refuseLeftWorktree` rejects a succeeded run whose branch is still
checked out, and a new directory is created with `os.Mkdir`, never reused, before `PruneSucceeded`
keeps the newest 20 succeeded runs. The descriptor records `stripDetachArgs(os.Args[1:])`, which drops
`--detach`, `--detached-run`, `--t3-thread`, `--acp-thread`, and `--acp-session` in both value forms,
and the set `cmuxEnvOptions` as `env_options`, on every launch, because Mode 2 relaunches from them.
The child is the hidden `--detached-run <id>`, started by `startDetachedProcess` with `Setsid`, stdin
on the null device, stdout and stderr appended to `console.log` after a per-attempt separator, and an
environment `detachScrubbed` cleans of `T3_ACP_MCP_*`, `LOOPAI_T3_TOKEN`, and `CMUX_*`, behind
`detachScopePrefix` (`systemd-run --user --scope --quiet --collect --` where a Linux user manager
exists; a scoped child that ends before answering is retried once without the scope). Windows refuses
every detached command, and with it every run a loopai thread asks for, through
`errDetachUnsupported`, while the code builds.

The handshake uses two pipes passed as `ExtraFiles`: fd 3 carries the launcher's one line
`{"t3_token":"…"}` (empty without `--t3-thread`), fd 4 exactly one child line,
`{"ok":true,"run_id":…,"attempt":…}` or `{"ok":false,"error":…}`; success is that line, never EOF.
Inherited `ExtraFiles` descriptors lack `FD_CLOEXEC`, which is also how `checkHandshakeFD` tells them
from the pipe the Go runtime itself opens on fds 3 and 4 on Darwin; the child marks both close-on-exec
before anything else, so no provider or Git process holds the launcher's pipes. The launcher waits
`detachAckTimeout` (120 s), detects a dead child through fd-4 EOF because `Process.Release` precludes
`Wait`, and prints `run:`/`follow:` on success or the console tail on failure. `detachedChild.run`
takes the run's lifetime lock (`run.lock`, separate from the worktree run lock), records its pid and
`runs.ProcessStartToken`, loads config through `loadDetachedConfig` (worktree mode set on `o` before
loading, `cfg.Orca` off), builds the request through `prepareNonInteractiveRequest`
(`executePlanRequest.NonInteractive`: a missing plan is an error instead of a prompt, and no pause
handler, break signal, or cmux or Orca reporter is installed; a nil-stdin `executionIO` makes an empty
repository an error), records launch history, and executes. An error before the attempt starts becomes a failed outcome and the `ok:false`
answer. SIGHUP is caught with `signal.Notify`, not ignored, because an ignored disposition is
inherited by every process the run starts.

`executePlanRequest` carries the detached-run knobs. `AttemptStarted` fires in `fireAttemptStarted`
once the worktree and its progress log exist; the child's hook records the branch, worktree, and
progress log in the descriptor, appends `attempt_start`, and only then answers ok. A failed answer is
only reported on the console log, so a launcher that stopped waiting never ends a 40-hour run; a failed
descriptor or journal write fails the hook. `KeepWorktreeOnFailure` keeps the worktree on every exit
after a successful hook except a clean success: `keepWorktreeDecision` decides once, memoized, before
removal, and `keepDetachedWorktree` also keeps a success whose finalize close-out or plan archive
stopped short or whose tree is dirty or has an operation in progress, because removal is
`git worktree remove --force`. Without the hook, or when it fails, a fresh worktree is removed as
before and a resumed one is kept. A review stalemate or review max-iterations is not a failure: the
post-review loop commits leftovers on the branch. `ArchiveOnRunBranch` archives the single plan
through the run's own Git service and plan copy, as a chain does, so nothing is committed into the
invoking checkout through `MainGitSvc`; `captureDetachedSourcePlan` and
`reconcileDetachedSourcePlan` (`source-plans.json`) make that checkout's unchanged plan copy
merge-ready after success. `planExecutionOutcome.recordFinalize`, `recordArchive`, and
`recordWorktreeKept` feed `complete`, which classifies the attempt (exit codes 0 succeeded, 1 failed,
2 stopped), fills the commit, writes `report.md`, appends the closing journal events (noting every
failed append), reconciles, writes `outcome.json` (report capped at 256 KiB, summary at 4 KiB), and
runs `beforeExit` last.

`pkg/runs` is the registry under `<checkout>/.loopai/progress/runs/<run-id>/`, with run ids
`<plan stem>-<YYYYMMDD-HHMMSS>-<4 hex>` and no file matching the dashboard's `progress-*.txt`.
`Classify` probes the launch lock (`starting`), then the lifetime lock (`running`), then the outcome
(`succeeded|failed|stopped`), else `crashed`; a probe never blocks a run. `Journal` appends
`events.jsonl` (`attempt_start`, `phase`, `task`, `tasks`, `review`, `finalize`, `report`, `finish`);
`journalHooks` (`detach_journal.go`) fills `LogDecorator` with section events and `PhaseObserver` with
phases, the limit wait, and task statuses re-read from the worktree plan, and queues `progressLogger`
lines until the decorated logger exists. `Snapshot`/`Fold` is the only structured state source for
`--attach`, the Mode 1 bridge, and the Mode 2 renderer: an external review block entered from the task
phase is a per-task block that leaves the stages alone, a stage never reopens, and entering one
completes every earlier one. `Follow` ends on the lifetime lock and outcome; `FollowUntilFinish` ends
on the attempt's `finish` event, for a reader inside the run process, which holds the lock itself.
`--attach`, `--runs`, and `--stop` (`runs_cmd.go`) read the runs of `runsRoot`: the root of the repository
or worktree holding the working directory, found through `git.NewService` with `vcs_command`, so they work
from a subdirectory and refuse outside a repository instead of answering "no detached runs".
`--attach` replays at most `consoleReplayLimit` (1 MiB) of `console.log`, from the first line start in
that tail after an `(earlier output omitted; full log: <path>)` line, since the bridge re-types it after
every T3 restart and a 40-hour log would otherwise pass through the terminal whole each time.
`stopRun` rereads the descriptor and, only while the lifetime lock is held and the recorded process
still carries its start token, writes `stop-requested` and then sends SIGTERM to that process alone;
the marker comes first so a run killed by its own force exit classifies as stopped. `--stop` then
waits `stopWaitTimeout`. `refuseLiveDetachedRun` (`detach_guard.go`), called from `runMergeCommand`
and `runPRCommand` once the feature is resolved, checks `runs.LiveRunForBranch` under every
`progressRecordRoots` root, then probes the feature worktree's run lock without blocking, which also
catches a foreground `--worktree` run.

Mode 1 (`--t3-thread <id>`, `cmd/loopai/t3_bridge.go`) needs `LOOPAI_T3_TOKEN`, read only by the
launcher (`detachT3Token`) and handed over fd 3. The child moves it into the bridge's `t3.Client` and
keeps no copy, so it never reaches a file, log, descriptor, outcome, journal, or the environment of the
processes the run spawns. Thread ids are opaque (`^[A-Za-z0-9_.:%-]{1,1024}$`, the length checked
through `maxT3ThreadIDLen` because Go's regexp caps a repeat count at 1000), sent unchanged and always
single-quoted in shell text. `pkg/t3` speaks T3 Code's orchestration protocol 2:
`/ws?orchestrationProtocol=2` with `Authorization: Bearer`, plain-JSON RPC frames with `headers: []`,
an `Ack` after every stream chunk (the server stalls otherwise), and a fresh `commandId` per command;
the origin comes from `<T3 home>/userdata/server-runtime.json`, re-read on every reconnect, or
`LOOPAI_T3_URL`, and a 426 or undecodable reply is the typed `t3.ErrProtocolUnsupported`. The bridge
follows the journal with `FollowUntilFinish`, writes `[loopai: <state>]` title prefixes on transitions
only (`thread.metadata.update` carrying the title alone, read first so the user's name survives, the
first one after waiting up to about 2 minutes for a pending `titleRegeneration`), pins only an
unpinned thread and unpins only what it pinned (the pin is what stops the 3-day auto-settle), sends
`thread.mark-unread` on every finish as its last command, after the final title and the unpin, because
T3 Code derives the sidebar's "Done" badge from the latest run's `completedAt` versus `lastVisitedAt` and
the title and unpin advance `updatedAt`, which gets an open thread visited again (`syncUnread` treats a
typed `*t3.RemoteError`, such as no completed run while a new agent turn runs, as done), and opens
terminal `loopai-<run id>` with the UI's launch context (`thread.worktreePath ?? project.workspaceRoot`,
the two `T3CODE_*` variables, never `providerInstanceId`), typing
` cd '<checkout>' && '<loopai>' --attach <id>` — never through `exec` and never with `; exit`. After a
server restart it reopens an absent terminal once per server start unless a `remove` was seen while
connected, and it re-types only into a terminal idle for 60 s with nothing typed for 90 s after the
previous command was seen running. A typed error answering a terminal request, or an unlisted project,
stops only the terminal half (`failTerminal`); titles, pin, unpin, and mark-unread go on. Other `t3BridgeFatal`
errors (401, protocol, remote) disable it with one progress-log line; the run is unaffected. Unless the
error is a refused connection (`t3BridgeUnreachable`), `closeOut` first makes a best-effort last write:
the final title of a finished attempt or the title without the prefix, then the unpin of a thread the
bridge pinned, since a left pin blocks T3's auto-settle, then the mark-unread of a finished attempt. The child
stops it through `beforeExit`, bounded by `t3BridgeStopTimeout`, so the final title, unpin, and mark-unread
are sent. `pkg/t3/t3test` is an importable fake
protocol-2 server enforcing the query parameter, the Bearer header, request headers, and ack-gated
streams.

Mode 2 (`--acp`, `cmd/loopai/acp_*.go`, `pkg/acp`) is a provider for T3 Code's `acpRegistry` driver.
`cmd/loopai-acp` is argument-free: it resolves `loopai` from `LOOPAI_ACP_LOOPAI`, a sibling
(`loopai.exe` only on Windows), or `PATH`, and on Unix `syscall.Exec`s `loopai --acp`, staying one
stable process in the process group T3 Code leads and signals as a whole (SIGTERM, then SIGKILL a
second later); Windows starts it with inherited stdio and returns its exit code. `runPreConfigCommand`
routes `--acp` before config, since each session has a cwd of its own and probes must read no config;
`validateACPFlags` keeps it standalone. `main` prints the version banner to stderr when `acpRequested`
sees the flag, `redirectStdioForACP` reserves the real stdin and stdout for JSON-RPC and points
`os.Stdin` at the null device and `os.Stdout`/`color.Output` at stderr, and `catchSIGPIPE` turns a
write to a gone client into EPIPE. `pkg/acp` is `Conn` (newline-delimited JSON-RPC 2.0, 16 MiB lines,
ids kept as `json.RawMessage`), `Provider` (`initialize` answers protocol 1 with `loadSession: true`,
no `info` key, which would switch T3 Code to v2 methods, and no `mcpCapabilities.acp`, which would make
it send an MCP entry without a URL; `session/load` always succeeds; handler panics are recovered, so the
provider never exits by itself during a held turn, and it exits promptly on SIGTERM or stdin EOF),
`ParsePrompt`/`IsContinuation` (the `<user_request>` envelope, the text after the last
`User message:`, and `Continue where you left off.` with or without a restart note or handoff block),
and the `session/update` encoders. `acpIsProbe` recognizes T3 Code's background connections
(`t3-code-provider-test`, `t3-code-session-manager`, `t3-code-provider-auth`, or no MCP servers and no
`T3_ACP_MCP_*`), which read no config and write nothing. `openSession` looks the thread id up through
the session's MCP credential (`t3.MCPCredentialFromServers` from the `t3-code` entry's `env` array,
else the spawn-time `T3_ACP_MCP_*`; `t3.MCPClient` over streamable HTTP, `t3_thread_list` →
`currentThreadId`, bounded by `acpLookupTimeout`), never logging header values. Bindings
(`acp_binding.go`) live in `<session cwd>/.loopai/acp/`: `binding-thread-<runs.ThreadKey(id)>.json`,
named by a hash because ids exceed 300 characters and carry `:` and `%`, and
`binding-session-<session id>.json` as the fallback, because T3 Code replaces the session id when
`session/load` falls back. `requireBindingStore` refuses a relative or missing cwd and a symlinked
`.loopai` or `.loopai/acp` while accepting a symlinked cwd, and every write first runs
`git.EnsureRuntimeDirectoryIgnored`, because `.loopai/.gitignore` is never committed and a T3 Code
worktree lacks it. `acpTurn.route` (`acp_route.go`) applies, in order: a bound live run, `starting`
included, is attached whatever the text says, a bound run that ended without success is relaunched on
continuation text or `resume`, and a bound run whose directory is gone is unbound once; a message whose
first word is a `.md` path (`parseACPPlanMessage`: the three pass-through flags only, checked by
`validatePassThroughValues`) launches that plan through `launchACPRun`, which runs
`loopai --detach --acp-thread=<id> --acp-session=<sid> <args>` in the session cwd with `acpLaunchEnv`
(scrubbed, recorded environment options restored on a relaunch), and binds it; without a binding, a
continuation adopts the single live `t3-thread` run of the same cwd whose `attach.lock` is free,
binding only once it holds that lock; anything else gets the status reply. `follow` holds the run's
`attach.lock` and one `session/prompt` open for the whole run. `acpRenderer` (`acp_render.go`) sends
plan entries and one `kind: other` tool call per phase on transitions only, plus a liveness retitle
every `acpLivenessInterval` (20 minutes), never reasoning or output, since T3 Code stores every update
and never compacts them, and a final message capped at `acpFinalMessageLimit` (12,000 JSON-escaped
bytes, within the model-switch handoff) before `end_turn`, which every finished run gets. Stop
(`acp_stop.go`) answers `cancelled` at once and arms a stop for `acpStopGrace` (5 s) measured from that
answer; a new prompt of the session or the provider shutting down drops it. After the grace
`acpThreadInterrupted` (MCP `t3_thread_read`, at most `acpStopGuardTimeout`) checks that T3 Code
recorded a `run_interrupt_request`, which a steer or T3 Code's own cancel lacks, and a failed read
stops anyway; `stopRun` then runs only on a live run of origin `t3-thread` whose binding key matches
(`acpOwnsRun`). The provider reports status only: no chat, merge, or pull request. These contracts come
from T3 Code 0.1.1's source rather than a documented API; `docs/t3-code.md` lists the checks still
open against a live T3 Code.

`cmd/loopai`'s `TestMain` replaces `newAwakeHolder`, `detachScopePrefix`, `newT3Bridge`,
`lookupACPThreadID`, `acpThreadInterrupted`, and `acpLaunchDetached`, and unsets `LOOPAI_T3_TOKEN` and
`LOOPAI_T3_URL`, so no test reaches a T3 Code server, a systemd user manager, or a real inhibitor;
detached-run tests re-exec the test binary through `detachExecutable` and `detachChildArgs`.

Keep `cmux.Reporter.WrapLogger` in the logger chain after dashboard setup. The
Orca title wrapper sits below the cmux wrapper, and the keep-awake wrapper between Orca and
`progress.SectionTimer`, which in turn sits above the dashboard broadcast logger.
`executePlanRequest.LogDecorator`, a detached run's journal hooks, wraps the runner logger directly
below `progress.SectionTimer`, so it sees sections and the timer's timing lines; `PhaseObserver`
subscribes beside the cmux, orca, and keep-awake observers. This preserves cmux's
outermost rate-limit interfaces while timing structured sections. Orca's
limit-wait title comes from its `status.PhaseHolder` observer, not from the logger
chain. `progress.ValidationTimer` receives that wrapped runner
logger, filters executor command-timing events against the plan's `## Validation
Commands`, and writes per-run and aggregate `validation:` lines through the same
chain. Its aggregate sums command durations, so concurrent commands can overlap
and the total can exceed section or run wall-clock time. Use
`runWithSectionTiming` for both the main runner and interactive plan-creation
paths; it calls `SectionTimer.FinishRun` immediately after `Runner.Run` returns.
The main execution path then calls `ValidationTimer.FinishRun` before handling
the run error. Do not defer either finish because dashboard shutdown can close
the underlying log first. Plan-creation mode has no validation timer. A nil
reporter must return the section timer unchanged.

Executor display streams are filtered by provenance, not text shape. Codex stderr forwards only the
first run's resolved `model:`/`sandbox:`/`reasoning effort:` header lines; reasoning titles come from
the rollout file's typed `reasoning` records through `formatParsedRolloutEvent`, because codex 0.144+
echoes loaded skill and tool markdown onto stderr and a skill header such as `**Detect stale base:**`
is indistinguishable from a real bold reasoning title. Only the first line of each summary is shown.
Claude Task subagents stream as `system/task_started` and `system/task_progress` events with no text
block; `subagentLine` turns their `description` into an indented heartbeat sent to `OutputHandler`
only, never into the output, recent text, or signal detection. Titles are unthrottled, per-step
progress is limited to one line per `subagentProgressInterval` (10s).

Claude pattern classification is by provenance. `parseStream` keeps three views: full surfaced
`Output`, a bounded `RecentText` window, and a bounded `DiagnosticText` window filled by
`extractDiagnostic` from self-authenticating error records (`is_error`, `terminal_reason:"api_error"`,
non-zero `api_error_status`), assistant API-error messages, `system` error subtypes, `error` events,
and non-JSON CLI lines. Ordinary assistant text, successful result summaries, `api_retry` telemetry,
and subagent heartbeats never enter it. `resolveRunResult` always matches `DiagnosticText` and adds
`RecentText` only on a non-zero exit or stream error; signal absence is deliberately not a promotion
trigger, because a successful review path B emits no marker. This matters more here than upstream:
`wait_on_limit` defaults to 10m and a limit match drives `claude-swap` failover, so a quoted limit
phrase in narration would otherwise loop forever or rotate accounts for nothing. A non-JSON limit
line with a non-zero exit, as `You're out of usage credits` arrives, still classifies.

Claude command timing pairs foreground Bash `tool_use` and `tool_result` events
by tool-use ID and measures their arrival times; background Bash calls are
omitted because their first result is not process completion. Codex accepts both
legacy `exec_command` function calls and current custom `exec` rollout records,
follows yielded sessions through continuation/wait events, and tails child-agent
rollouts, including child rollouts stored on the next calendar day. For output-only
custom records, it uses valid native timestamps to prove
that a command returned before its configured yield threshold; calls at or beyond
the threshold attach to a later session continuation only when the association is
unique. Ambiguous batches are omitted rather than paired by order. It otherwise
prefers valid native event timestamps and falls back to arrival times; the fallback
is approximate because the final drain can deliver buffered events late. Executors
report completed shell commands; `ValidationTimer` alone performs classification
and logs the canonical configured label rather than raw provider arguments.
Unpaired commands and providers that omit tool events produce no timing lines.
Current custom `exec` records expose nested commands only as model-generated
JavaScript, so their parser is deliberately heuristic and best-effort rather
than a complete JavaScript grammar. Unknown or ambiguous source shapes are
omitted.

## Code style

- Use idiomatic Go and keep packages focused.
- Comments are lowercase except exported godoc.
- Wrap errors with context using `fmt.Errorf("context: %w", err)`.
- Define small interfaces at the consumer.
- Prefer existing dependencies; discuss new dependencies first.
- Use table-driven tests with `testify`.
- Test filesystem operations with `t.TempDir()`.
- Put tests in one corresponding `_test.go` file per source file.
- Generated mocks use `moq`; do not hand-edit them.
- Keep behavior cross-platform and use `filepath` for paths.

## Testing

After any code change:

```bash
make test
make lint
```

The full suite is required because configuration and progress paths cross package boundaries.
The Go suite assumes a POSIX host: many `cmd/loopai` tests use `/bin/sh` stubs and several packages test
file permissions, so on Windows run `make test` in WSL. Git fixtures that assert exact file contents
set `core.autocrlf=false` so a system-wide Git for Windows setting cannot change them.
`cmd/loopai`'s `TestMain` resolves `TMPDIR` through `filepath.EvalSymlinks`, so on macOS
(`/var` -> `/private/var`) fixture paths match the symlink-resolved roots Git reports; those tests
therefore do not cover a checkout reached through a symlink (see
`docs/backlog/symlinked-checkout-path-containment.md`).
`make test` first validates Claude skill assets, plugin manifests, and the Codex
skill tree, runs their regression suites and shell-completion checks, then runs
the race-enabled Go suite with coverage and every retained provider-wrapper and wrapper-documentation
shell suite. The asset and manifest checks require Bash and `jq`; the focused
`test-grill-skill` suite checks the grill skill's metadata and operational
contracts. CI runs the same focused asset, manifest, grill-skill, completion,
and wrapper checks.

Dashboard e2e:

```bash
make e2e-setup
make e2e
make e2e-ui
```

Cross-compile when changing platform-sensitive code:

```bash
GOOS=windows GOARCH=amd64 go build ./...
```

Before tests involving configuration, snapshot or otherwise verify that real `~/.config/loopai/` and legacy `~/.config/ralphex/` content is unchanged.

## Development workflow

1. Read the relevant source, tests, embedded defaults, and plan.
2. Make the smallest cohesive change.
3. Add or update tests with the implementation.
4. Run focused tests while iterating.
5. Run `make test` and `make lint`.
6. Update user and developer documentation when behavior changes.
7. Commit a focused change with a meaningful message.

Do not edit `CHANGELOG.md` during normal development; it belongs to the release process.

Do not weaken tests to make a change pass. Do not skip validation between plan tasks. Preserve unrelated user changes in a dirty worktree.

## End-to-end smoke test

Prepare a toy repository and binary:

```bash
make e2e-prep
cd /tmp/loopai-test
.bin/loopai docs/plans/fix-issues.md
tail -f .loopai/progress/progress-fix-issues.txt
```

Review an existing branch:

```bash
make e2e-review
cd /tmp/loopai-review-test
.bin/loopai --review
tail -f .loopai/progress/progress-review.txt
```

Smoke-test Claude findings with Codex evaluation and fixes:

```bash
make e2e-codex
cd /tmp/loopai-review-test
.bin/loopai --task-model codex --external-only
tail -f .loopai/progress/progress-codex.txt
```

## Pull-request checklist

- New behavior has tests, including error cases.
- `make test` passes.
- `make lint` reports no issues.
- Documentation and embedded config comments match behavior.
- Any Claude skill change bumps both manifest versions to the same value.
- A changed contract surface (`TestSkillContract`) either bumps `skillContract` and every
  skill's `expected=`, or is deliberately accepted through `-update` without a bump.
- A new or removed Claude skill has a matching change under `assets/codex/skills/`
  or an explicit `exempt_skills` entry in `scripts/check-codex-skills.sh`.
- No test touched real user configuration.
- The module path and `<<<RALPHEX:...>>>` signals remain unchanged.
- `CHANGELOG.md` is untouched unless release work explicitly requires it.
