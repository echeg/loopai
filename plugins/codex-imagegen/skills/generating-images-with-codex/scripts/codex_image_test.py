#!/usr/bin/env python3
"""Tests for codex_image.py against a fake `codex exec` that never calls a model.

The fake reads a scenario from $FAKE_CODEX_SCENARIO: one run per sandbox mode ("workspace-write" or "bypass"),
{"generation": "ok" | "sandbox" | "limit" | "fail" | null, "preThreadLimit": bool}. Like the real CLI it prints
--json events, writes a rollout under $CODEX_HOME/sessions and saves the image under $CODEX_HOME/generated_images.
It appends each call's arguments to $FAKE_CODEX_CALLS.
Run: python3 -m unittest discover -s <this dir> -p '*_test.py'
"""
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))
import codex_image  # noqa: E402

SCRIPT = Path(__file__).with_name("codex_image.py")
PNG = bytes.fromhex("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d6a9f00000000049454e44ae426082")

FAKE = textwrap.dedent('''\
    import json, os, sys, uuid
    from pathlib import Path
    args = sys.argv[1:]
    mode = "bypass" if "--dangerously-bypass-approvals-and-sandbox" in args else args[args.index("-s") + 1]
    run = json.loads(Path(os.environ["FAKE_CODEX_SCENARIO"]).read_text(encoding="utf-8"))[mode]
    with open(os.environ["FAKE_CODEX_CALLS"], "a", encoding="utf-8") as calls:
        calls.write(json.dumps(args) + "\\n")
    for key, expected in run.get("requiredEnv", {}).items():
        if os.environ.get(key) != expected:
            sys.exit("fake Codex did not receive the expected child environment")
    def emit(event):
        sys.stdout.buffer.write((json.dumps(event, ensure_ascii=False) + "\\n").encode("utf-8"))
        sys.stdout.buffer.flush()
    if run.get("stdoutText"):
        emit({"type": "item.completed", "item": {"type": "agent_message", "text": run["stdoutText"]}})
    if run.get("stderr"):
        sys.stderr.buffer.write(run["stderr"].encode("utf-8"))
    if run.get("preThreadLimit"):
        emit({"type": "error", "message": "usage_limit_reached: 429 Too Many Requests"})
        sys.exit(1)
    thread = str(uuid.uuid4())
    home = Path(os.environ["CODEX_HOME"])
    emit({"type": "thread.started", "thread_id": thread})
    log = home / "sessions" / "2026" / "09" / "25" / f"rollout-2026-09-25T12-00-00-{thread}.jsonl"
    log.parent.mkdir(parents=True, exist_ok=True)
    entries = [{"type": "session_meta", "payload": {"id": thread}}]
    kind = run.get("generation")
    if kind:
        item = {"type": "Extension", "kind": "image_gen.generation", "id": "exec-1", "status": "completed",
                "revisedPrompt": run.get("revisedPrompt", "revised"), "transparentBackground": True, "failure": None, "savedPath": None}
        if kind == "ok":
            image_root = Path.cwd() if run.get("imageInWorkDir") else home / "generated_images" / thread
            image = image_root / run.get("filename", "ig_1.png")
            image.parent.mkdir(parents=True, exist_ok=True)
            image.write_bytes(bytes.fromhex(os.environ["FAKE_PNG"]))
            item["savedPath"] = str(image)
        else:
            item["status"] = "failed"
            item["failure"] = {"sandbox": "Operation not permitted (os error 1)", "limit": "usage_limit_reached: 429", "fail": "content policy"}[kind]
        entries.append({"type": "event_msg", "payload": {"type": "item_completed", "thread_id": thread, "item": item}})
    log.write_text("\\n".join(json.dumps(e, ensure_ascii=False) for e in entries) + "\\n", encoding="utf-8")
    emit({"type": "turn.completed"})
''')


class CodexCommandTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def which(self, mapping):
        return lambda name: mapping.get(name)

    def test_missing_binary(self):
        with mock.patch.object(codex_image.shutil, "which", self.which({})):
            self.assertIsNone(codex_image.codex_command("codex"))

    def test_plain_binary_uses_the_resolved_path(self):
        with mock.patch.object(codex_image.shutil, "which", self.which({"codex": "/usr/local/bin/codex"})):
            self.assertEqual(codex_image.codex_command("codex"), ["/usr/local/bin/codex"])

    def test_windows_npm_shim_runs_the_node_entry_point(self):
        shim = self.root / "codex.CMD"
        shim.write_text("@echo off\n")
        entry = self.root / "node_modules" / "@openai" / "codex" / "bin" / "codex.js"
        entry.parent.mkdir(parents=True)
        entry.write_text("")
        which = self.which({"codex": str(shim), "node": "C:/node/node.exe"})
        with mock.patch.object(codex_image, "IS_WINDOWS", True), mock.patch.object(codex_image.shutil, "which", which):
            self.assertEqual(codex_image.codex_command("codex"), ["C:/node/node.exe", str(entry)])

    def test_windows_shim_without_entry_point_falls_back_to_the_shim(self):
        shim = self.root / "codex.cmd"
        shim.write_text("@echo off\n")
        which = self.which({"codex": str(shim), "node": "C:/node/node.exe"})
        with mock.patch.object(codex_image, "IS_WINDOWS", True), mock.patch.object(codex_image.shutil, "which", which):
            self.assertEqual(codex_image.codex_command("codex"), [str(shim)])

    def test_windows_shim_without_node_falls_back_to_the_shim(self):
        shim = self.root / "codex.cmd"
        shim.write_text("@echo off\n")
        entry = self.root / "node_modules" / "@openai" / "codex" / "bin" / "codex.js"
        entry.parent.mkdir(parents=True)
        entry.write_text("")
        with mock.patch.object(codex_image, "IS_WINDOWS", True), mock.patch.object(codex_image.shutil, "which", self.which({"codex": str(shim)})):
            self.assertEqual(codex_image.codex_command("codex"), [str(shim)])


class CodexImageTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.root = root
        self.fake = root / "fake_codex.py"
        self.fake.write_text(FAKE, encoding="utf-8")
        # Only command discovery is replaced; the helper still starts a real child process.
        # This keeps the CLI integration tests runnable on Windows without a shell shim.
        self.launcher = root / "run_helper.py"
        self.launcher.write_text(textwrap.dedent(f'''\
            import locale, sys
            sys.path.insert(0, {str(SCRIPT.parent)!r})
            import codex_image
            if __import__("os").environ.get("FAKE_LOCALE_CP1252"):
                locale.getpreferredencoding = lambda *args: "cp1252"
                locale.getencoding = lambda: "cp1252"
            codex_image.codex_command = lambda _: [sys.executable, {str(self.fake)!r}]
            sys.exit(codex_image.main())
        '''), encoding="utf-8")
        self.home = root / "codex-home"
        self.home.mkdir()
        self.calls = root / "calls.jsonl"
        self.prompt = root / "prompt.txt"
        self.prompt.write_text("A red cartoon apple with a leaf, transparent background.\n", encoding="utf-8")
        self.ref = root / "ref.png"
        self.ref.write_bytes(PNG)

    def tearDown(self):
        self.tmp.cleanup()

    def run_script(self, scenario, *extra, env_overrides=None):
        scenario_file = self.root / "scenario.json"
        scenario_file.write_text(json.dumps(scenario), encoding="utf-8")
        env = {**os.environ, "CODEX_HOME": str(self.home), "FAKE_CODEX_SCENARIO": str(scenario_file),
               "FAKE_CODEX_CALLS": str(self.calls), "FAKE_PNG": PNG.hex(),
               "HOME": str(self.root / "user-home"), "USERPROFILE": str(self.root / "user-home")}
        env.pop("CODEX_IMAGEGEN_PROFILE", None)
        env.pop("CODEX_IMAGE_TEST_KEY", None)
        env.update(env_overrides or {})
        args = [sys.executable, str(self.launcher), "--prompt-file", str(self.prompt), "--out", str(self.root / "out" / "apple.png"), *extra]
        return subprocess.run(args, env=env, capture_output=True, text=True, encoding="utf-8")

    def calls_made(self):
        return [json.loads(line) for line in self.calls.read_text(encoding="utf-8").splitlines()] if self.calls.exists() else []

    def test_saves_the_image_and_a_record(self):
        result = self.run_script({"workspace-write": {"generation": "ok"}})
        self.assertEqual(result.returncode, 0, result.stderr)
        out = self.root / "out" / "apple.png"
        self.assertEqual(out.read_bytes(), PNG)
        record = json.loads((self.root / "out" / "apple.png.json").read_text())
        self.assertEqual(record["prompt"], self.prompt.read_text())
        self.assertEqual(record["codex"]["sandbox"], "workspace-write")
        self.assertEqual(record["codex"]["imageGenCalls"], 1)
        self.assertEqual(record["codex"]["revisedPrompt"], "revised")
        (args,) = self.calls_made()
        self.assertIn("--skip-git-repo-check", args)
        self.assertIn("project_doc_max_bytes=0", args)
        self.assertIn(str(self.prompt.resolve()), args[-1])
        self.assertIn("none, omit the parameter", args[-1])

    def test_codex_runs_outside_the_callers_directory(self):
        self.run_script({"workspace-write": {"generation": "ok"}})
        (args,) = self.calls_made()
        work_dir = args[args.index("-C") + 1]
        self.assertTrue(Path(work_dir).name.startswith("codex-image-"))
        self.assertFalse(Path(work_dir).exists(), "the throwaway directory is removed")

    def test_image_in_throwaway_directory_is_saved_before_cleanup(self):
        result = self.run_script({"workspace-write": {"generation": "ok", "imageInWorkDir": True}})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / "out" / "apple.png").read_bytes(), PNG)
        (args,) = self.calls_made()
        self.assertFalse(Path(args[args.index("-C") + 1]).exists())

    def test_utf8_output_and_unicode_paths_with_a_windows_legacy_encoding(self):
        self.prompt = self.root / "задание.txt"
        prompt = "Орк и тёмная эльфийка на закате.\n"
        self.prompt.write_text(prompt, encoding="utf-8")
        out = self.root / "картинки" / "закат.png"
        result = self.run_script({"workspace-write": {
            "generation": "ok", "filename": "эльфийка.png", "revisedPrompt": prompt,
            "stdoutText": "Изображение готово", "stderr": "Проверка вывода Codex",
        }}, "--out", str(out), env_overrides={
            "FAKE_LOCALE_CP1252": "1", "PYTHONIOENCODING": "cp1252", "PYTHONUTF8": "0",
        })
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(out.read_bytes(), PNG)
        self.assertIn(str(out), result.stdout)
        record = json.loads(out.with_name(out.name + ".json").read_text(encoding="utf-8"))
        self.assertEqual(record["prompt"], prompt)
        self.assertEqual(record["codex"]["revisedPrompt"], prompt)
        self.assertEqual(Path(record["codex"]["savedPath"]).name, "эльфийка.png")

    def test_utf8_failure_diagnostic_with_a_windows_legacy_encoding(self):
        diagnostic = "Изображение недоступно: проверьте профиль"
        result = self.run_script({"workspace-write": {"generation": None, "stderr": diagnostic}},
                                 env_overrides={"FAKE_LOCALE_CP1252": "1", "PYTHONIOENCODING": "cp1252", "PYTHONUTF8": "0"})
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(diagnostic, result.stderr)
        self.assertEqual(len(self.calls_made()), 1)

    def test_session_log_is_read_from_the_active_codex_home(self):
        runtime_home = self.root / "orca" / "codex-runtime-home" / "home"
        result = self.run_script({"workspace-write": {"generation": "ok"}},
                                 env_overrides={"CODEX_HOME": str(runtime_home)})
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads((self.root / "out" / "apple.png.json").read_text(encoding="utf-8"))
        self.assertIn(runtime_home, Path(record["codex"]["savedPath"]).parents)
        self.assertFalse((self.home / "sessions").exists())

    def test_explicit_profile_overrides_environment_and_is_passed_to_codex(self):
        result = self.run_script({"workspace-write": {"generation": "ok"}}, "--profile", "image-direct",
                                 env_overrides={"CODEX_IMAGEGEN_PROFILE": "image-from-env"})
        self.assertEqual(result.returncode, 0, result.stderr)
        (args,) = self.calls_made()
        self.assertEqual(args[args.index("--profile") + 1], "image-direct")
        self.assertNotIn("image-from-env", args)

    def test_profile_from_environment_is_passed_to_codex(self):
        result = self.run_script({"workspace-write": {"generation": "ok"}},
                                 env_overrides={"CODEX_IMAGEGEN_PROFILE": "image-from-env"})
        self.assertEqual(result.returncode, 0, result.stderr)
        (args,) = self.calls_made()
        self.assertEqual(args[args.index("--profile") + 1], "image-from-env")

    def test_existing_proxy_profile_in_active_home_is_selected(self):
        (self.home / "cliproxy-images.config.toml").write_text('[features]\nimage_generation = true\n', encoding="utf-8")
        result = self.run_script({"workspace-write": {"generation": "ok"}})
        self.assertEqual(result.returncode, 0, result.stderr)
        (args,) = self.calls_made()
        self.assertEqual(args[args.index("--profile") + 1], "cliproxy-images")

    def test_profile_in_default_home_does_not_override_active_home(self):
        default_home = self.root / "user-home" / ".codex"
        default_home.mkdir(parents=True)
        (default_home / "cliproxy-images.config.toml").write_text('[features]\nimage_generation = true\n', encoding="utf-8")
        result = self.run_script({"workspace-write": {"generation": "ok"}})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("--profile", self.calls_made()[0])

    def test_explicit_dash_disables_automatic_profile_and_environment(self):
        (self.home / "cliproxy-images.config.toml").write_text('[features]\nimage_generation = true\n', encoding="utf-8")
        result = self.run_script({"workspace-write": {"generation": "ok"}}, "--profile", "-",
                                 env_overrides={"CODEX_IMAGEGEN_PROFILE": "image-from-env"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("--profile", self.calls_made()[0])

    def write_credential_settings(self, output="fixture-key", exit_code=0):
        marker = self.root / "credential-calls.txt"
        helper = (f"from pathlib import Path; Path({str(marker)!r}).write_text('called'); "
                  f"print({output!r}); raise SystemExit({exit_code})")
        settings = {"profile": "local-images", "credential": {
            "env": "CODEX_IMAGE_TEST_KEY", "command": [sys.executable, "-c", helper], "timeout": 10,
        }}
        (self.home / "codex-imagegen.json").write_text(json.dumps(settings), encoding="utf-8")
        return settings, marker

    def test_local_profile_resolves_credential_for_codex_without_exposing_it(self):
        _, marker = self.write_credential_settings()
        result = self.run_script({"workspace-write": {
            "generation": "ok", "requiredEnv": {"CODEX_IMAGE_TEST_KEY": "fixture-key"},
        }})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(marker.is_file())
        (args,) = self.calls_made()
        self.assertEqual(args[args.index("--profile") + 1], "local-images")
        self.assertNotIn("fixture-key", result.stdout + result.stderr + json.dumps(args))
        record = (self.root / "out" / "apple.png.json").read_text(encoding="utf-8")
        self.assertNotIn("fixture-key", record)

    def test_credential_helper_does_not_mutate_parent_environment(self):
        settings, _ = self.write_credential_settings()
        with mock.patch.dict(os.environ, {"CODEX_IMAGE_TEST_KEY": ""}):
            env = codex_image.codex_environment(str(self.home), "local-images", settings)
            self.assertEqual(env["CODEX_IMAGE_TEST_KEY"], "fixture-key")
            self.assertEqual(os.environ["CODEX_IMAGE_TEST_KEY"], "")

    def test_existing_credential_skips_helper(self):
        _, marker = self.write_credential_settings(exit_code=1)
        result = self.run_script({"workspace-write": {
            "generation": "ok", "requiredEnv": {"CODEX_IMAGE_TEST_KEY": "already-set"},
        }}, env_overrides={"CODEX_IMAGE_TEST_KEY": "already-set"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(marker.exists())

    def test_profile_override_skips_unrelated_credential_helper(self):
        _, marker = self.write_credential_settings(exit_code=1)
        result = self.run_script({"workspace-write": {"generation": "ok"}}, "--profile", "other-images")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(marker.exists())
        (args,) = self.calls_made()
        self.assertEqual(args[args.index("--profile") + 1], "other-images")

    def test_failed_credential_helper_stops_before_codex_and_hides_output(self):
        self.write_credential_settings(output="fixture-secret-from-failed-helper", exit_code=1)
        result = self.run_script({})
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("credential helper failed", result.stderr)
        self.assertNotIn("fixture-secret-from-failed-helper", result.stdout + result.stderr)
        self.assertEqual(self.calls_made(), [])

    def test_multiline_credential_output_is_rejected_before_codex(self):
        self.write_credential_settings(output="fixture-secret\nunexpected-extra-line")
        result = self.run_script({})
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("credential helper failed", result.stderr)
        self.assertNotIn("fixture-secret", result.stdout + result.stderr)
        self.assertEqual(self.calls_made(), [])

    def test_never_overwrites(self):
        scenario = {"workspace-write": {"generation": "ok"}}
        self.run_script(scenario)
        second = self.run_script(scenario)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertIn("apple-v2.png", second.stdout)
        self.assertTrue((self.root / "out" / "apple-v2.png").is_file())

    def test_references_and_edit_are_passed_in_order(self):
        other = self.root / "style.png"
        other.write_bytes(PNG)
        result = self.run_script({"workspace-write": {"generation": "ok"}}, "--edit", "--ref", str(self.ref), "--ref", str(other))
        self.assertEqual(result.returncode, 0, result.stderr)
        instructions = self.calls_made()[0][-1]
        self.assertIn("the first one is the image to edit", instructions)
        self.assertLess(instructions.index(str(self.ref.resolve())), instructions.index(str(other.resolve())))

    def test_usage_limit_exits_3(self):
        result = self.run_script({"workspace-write": {"generation": "limit"}})
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("usage limit", result.stderr)
        self.assertEqual(len(self.calls_made()), 1, "no retry after a limit")

    def test_usage_limit_before_the_session_exits_3(self):
        result = self.run_script({"workspace-write": {"preThreadLimit": True}})
        self.assertEqual(result.returncode, 3, result.stderr)

    def test_sandbox_failure_retries_once_without_the_sandbox(self):
        result = self.run_script({"workspace-write": {"generation": "sandbox"}, "bypass": {"generation": "ok"}})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.calls_made()), 2)
        self.assertIn("--dangerously-bypass-approvals-and-sandbox", self.calls_made()[1])
        record = json.loads((self.root / "out" / "apple.png.json").read_text())
        self.assertEqual(record["codex"]["sandbox"], "bypass")
        self.assertEqual(record["codex"]["imageGenCalls"], 2)

    def test_no_bypass_keeps_the_sandbox(self):
        result = self.run_script({"workspace-write": {"generation": "sandbox"}}, "--no-bypass")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(len(self.calls_made()), 1)

    def test_other_failures_are_not_retried(self):
        result = self.run_script({"workspace-write": {"generation": "fail"}})
        self.assertEqual(result.returncode, 1)
        self.assertIn("content policy", result.stderr)
        self.assertEqual(len(self.calls_made()), 1)

    def test_a_session_without_generation_fails(self):
        result = self.run_script({"workspace-write": {"generation": None}})
        self.assertEqual(result.returncode, 1)
        self.assertIn("no image_gen generation", result.stderr)

    def test_bad_arguments_exit_2_before_codex(self):
        self.prompt.write_text("  \n")
        self.assertEqual(self.run_script({}).returncode, 2)
        self.prompt.write_text("prompt")
        self.assertEqual(self.run_script({}, "--edit").returncode, 2)
        self.assertEqual(self.run_script({}, "--ref", str(self.root / "missing.png")).returncode, 2)
        self.assertEqual(self.calls_made(), [])

    def test_missing_codex_exits_2(self):
        env = {**os.environ, "CODEX_HOME": str(self.home)}
        result = subprocess.run([sys.executable, str(SCRIPT), "--prompt-file", str(self.prompt), "--out", str(self.root / "a.png"),
                                 "--codex-bin", str(self.root / "no-codex")], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("codex login", result.stderr)


if __name__ == "__main__":
    unittest.main()
