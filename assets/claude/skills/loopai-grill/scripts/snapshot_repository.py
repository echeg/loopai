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


def git_visible_paths(root: Path) -> list[str]:
    result = subprocess.run(
        [
            "git",
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
    for raw_path in result.stdout.split(b"\0"):
        if not raw_path:
            continue
        path = os.fsdecode(raw_path)
        pure = PurePosixPath(path)
        parts = pure.parts
        if pure.is_absolute() or not parts or any(part in {"", ".", ".."} for part in parts):
            raise SnapshotError(f"Git returned an unsafe repository path: {path!r}")
        if parts[0] in {".git", ".loopai"} or parts[0].startswith(
            ".loopai-grill-recovery-"
        ):
            continue
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


def copy_all(source_fd: int, destination_fd: int) -> None:
    while chunk := os.read(source_fd, 1024 * 1024):
        view = memoryview(chunk)
        while view:
            written = os.write(destination_fd, view)
            if written == 0:
                raise OSError("write returned zero bytes")
            view = view[written:]


def copy_one(root_fd: int, destination_fd: int, path: str) -> None:
    parts = PurePosixPath(path).parts
    source_fd = open_source(root_fd, parts)
    if source_fd is None:
        return

    parent_fd: int | None = None
    output_fd: int | None = None
    created = False
    try:
        parent_fd = open_destination_parent(destination_fd, parts)
        output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        output_flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        output_fd = os.open(parts[-1], output_flags, 0o600, dir_fd=parent_fd)
        created = True
        copy_all(source_fd, output_fd)
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


def snapshot_repository(root_arg: str, destination_arg: str) -> None:
    root = Path(root_arg).resolve(strict=True)
    destination = Path(destination_arg).resolve(strict=True)
    if not root.is_dir() or not destination.is_dir():
        raise SnapshotError("repository root and snapshot destination must be directories")

    root_fd = os.open(root, directory_flags())
    destination_fd = os.open(destination, directory_flags())
    try:
        if os.listdir(destination_fd):
            raise SnapshotError("snapshot destination must be empty")
        for path in git_visible_paths(root):
            copy_one(root_fd, destination_fd, path)
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
