#!/usr/bin/env python3

"""Copy a Git-visible repository snapshot without following links."""

from __future__ import annotations

import errno
import os
import stat
import subprocess
import sys
from pathlib import Path, PurePosixPath
from typing import NoReturn


MAX_SNAPSHOT_FILE_BYTES = 64 * 1024 * 1024
MAX_SNAPSHOT_TOTAL_BYTES = 512 * 1024 * 1024


class SnapshotError(RuntimeError):
    pass


def fail(message: str) -> NoReturn:
    print(f"error: {message}", file=sys.stderr)
    raise SystemExit(1)


def directory_flags() -> int:
    return (
        os.O_RDONLY
        | getattr(os, "O_DIRECTORY", 0)
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )


def file_read_flags() -> int:
    return (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_NOFOLLOW", 0)
        | getattr(os, "O_NONBLOCK", 0)
    )


def git_path(root: Path, argument: str, label: str) -> Path:
    result = subprocess.run(
        [
            "git",
            "-c",
            "core.fsmonitor=false",
            "-C",
            os.fspath(root),
            "rev-parse",
            "--path-format=absolute",
            argument,
        ],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.returncode != 0:
        detail = result.stderr.strip()
        raise SnapshotError(f"cannot resolve {label}: {detail or 'git failed'}")
    try:
        return Path(result.stdout.strip()).resolve(strict=True)
    except OSError as exc:
        raise SnapshotError(f"cannot resolve {label}: {exc}") from exc


def validate_git_layout(root: Path) -> None:
    top_level = git_path(root, "--show-toplevel", "Git worktree root")
    if top_level != root:
        raise SnapshotError("repository root is not the Git worktree root")

    canonical_private_directory = root / ".git"
    for argument, label in (
        ("--absolute-git-dir", "Git private directory"),
        ("--git-common-dir", "Git common directory"),
    ):
        private_directory = git_path(root, argument, label)
        if (
            private_directory == root or root in private_directory.parents
        ) and private_directory != canonical_private_directory:
            raise SnapshotError(
                f"{label} must not be inside the repository outside .git"
            )


def git_visible_paths(root: Path) -> list[str]:
    result = subprocess.run(
        [
            "git",
            "-c",
            "core.fsmonitor=false",
            "-C",
            os.fspath(root),
            "ls-files",
            "--cached",
            "--others",
            "--exclude-standard",
            "-z",
        ],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if result.returncode != 0:
        detail = result.stderr.decode(errors="replace").strip()
        raise SnapshotError(f"cannot enumerate Git-visible files: {detail or 'git failed'}")

    paths: list[str] = []
    seen_paths: set[str] = set()
    for raw_path in result.stdout.split(b"\0"):
        if not raw_path:
            continue
        path = os.fsdecode(raw_path)
        pure = PurePosixPath(path)
        parts = pure.parts
        if pure.is_absolute() or not parts or any(part in {"", ".", ".."} for part in parts):
            raise SnapshotError(f"Git returned an unsafe repository path: {path!r}")
        first_component = parts[0].casefold()
        if first_component in {".git", ".loopai"} or first_component.startswith(
            ".loopai-grill-recovery-"
        ):
            continue
        if path in seen_paths:
            continue
        seen_paths.add(path)
        paths.append(path)
    return paths


def open_source(root_fd: int, parts: tuple[str, ...]) -> int | None:
    parent_fd = os.dup(root_fd)
    try:
        for component in parts[:-1]:
            next_fd = os.open(component, directory_flags(), dir_fd=parent_fd)
            os.close(parent_fd)
            parent_fd = next_fd
        source_fd = os.open(parts[-1], file_read_flags(), dir_fd=parent_fd)
    except OSError as exc:
        if exc.errno in {errno.ENOENT, errno.ENOTDIR, errno.ELOOP}:
            return None
        raise
    finally:
        os.close(parent_fd)

    source_stat = os.fstat(source_fd)
    if not stat.S_ISREG(source_stat.st_mode) or source_stat.st_nlink != 1:
        os.close(source_fd)
        return None
    return source_fd


def open_destination_parent(destination_fd: int, parts: tuple[str, ...]) -> int:
    parent_fd = os.dup(destination_fd)
    try:
        for component in parts[:-1]:
            try:
                os.mkdir(component, 0o700, dir_fd=parent_fd)
            except FileExistsError:
                pass
            next_fd = os.open(component, directory_flags(), dir_fd=parent_fd)
            os.close(parent_fd)
            parent_fd = next_fd
        return parent_fd
    except Exception:
        os.close(parent_fd)
        raise


def copy_all(source_fd: int, destination_fd: int, byte_limit: int, path: str) -> int:
    copied = 0
    while True:
        remaining = byte_limit - copied
        chunk = os.read(source_fd, min(1024 * 1024, remaining + 1))
        if not chunk:
            return copied
        copied += len(chunk)
        if copied > byte_limit:
            raise SnapshotError(f"snapshot byte limit exceeded while copying {path!r}")
        view = memoryview(chunk)
        while view:
            written = os.write(destination_fd, view)
            if written == 0:
                raise OSError("write returned zero bytes")
            view = view[written:]


def copy_one(root_fd: int, destination_fd: int, path: str, remaining_total: int) -> int:
    parts = PurePosixPath(path).parts
    source_fd = open_source(root_fd, parts)
    if source_fd is None:
        return 0

    source_size = os.fstat(source_fd).st_size
    if source_size > MAX_SNAPSHOT_FILE_BYTES:
        os.close(source_fd)
        raise SnapshotError(f"snapshot file exceeds the 64 MiB limit: {path!r}")
    if source_size > remaining_total:
        os.close(source_fd)
        raise SnapshotError("snapshot exceeds the 512 MiB total limit")

    parent_fd: int | None = None
    output_fd: int | None = None
    created = False
    try:
        parent_fd = open_destination_parent(destination_fd, parts)
        output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        output_flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        output_fd = os.open(parts[-1], output_flags, 0o600, dir_fd=parent_fd)
        created = True
        copied = copy_all(
            source_fd,
            output_fd,
            min(MAX_SNAPSHOT_FILE_BYTES, remaining_total),
            path,
        )
        os.fsync(output_fd)
    except Exception:
        if created and parent_fd is not None:
            try:
                os.unlink(parts[-1], dir_fd=parent_fd)
            except OSError:
                pass
        raise
    finally:
        if output_fd is not None:
            os.close(output_fd)
        if parent_fd is not None:
            os.close(parent_fd)
        os.close(source_fd)
    return copied


def snapshot_repository(root_arg: str, destination_arg: str) -> None:
    root = Path(root_arg).resolve(strict=True)
    destination = Path(destination_arg).resolve(strict=True)
    if not root.is_dir() or not destination.is_dir():
        raise SnapshotError("repository root and snapshot destination must be directories")
    validate_git_layout(root)

    root_fd = os.open(root, directory_flags())
    destination_fd = os.open(destination, directory_flags())
    try:
        if os.listdir(destination_fd):
            raise SnapshotError("snapshot destination must be empty")
        copied_total = 0
        for path in git_visible_paths(root):
            copied_total += copy_one(
                root_fd,
                destination_fd,
                path,
                MAX_SNAPSHOT_TOTAL_BYTES - copied_total,
            )
        os.fsync(destination_fd)
    finally:
        os.close(destination_fd)
        os.close(root_fd)


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        fail("usage: snapshot_repository.py <repository-root> <snapshot-directory>")
    try:
        snapshot_repository(argv[1], argv[2])
    except (OSError, SnapshotError) as exc:
        fail(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
