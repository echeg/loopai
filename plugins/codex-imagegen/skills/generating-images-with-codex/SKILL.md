---
name: generating-images-with-codex
description: Use when the user asks to generate, draw, create, or edit a raster image (icon, illustration, sprite, concept art, texture, photo-like picture, image variants), or when about to hand-draw an SVG or reach for a paid image service because Claude cannot produce pixels itself.
---

# Generating Images with Codex

## Overview

Claude cannot paint raster images, but the local Codex CLI can: its built-in `image_gen` tool runs on the user's ChatGPT plan. The user has already authorized this route. A generated image is the answer to "generate an image"; a hand-drawn SVG is not.

## Permissions the user granted

- Up to **10 `image_gen` calls per batch** without asking. More than 10: ask first and say why.
- Calls run in the background on the user's ChatGPT account. Do not set up an `OPENAI_API_KEY` and do not use paid image services (LoreMax, fal, Replicate and the like) unless the user asks for them.
- Exit code 3 means the plan limit (HTTP 429): stop the batch, report how many images were made, and do not retry until the user says the limit has reset.

## Project rules come first

If the project defines its own image pipeline or art guide (AGENTS.md or CLAUDE.md points to one, a generation tool, a generations folder), follow it: it overrides the steps below. This skill then only supplies the Codex mechanism and the permissions.

## Workflow

1. `codex --version`. If Codex is missing or signed out, tell the user to install it and run `codex login`, and stop there. Do not substitute a drawing.
2. Write the prompt to a file: subject, style, composition, aspect, background (for example "true alpha transparency, no checkerboard"), palette, and what to avoid. Codex passes it to `image_gen` verbatim.
3. Generate one image per call:

   ```bash
   python3 "${CLAUDE_SKILL_DIR}/scripts/codex_image.py" --prompt-file prompt.txt --out assets/apple.png
   ```

   Add `--ref <image>` (repeatable, in order) for style or content references. Add `--edit --ref <source>` to change an existing image. A call takes 1–3 minutes, so run batches in the background.
4. Open the result with Read before reporting. Check every requirement: framing, transparency, no stray text, no checkerboard. If one fails, sharpen the prompt and regenerate within the budget, or report the defect.
5. Adjust locally when needed (resize, crop, trim) with `sips`, `magick` or Pillow. `image_gen` picks its own size, usually 1024 px or more.
6. Report the path, the number of `image_gen` calls recorded, and the record `<out>.json` (prompt, references, Codex session and profile). The count comes from image events in the session log; text requests and billing are separate. When taste matters, make two or three variants and let the user choose.

## Script reference

| Exit | Meaning |
| --- | --- |
| 0 | Image saved to `--out`, or to `<name>-v2` etc.: an existing file is never overwritten |
| 1 | Generation failed; the reason is on stderr, and nothing is retried |
| 2 | Bad arguments or Codex missing; no call was made |
| 3 | Plan usage limit (429): stop the batch |

- Codex runs in a throwaway directory outside the repository, with project docs disabled. It cannot touch project files; only the script copies the result.
- If the Codex sandbox blocks `image_gen`, the script retries once outside the sandbox. That counts as a second call; `--no-bypass` disables the retry.
- `--no-record` skips `<out>.json`. `--timeout` defaults to 900 s.
- UTF-8 is explicit on Windows. The runner uses `CODEX_HOME` for both Codex and session lookup; if unset, it uses `~/.codex`. Orca can set a different home.
- `--profile NAME` selects a Codex profile. Otherwise the runner checks `CODEX_IMAGEGEN_PROFILE`, then `profile` in `$CODEX_HOME/codex-imagegen.json`, then an existing `cliproxy-images.config.toml` in that home. `--profile -` keeps the ordinary Codex configuration and skips the configured credential helper.
- A missing result is not evidence that a generation was billed, nor permission to repeat it. Check the reported thread's rollout before retrying. An `image_gen unavailable` response requires a compatible provider/profile; bypassing the sandbox does not enable that tool.

## Proxy profile and credential helper

When the normal provider hides `image_gen`, use a prepared native-image profile instead of changing the global provider or forcing a direct OpenAI launch. For Codex 0.160.0, profiles are adjacent files such as `$CODEX_HOME/cliproxy-images.config.toml`. A CLIProxyAPI profile can use `requires_openai_auth = true` and `env_key = "CLIPROXY_API_KEY"`; the same home must retain a ChatGPT login, and the provider's `base_url` must point to the proxy's `/v1` routes. The proxy selects the subscription for the image request.

To supply an existing proxy key without putting it in a prompt, command line, or another plaintext config, create `$CODEX_HOME/codex-imagegen.json`:

```json
{
  "profile": "cliproxy-images",
  "credential": {
    "env": "CLIPROXY_API_KEY",
    "command": ["/absolute/path/to/get-proxy-key"],
    "timeout": 10
  }
}
```

`command` is an executable and its arguments, run without a shell; it must print only the key to stdout. On Windows, use `powershell.exe` with `-NoProfile`, `-NonInteractive`, `-File`, and the absolute helper path as separate arguments. The helper runs only for the configured profile and only if its environment variable is empty. The key is passed to the Codex child environment, never printed or saved in the image record. Helper failure stops before launching Codex. With an exported key, omit `credential` entirely.

The runner selects the profile automatically after this setup; the usual `--prompt-file` and `--out` invocation remains sufficient. Do not add `-c model_provider="openai"` to this route, since it overrides the profile and bypasses the proxy pool.

## Common mistakes

| Mistake | Instead |
| --- | --- |
| Drawing an SVG or Pillow sketch when the user asked for a generated image | Generate with Codex. Offer vector art only when the user wants vector |
| Asking permission for one or two images | Up to 10 calls are already authorized |
| Offering a paid service first | Codex is the default. Paid services only on request |
| Retrying after exit code 3 | Stop the batch and report |
| Reporting success without looking at the image | Read it first |
