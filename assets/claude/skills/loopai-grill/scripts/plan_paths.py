#!/usr/bin/env python3

"""Validate loopai plan paths and create final plans without clobbering files."""

from __future__ import annotations

import os
import re
import stat
import sys
from pathlib import Path, PurePosixPath
from typing import NoReturn


OUTPUT_RE = re.compile(
    r"^docs/plans/(?P<date>\d{8}|\d{4}-\d{2}-\d{2})-"
    r"(?P<slug>[a-z0-9]+(?:-[a-z0-9]+)*)\.md$"
)


class PathError(ValueError):
    pass


def fail(message: str) -> NoReturn:
    print(f"error: {message}", file=sys.stderr)
    raise SystemExit(1)


def repository_layout(root_arg: str) -> tuple[Path, Path, Path]:
    try:
        root = Path(root_arg).resolve(strict=True)
    except OSError as exc:
        raise PathError(f"cannot resolve repository root: {exc}") from exc
    if not root.is_dir():
        raise PathError("repository root is not a directory")

    docs = root / "docs"
    plans = docs / "plans"
    for label, path in (("docs", docs), ("docs/plans", plans)):
        if path.is_symlink():
            raise PathError(f"{label} must not be a symbolic link")
        try:
            mode = path.lstat().st_mode
        except OSError as exc:
            raise PathError(f"cannot inspect {label}: {exc}") from exc
        if not stat.S_ISDIR(mode):
            raise PathError(f"{label} is not a directory")

    expected = root / "docs" / "plans"
    if plans.resolve(strict=True) != expected:
        raise PathError("canonical docs/plans is not the repository plans directory")
    return root, plans, root / ".loopai"


def direct_plan_relative(literal: str) -> tuple[str, str]:
    if "\\" in literal:
        raise PathError("plan path must use repository-relative forward slashes")
    path = PurePosixPath(literal)
    if path.is_absolute() or len(path.parts) != 3 or path.parts[:2] != ("docs", "plans"):
        raise PathError("plan path must be a direct child of docs/plans")
    basename = path.name
    if basename in {".", ".."} or not basename.endswith(".md"):
        raise PathError("plan path must name a Markdown file")
    normalized = f"docs/plans/{basename}"
    if literal != normalized:
        raise PathError("plan path is not in canonical repository-relative form")
    return normalized, basename


def validate_active(root_arg: str, literal: str) -> tuple[Path, str]:
    root, plans, loopai = repository_layout(root_arg)
    relative, basename = direct_plan_relative(literal)
    candidate = plans / basename
    if candidate.is_symlink():
        raise PathError("plan must not be a symbolic link")
    try:
        mode = candidate.lstat().st_mode
    except OSError as exc:
        raise PathError(f"active plan does not exist: {relative}") from exc
    if not stat.S_ISREG(mode):
        raise PathError("active plan is not a regular file")
    if not any(entry.name == basename for entry in os.scandir(plans)):
        raise PathError("plan path case does not match the filesystem entry")
    resolved = candidate.resolve(strict=True)
    if resolved.parent != plans or resolved == loopai or loopai in resolved.parents:
        raise PathError("active plan resolves outside the repository plans directory")
    return resolved, relative


def looks_path_like(value: str) -> bool:
    stripped = value.strip()
    return (
        stripped.startswith(("/", "./", "../", "~/", "docs/"))
        or stripped.endswith(".md")
        or "\\" in stripped
    )


def classify(root_arg: str, value: str) -> str:
    try:
        _, relative = validate_active(root_arg, value)
    except PathError:
        if looks_path_like(value):
            raise
        return "description"
    return f"plan\t{relative}"


def newest_active(root_arg: str) -> str:
    _, plans, _ = repository_layout(root_arg)
    candidates: list[tuple[int, str]] = []
    for entry in os.scandir(plans):
        relative = f"docs/plans/{entry.name}"
        if not entry.name.endswith(".md"):
            continue
        try:
            path, _ = validate_active(root_arg, relative)
        except PathError:
            continue
        candidates.append((path.stat().st_mtime_ns, relative))
    if not candidates:
        raise PathError("no safe active plan found")
    return max(candidates, key=lambda item: (item[0], item[1]))[1]


