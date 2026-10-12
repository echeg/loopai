#!/usr/bin/env python3
"""Own one CLI run and preserve its real exit for a native T3 observer."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
from datetime import datetime, timezone


def timestamp():
    return datetime.now(timezone.utc).isoformat()


def run_directory(repo, plan):
    key = hashlib.sha256(str(plan).encode()).hexdigest()[:20]
    return repo / ".loopai" / "progress" / ("t3-current-" + key)


def write_state(path, state):
    temporary = path.with_suffix(".tmp")
    with temporary.open("w", encoding="utf-8") as stream:
        json.dump(state, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)


def observe(repo, plan, observer_id, command):
    directory = run_directory(repo, plan)
    directory.parent.mkdir(parents=True, exist_ok=True)
    try:
        # Atomic reservation prevents two observers from launching the same plan.
        # Keep it after exit/crash: retry is an explicit, reviewed operation.
        directory.mkdir(mode=0o700)
    except FileExistsError:
        print(f"existing run: {directory}; inspect or reattach, do not relaunch", file=sys.stderr)
        return 2
    (directory / ".gitignore").write_text("*\n", encoding="utf-8")
    state_path = directory / "state.json"
    log_path = directory / "output.log"
    state = {
        "version": 1,
        "repository": str(repo),
        "plan": str(plan.relative_to(repo)),
        "observer_id": observer_id,
        "wrapper_pid": os.getpid(),
        "cli_pid": None,
        "command": command,
        "status": "starting",
        "started_at": timestamp(),
        "finished_at": None,
        "exit_code": None,
        "log": str(log_path),
    }
    write_state(state_path, state)
    child = None
    requested_signal = None

    def forward(signum, _frame):
        nonlocal requested_signal
        requested_signal = signum
        if child is not None and child.poll() is None:
            try:
                if os.name == "posix":
                    os.killpg(child.pid, signum)
                else:
                    child.send_signal(signum)
            except ProcessLookupError:
                pass

    previous_handlers = {}
    for signum in (signal.SIGINT, signal.SIGTERM):
        previous_handlers[signum] = signal.signal(signum, forward)
    try:
        with log_path.open("wb") as log:
            try:
                child = subprocess.Popen(
                    command, cwd=repo, stdin=subprocess.DEVNULL,
                    stdout=log, stderr=subprocess.STDOUT, start_new_session=True,
                )
            except OSError as error:
                state.update(status="launch_failed", error=str(error), finished_at=timestamp())
                write_state(state_path, state)
                print(f"launch failed: {error}; state: {state_path}", file=sys.stderr)
                return 127
            state.update(status="running", cli_pid=child.pid)
            write_state(state_path, state)
            print(f"running: pid={child.pid}; state: {state_path}; log: {log_path}", flush=True)
            if requested_signal is not None:
                forward(requested_signal, None)
            exit_code = child.wait()
            log.flush()
            os.fsync(log.fileno())
        outcome = "completed" if exit_code == 0 else "failed"
        if requested_signal is not None or exit_code < 0:
            outcome = "stopped"
        state.update(status=outcome, exit_code=exit_code, finished_at=timestamp())
        write_state(state_path, state)
        print(f"{outcome}: exit={exit_code}; state: {state_path}", flush=True)
        return exit_code if exit_code >= 0 else 128 - exit_code
    finally:
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, type=Path)
    parser.add_argument("--plan", required=True, type=Path)
    parser.add_argument("--observer-id")
    parser.add_argument("--describe", action="store_true")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    repo = args.repo.resolve()
    plan = (repo / args.plan).resolve()
    if not repo.is_dir() or not plan.is_relative_to(repo) or plan == repo:
        parser.error("plan must be inside an existing repository directory")
    if args.describe:
        print(run_directory(repo, plan))
        return 0
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not args.observer_id or not command:
        parser.error("--observer-id and a command after -- are required")
    if not plan.is_file():
        parser.error("plan file does not exist")
    return observe(repo, plan, args.observer_id, command)


if __name__ == "__main__":
    sys.exit(main())
