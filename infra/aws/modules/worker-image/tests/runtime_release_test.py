import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).parents[1] / "templates" / "runtime-release.py"
SPEC = importlib.util.spec_from_file_location("runtime_release", SCRIPT)
runtime_release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime_release)


def corpus(architecture):
    return json.dumps(
        {
            "formatVersion": 0,
            "valid": {
                "descriptor": {"architecture": architecture},
                "expectedIndex": {"architecture": architecture},
            },
            "invalid": {"descriptor": {"architecture": architecture}},
        },
        separators=(",", ":"),
    ).encode()


def toolchain(architecture, content, size=None):
    if size is None:
        size = len(content)
    return {
        "architecture": architecture,
        "formatVersion": 0,
        "managedRuntimeDigest": "sha256:" + hashlib.sha256(b"runtime").hexdigest(),
        "toolchainClosure": {
            "digest": "sha256:" + hashlib.sha256(content).hexdigest(),
            "mediaType": "application/vnd.helmr.standard-toolchain.v0+squashfs",
            "sizeBytes": size,
        },
    }


def package(
    path,
    architecture="x86_64",
    duplicate=False,
    toolchains=None,
    corrupt=False,
    extra_object=False,
    omit_object=False,
):
    if toolchains is None:
        toolchains = [(architecture, b"toolchain")]
    candidates = [
        entry if len(entry) == 3 else (*entry, None)
        for entry in toolchains
    ]
    catalog = {
        "formatVersion": 0,
        "toolchains": [
            toolchain(member_architecture, content, size)
            for member_architecture, content, size in candidates
        ],
    }
    contents = {
        "catalog.json": b"catalog",
        "catalog.sigstore.json": b"bundle",
        "trusted-root.json": b"root",
        "toolchain-release/catalog.json": json.dumps(
            catalog,
            separators=(",", ":"),
            sort_keys=True,
        ).encode(),
        "toolchain-release/catalog.sigstore.json": b"toolchain bundle",
        "toolchain-release/trusted-root.json": b"root",
        "verifier-corpus.json": corpus(architecture),
        "verifier-invalid.squashfs": b"invalid",
        "verifier-valid.squashfs": b"valid",
    }
    for member_architecture, content, _ in candidates:
        if member_architecture != architecture:
            continue
        digest = hashlib.sha256(content).hexdigest()
        contents[f"toolchain-release/objects/sha256/{digest}"] = (
            b"corrupt" if corrupt else content
        )
    if omit_object:
        del contents[next(name for name in contents if name.startswith(runtime_release.OBJECT_PREFIX))]
    if extra_object:
        contents[runtime_release.OBJECT_PREFIX + "f" * 64] = b"extra"
    with tarfile.open(path, "w", format=tarfile.USTAR_FORMAT) as archive:
        for name, content in contents.items():
            member = tarfile.TarInfo(name)
            member.size = len(content)
            archive.addfile(member, io.BytesIO(content))
        if duplicate:
            content = contents["catalog.json"]
            member = tarfile.TarInfo("catalog.json")
            member.size = len(content)
            archive.addfile(member, io.BytesIO(content))


def checksum(header):
    header[148:156] = b"        "
    header[148:156] = f"{sum(header):06o}\0 ".encode()


