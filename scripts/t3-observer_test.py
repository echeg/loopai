#!/usr/bin/env python3
"""Exercise observer ownership, argv preservation, and durable outcomes without models."""

import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "assets/codex/skills/loopai-t3/scripts/observe.py"


class ObserverTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.repo = Path(self.temporary.name).resolve()
        self.plan = self.repo / "docs/plans/plan with spaces.md"
        self.plan.parent.mkdir(parents=True)
        self.plan.write_text("# Plan\n", encoding="utf-8")
        key = hashlib.sha256(str(self.plan).encode()).hexdigest()[:20]
        self.run_dir = self.repo / ".loopai/progress" / ("t3-current-" + key)
        self.base = [sys.executable, str(SCRIPT), "--repo", str(self.repo),
                     "--plan", str(self.plan.relative_to(self.repo))]

    def command(self, code, *args):
        return [*self.base, "--observer-id", "/root/loopai_observer", "--",
                sys.executable, "-c", code, *args]

    def state(self):
        return json.loads((self.run_dir / "state.json").read_text(encoding="utf-8"))

    def run_command(self, command):
        return subprocess.run(command, text=True, capture_output=True, timeout=10, check=False)

    def start_waiting_child(self):
        # Announce readiness only after the child's signal handler is installed.
        code = """
import os, signal, time
from pathlib import Path
signal.signal(signal.SIGTERM, lambda *_: exit(23))
Path('ready').write_text(str(os.getpid()))
while not Path('release').exists():
    time.sleep(0.01)
"""
        process = subprocess.Popen(self.command(code), text=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)

        def cleanup():
            if process.poll() is None:
                process.terminate()
            process.communicate(timeout=10)

        self.addCleanup(cleanup)
        deadline = time.monotonic() + 5
        while not (self.repo / "ready").exists():
            if process.poll() is not None or time.monotonic() > deadline:
                self.fail("observer child did not become ready")
            time.sleep(0.01)
        return process

    def test_success_preserves_argv_output_and_ignored_state(self):
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        literal = "quoted spaces; $(do-not-execute) `nor-this`"
        command = self.command("import json, sys; print(json.dumps(sys.argv[1:])); "
                               "print('stderr evidence', file=sys.stderr)", literal)
        result = self.run_command(command)
        self.assertEqual(result.returncode, 0, result.stderr)
        state = self.state()
        self.assertEqual(state["status"], "completed")
        self.assertEqual(state["exit_code"], 0)
        self.assertEqual(state["observer_id"], "/root/loopai_observer")
        self.assertEqual(state["command"][-1], literal)
        self.assertEqual(state["plan"], "docs/plans/plan with spaces.md")
        self.assertTrue(state["finished_at"])
        output = Path(state["log"]).read_text(encoding="utf-8")
        self.assertIn(json.dumps([literal]), output)
        self.assertIn("stderr evidence", output)
        ignored = subprocess.run(["git", "-C", str(self.repo), "check-ignore", "-q", "--",
                                  str(self.run_dir / "state.json")], check=False)
        self.assertEqual(ignored.returncode, 0)

    def test_nonzero_exit_is_saved_and_propagated(self):
        result = self.run_command(self.command("import sys; print('failure evidence'); sys.exit(7)"))
        self.assertEqual(result.returncode, 7)
        self.assertEqual(self.state()["status"], "failed")
        self.assertEqual(self.state()["exit_code"], 7)
        self.assertIn("failure evidence", Path(self.state()["log"]).read_text())

    def test_spawn_failure_has_durable_outcome(self):
        result = self.run_command([*self.base, "--observer-id", "observer", "--",
                                   str(self.repo / "missing-loopai")])
        self.assertEqual(result.returncode, 127)
        self.assertEqual(self.state()["status"], "launch_failed")
        self.assertIsNone(self.state()["exit_code"])
        self.assertIsNone(self.state()["cli_pid"])
        self.assertIn("missing-loopai", self.state()["error"])

    def test_live_and_finished_runs_cannot_be_replaced(self):
        process = self.start_waiting_child()
        first_state = self.state()
        self.assertEqual(first_state["status"], "running")
        second = self.run_command(self.command("raise RuntimeError('must not start')"))
        self.assertEqual(second.returncode, 2)
        self.assertEqual(self.state(), first_state)
        (self.repo / "release").touch()
        process.communicate(timeout=10)
        self.assertEqual(process.returncode, 0)
        final_state = self.state()
        third = self.run_command(self.command("raise RuntimeError('must not restart')"))
        self.assertEqual(third.returncode, 2)
        self.assertEqual(self.state(), final_state)

    @unittest.skipUnless(os.name == "posix", "POSIX signal forwarding")
    def test_signal_reaches_cli_and_records_stop(self):
        process = self.start_waiting_child()
        process.send_signal(signal.SIGTERM)
        process.communicate(timeout=10)
        self.assertEqual(process.returncode, 23)
        self.assertEqual(self.state()["status"], "stopped")
        self.assertEqual(self.state()["exit_code"], 23)

    def test_describe_remains_read_only_after_plan_archival(self):
        self.plan.unlink()
        result = self.run_command([*self.base, "--describe"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), str(self.run_dir))
        self.assertFalse((self.repo / ".loopai").exists())

    def test_outside_plan_is_rejected_without_runtime_files(self):
        result = self.run_command([sys.executable, str(SCRIPT), "--repo", str(self.repo),
                                   "--plan", "../elsewhere.md", "--describe"])
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.repo / ".loopai").exists())


if __name__ == "__main__":
    unittest.main()
