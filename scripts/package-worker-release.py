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
        values = {}
        for member in members:
            source = archive.extractfile(member)
            if source is None:
                raise ValueError(f"worker release member {member.name!r} has no content")
            with source:
                values[member.name] = source.read()
        return values


def manager_members(directory):
    values = {}
    for name in MANAGER_FILES:
        path = os.path.join(directory, name)
        stat = os.lstat(path)
        if not os.path.isfile(path) or os.path.islink(path) or stat.st_size < 1:
            raise ValueError(f"Manager release file {name!r} is not a non-empty regular file")
        with open(path, "rb") as source:
            values[f"manager-release/{name}"] = source.read()
    return values


def write_package(destination, values):
    if os.path.exists(destination):
        raise ValueError("worker release output already exists")
    with tarfile.open(destination, mode="x:", format=tarfile.USTAR_FORMAT) as archive:
        for name in sorted(values):
            body = values[name]
            member = tarfile.TarInfo(name)
            member.size = len(body)
            member.mode = 0o444
            member.uid = 0
            member.gid = 0
            member.uname = ""
            member.gname = ""
            member.mtime = 0
            archive.addfile(member, fileobj=BytesReader(body))


class BytesReader:
    def __init__(self, value):
        self.value = value
        self.offset = 0

    def read(self, size=-1):
        if size < 0:
            size = len(self.value) - self.offset
        start = self.offset
        self.offset = min(len(self.value), self.offset + size)
        return self.value[start:self.offset]


def main():
    if len(sys.argv) != 4:
        raise SystemExit(
            "usage: package-worker-release.py <runtime-package> <manager-release-dir> <output>"
        )
    values = regular_members(sys.argv[1])
    for name, body in manager_members(sys.argv[2]).items():
        if name in values:
            raise ValueError(f"worker release input already contains {name!r}")
        values[name] = body
    write_package(sys.argv[3], values)


if __name__ == "__main__":
    main()
