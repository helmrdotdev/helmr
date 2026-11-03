import gzip
import hashlib
import os
import subprocess
import time
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/release"))
from security_scan import check_terraform_inputs, collect, guest_files, initramfs_inventory, tracked_source
from security_decision import classify
from security_policy import POLICY, load_policy, check_current


class SecurityScanTest(unittest.TestCase):
    def test_inventory_observes_both_guest_filesystems(self):
        def entry(name, data=b""):
            fields = [1, 0o100755, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name) + 1, 0]
            header = ("070701" + "".join(f"{v:08x}" for v in fields) + name).encode() + b"\0"
            return header + bytes(-len(header) % 4) + data + bytes(-len(data) % 4)
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            archive = work / "initramfs"
            fs = work / "root"
            fs.mkdir()
            image = (entry("sbin/perf_6.12") + entry("lib/mlx5_core.ko", b"init copy")
                     + entry("lib/ntfs3.ko.xz") + entry("TRAILER!!!"))
            archive.write_bytes(gzip.compress(image))
            listing, hashes = initramfs_inventory(archive)
            (fs / "mlx5_core.ko").write_bytes(b"root copy")
            (fs / "airoha-eth.ko.zst").touch()
            modules, facts = guest_files(fs, listing, hashes, [], {"busybox"})
            self.assertFalse(facts["perf_absent"])
            self.assertFalse(facts["ntfs_absent"])
            self.assertFalse(facts["airoha_ethernet_absent"])
            self.assertEqual(modules, ["airoha_eth", "mlx5_core", "ntfs3"])
            self.assertEqual(set(facts["mlx5_core_hashes"]), {"sha256:" + hashlib.sha256(data).hexdigest()
                                      for data in (b"init copy", b"root copy")})
            (fs / "perf").symlink_to("/another/path")
            _, facts = guest_files(fs, [], set(), ["mlx5_core", "ntfs"], {"busybox"})
            self.assertFalse(facts["perf_absent"])
            self.assertFalse(facts["ntfs_absent"])
            self.assertEqual(facts["mlx5_core_hashes"], [])
            archive.write_bytes(gzip.compress(image + entry("bin/perf")))
            with self.assertRaisesRegex(ValueError, "trailing"):
                initramfs_inventory(archive)
            archive.write_bytes(gzip.compress(image[:-16]))
            with self.assertRaises(ValueError):
                initramfs_inventory(archive)

    def test_perf_aliases_packages_and_headers(self):
        with tempfile.TemporaryDirectory() as directory:
            fs = Path(directory)
            header = fs / "usr/include/linux/perf_event.h"
            header.parent.mkdir(parents=True)
            header.touch()
            def absent(packages):
                return guest_files(fs, [], set(), [], packages)[1]["perf_absent"]
            self.assertTrue(absent(["linux-headers"]))
            for name in ("perf", "linux-perf", "linux-tools-6.12"):
                self.assertFalse(absent([name]))
            for name in ("perf.real", "linux-perf", "perf_6.12"):
                tool = fs / name
                tool.touch()
                self.assertFalse(absent(["busybox"]))
                tool.unlink()

    def test_scan_reads_tracked_files_even_with_export_ignore(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo = root / "repo"
            subprocess.run(["git", "init", "-q", repo], check=True)
            (repo / ".gitattributes").write_text("network.tf export-ignore\n")
            (repo / "network.tf").write_text("#trivy:ignore:AVD-AWS-0104\n")
            subprocess.run(["git", "add", "."], cwd=repo, check=True)
            subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                            "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"], cwd=repo, check=True)
            tree, _, _ = tracked_source(root, repo)
            with self.assertRaisesRegex(ValueError, "inline suppression"):
                check_terraform_inputs(tree)

    def test_keys_ignore_transport_metadata_and_preserve_advisory_changes(self):
        entry = dict(VulnerabilityID="V", Severity="CRITICAL", PkgName="P", InstalledVersion="1")
        def key(**updates):
            row = dict(Type="debian", Target="debian", Vulnerabilities=[dict(entry, **updates)])
            return collect([row], "image", "digest")[0]["key"]
        self.assertEqual(key(Layer="registry"), key(Fingerprint="local"))
        self.assertNotEqual(key(), key(FixedVersion="2"))

    def test_debian_package_assessment_only_follows_the_reviewed_binary(self):
        entry = dict(VulnerabilityID="CVE-example", Severity="CRITICAL", PkgName="headers",
                     InstalledVersion="1", Layer={"DiffID": "sha256:" + "a"*64},
                     PkgIdentifier={"PURL": "pkg:deb/debian/headers@1?arch=amd64&distro=debian-12"})
        def finding(identity="image", scope="bundle-builder", **updates):
            row = dict(Type="debian", Class="os-pkgs", Target="debian", Vulnerabilities=[dict(entry, **updates)])
            return collect([row], scope, identity)[0]
        rule = dict(id="CVE-example", status="not_affected", debian_package_key=finding()["debian_package_key"],
                    reason="Header-only binary", evidence="Reviewed package contents")
        def state(item):
            return classify(dict(findings=[item]), [rule])["state"]
        self.assertEqual(state(finding(identity="rebuilt")), "clear")
        for changed in (dict(Layer={}), dict(Layer={"DiffID": "sha256:" + "b"*64}),
                        dict(PkgIdentifier={}), dict(PkgName="kernel"), dict(InstalledVersion="2"),
                        dict(Description="advisory changed"), dict(scope="control-plane")):
            self.assertEqual(state(finding(**changed)), "needs_review")

    def test_template_key_requires_same_template_and_occurrence(self):
        def key(source="source", templates="templates", resource="control", check="AWS-0104"):
            entry = dict(ID=check, Status="FAIL", Severity="CRITICAL",
                         CauseMetadata={"Occurrences": [{"Resource": resource}]})
            row = dict(Type="terraform", Target="infra/aws/example/main.tf", Misconfigurations=[entry])
            return collect([row], "source", source, templates)[0].get("template_key")
        self.assertIsNotNone(key())
        self.assertEqual(key(), key(source="unrelated change"))
        self.assertNotEqual(key(), key(templates="changed"))
        self.assertNotEqual(key(), key(resource="another rule"))
        self.assertIsNone(key(check="another check"))

    def test_secret_summary_has_location_without_matched_bytes(self):
        secret = dict(RuleID="rule", Severity="HIGH", StartLine=7, EndLine=8, Match="secret bytes")
        finding, = collect([dict(Type="text", Target="config", Secrets=[secret])], "source", "digest")
        self.assertEqual(finding["start"], 7)
        self.assertNotIn("secret bytes", str(finding))

    def test_publish_rejects_expired_or_replaced_assessment(self):
        now = int(time.time())
        policy = hashlib.sha256(b"[]").hexdigest()
        check_current(b"[]", policy, now, now)
        with self.assertRaisesRegex(ValueError, "expired"):
            check_current(b"[]", policy, now - 86401, now)
        with self.assertRaisesRegex(ValueError, "changed"):
            check_current(b"[ ]", policy, now, now)

    def test_assessment_requires_main_merge_by_release_reviewer(self):
        class API:
            def pages(self, path):
                assert path == "issues/7/comments"
                yield dict(node_id="comment", user={"id": self.reviewer, "node_id": "actor"}, body=self.marker)

            def original_comment(self, node_id):
                assert node_id == "comment"
                return dict(author={"id": "actor"}, lastEditedAt=self.edited,
                            isMinimized=False, body=self.marker)

            def request(self, path):
                return {
                    "git/ref/heads/main": {"object": {"sha": "head"}},
                    f"commits?sha=head&path={POLICY}&per_page=1": [{"sha": "merge"}],
                    "commits/merge/pulls": [dict(base={"ref": "main"}, merge_commit_sha="merge", merged_at="now", number=7)],
                    "environments/release": {"protection_rules": [dict(type="required_reviewers",
                        reviewers=[dict(type="User", reviewer={"id": 1})])]},
                    f"contents/{POLICY}?ref=head": dict(sha="blob", content="W10="),
                    f"contents/{POLICY}?ref=merge": dict(sha="blob"),
                }[path]
        api = API()
        api.marker = "security-assessments-sha256 " + hashlib.sha256(b"[]").hexdigest()
        api.edited = None
        api.reviewer = 1
        self.assertEqual(load_policy(api), b"[]")
        api.edited = "later"
        with self.assertRaisesRegex(ValueError, "entire policy"):
            load_policy(api)
        api.edited = None
        api.reviewer = 2
        with self.assertRaisesRegex(ValueError, "designated release reviewer"):
            load_policy(api)
        api.reviewer = 1
        api.marker = "security-assessments-sha256 " + "0" * 64
        with self.assertRaisesRegex(ValueError, "entire policy"):
            load_policy(api)


if __name__ == "__main__":
    unittest.main()
