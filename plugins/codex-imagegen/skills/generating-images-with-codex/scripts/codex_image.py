#!/usr/bin/env python3
"""Generate or edit one raster image with the local Codex CLI.

Codex runs its built-in `image_gen` tool on the user's ChatGPT plan. This script launches
`codex exec` in a throwaway directory outside any repository, tells it to make exactly one
`image_gen` call with the prompt file verbatim, finds the generated file through the Codex
session log and copies it to --out. A JSON record with the prompt, references and Codex
session sits next to the image unless --no-record is given.

Exit codes: 0 image saved, 1 generation failed, 2 bad arguments or Codex missing,
3 plan usage limit (HTTP 429): stop the batch and wait for the limit to reset.
"""
import argparse
import datetime
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

USAGE_LIMIT = re.compile(r"usage_limit_reached|usage limit|rate.?limit|\b429\b|too many requests", re.I)
# npm installs Codex on Windows as a codex.cmd shim; see codex_command
IS_WINDOWS = os.name == "nt"
SANDBOX_FAILURE = re.compile(r"sandbox|operation not permitted|permission denied|read-only file system|EPERM|EACCES", re.I)

INSTRUCTIONS = """You are a headless runner for one image generation. Call the built-in `image_gen` tool exactly once and do nothing else.

- prompt: read the file `{prompt_file}` with a plain `cat` and pass its whole text verbatim. Do not rewrite, shorten, extend, translate or append anything.
- referenced_image_paths: {references}
- Do not pass images any other way: no `view_image`, no `num_last_images_to_include`.
- Do not open, inspect, redo, move, copy or rename the generated image. It stays where `image_gen` saved it.
- Do not create, modify or delete any file.
- Never call `image_gen` a second time, even if the result looks wrong.
- If the call fails with a usage limit, a rate limit or HTTP 429, stop. If it fails for any other reason, do not retry.
- Finish with one line: `generated`, `failed: <reason>` or `limit: <reason>`.
"""


def references_text(references, edit):
    if not references:
        return "none, omit the parameter."
    head = "pass exactly these absolute paths, in this order"
    if edit:
        head += " (the first one is the image to edit; keep everything the prompt does not ask to change)"
    return head + ":\n" + "\n".join(f"  {i}. `{path}`" for i, path in enumerate(references, 1))


def json_lines(text):
    """JSON objects of a JSONL text; torn or non-JSON lines are skipped."""
    entries = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if isinstance(entry, dict):
            entries.append(entry)
    return entries


def usage_limited(events):
    # Only failure events count: a prompt may mention 429 without any limit being hit.
    return any(e.get("type") in ("error", "turn.failed") and USAGE_LIMIT.search(json.dumps(e)) for e in events)


def find_session_log(codex_home, thread_id):
    root = Path(codex_home) / "sessions"
    if not thread_id or not root.is_dir():
        return None
    for path in root.rglob(f"rollout-*-{thread_id}.jsonl"):
        return path
    return None


def generations(log_text):
    found = []
    for entry in json_lines(log_text):
        payload = entry.get("payload") or {}
        item = payload.get("item") or {}
        if entry.get("type") == "event_msg" and payload.get("type") == "item_completed" and item.get("kind") == "image_gen.generation":
            found.append(item)
    return found


def succeeded(item):
    return item.get("status") == "completed" and item.get("savedPath") and not item.get("failure") and Path(item["savedPath"]).is_file()


def codex_command(codex_bin):
    """Resolve --codex-bin into the argv prefix that starts Codex, or None when it is missing.

    On Windows npm installs Codex as a `codex.cmd` shim. CreateProcess cannot start it by its
    bare name, and going through cmd.exe would mangle the multi-line instructions (`%`, `^`,
    `&`, newlines), so the shim's own node entry point runs directly instead.
    """
    path = shutil.which(codex_bin)
    if not path:
        return None
    if IS_WINDOWS and path.lower().endswith((".cmd", ".bat")):
        entry = Path(path).parent / "node_modules" / "@openai" / "codex" / "bin" / "codex.js"
        node = shutil.which("node")
        if entry.is_file() and node:
            return [node, str(entry)]
    return [path]


def run_codex(codex_cmd, codex_home, work_dir, instructions, sandbox, timeout):
    policy = ["--dangerously-bypass-approvals-and-sandbox"] if sandbox == "bypass" else ["-s", "workspace-write", "-c", 'approval_policy="never"']
    args = [*codex_cmd, "exec", "-C", work_dir, "--skip-git-repo-check", "-c", "project_doc_max_bytes=0", "--json", *policy, instructions]
    # stdin stays closed: `codex exec` appends piped stdin to the prompt and would wait for it.
    proc = subprocess.run(args, cwd=work_dir, env={**os.environ, "CODEX_HOME": codex_home},
                          stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=timeout)
    events = json_lines(proc.stdout)
    thread_id = next((e.get("thread_id") for e in events if e.get("type") == "thread.started"), None)
    log = find_session_log(codex_home, thread_id)
    items = generations(log.read_text(encoding="utf-8")) if log else []
    limited = usage_limited(events) or any(USAGE_LIMIT.search(json.dumps(i.get("failure") or "")) for i in items)
    return {"threadId": thread_id, "items": items, "limited": limited, "code": proc.returncode,
            "stderr": proc.stderr.strip()[-500:], "log": str(log) if log else None}


