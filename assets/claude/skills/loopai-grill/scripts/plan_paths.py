#!/usr/bin/env python3

"""Validate loopai plan paths and create final plans without clobbering files."""

from __future__ import annotations

import errno
import fcntl
import hashlib
import os
import re
import secrets
import stat
import sys
from contextlib import contextmanager
from pathlib import Path, PurePosixPath
from typing import Iterator, NoReturn


OUTPUT_RE = re.compile(
    r"^docs/plans/(?P<date>\d{8}|\d{4}-\d{2}-\d{2})-"
    r"(?P<slug>[a-z0-9]+(?:-[a-z0-9]+)*)\.md$"
)
ACTIVE_TOKEN_RE = re.compile(r"^v1:(?P<device>\d+):(?P<inode>\d+):(?P<digest>[0-9a-f]{64})$")


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

    loopai = root / ".loopai"
    if loopai.is_symlink():
        raise PathError(".loopai must not be a symbolic link")

    expected = root / "docs" / "plans"
    if plans.resolve(strict=True) != expected:
        raise PathError("canonical docs/plans is not the repository plans directory")
    return root, plans, loopai


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
    with open_active(root_arg, literal) as (_, plans, relative, basename, _, _, _):
        return plans / basename, relative


def directory_open_flags() -> int:
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


@contextmanager
def open_active(
    root_arg: str, literal: str
) -> Iterator[tuple[Path, Path, str, str, int, int, os.stat_result]]:
    root, plans, _ = repository_layout(root_arg)
    relative, basename = direct_plan_relative(literal)
    root_fd = os.open(root, directory_open_flags())
    docs_fd: int | None = None
    plans_fd: int | None = None
    plan_fd: int | None = None
    try:
        docs_fd = os.open("docs", directory_open_flags(), dir_fd=root_fd)
        plans_fd = os.open("plans", directory_open_flags(), dir_fd=docs_fd)
        plan_fd = os.open(basename, file_read_flags(), dir_fd=plans_fd)
        plan_stat = os.fstat(plan_fd)
        if not stat.S_ISREG(plan_stat.st_mode):
            raise PathError("active plan is not a regular file")
        if plan_stat.st_nlink != 1:
            raise PathError("active plan must not be hard-linked")
        if basename not in os.listdir(plans_fd):
            raise PathError("plan path case does not match the filesystem entry")
        yield root, plans, relative, basename, plans_fd, plan_fd, plan_stat
    except FileNotFoundError as exc:
        raise PathError(f"active plan does not exist: {relative}") from exc
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            raise PathError("plan must not be a symbolic link") from exc
        raise
    finally:
        if plan_fd is not None:
            os.close(plan_fd)
        if plans_fd is not None:
            os.close(plans_fd)
        if docs_fd is not None:
            os.close(docs_fd)
        os.close(root_fd)


def read_all(file_fd: int) -> bytes:
    chunks: list[bytes] = []
    while chunk := os.read(file_fd, 1024 * 1024):
        chunks.append(chunk)
    return b"".join(chunks)


def write_all(file_fd: int, payload: bytes) -> None:
    view = memoryview(payload)
    while view:
        written = os.write(file_fd, view)
        if written == 0:
            raise OSError("write returned zero bytes")
        view = view[written:]


def resolve_scratch_source(root: Path, plans: Path, source_arg: str) -> Path:
    source = Path(source_arg)
    if source.is_symlink():
        raise PathError("draft source must not be a symbolic link")
    try:
        resolved_source = source.resolve(strict=True)
    except OSError as exc:
        raise PathError(f"cannot resolve draft source: {exc}") from exc
    loopai = root / ".loopai"
    try:
        lexical_source = source.absolute()
    except OSError as exc:
        raise PathError(f"cannot inspect draft source: {exc}") from exc
    if loopai == lexical_source or loopai in lexical_source.parents:
        raise PathError("draft source must not be under .loopai")
    if loopai == resolved_source or loopai in resolved_source.parents:
        raise PathError("draft source must not be under .loopai")
    if plans == resolved_source or plans in resolved_source.parents:
        raise PathError("draft source must be outside docs/plans")
    return resolved_source


