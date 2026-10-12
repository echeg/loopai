#!/usr/bin/env python3
"""Check exact parent identity resolution against isolated real SQLite databases."""

import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "assets/codex/skills/loopai-t3/scripts/resolve-thread.py"


class ResolverTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name).resolve()
        self.t3_home = self.home / "T3 home #?"
        self.state_dir = self.t3_home / "userdata"
        self.state_dir.mkdir(parents=True)
        self.runtime = self.state_dir / "server-runtime.json"
        self.runtime.write_text(json.dumps({"version": 1, "origin": "http://127.0.0.1:3773"}))
        self.database = self.state_dir / "state.sqlite"
        with sqlite3.connect(self.database) as connection:
            connection.executescript("""
                CREATE TABLE provider_session_runtime (
                    thread_id TEXT PRIMARY KEY, provider_name TEXT, resume_cursor_json TEXT);
                CREATE TABLE projection_threads (
                    thread_id TEXT PRIMARY KEY, deleted_at TEXT, archived_at TEXT);
            """)
        self.env = dict(os.environ, HOME=str(self.home), T3CODE_HOME=str(self.t3_home),
                        CODEX_THREAD_ID="parent-native-id")
        self.env.pop("LOOPAI_T3_URL", None)

    def add_binding(self, t3_id="t3-parent", native_id="parent-native-id", provider="codex",
                    deleted=None, archived=None, cursor=None):
        with sqlite3.connect(self.database) as connection:
            connection.execute("INSERT INTO provider_session_runtime VALUES (?, ?, ?)",
                               (t3_id, provider, cursor if cursor is not None else json.dumps({"threadId": native_id})))
            connection.execute("INSERT INTO projection_threads VALUES (?, ?, ?)", (t3_id, deleted, archived))

    def run_resolver(self, *arguments, env=None):
        return subprocess.run([sys.executable, str(SCRIPT), *arguments],
                              env=self.env if env is None else env, text=True, capture_output=True,
                              timeout=10, check=False)

    def assert_unavailable(self, result):
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("progress titles unavailable", result.stderr)

    def test_exact_environment_identity_is_read_only(self):
        self.add_binding()
        self.add_binding("other-thread", "other-native-id")
        before = {path.name: path.read_bytes() for path in self.state_dir.iterdir()}
        result = self.run_resolver()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "t3-parent\n")
        self.assertEqual(result.stderr, "")
        self.assertEqual(before, {path.name: path.read_bytes() for path in self.state_dir.iterdir()})

    def test_explicit_parent_and_home_override_child_environment(self):
        self.add_binding()
        self.add_binding("t3-child", "child-native-id")
        env = dict(self.env, CODEX_THREAD_ID="child-native-id", T3CODE_HOME=str(self.home / "wrong"))
        result = self.run_resolver("--t3-home", str(self.t3_home), "--codex-thread-id", "parent-native-id", env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "t3-parent\n")

    def test_default_home(self):
        self.add_binding()
        self.t3_home.rename(self.home / ".t3")
        env = dict(self.env)
        del env["T3CODE_HOME"]
        result = self.run_resolver(env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "t3-parent\n")

    def test_whitespace_home_uses_default_for_environment_and_explicit_option(self):
        self.add_binding()
        self.t3_home.rename(self.home / ".t3")
        for value in ["", " \t\n"]:
            with self.subTest(value=value):
                result = self.run_resolver(env=dict(self.env, T3CODE_HOME=value))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "t3-parent\n")
                result = self.run_resolver("--t3-home", value)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "t3-parent\n")

    def test_home_override_trims_outer_whitespace_and_expands_tilde(self):
        self.add_binding()
        for value in [" \t" + str(self.t3_home) + "\n", " ~/" + self.t3_home.name + " "]:
            with self.subTest(value=value):
                result = self.run_resolver(env=dict(self.env, T3CODE_HOME=value))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "t3-parent\n")
                result = self.run_resolver("--t3-home", value)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "t3-parent\n")

    def test_missing_or_empty_native_identity_cannot_guess(self):
        self.add_binding()
        for value in [None, "", " "]:
            with self.subTest(value=value):
                env = dict(self.env)
                if value is None:
                    del env["CODEX_THREAD_ID"]
                else:
                    env["CODEX_THREAD_ID"] = value
                self.assert_unavailable(self.run_resolver(env=env))
        self.assert_unavailable(self.run_resolver("--codex-thread-id", ""))

    def test_no_match_does_not_expose_other_identities(self):
        self.add_binding("private-thread-name", "private-native-identity")
        result = self.run_resolver()
        self.assert_unavailable(result)
        self.assertNotIn("private", result.stderr)

    def test_parent_identity_cannot_target_a_different_server(self):
        self.add_binding()
        matching = dict(self.env, LOOPAI_T3_URL=" http://127.0.0.1:3773/ ")
        self.assertEqual(self.run_resolver(env=matching).stdout, "t3-parent\n")
        for token in ("", "explicit-token"):
            env = dict(self.env, LOOPAI_T3_URL="http://127.0.0.1:9999", LOOPAI_T3_TOKEN=token)
            result = self.run_resolver(env=env)
            self.assert_unavailable(result)
            self.assertIn("differs", result.stderr)
            self.assertNotIn("explicit-token", result.stderr)

    def test_ambiguous_native_identity_is_rejected(self):
        self.add_binding("thread-one")
        self.add_binding("thread-two")
        result = self.run_resolver()
        self.assert_unavailable(result)
        self.assertIn("multiple", result.stderr)
        self.assertNotIn("thread-one", result.stderr)

    def test_wrong_provider_and_archived_deleted_bindings_are_ignored(self):
        self.add_binding("grok-thread", provider="grok")
        self.add_binding("archived-thread", archived="now")
        self.add_binding("deleted-thread", deleted="now")
        self.assert_unavailable(self.run_resolver())
        self.add_binding()
        self.assertEqual(self.run_resolver().stdout, "t3-parent\n")

    def test_invalid_cursors_and_substring_identity_do_not_match(self):
        for index, cursor in enumerate(["not-json", "null", "[]", '{"threadId": 42}',
                                         '{"threadId":"prefix-parent-native-id"}',
                                         '{"other":"parent-native-id"}']):
            self.add_binding("thread-" + str(index), cursor=cursor)
        self.assert_unavailable(self.run_resolver())
        self.add_binding()
        self.assertEqual(self.run_resolver().stdout, "t3-parent\n")

    def test_missing_database_is_not_created(self):
        self.database.unlink()
        self.assert_unavailable(self.run_resolver())
        self.assertFalse(self.database.exists())

    def test_missing_schema_and_corrupt_database_are_rejected(self):
        with sqlite3.connect(self.database) as connection:
            connection.execute("DROP TABLE provider_session_runtime")
        self.assert_unavailable(self.run_resolver())
        self.database.write_bytes(b"not a sqlite database")
        self.assert_unavailable(self.run_resolver())

    def test_missing_or_unsupported_runtime_is_rejected(self):
        self.add_binding()
        self.runtime.unlink()
        self.assert_unavailable(self.run_resolver())
        for content in ["not-json", "null", "[]", "{}", '{"version":2}', '{"version":true}']:
            with self.subTest(content=content):
                self.runtime.write_text(content)
                self.assert_unavailable(self.run_resolver())

    def test_explicit_missing_home_does_not_search_other_databases(self):
        self.add_binding()
        missing = self.home / "missing"
        self.assert_unavailable(self.run_resolver("--t3-home", str(missing)))
        self.assertFalse(missing.exists())

    def test_invalid_output_identity_is_rejected(self):
        self.add_binding("thread\nsecond-line")
        self.assert_unavailable(self.run_resolver())


if __name__ == "__main__":
    unittest.main()
