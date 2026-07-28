# codex-as-claude

Wraps the Codex CLI to produce Claude-compatible `stream-json` output, allowing Codex to be used as a drop-in replacement for Claude Code in loopai task and review phases.

## How it works

The script translates Codex JSONL events into Claude's `stream-json` format that loopai's `ClaudeExecutor` can parse. It extracts the prompt from `-p` flag and ignores all other Claude-specific flags gracefully.

Event mapping:

| Codex event | Claude event | Notes |
|---|---|---|
| `item.completed` (agent_message) | `content_block_delta` (text_delta) | Always emitted |
| `item.completed` (command_execution) | `content_block_delta` (text_delta) | Only when `CODEX_VERBOSE=1` |
| `turn.completed` | `result` | End of execution |
| Other events | Skipped | |

## Configuration

Add to `~/.config/loopai/config` or `.loopai/config`:

```ini
claude_command = /path/to/scripts/codex-as-claude/codex-as-claude.sh
claude_args =
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `CODEX_MODEL` | (codex default) | Model to use |
| `CODEX_SANDBOX` | `danger-full-access` | Sandbox mode |
| `CODEX_VERBOSE` | `0` | Set to `1` to include command execution output |

## Requirements

- `codex` CLI installed and configured
- `jq` for JSON translation
