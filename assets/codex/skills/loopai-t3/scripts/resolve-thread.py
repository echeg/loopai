#!/usr/bin/env python3
"""Resolve the parent Codex identity through one local T3 runtime, read-only."""

import argparse
import json
import os
from pathlib import Path
import sqlite3
import sys


class ResolutionError(Exception):
    """A missing or uncertain identity must leave progress titles disabled."""


def resolve_thread(t3_home, codex_thread_id):
    if not codex_thread_id or not codex_thread_id.strip():
        raise ResolutionError("parent CODEX_THREAD_ID is unavailable")

    state_dir = Path(t3_home).expanduser().resolve() / "userdata"
    try:
        runtime = json.loads((state_dir / "server-runtime.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise ResolutionError("T3 server runtime is missing or unreadable") from None
    if not isinstance(runtime, dict) or type(runtime.get("version")) is not int or runtime["version"] != 1:
        raise ResolutionError("T3 server runtime format is unsupported")
    origin = runtime.get("origin")
    if not isinstance(origin, str) or not origin.strip():
        raise ResolutionError("T3 server runtime origin is unavailable")
    override = os.environ.get("LOOPAI_T3_URL", "").strip().rstrip("/")
    if override and override != origin.strip().rstrip("/"):
        raise ResolutionError("LOOPAI_T3_URL differs from the parent T3 runtime")

    database = state_dir / "state.sqlite"
    matches = []
    try:
        # mode=ro both prevents writes and refuses to create a missing database.
        connection = sqlite3.connect(database.as_uri() + "?mode=ro", uri=True, timeout=2)
        try:
            rows = connection.execute("""
                SELECT runtime.thread_id, runtime.resume_cursor_json
                FROM provider_session_runtime AS runtime
                JOIN projection_threads AS thread ON thread.thread_id = runtime.thread_id
                WHERE runtime.provider_name = 'codex'
                  AND thread.deleted_at IS NULL
                  AND thread.archived_at IS NULL
            """)
            for thread_id, raw_cursor in rows:
                try:
                    cursor = json.loads(raw_cursor)
                except (TypeError, ValueError):
                    continue
                if isinstance(cursor, dict) and cursor.get("threadId") == codex_thread_id:
                    matches.append(thread_id)
        finally:
            connection.close()
    except sqlite3.Error:
        raise ResolutionError("T3 identity database is missing, unreadable, or unsupported") from None

    if not matches:
        raise ResolutionError("no active T3 thread matches the parent Codex identity")
    if len(matches) != 1:
        raise ResolutionError("parent Codex identity matches multiple T3 threads")
    thread_id = matches[0]
    if not isinstance(thread_id, str) or not thread_id or any(char.isspace() for char in thread_id):
        raise ResolutionError("matching T3 thread identity is invalid")
    return thread_id


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex-thread-id", help="parent native identity; defaults to CODEX_THREAD_ID")
    parser.add_argument("--t3-home", help="T3 home; defaults to T3CODE_HOME or ~/.t3")
    args = parser.parse_args()
    codex_thread_id = args.codex_thread_id if args.codex_thread_id is not None else os.environ.get("CODEX_THREAD_ID")
    home_override = args.t3_home if args.t3_home is not None else os.environ.get("T3CODE_HOME", "")
    t3_home = home_override.strip() or Path.home() / ".t3"
    try:
        thread_id = resolve_thread(t3_home, codex_thread_id)
    except (ResolutionError, OSError, ValueError) as error:
        message = str(error) if isinstance(error, ResolutionError) else "T3 identity lookup failed"
        print("loopai: progress titles unavailable: " + message, file=sys.stderr)
        return 1
    print(thread_id)
    return 0


if __name__ == "__main__":
    sys.exit(main())