def free_path(out):
    """Never overwrite: an existing out.png becomes out-v2.png, then -v3 and so on."""
    if not out.exists():
        return out
    n = 2
    while (candidate := out.with_name(f"{out.stem}-v{n}{out.suffix}")).exists():
        n += 1
    return candidate


def main(argv=None):
    parser = argparse.ArgumentParser(description="Generate or edit one image with the local Codex CLI (image_gen, ChatGPT plan).")
    parser.add_argument("--prompt-file", required=True, help="text file with the full image prompt, passed to image_gen verbatim")
    parser.add_argument("--out", required=True, help="where to save the image; an existing file is never overwritten")
    parser.add_argument("--ref", action="append", default=[], help="reference image, repeatable, in order")
    parser.add_argument("--edit", action="store_true", help="edit the first --ref instead of generating from scratch")
    parser.add_argument("--no-bypass", action="store_true", help="do not retry outside the Codex sandbox when image_gen fails because of it")
    parser.add_argument("--no-record", action="store_true", help="do not write <out>.json next to the image")
    parser.add_argument("--timeout", type=int, default=900, help="seconds to wait for Codex (default 900)")
    parser.add_argument("--codex-bin", default=os.environ.get("CODEX_BIN", "codex"))
    args = parser.parse_args(argv)

    prompt_file = Path(args.prompt_file).resolve()
    if not prompt_file.is_file() or not prompt_file.read_text(encoding="utf-8").strip():
        parser.error(f"prompt file is missing or empty: {prompt_file}")
    references = [str(Path(r).resolve()) for r in args.ref]
    missing = [r for r in references if not Path(r).is_file()]
    if missing:
        parser.error("reference images not found: " + ", ".join(missing))
    if args.edit and not references:
        parser.error("--edit needs the image to edit as the first --ref")
    codex_cmd = codex_command(args.codex_bin)
    if not codex_cmd:
        print(f"Codex CLI not found ({args.codex_bin}). Install it and sign in with the ChatGPT account: codex login", file=sys.stderr)
        return 2
    codex_home = os.environ.get("CODEX_HOME") or str(Path.home() / ".codex")

    instructions = INSTRUCTIONS.format(prompt_file=prompt_file, references=references_text(references, args.edit))
    runs = []
    with tempfile.TemporaryDirectory(prefix="codex-image-") as work_dir:
        sandbox = "workspace-write"
        try:
            run = run_codex(codex_cmd, codex_home, work_dir, instructions, sandbox, args.timeout)
            runs.append({**run, "sandbox": sandbox})
            blocked = not any(succeeded(i) for i in run["items"]) and any(SANDBOX_FAILURE.search(json.dumps(i.get("failure") or "")) for i in run["items"])
            if blocked and not run["limited"] and not args.no_bypass:
                print("image_gen was blocked by the Codex sandbox; retrying once without it", file=sys.stderr)
                sandbox = "bypass"
                run = run_codex(codex_cmd, codex_home, work_dir, instructions, sandbox, args.timeout)
                runs.append({**run, "sandbox": sandbox})
        except subprocess.TimeoutExpired:
            print(f"Codex did not finish in {args.timeout} s", file=sys.stderr)
            return 1

    calls = sum(len(r["items"]) for r in runs)
    done = next((i for r in runs for i in r["items"] if succeeded(i)), None)
    if not done:
        if any(r["limited"] for r in runs):
            print(f"ChatGPT plan usage limit (429): stop the batch and retry after the reset. image_gen calls made: {calls}", file=sys.stderr)
            return 3
        last = runs[-1]
        reason = next((json.dumps(i.get("failure")) for i in last["items"] if i.get("failure")), None) or last["stderr"] or "no image_gen generation in the session"
        print(f"generation failed (image_gen calls made: {calls}, Codex session {last['threadId'] or '-'}): {reason}", file=sys.stderr)
        return 1

    out = free_path(Path(args.out).resolve())
    out.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(done["savedPath"], out)
    if not args.no_record:
        record = {
            "createdAt": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
            "prompt": prompt_file.read_text(encoding="utf-8"),
            "promptFile": str(prompt_file),
            "references": references,
            "edit": args.edit,
            "codex": {"threadId": next(r["threadId"] for r in runs if done in r["items"]), "sandbox": sandbox,
                      "imageGenCalls": calls, "savedPath": done["savedPath"],
                      "revisedPrompt": done.get("revisedPrompt"), "transparentBackground": done.get("transparentBackground")},
        }
        out.with_name(out.name + ".json").write_text(json.dumps(record, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"saved {out} (image_gen calls made: {calls}, sandbox {sandbox})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
