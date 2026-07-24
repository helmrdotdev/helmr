#!/usr/bin/env python3
import os
import sys
import tarfile


MANAGER_FILES = (
    "catalog.json",
    "catalog.sigstore.json",
    "trusted-root.json",
)


def regular_members(path):
    with tarfile.open(path, mode="r:") as archive:
        members = archive.getmembers()
        if any(not member.isfile() for member in members):
            raise ValueError("worker release input contains a non-regular member")
        names = [member.name for member in members]
        if names != sorted(names) or len(names) != len(set(names)):
            raise ValueError("worker release input members are not unique and sorted")
        return members


def manager_members(directory):
    members = {}
    for name in MANAGER_FILES:
        path = os.path.join(directory, name)
        stat = os.lstat(path)
        if not os.path.isfile(path) or os.path.islink(path) or stat.st_size < 1:
            raise ValueError(f"Manager release file {name!r} is not a non-empty regular file")
        members[f"manager-release/{name}"] = (path, stat.st_size)
    return members


def canonical_member(name, size):
    member = tarfile.TarInfo(name)
    member.size = size
    member.mode = 0o444
    member.uid = 0
    member.gid = 0
    member.uname = ""
    member.gname = ""
    member.mtime = 0
    return member


def write_package(destination, runtime_path, runtime_members, managers):
    if os.path.exists(destination):
        raise ValueError("worker release output already exists")
    runtime_by_name = {member.name: member for member in runtime_members}
    if runtime_by_name.keys() & managers.keys():
        raise ValueError("worker release input collides with Manager release members")
    names = sorted((*runtime_by_name, *managers))
    with (
        tarfile.open(runtime_path, mode="r:") as source_archive,
        tarfile.open(destination, mode="x:", format=tarfile.USTAR_FORMAT) as output,
    ):
        for name in names:
            runtime_member = runtime_by_name.get(name)
            if runtime_member is not None:
                source = source_archive.extractfile(runtime_member)
                if source is None:
                    raise ValueError(f"worker release member {name!r} has no content")
                with source:
                    output.addfile(
                        canonical_member(name, runtime_member.size),
                        fileobj=source,
                    )
                continue
            path, size = managers[name]
            with open(path, "rb") as source:
                output.addfile(canonical_member(name, size), fileobj=source)


def main():
    if len(sys.argv) != 4:
        raise SystemExit(
            "usage: package-worker-release.py <runtime-package> <manager-release-dir> <output>"
        )
    runtime_path = sys.argv[1]
    write_package(
        sys.argv[3],
        runtime_path,
        regular_members(runtime_path),
        manager_members(sys.argv[2]),
    )


if __name__ == "__main__":
    main()
