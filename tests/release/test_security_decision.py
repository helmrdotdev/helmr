import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/release"))
from security_decision import classify


class SecurityDecisionTest(unittest.TestCase):
    def setUp(self):
        self.item = dict(id="example", key="a"*64, component="kernel", severity="CRITICAL", advisory="rev")
        self.guest = dict(optional=["driver"], modules=[], builtin=[], digest="kernel")
        self.report = dict(findings=[self.item], guest=self.guest)
        self.rule = dict(id="example", status="not_affected", reason="Requires the optional driver",
                         evidence="upstream advisory and module inventory", advisory="rev",
                         kernel="kernel", absent_module="driver")

    def test_kernel_applicability_reopens(self):
        self.assertEqual(classify(self.report, [self.rule])["state"], "clear")
        for change in [dict(modules=["driver"]), dict(builtin=["driver"]), dict(optional=[]), dict(digest="new")]:
            self.assertEqual(classify(dict(self.report, guest=dict(self.guest, **change)), [self.rule])["state"], "needs_review")
        self.item["advisory"] = "revised"
        self.assertEqual(classify(self.report, [self.rule])["state"], "needs_review")

    def test_release_guest_predicates(self):
        policy = Path(__file__).resolve().parents[2] / ".github/security-assessments.json"
        rules = [rule for rule in json.loads(policy.read_text()) if "guest_fact" in rule]
        self.assertEqual(len(rules), 5)
        for rule in rules:
            with self.subTest(advisory=rule["id"]):
                fact, expected = rule["guest_fact"], rule["equals"]
                item = dict(self.item, id=rule["id"], advisory=rule["advisory"])
                guest = dict(digest=rule.get("kernel", "another kernel"), facts={fact: expected})
                def state():
                    return classify(dict(findings=[item], guest=guest), [rule])["state"]
                self.assertEqual(state(), "clear")
                item["key"] = "b"*64  # Unrelated runtime manifest change.
                self.assertEqual(state(), "clear")
                guest["facts"][fact] = False if expected is True else []
                self.assertEqual(state(), "needs_review")
                guest["facts"].clear()
                self.assertEqual(state(), "needs_review")
                guest["facts"][fact] = expected
                guest["digest"] = "new"
                self.assertEqual(state(), "needs_review" if "kernel" in rule else "clear")
                guest["digest"] = rule.get("kernel", "new")
                item["advisory"] = "changed"
                self.assertEqual(state(), "needs_review")

    def test_approved_template_default_only_clears_its_own_finding(self):
        rule = dict(id="AWS-0104", template_key="f"*64, status="accepted_default",
                    reason="Operator-configured destinations", evidence="template and operator guide")
        item = dict(id=rule["id"], template_key=rule["template_key"], key="a"*64,
                    severity="CRITICAL", component="source")
        verdict = classify(dict(findings=[item]), [rule])
        self.assertEqual(verdict["state"], "clear")
        self.assertEqual(verdict["critical"][0]["status"], "accepted_default")
        for other in [dict(item, template_key="c"*64), dict(item, component="kernel"),
                      dict(item, id="another check"), {k:v for k,v in item.items() if k!="template_key"}]:
            self.assertEqual(classify(dict(findings=[item, dict(other, key="b"*64)]), [rule])["state"],
                             "needs_review")
        with self.assertRaises(ValueError):
            classify(dict(findings=[item]), [dict(rule, id="another check")])

    def test_findings_and_scanner_errors_are_distinct(self):
        self.assertEqual(classify(self.report, [])["state"], "needs_review")
        entry = dict(id="example", status="affected", key=self.item["key"], basis="component", reason="callable", evidence="source")
        self.assertEqual(classify(self.report, [entry])["state"], "affected")
        self.assertEqual(classify(self.report, [dict(entry, status="not_affected")])["state"], "clear")
        product = dict(entry, status="not_affected", basis="product", source="c"*64)
        self.assertEqual(classify(dict(self.report, source="c"*64), [product])["state"], "clear")
        self.assertEqual(classify(dict(self.report, source="d"*64), [product])["state"], "needs_review")
        with self.assertRaises(KeyError):
            classify(self.report, [{k: v for k, v in entry.items() if k != "basis"}])
        self.item["key"] = "b"*64
        self.assertEqual(classify(self.report, [entry])["state"], "needs_review")
        with self.assertRaises(ValueError):
            classify(self.report, [dict(entry, status="accept_risk")])
        with self.assertRaises(KeyError):
            classify({}, [])
        self.item.update(component="source", severity="HIGH", kind="secret")
        self.assertEqual(classify(self.report, [])["state"], "needs_review")


if __name__ == "__main__":
    unittest.main()
