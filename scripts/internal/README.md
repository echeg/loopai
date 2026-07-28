# Internal Scripts

Development and build utility scripts. Not intended for end users.

- **prep-toy-test.sh** - Creates a toy Go project at `/tmp/ralphex-test` with buggy code and a plan file for end-to-end testing of ralphex's full execution mode.
- **prep-review-test.sh** - Creates a toy Go project at `/tmp/ralphex-review-test` with subtle code issues on a feature branch for testing review-only mode.
- **update-plugin-version.sh** - Updates version in `.claude-plugin/plugin.json` and `marketplace.json`. Called by goreleaser as a pre-release hook.
