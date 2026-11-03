import hashlib
import io
import json
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/release"))
import guest_kernel_scan as scan


class GuestKernelScanTest(unittest.TestCase):
    def test_ipv4_race(self):
        affected = dict(package=dict(ecosystem="Linux", name="Kernel"), ranges=[
            dict(type="ECOSYSTEM", events=[{"introduced": "6.7.0"}, {"fixed": "6.12.113"}]),
            dict(type="ECOSYSTEM", events=[{"introduced": "6.13.0"}, {"fixed": "6.18.56"}]),
        ])
        record = dict(id="CVE-2026-98432", affected=[affected], severity=[
            dict(type="CVSS_V3", score="CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H")])
        old = scan.findings([record], "6.12.51")
        self.assertEqual(old["findings"][0]["fixes"], ["6.12.113"])
        for release in ("6.12.113", "6.12.114"):
            self.assertEqual(scan.findings([record], release)["findings"], [])
        affected["ranges"][1]["events"][0] = {"introduced": "6.7.0"}
        self.assertEqual(scan.findings([record], "6.12.114")["findings"], [])
        record["affected"].append(dict(versions=["v6.12.114"], ranges=[
            dict(type="GIT", repo="https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git")]))
        explicit = scan.findings([record], "6.12.114")
        self.assertEqual(explicit["findings"][0]["fixes"], ["6.18.56"])
        record["severity"] = []
        self.assertTrue(scan.findings([record], "6.12.51")["findings"])
        self.assertIsNone(scan.findings([record], "6.12.51")["findings"][0]["score"])

    def test_successive_stable_fixes(self):
        events = [{"introduced": "5.5.0"}, {"fixed": "5.10.96"}, {"fixed": "5.15.19"}]
        self.assertEqual(scan.affected_range(events, scan.version("5.15.18")), (True, "5.15.19"))
        self.assertEqual(scan.affected_range(events, scan.version("5.10.96")), (False, None))

    def test_packaged_kernel(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for kernel, valid in [(b"unidentified", False), (b"Linux version 6.12.114-0-virt (fixture)", True)]:
                data = b"\x7fELF" + kernel
                member = tarfile.TarInfo("vmlinuz")
                member.size = len(data)
                with tarfile.open(root / "runtime-artifacts.tar", "w") as archive:
                    archive.addfile(member, io.BytesIO(data))
                digest = "sha256:" + hashlib.sha256(data).hexdigest()
                manifest = root / "runtime-artifacts.json"
                manifest.write_text(json.dumps(dict(kernel=dict(path="vmlinuz", digest=digest, size_bytes=len(data)))))
                if valid:
                    self.assertEqual(scan.guest_kernel(root), ("6.12.114", digest))
                else:
                    with self.assertRaisesRegex(ValueError, "identify"):
                        scan.guest_kernel(root)
                manifest.write_text('{"kernel": {}}')
                with self.assertRaisesRegex(ValueError, "manifest"):
                    scan.guest_kernel(root)


if __name__ == "__main__":
    unittest.main()
