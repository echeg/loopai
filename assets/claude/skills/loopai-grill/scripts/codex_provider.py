#!/usr/bin/env python3
"""Print the Codex model-provider overrides that run-codex.sh forwards to its isolated run.

The wrapper runs Codex with --ignore-user-config, which also drops a custom model provider (a proxy
or gateway) selected in the user's config.toml and leaves Codex on its built-in login. This helper
reads only the selected provider and model from that file and prints tab-separated records:

    arg<TAB>key=toml-value   one `-c` override
    token<TAB>value          bearer token; the wrapper hands it to Codex through the environment
    note<TAB>text            non-fatal message for the wrapper's stderr

It prints nothing when no custom provider is selected, so the wrapper keeps Codex's built-in
provider. A selected provider that cannot be forwarded faithfully is an error, never a silent
fallback to another account. Secret values are never written to stderr.
"""

import json
import os
import sys

PROVIDER_ID = "loopai_grill"
TOKEN_ENV = "LOOPAI_GRILL_CODEX_TOKEN"
STRING_FIELDS = ("name", "base_url", "wire_api")
INTEGER_FIELDS = ("request_max_retries", "stream_max_retries", "stream_idle_timeout_ms")
BOOLEAN_FIELDS = ("supports_websockets",)
STRING_TABLE_FIELDS = ("query_params", "env_http_headers")


class ProviderError(Exception):
    pass


def toml_string(value: object, what: str) -> str:
    """Render a TOML basic string; a JSON string without control characters is one."""
    if not isinstance(value, str):
        raise ProviderError(f"{what} must be a string")
    if any(ord(char) < 0x20 or ord(char) == 0x7F for char in value):
        raise ProviderError(f"{what} must not contain control characters")
    return json.dumps(value, ensure_ascii=False)


def toml_string_table(value: object, what: str) -> str:
    if not isinstance(value, dict):
        raise ProviderError(f"{what} must be a table of strings")
    pairs = [f"{toml_string(key, what)} = {toml_string(item, what)}" for key, item in value.items()]
    return "{ " + ", ".join(pairs) + " }" if pairs else "{}"


def effective_settings(config: dict) -> dict:
    """Apply the selected profile on top of the top-level settings, as Codex does."""
    settings = dict(config)
    profile = config.get("profile")
    profiles = config.get("profiles")
    if isinstance(profile, str) and isinstance(profiles, dict) and isinstance(profiles.get(profile), dict):
        settings.update(profiles[profile])
    return settings


def provider_records(config: dict) -> list[tuple[str, str]]:
    settings = effective_settings(config)
    selected = settings.get("model_provider")
    if not isinstance(selected, str) or not selected:
        return []
    providers = config.get("model_providers")
    provider = providers.get(selected) if isinstance(providers, dict) else None
    if not isinstance(provider, dict):
        # a built-in provider id has no table to forward
        if selected == "openai":
            return []
        return [("note", f"Codex provider {selected!r} has no [model_providers] table; using the built-in provider")]

    where = f"model_providers.{selected}"
    prefix = f"model_providers.{PROVIDER_ID}"
    if not provider.get("base_url"):
        raise ProviderError(f"{where}.base_url is required to forward the provider")

    records = [("arg", f"model_provider={toml_string(PROVIDER_ID, 'provider id')}")]
    for field in STRING_FIELDS:
        if field in provider:
            records.append(("arg", f"{prefix}.{field}={toml_string(provider[field], f'{where}.{field}')}"))
    for field in INTEGER_FIELDS:
        if field in provider:
            if isinstance(provider[field], bool) or not isinstance(provider[field], int):
                raise ProviderError(f"{where}.{field} must be an integer")
            records.append(("arg", f"{prefix}.{field}={provider[field]}"))
    for field in BOOLEAN_FIELDS:
        if field in provider:
            if not isinstance(provider[field], bool):
                raise ProviderError(f"{where}.{field} must be a boolean")
            records.append(("arg", f"{prefix}.{field}={'true' if provider[field] else 'false'}"))
    for field in STRING_TABLE_FIELDS:
        if field in provider:
            records.append(("arg", f"{prefix}.{field}={toml_string_table(provider[field], f'{where}.{field}')}"))

    token = provider.get("experimental_bearer_token")
    env_key = provider.get("env_key")
    if token is not None:
        if not token:
            raise ProviderError(f"{where}.experimental_bearer_token must not be empty")
        toml_string(token, f"{where}.experimental_bearer_token")
        env_key = TOKEN_ENV
        records.append(("token", token))
    if env_key is not None:
        # the credential comes from the environment, so Codex must not substitute its own login
        records.append(("arg", f"{prefix}.env_key={toml_string(env_key, f'{where}.env_key')}"))
        records.append(("arg", f"{prefix}.requires_openai_auth=false"))
    elif "requires_openai_auth" in provider:
        if not isinstance(provider["requires_openai_auth"], bool):
            raise ProviderError(f"{where}.requires_openai_auth must be a boolean")
        value = "true" if provider["requires_openai_auth"] else "false"
        records.append(("arg", f"{prefix}.requires_openai_auth={value}"))

    if "http_headers" in provider:
        records.append(
            (
                "note",
                f"{where}.http_headers is not forwarded: its values would be visible in process"
                " arguments; use env_http_headers",
            )
        )
    if "model" in settings:
        records.append(("arg", f"model={toml_string(settings['model'], 'model')}"))
    return records


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: codex_provider.py <codex-config-toml>", file=sys.stderr)
        return 64
    path = argv[1]
    if not os.path.isfile(path):
        return 0
    try:
        import tomllib
    except ImportError:
        print(f"note\tpython3 older than 3.11 cannot read {path}; using Codex's built-in provider")
        return 0
    try:
        with open(path, "rb") as handle:
            config = tomllib.load(handle)
    except (OSError, tomllib.TOMLDecodeError):
        print(f"note\tcannot read {path}; using Codex's built-in provider")
        return 0
    try:
        records = provider_records(config)
    except ProviderError as error:
        print(f"cannot forward the Codex model provider from {path}: {error}", file=sys.stderr)
        return 1
    for kind, value in records:
        print(f"{kind}\t{value}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
