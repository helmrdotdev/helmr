import gzip
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


def package(path, architecture="x86_64", duplicate=False):
    contents = {
        "catalog.json": b"catalog",
        "catalog.sigstore.json": b"bundle",
        "trusted-root.json": b"root",
        "verifier-corpus.json": corpus(architecture),
        "verifier-invalid.squashfs": b"invalid",
        "verifier-valid.squashfs": b"valid",
    }
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

        self.assertEqual(
            {entry.name for entry in destination.iterdir()},
            runtime_release.ALLOWED,
        )

    def test_rejects_duplicate_member(self):
        archive = self.root / "duplicate.tar"
        package(archive, duplicate=True)

        with self.assertRaisesRegex(ValueError, "exactly six raw"):
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

        with self.assertRaisesRegex(ValueError, "closed x86_64"):
            self.extract(archive)


if __name__ == "__main__":
    unittest.main()