def validate_scratch(root_arg: str, scratch_arg: str) -> Path:
    root, _, _ = repository_layout(root_arg)
    scratch = Path(scratch_arg)
    if scratch.is_symlink():
        raise PathError("scratch directory must not be a symbolic link")
    try:
        resolved_scratch = scratch.resolve(strict=True)
        scratch_stat = resolved_scratch.stat()
    except OSError as exc:
        raise PathError(f"cannot resolve scratch directory: {exc}") from exc
    if not stat.S_ISDIR(scratch_stat.st_mode):
        raise PathError("scratch path is not a directory")
    if root == resolved_scratch or root in resolved_scratch.parents:
        raise PathError("scratch directory must be outside the repository")
    return resolved_scratch


def active_token(plan_stat: os.stat_result, payload: bytes) -> str:
    digest = hashlib.sha256(payload).hexdigest()
    return f"v1:{plan_stat.st_dev}:{plan_stat.st_ino}:{digest}"


def parse_active_token(token: str) -> tuple[int, int, str]:
    match = ACTIVE_TOKEN_RE.fullmatch(token)
    if match is None:
        raise PathError("active-plan token is invalid")
    return int(match.group("device")), int(match.group("inode")), match.group("digest")


def read_active(root_arg: str, literal: str, destination_arg: str) -> tuple[str, str]:
    with open_active(root_arg, literal) as (root, _, relative, _, _, plan_fd, plan_stat):
        payload = read_all(plan_fd)

    destination = Path(destination_arg)
    try:
        destination_parent = destination.parent.resolve(strict=True)
    except OSError as exc:
        raise PathError(f"cannot resolve snapshot directory: {exc}") from exc
    destination = destination_parent / destination.name
    if root == destination or root in destination.parents:
        raise PathError("active-plan snapshot must be outside the repository")
    snapshot_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    snapshot_flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    snapshot_fd: int | None = None
    created = False
    try:
        snapshot_fd = os.open(destination, snapshot_flags, 0o600)
        created = True
        write_all(snapshot_fd, payload)
        os.fsync(snapshot_fd)
    except Exception:
        if created:
            try:
                destination.unlink()
            except OSError:
                pass
        raise
    finally:
        if snapshot_fd is not None:
            os.close(snapshot_fd)
    return relative, active_token(plan_stat, payload)


def restore_recovery(
    root_fd: int,
    plans_fd: int,
    recovery_fd: int,
    recovery_name: str,
    basename: str,
) -> bool:
    """Restore a displaced plan without overwriting a concurrent writer."""

    try:
        os.link(
            "original-plan",
            basename,
            src_dir_fd=recovery_fd,
            dst_dir_fd=plans_fd,
            follow_symlinks=False,
        )
    except FileExistsError:
        return False
    os.unlink("original-plan", dir_fd=recovery_fd)
    os.fsync(plans_fd)
    os.rmdir(recovery_name, dir_fd=root_fd)
    os.fsync(root_fd)
    return True


