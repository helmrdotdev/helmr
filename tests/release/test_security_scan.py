import hashlib
import os
import subprocess
import time
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/release"))
from security_scan import check_terraform_inputs, collect, tracked_source
from security_policy import POLICY, load_policy, check_current


class SecurityScanTest(unittest.TestCase):
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
            tree, _ = tracked_source(root, repo)
            with self.assertRaisesRegex(ValueError, "inline suppression"):
                check_terraform_inputs(tree)

    def test_keys_ignore_transport_metadata_and_preserve_advisory_changes(self):
        entry = dict(VulnerabilityID="V", Severity="CRITICAL", PkgName="P", InstalledVersion="1")
        def key(**updates):
            row = dict(Type="debian", Target="debian", Vulnerabilities=[dict(entry, **updates)])
            return collect([row], "image", "digest")[0]["key"]
        self.assertEqual(key(Layer="registry"), key(Fingerprint="local"))
        self.assertNotEqual(key(), key(FixedVersion="2"))

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
