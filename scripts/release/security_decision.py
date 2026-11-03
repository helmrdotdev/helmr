"""Classify completed scans separately from the success of the scanner."""
import argparse
import hashlib
import os
from pathlib import Path
import re
from contract import read, require, write


def classify(report, rules):
    critical = [i for i in report["findings"] if i["severity"] == "CRITICAL"
                or (i.get("kind") == "secret" and i["severity"] == "HIGH")]
    require(isinstance(rules, list), "assessments must be a list")
    outcomes = {}
    guest = report.get("guest", {})
    for rule in rules:
        reason = rule["reason"]
        require(isinstance(reason, str) and reason.strip() and rule["evidence"].strip(),
                "assessment requires a reason and evidence")
        status = rule["status"]
        require(status in ("affected", "not_affected"), "unsupported assessment")
        if "absent_module" in rule:
            require(status == "not_affected" and re.fullmatch(r"[a-z0-9_]+", rule["absent_module"]),
                    "invalid module predicate")
            module = rule["absent_module"]
            matched = [i for i in critical if i["component"] == "kernel" and i["id"] == rule["id"]
                       and i["advisory"] == rule["advisory"] and guest["digest"] == rule["kernel"]
                       and module in guest["optional"] and module not in guest["builtin"]
                       and module not in guest["modules"]]
        else:
            require(re.fullmatch("[a-f0-9]{64}", rule["key"]), "invalid finding key")
            matched = [i for i in critical if i["id"] == rule["id"] and i["key"] == rule["key"]]
            basis = rule["basis"]
            require(basis in ("component", "product"), "assessment must identify its evidence scope")
            if basis == "product":
                require(re.fullmatch("[a-f0-9]{64}", rule["source"]), "invalid source evidence")
                if rule["source"] != report["source"]:
                    matched = []
        for item in matched:
            require(item["key"] not in outcomes, "conflicting assessments for " + item["id"])
            outcomes[item["key"]] = dict(status=status, reason=reason, evidence=rule["evidence"])
    rows = [dict(item, **outcomes.get(item["key"], {"status": "needs_review"})) for item in critical]
    state = ("affected" if any(i["status"] == "affected" for i in rows) else
             "needs_review" if any(i["status"] == "needs_review" for i in rows) else "clear")
    return dict(state=state, critical=rows)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path)
    parser.add_argument("assessments", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--previous", type=Path, help="daily notification comparison only")
    args = parser.parse_args()
    verdict = classify(read(args.report), read(args.assessments))
    verdict["policy"] = hashlib.sha256(args.assessments.read_bytes()).hexdigest()
    write(args.output, verdict)
    print(f"Release security: {verdict['state']} ({len(verdict['critical'])} observations requiring assessment)")
    if "GITHUB_OUTPUT" in os.environ:
        with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
            print("state=" + verdict["state"], file=stream)
            if args.previous:
                previous = read(args.previous)["critical"] if args.previous.exists() else []
                known = {(i["key"], i["status"]) for i in previous}
                attention = any(i["status"] != "not_affected" and (i["key"], i["status"]) not in known
                                for i in verdict["critical"])
                print(f"attention={str(attention).lower()}", file=stream)
            print("policy=" + verdict["policy"], file=stream)
    if "GITHUB_STEP_SUMMARY" in os.environ:
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            print(f"### Security assessment — {verdict['state']}\n", file=stream)
            print("See the attached JSON for affected packages, versions, evidence keys and all severities.\n", file=stream)
            for item in verdict["critical"][:100]:
                label = re.sub(r"[^a-zA-Z0-9._/-]", "_", f"{item['component']}/{item['id']}")
                print(f"- `{label}` · {item['status']} · `{item['key']}`", file=stream)


if __name__ == "__main__":
    main()