def replace_active(root_arg: str, literal: str, token: str, source_arg: str) -> str:
    expected_device, expected_inode, expected_digest = parse_active_token(token)
    with open_active(root_arg, literal) as (
        root,
        plans,
        relative,
        basename,
        plans_fd,
        plan_fd,
        plan_stat,
    ):
        if (plan_stat.st_dev, plan_stat.st_ino) != (expected_device, expected_inode):
            raise PathError("active plan was replaced after it was read; refusing to update it")
        if hashlib.sha256(read_all(plan_fd)).hexdigest() != expected_digest:
            raise PathError("active plan changed after it was read; refusing to replace it")

        resolved_source = resolve_scratch_source(root, plans, source_arg)
        source_fd = os.open(resolved_source, file_read_flags())
        try:
            source_stat = os.fstat(source_fd)
            if not stat.S_ISREG(source_stat.st_mode):
                raise PathError("edited plan draft is not a regular file")
            if source_stat.st_nlink != 1:
                raise PathError("edited plan draft must not be hard-linked")
            payload = read_all(source_fd)
        finally:
            os.close(source_fd)

        temporary_name = f".{basename}.loopai-grill-{secrets.token_hex(8)}"
        temporary_fd: int | None = None
        temporary_created = False
        root_fd: int | None = None
        recovery_fd: int | None = None
        recovery_name = f".loopai-grill-recovery-{secrets.token_hex(16)}"
        recovery_display = f"{recovery_name}/original-plan"
        recovery_created = False
        displaced = False
        published = False
        prior_preserved = False
        try:
            temporary_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
            temporary_flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
            temporary_fd = os.open(
                temporary_name,
                temporary_flags,
                stat.S_IMODE(plan_stat.st_mode),
                dir_fd=plans_fd,
            )
            temporary_created = True
            os.fchmod(temporary_fd, stat.S_IMODE(plan_stat.st_mode))
            write_all(temporary_fd, payload)
            os.fsync(temporary_fd)
            os.close(temporary_fd)
            temporary_fd = None

            root_fd = os.open(root, directory_open_flags())
            os.mkdir(recovery_name, 0o700, dir_fd=root_fd)
            recovery_created = True
            recovery_fd = os.open(recovery_name, directory_open_flags(), dir_fd=root_fd)
            os.rename(
                basename,
                "original-plan",
                src_dir_fd=plans_fd,
                dst_dir_fd=recovery_fd,
            )
            displaced = True
            prior_preserved = True
            os.fsync(plans_fd)
            os.fsync(recovery_fd)

            displaced_fd = os.open(
                "original-plan",
                file_read_flags(),
                dir_fd=recovery_fd,
            )
            try:
                displaced_stat = os.fstat(displaced_fd)
                displaced_digest = hashlib.sha256(read_all(displaced_fd)).hexdigest()
            finally:
                os.close(displaced_fd)
            if (displaced_stat.st_dev, displaced_stat.st_ino) != (
                expected_device,
                expected_inode,
            ) or displaced_digest != expected_digest:
                raise PathError("active plan changed before publication; refusing to replace it")

            os.link(
                temporary_name,
                basename,
                src_dir_fd=plans_fd,
                dst_dir_fd=plans_fd,
                follow_symlinks=False,
            )
            published = True
            os.unlink(temporary_name, dir_fd=plans_fd)
            temporary_created = False
            os.fsync(plans_fd)
            os.unlink("original-plan", dir_fd=recovery_fd)
            displaced = False
            prior_preserved = False
            os.rmdir(recovery_name, dir_fd=root_fd)
            recovery_created = False
            os.fsync(root_fd)
        except Exception as exc:
            if published:
                if prior_preserved:
                    raise PathError(
                        f"active plan was published but cleanup failed; the prior version is "
                        f"preserved at {recovery_display}"
                    ) from exc
                raise PathError("active plan was published but recovery cleanup failed") from exc
            if displaced and root_fd is not None and recovery_fd is not None:
                try:
                    restored = restore_recovery(
                        root_fd,
                        plans_fd,
                        recovery_fd,
                        recovery_name,
                        basename,
                    )
                    if restored:
                        displaced = False
                        prior_preserved = False
                        recovery_created = False
                except OSError as restore_exc:
                    raise PathError(
                        f"active-plan update failed; recover the displaced plan from "
                        f"{recovery_display}: {restore_exc}"
                    ) from exc
                if not restored:
                    raise PathError(
                        f"active-plan update conflicted with another writer; the current path "
                        f"was not overwritten and the displaced plan is preserved at "
                        f"{recovery_display}"
                    ) from exc
            raise
        finally:
            if temporary_fd is not None:
                os.close(temporary_fd)
            if temporary_created:
                try:
                    os.unlink(temporary_name, dir_fd=plans_fd)
                except OSError:
                    pass
            if recovery_fd is not None:
                os.close(recovery_fd)
            if recovery_created and not displaced and root_fd is not None:
                try:
                    os.rmdir(recovery_name, dir_fd=root_fd)
                except OSError:
                    pass
            if root_fd is not None:
                os.close(root_fd)
    return relative


