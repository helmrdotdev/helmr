import json
import os
import shutil
import sys
import tarfile

ALLOWED = {
    "catalog.json",
    "catalog.sigstore.json",
    "trusted-root.json",
    "verifier-corpus.json",
    "verifier-invalid.squashfs",
    "verifier-valid.squashfs",
}


def raw_members(path):
    names = []
    with open(path, "rb") as archive:
        while True:
            header = archive.read(512)
            if len(header) != 512:
                raise ValueError("runtime release package has no complete end marker")
            if not any(header):
                second = archive.read(512)
                if len(second) != 512 or any(second):
                    raise ValueError("runtime release package has trailing archive data")
                while remainder := archive.read(1024 * 1024):
                    if any(remainder):
                        raise ValueError("runtime release package has trailing archive data")
                break
            if header[156:157] not in (b"0", b"\0") or any(header[345:500]):
                raise ValueError("runtime release package contains a non-regular raw member")
            name_field = header[:100]
            name, separator, suffix = name_field.partition(b"\0")
            if separator and any(suffix):
                raise ValueError("runtime release package contains a malformed member name")
            try:
                names.append(name.decode("ascii"))
            except UnicodeDecodeError as error:
                raise ValueError("runtime release package member name is not ASCII") from error
            size_field = header[124:136].strip(b" \0")
            if not size_field or any(value not in b"01234567" for value in size_field):
                raise ValueError("runtime release package member size is not portable octal")
            size = int(size_field, 8)
            archive.seek((size + 511) // 512 * 512, os.SEEK_CUR)
    return names


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"runtime verifier corpus contains duplicate key: {key}")
        result[key] = value
    return result


def extract(archive, destination, architecture):
    names = raw_members(archive)
    if len(names) != len(ALLOWED) or len(names) != len(set(names)) or set(names) != ALLOWED:
        raise ValueError("runtime release package must contain exactly six raw allowlisted members")

    os.mkdir(destination, 0o700)
    with tarfile.open(archive, mode="r:") as package:
        if package.pax_headers:
            raise ValueError("runtime release package must not contain global PAX headers")
        members = package.getmembers()
        logical_names = [member.name for member in members]
        if (
            len(members) != len(ALLOWED)
            or len(logical_names) != len(set(logical_names))
            or set(logical_names) != ALLOWED
        ):
            raise ValueError("runtime release package must contain exactly the six allowlisted members")

        for member in members:
            if (
                member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE)
                or member.pax_headers
                or member.sparse is not None
            ):
                raise ValueError(f"runtime release package member is not a plain regular file: {member.name}")
            source = package.extractfile(member)
            if source is None:
                raise ValueError(f"runtime release package member has no content: {member.name}")
            target = os.path.join(destination, member.name)
            descriptor = os.open(
                target,
                os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0),
                0o600,
            )
            with source, os.fdopen(descriptor, "wb") as output:
                shutil.copyfileobj(source, output)

    with open(os.path.join(destination, "verifier-corpus.json"), "rb") as manifest:
        corpus = json.load(manifest, object_pairs_hook=unique_object)

    if (
        set(corpus) != {"formatVersion", "valid", "invalid"}
        or corpus["formatVersion"] != 0
        or set(corpus["valid"]) != {"descriptor", "expectedIndex"}
        or set(corpus["invalid"]) != {"descriptor"}
        or corpus["valid"]["descriptor"].get("architecture") != architecture
        or corpus["valid"]["expectedIndex"].get("architecture") != architecture
        or corpus["invalid"]["descriptor"].get("architecture") != architecture
    ):
        raise ValueError(f"runtime verifier corpus is not the closed {architecture} v0 package")


if __name__ == "__main__":
    extract(*sys.argv[1:])
