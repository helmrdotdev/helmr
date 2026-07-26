import hashlib
import json
import os
import re
import sys
import tarfile

FIXED = {
    "catalog.json",
    "catalog.sigstore.json",
    "manager-release/catalog.json",
    "manager-release/catalog.sigstore.json",
    "manager-release/trusted-root.json",
    "trusted-root.json",
    "toolchain-release/catalog.json",
    "toolchain-release/catalog.sigstore.json",
    "toolchain-release/trusted-root.json",
    "verifier-corpus.json",
    "verifier-invalid.squashfs",
    "verifier-valid.squashfs",
}
OBJECT_PREFIX = "toolchain-release/objects/sha256/"
SHA256 = re.compile(r"sha256:[0-9a-f]{64}")
TOOLCHAIN_MEDIA_TYPE = "application/vnd.helmr.standard-toolchain.v0+squashfs"
ARCHITECTURE = "x86_64"
MAX_TOOLCHAINS = 1024
MAX_TOOLCHAIN_BYTES = 4 << 30
MAX_TOOLCHAIN_CORPUS_BYTES = 16 << 30


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
            raise ValueError(f"runtime release document contains duplicate key: {key}")
        result[key] = value
    return result


def reject_constant(value):
    raise ValueError(f"runtime release document contains invalid number: {value}")


def load_document(source):
    return json.load(
        source,
        object_pairs_hook=unique_object,
        parse_constant=reject_constant,
    )


def toolchain_objects(catalog):
    if (
        set(catalog) != {"formatVersion", "toolchains"}
        or type(catalog["formatVersion"]) is not int
        or catalog["formatVersion"] != 0
        or type(catalog["toolchains"]) is not list
        or not 1 <= len(catalog["toolchains"]) <= MAX_TOOLCHAINS
    ):
        raise ValueError("standard-toolchain catalog is not the closed v0 document")

    objects = {}
    total = 0
    for position, toolchain in enumerate(catalog["toolchains"]):
        if (
            type(toolchain) is not dict
            or set(toolchain)
            != {
                "architecture",
                "formatVersion",
                "managedRuntimeDigest",
                "toolchainClosure",
            }
            or toolchain["architecture"] != ARCHITECTURE
            or type(toolchain["formatVersion"]) is not int
            or toolchain["formatVersion"] != 0
            or type(toolchain["managedRuntimeDigest"]) is not str
            or SHA256.fullmatch(toolchain["managedRuntimeDigest"]) is None
        ):
            raise ValueError(
                f"standard-toolchain catalog member {position} is not a closed v0 toolchain"
            )
        closure = toolchain["toolchainClosure"]
        if (
            type(closure) is not dict
            or set(closure) != {"digest", "mediaType", "sizeBytes"}
            or type(closure["digest"]) is not str
            or SHA256.fullmatch(closure["digest"]) is None
            or closure["mediaType"] != TOOLCHAIN_MEDIA_TYPE
            or type(closure["sizeBytes"]) is not int
            or not 1 <= closure["sizeBytes"] <= MAX_TOOLCHAIN_BYTES
        ):
            raise ValueError(
                f"standard-toolchain catalog member {position} has an invalid closure"
            )
        name = OBJECT_PREFIX + closure["digest"].removeprefix("sha256:")
        descriptor = (closure["digest"], closure["sizeBytes"])
        if name in objects:
            if objects[name] != descriptor:
                raise ValueError(
                    "standard-toolchain catalog maps one closure digest to divergent bytes"
                )
            continue
        if total > MAX_TOOLCHAIN_CORPUS_BYTES - closure["sizeBytes"]:
            raise ValueError(
                "standard-toolchain catalog exceeds the physical corpus bound"
            )
        objects[name] = descriptor
        total += closure["sizeBytes"]

    if not objects:
        raise ValueError("standard-toolchain catalog contains no x86_64 closure")
    return objects


def extract(archive, destination):
    names = raw_members(archive)
    with tarfile.open(archive, mode="r:") as package:
        if package.pax_headers:
            raise ValueError("runtime release package must not contain global PAX headers")
        members = package.getmembers()
        logical_names = [member.name for member in members]
        if names != logical_names or len(names) != len(set(names)):
            raise ValueError("runtime release package contains duplicate or divergent members")
        try:
            catalog_member = package.getmember("toolchain-release/catalog.json")
        except KeyError as error:
            raise ValueError("runtime release package has no standard-toolchain catalog") from error
        catalog_source = package.extractfile(catalog_member)
        if catalog_source is None:
            raise ValueError("standard-toolchain catalog has no content")
        with catalog_source:
            objects = toolchain_objects(load_document(catalog_source))
        expected = FIXED | set(objects)
        if set(names) != expected or len(names) != len(expected):
            raise ValueError(
                "runtime release package does not exact-match its standard-toolchain catalog"
            )

        os.mkdir(destination, 0o700)
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
            os.makedirs(os.path.dirname(target), mode=0o700, exist_ok=True)
            descriptor = os.open(
                target,
                os.O_CREAT | os.O_EXCL | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0),
                0o600,
            )
            digest = hashlib.sha256()
            size = 0
            with source, os.fdopen(descriptor, "wb") as output:
                while chunk := source.read(1024 * 1024):
                    size += len(chunk)
                    digest.update(chunk)
                    output.write(chunk)
            if member.name in objects:
                expected_digest, expected_size = objects[member.name]
                actual_digest = "sha256:" + digest.hexdigest()
                if size != expected_size or actual_digest != expected_digest:
                    raise ValueError(
                        f"standard-toolchain closure does not match its descriptor: {member.name}"
                    )

    with open(os.path.join(destination, "verifier-corpus.json"), "rb") as manifest:
        corpus = load_document(manifest)

    if (
        set(corpus) != {"formatVersion", "valid", "invalid"}
        or corpus["formatVersion"] != 0
        or set(corpus["valid"]) != {"descriptor", "expectedIndex"}
        or set(corpus["invalid"]) != {"descriptor"}
        or corpus["valid"]["descriptor"].get("architecture") != ARCHITECTURE
        or corpus["valid"]["expectedIndex"].get("architecture") != ARCHITECTURE
        or corpus["invalid"]["descriptor"].get("architecture") != ARCHITECTURE
    ):
        raise ValueError("runtime verifier corpus is not the closed x86_64 v0 package")


if __name__ == "__main__":
    extract(*sys.argv[1:])