def looks_path_like(value: str) -> bool:
    stripped = value.strip()
    single_token = not any(character.isspace() for character in stripped)
    return (
        stripped.startswith(("/", "./", "../", "~/", "docs/plans/"))
        or (single_token and (stripped.endswith(".md") or "\\" in stripped))
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
            modified = path.stat().st_mtime_ns
        except (OSError, PathError):
            continue
        candidates.append((modified, relative))
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


def ensure_no_collision_fd(plans_fd: int, names: set[str]) -> None:
    folded = {name.casefold() for name in names}
    for name in os.listdir(plans_fd):
        if name.casefold() in folded:
            raise PathError(f"plan name collides with docs/plans/{name}")

    completed_fd: int | None = None
    try:
        completed_fd = os.open("completed", directory_open_flags(), dir_fd=plans_fd)
    except FileNotFoundError:
        return
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            raise PathError("docs/plans/completed must not be a symbolic link") from exc
        if exc.errno == errno.ENOTDIR:
            raise PathError("docs/plans/completed is not a directory") from exc
        raise
    try:
        for name in os.listdir(completed_fd):
            if name.casefold() in folded:
                raise PathError(f"plan name collides with docs/plans/completed/{name}")
    finally:
        os.close(completed_fd)


def ensure_no_collision(root_arg: str, relative: str) -> tuple[Path, Path, str]:
    root, plans, _ = repository_layout(root_arg)
    normalized, basename = direct_plan_relative(relative)
    names = collision_names(normalized)

    root_fd = os.open(root, directory_open_flags())
    docs_fd: int | None = None
    plans_fd: int | None = None
    try:
        docs_fd = os.open("docs", directory_open_flags(), dir_fd=root_fd)
        plans_fd = os.open("plans", directory_open_flags(), dir_fd=docs_fd)
        fcntl.flock(plans_fd, fcntl.LOCK_SH)
        ensure_no_collision_fd(plans_fd, names)
    finally:
        if plans_fd is not None:
            os.close(plans_fd)
        if docs_fd is not None:
            os.close(docs_fd)
        os.close(root_fd)
    return root, plans, basename


def write_final(root_arg: str, relative: str, source_arg: str) -> str:
    root, plans, _ = repository_layout(root_arg)
    normalized, basename = direct_plan_relative(relative)
    names = collision_names(normalized)
    resolved_source = resolve_scratch_source(root, plans, source_arg)

    directory_flags = os.O_RDONLY
    directory_flags |= getattr(os, "O_DIRECTORY", 0)
    directory_flags |= getattr(os, "O_CLOEXEC", 0)
    directory_flags |= getattr(os, "O_NOFOLLOW", 0)
    file_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    file_flags |= getattr(os, "O_CLOEXEC", 0)
    file_flags |= getattr(os, "O_NOFOLLOW", 0)
    source_fd = os.open(resolved_source, file_read_flags())
    try:
        source_stat = os.fstat(source_fd)
        if not stat.S_ISREG(source_stat.st_mode):
            raise PathError("final draft source is not a regular file")
        if source_stat.st_nlink != 1:
            raise PathError("final draft source must not be hard-linked")
        payload = read_all(source_fd)
    finally:
        os.close(source_fd)

    root_fd = os.open(root, directory_flags)
    docs_fd: int | None = None
    directory_fd: int | None = None
    temporary_name = f".{basename}.loopai-grill-{secrets.token_hex(8)}"
    temporary_fd: int | None = None
    temporary_created = False
    try:
        docs_fd = os.open("docs", directory_flags, dir_fd=root_fd)
        directory_fd = os.open("plans", directory_flags, dir_fd=docs_fd)
        fcntl.flock(directory_fd, fcntl.LOCK_EX)
        ensure_no_collision_fd(directory_fd, names)
        temporary_fd = os.open(temporary_name, file_flags, 0o644, dir_fd=directory_fd)
        temporary_created = True
        write_all(temporary_fd, payload)
        os.fsync(temporary_fd)
        os.close(temporary_fd)
        temporary_fd = None
        os.link(
            temporary_name,
            basename,
            src_dir_fd=directory_fd,
            dst_dir_fd=directory_fd,
            follow_symlinks=False,
        )
        os.unlink(temporary_name, dir_fd=directory_fd)
        temporary_created = False
        os.fsync(directory_fd)
    except FileExistsError as exc:
        raise PathError("output plan appeared before it could be created") from exc
    finally:
        if temporary_fd is not None:
            os.close(temporary_fd)
        if temporary_created and directory_fd is not None:
            try:
                os.unlink(temporary_name, dir_fd=directory_fd)
            except OSError:
                pass
        if directory_fd is not None:
            os.close(directory_fd)
        if docs_fd is not None:
            os.close(docs_fd)
        os.close(root_fd)
    return f"docs/plans/{basename}"


def main(argv: list[str]) -> int:
    if len(argv) < 3:
        fail("usage: plan_paths.py <validate-active|read-active|replace-active|validate-scratch|classify|newest-active|check-output|write-final> <repository-root> [arguments]")
    command, root_arg, *args = argv[1:]
    try:
        if command == "validate-active" and len(args) == 1:
            print(validate_active(root_arg, args[0])[1])
        elif command == "read-active" and len(args) == 2:
            print("\t".join(read_active(root_arg, args[0], args[1])))
        elif command == "replace-active" and len(args) == 3:
            print(replace_active(root_arg, args[0], args[1], args[2]))
        elif command == "validate-scratch" and len(args) == 1:
            print(validate_scratch(root_arg, args[0]))
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
