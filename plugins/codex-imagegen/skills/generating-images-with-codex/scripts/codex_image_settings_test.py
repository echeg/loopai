#!/usr/bin/env python3
"""Runner settings tests using temporary homes and fake credential helpers only."""
import contextlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).with_name("codex_image.py")
SPEC = importlib.util.spec_from_file_location("codex_image_settings_target", SCRIPT)
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class CodexImageSettingsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = Path(self.tmp.name) / "custom-codex-home"
        self.home.mkdir()
        self.prompt = self.home / "prompt.txt"
        self.prompt.write_text("A small blue square.", encoding="utf-8")
        self.environment = mock.patch.dict(os.environ, {"CODEX_HOME": str(self.home)}, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def settings(self, helper=None):
        return {
            "profile": "cliproxy-images",
            "credential": {
                "env": "IMAGE_TEST_API_KEY",
                "command": helper or [sys.executable, "-c", "print('fake-helper-secret')"],
                "timeout": 10,
            },
        }

    def write_settings(self, settings):
        (self.home / "codex-imagegen.json").write_text(json.dumps(settings), encoding="utf-8")

    def run_main(self, *extra):
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(RUNNER, "codex_command", return_value=["fake-codex"]), \
                mock.patch.object(RUNNER, "run_codex") as generation, \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = RUNNER.main(["--prompt-file", str(self.prompt), "--out", str(self.home / "image.png"), *extra])
        return code, stdout.getvalue() + stderr.getvalue(), generation

    def test_loads_settings_only_from_active_home(self):
        self.assertEqual(RUNNER.load_settings(self.home), {})
        settings = self.settings()
        self.write_settings(settings)
        self.assertEqual(RUNNER.load_settings(self.home), settings)

    def test_profile_precedence(self):
        (self.home / "cliproxy-images.config.toml").write_text("", encoding="utf-8")
        self.assertEqual(RUNNER.select_profile(None, self.home, {}), "cliproxy-images")
        self.assertEqual(RUNNER.select_profile(None, self.home, {"profile": "configured"}), "configured")
        os.environ["CODEX_IMAGEGEN_PROFILE"] = "environment"
        self.assertEqual(RUNNER.select_profile(None, self.home, {"profile": "configured"}), "environment")
        self.assertEqual(RUNNER.select_profile("command-line", self.home, {"profile": "configured"}), "command-line")

    def test_explicit_disable_overrides_automatic_profile(self):
        (self.home / "cliproxy-images.config.toml").write_text("", encoding="utf-8")
        for value in ("", "-"):
            with self.subTest(source="argument", value=value):
                self.assertIsNone(RUNNER.select_profile(value, self.home, self.settings()))
            with self.subTest(source="environment", value=value):
                os.environ["CODEX_IMAGEGEN_PROFILE"] = value
                self.assertIsNone(RUNNER.select_profile(None, self.home, self.settings()))
        del os.environ["CODEX_IMAGEGEN_PROFILE"]
        self.assertIsNone(RUNNER.select_profile(None, self.home, {"profile": "-"}))

    def test_default_profile_when_no_image_profile_exists(self):
        self.assertIsNone(RUNNER.select_profile(None, self.home, {}))

    def test_rejects_invalid_profile_names(self):
        for value in ("../other", "name with spaces", ["name"], 12, True):
            with self.subTest(value=value), self.assertRaises(ValueError):
                RUNNER.select_profile(value, self.home, {})

    def test_helper_timeout_output_is_hidden_and_generation_never_starts(self):
        self.write_settings(self.settings())
        failure = subprocess.TimeoutExpired(["fake-helper"], 10, output=b"stdout-secret", stderr=b"stderr-secret")
        with mock.patch.object(RUNNER.subprocess, "run", side_effect=failure):
            code, output, generation = self.run_main()
        self.assertEqual(code, 2)
        self.assertIn("no generation started", output)
        self.assertNotIn("stdout-secret", output)
        self.assertNotIn("stderr-secret", output)
        generation.assert_not_called()

    def test_invalid_credential_settings_fail_before_running_any_process(self):
        cases = [
            ("timeout", 0), ("timeout", -1), ("timeout", True),
            ("timeout", "10"), ("timeout", float("nan")), ("timeout", float("inf")),
            ("command", "fake-helper"), ("command", []), ("command", [""]),
            ("command", [123]), ("env", "INVALID=NAME"), ("env", ""),
        ]
        for field, value in cases:
            with self.subTest(field=field, value=value):
                settings = self.settings()
                settings["credential"][field] = value
                self.write_settings(settings)
                with mock.patch.object(RUNNER.subprocess, "run") as helper:
                    code, output, generation = self.run_main()
                self.assertEqual(code, 2, output)
                helper.assert_not_called()
                generation.assert_not_called()

    def test_malformed_settings_fail_before_generation_without_echoing_contents(self):
        for text in ('{"credential": "private-configuration-secret"', '["private-configuration-secret"]'):
            with self.subTest(text=text):
                (self.home / "codex-imagegen.json").write_text(text, encoding="utf-8")
                code, output, generation = self.run_main()
                self.assertEqual(code, 2)
                self.assertNotIn("private-configuration-secret", output)
                generation.assert_not_called()


if __name__ == "__main__":
    unittest.main()