class RuntimeReleaseTest(unittest.TestCase):
    def extract(self, archive):
        destination = Path(self.directory.name) / "out"
        runtime_release.extract(str(archive), str(destination), "x86_64")
        return destination

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)

    def tearDown(self):
        self.directory.cleanup()

    def test_extracts_exact_x86_64_package(self):
        archive = self.root / "release.tar"
        package(archive)

        destination = self.extract(archive)

        digest = hashlib.sha256(b"toolchain").hexdigest()
        self.assertEqual(
            {
                str(entry.relative_to(destination))
                for entry in destination.rglob("*")
                if entry.is_file()
            },
            runtime_release.FIXED
            | {f"{runtime_release.OBJECT_PREFIX}{digest}"},
        )

    def test_rejects_duplicate_member(self):
        archive = self.root / "duplicate.tar"
        package(archive, duplicate=True)

        with self.assertRaisesRegex(ValueError, "duplicate or divergent"):
            self.extract(archive)

    def test_rejects_concatenated_tar(self):
        first = self.root / "first.tar"
        second = self.root / "second.tar"
        package(first)
        package(second)
        first.write_bytes(first.read_bytes() + second.read_bytes())

        with self.assertRaisesRegex(ValueError, "trailing archive data"):
            self.extract(first)

    def test_rejects_gnu_extension_header(self):
        archive = self.root / "longlink.tar"
        package(archive)
        header = bytearray(archive.read_bytes()[:512])
        header[:100] = b"././@LongLink".ljust(100, b"\0")
        header[124:136] = b"00000000015\0"
        header[156:157] = b"L"
        checksum(header)
        payload = b"catalog.json\0".ljust(512, b"\0")
        archive.write_bytes(bytes(header) + payload + archive.read_bytes())

        with self.assertRaisesRegex(ValueError, "non-regular raw member"):
            self.extract(archive)

    def test_rejects_contiguous_file_type(self):
        archive = self.root / "contiguous.tar"
        package(archive)
        raw = bytearray(archive.read_bytes())
        raw[156:157] = b"7"
        header = raw[:512]
        checksum(header)
        raw[:512] = header
        archive.write_bytes(raw)

        with self.assertRaisesRegex(ValueError, "non-regular raw member"):
            self.extract(archive)

    def test_rejects_compressed_tar(self):
        source = self.root / "release.tar"
        archive = self.root / "release.tar.gz"
        package(source)
        archive.write_bytes(gzip.compress(source.read_bytes()))

        with self.assertRaises(ValueError):
            self.extract(archive)

    def test_rejects_wrong_architecture(self):
        archive = self.root / "arm.tar"
        package(archive, architecture="aarch64")

        with self.assertRaisesRegex(ValueError, "no x86_64 closure"):
            self.extract(archive)

    def test_extracts_all_predecessor_closures_for_architecture(self):
        archive = self.root / "release.tar"
        toolchains = [
            ("aarch64", b"arm"),
            ("x86_64", b"current"),
            ("x86_64", b"predecessor"),
        ]
        package(archive, toolchains=toolchains)

        destination = self.extract(archive)

        objects = destination / "toolchain-release" / "objects" / "sha256"
        self.assertEqual(
            {entry.name for entry in objects.iterdir()},
            {
                hashlib.sha256(b"current").hexdigest(),
                hashlib.sha256(b"predecessor").hexdigest(),
            },
        )

    def test_rejects_missing_catalog_object(self):
        archive = self.root / "release.tar"
        package(archive, omit_object=True)

        with self.assertRaisesRegex(ValueError, "does not exact-match"):
            self.extract(archive)

    def test_rejects_extra_catalog_object(self):
        archive = self.root / "release.tar"
        package(archive, extra_object=True)

        with self.assertRaisesRegex(ValueError, "does not exact-match"):
            self.extract(archive)

    def test_rejects_closure_digest_drift(self):
        archive = self.root / "release.tar"
        package(archive, corrupt=True)

        with self.assertRaisesRegex(ValueError, "does not match its descriptor"):
            self.extract(archive)

    def test_rejects_oversized_closure_before_extraction(self):
        archive = self.root / "release.tar"
        package(
            archive,
            toolchains=[
                (
                    "x86_64",
                    b"toolchain",
                    runtime_release.MAX_TOOLCHAIN_BYTES + 1,
                )
            ],
        )

        with self.assertRaisesRegex(ValueError, "invalid closure"):
            self.extract(archive)

    def test_rejects_aggregate_corpus_overflow_before_extraction(self):
        archive = self.root / "release.tar"
        package(
            archive,
            toolchains=[
                (
                    "x86_64",
                    f"toolchain-{position}".encode(),
                    runtime_release.MAX_TOOLCHAIN_BYTES,
                )
                for position in range(5)
            ],
        )

        with self.assertRaisesRegex(ValueError, "physical corpus bound"):
            self.extract(archive)


if __name__ == "__main__":
    unittest.main()