def collision_names(relative: str) -> set[str]:
    match = OUTPUT_RE.fullmatch(relative)
    if match is None:
        raise PathError("output must be docs/plans/YYYYMMDD-<slug>.md")
    date = match.group("date")
    slug = match.group("slug")
    if len(date) == 8:
        alternate = f"{date[:4]}-{date[4:6]}-{date[6:]}-{slug}.md"
    else:
        alternate = f"{date.replace('-', '')}-{slug}.md"
    return {PurePosixPath(relative).name, alternate}


def ensure_no_collision(root_arg: str, relative: str) -> tuple[Path, Path, str]:
    root, plans, _ = repository_layout(root_arg)
    normalized, basename = direct_plan_relative(relative)
    names = collision_names(normalized)
    folded = {name.casefold() for name in names}

    completed = plans / "completed"
    if completed.is_symlink():
        raise PathError("docs/plans/completed must not be a symbolic link")
    directories = [plans]
    if completed.exists():
        if not completed.is_dir():
            raise PathError("docs/plans/completed is not a directory")
        directories.append(completed)
    for directory in directories:
        for entry in os.scandir(directory):
            if entry.name.casefold() in folded:
                raise PathError(f"plan name collides with {entry.path}")
    return root, plans, basename


def write_final(root_arg: str, relative: str, source_arg: str) -> str:
    root, plans, basename = ensure_no_collision(root_arg, relative)
    source = Path(source_arg)
    if source.is_symlink():
        raise PathError("final draft source must not be a symbolic link")
    try:
        resolved_source = source.resolve(strict=True)
    except OSError as exc:
        raise PathError(f"cannot resolve final draft source: {exc}") from exc
    loopai = root / ".loopai"
    if loopai == resolved_source or loopai in resolved_source.parents:
        raise PathError("final draft source must not be under .loopai")
    if plans == resolved_source or plans in resolved_source.parents:
        raise PathError("final draft source must be outside docs/plans")

    directory_flags = os.O_RDONLY
    directory_flags |= getattr(os, "O_DIRECTORY", 0)
    directory_flags |= getattr(os, "O_CLOEXEC", 0)
    directory_flags |= getattr(os, "O_NOFOLLOW", 0)
    file_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    file_flags |= getattr(os, "O_CLOEXEC", 0)
    file_flags |= getattr(os, "O_NOFOLLOW", 0)
    source_flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)

    source_fd = os.open(source, source_flags)
    try:
        if not stat.S_ISREG(os.fstat(source_fd).st_mode):
            raise PathError("final draft source is not a regular file")
        chunks: list[bytes] = []
        while chunk := os.read(source_fd, 1024 * 1024):
            chunks.append(chunk)
        payload = b"".join(chunks)
    finally:
        os.close(source_fd)

    root_fd = os.open(root, directory_flags)
    docs_fd: int | None = None
    directory_fd: int | None = None
    target_fd: int | None = None
    created = False
    try:
        docs_fd = os.open("docs", directory_flags, dir_fd=root_fd)
        directory_fd = os.open("plans", directory_flags, dir_fd=docs_fd)
        target_fd = os.open(basename, file_flags, 0o644, dir_fd=directory_fd)
        created = True
        view = memoryview(payload)
        while view:
            written = os.write(target_fd, view)
            view = view[written:]
        os.fsync(target_fd)
    except FileExistsError as exc:
        raise PathError("output plan appeared before it could be created") from exc
    except Exception:
        if created and directory_fd is not None:
            try:
                os.unlink(basename, dir_fd=directory_fd)
            except OSError:
                pass
        raise
    finally:
        if target_fd is not None:
            os.close(target_fd)
        if directory_fd is not None:
            os.close(directory_fd)
        if docs_fd is not None:
            os.close(docs_fd)
        os.close(root_fd)
    return f"docs/plans/{basename}"


def main(argv: list[str]) -> int:
    if len(argv) < 3:
        fail("usage: plan_paths.py <validate-active|classify|newest-active|check-output|write-final> <repository-root> [arguments]")
    command, root_arg, *args = argv[1:]
    try:
        if command == "validate-active" and len(args) == 1:
            print(validate_active(root_arg, args[0])[1])
        elif command == "classify" and len(args) == 1:
            print(classify(root_arg, args[0]))
        elif command == "newest-active" and not args:
            print(newest_active(root_arg))
        elif command == "check-output" and len(args) == 1:
            ensure_no_collision(root_arg, args[0])
            print(args[0])
        elif command == "write-final" and len(args) == 2:
            print(write_final(root_arg, args[0], args[1]))
        else:
            raise PathError("invalid command arguments")
    except (OSError, PathError) as exc:
        fail(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
