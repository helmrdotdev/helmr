"""Read assessment data accepted on main by a designated release maintainer."""
import base64
import hashlib
import json
import subprocess
import time
from pathlib import Path
import sys
from api import GitHub
from contract import require

POLICY = ".github/security-assessments.json"


class PolicyAPI(GitHub):
    def original_comment(self, node_id):
        query = """query($id:ID!) { node(id:$id) { ... on IssueComment {
          body lastEditedAt isMinimized author { ... on User { id } }
        } } }"""
        raw = subprocess.check_output(["gh", "api", "graphql", "-f", "query=" + query, "-f", "id=" + node_id])
        return json.loads(raw)["data"]["node"]


def attested(api, comments, reviewers, marker):
    for comment in comments:
        if comment["user"]["id"] not in reviewers or comment["body"].strip() != marker:
            continue
        # REST attributes the current body to the original author even when a
        # different repository collaborator edited it. Verify one original view.
        original = api.original_comment(comment["node_id"])
        if (original and original["author"] and original["author"]["id"] == comment["user"]["node_id"]
                and original["lastEditedAt"] is None and not original["isMinimized"]
                and original["body"].strip() == marker):
            return True
    return False


def load_policy(api):
    head = api.request("git/ref/heads/main")["object"]["sha"]
    change, = api.request(f"commits?sha={head}&path={POLICY}&per_page=1")
    sha = change["sha"]
    prs = [p for p in api.request(f"commits/{sha}/pulls")
           if p["base"]["ref"] == "main" and p["merge_commit_sha"] == sha and p["merged_at"]]
    require(len(prs) == 1, "assessment changes require a merged main pull request")
    env = api.request("environments/release")
    reviewers = {r["reviewer"]["id"] for rule in env["protection_rules"] if rule["type"] == "required_reviewers"
                 for r in rule["reviewers"] if r["type"] == "User"}
    policy = api.request(f"contents/{POLICY}?ref={head}")
    accepted = api.request(f"contents/{POLICY}?ref={sha}")
    require(policy["sha"] == accepted["sha"], "main assessments differ from reviewed policy")
    data = base64.b64decode(policy["content"])
    marker = "security-assessments-sha256 " + hashlib.sha256(data).hexdigest()
    comments = api.pages(f"issues/{prs[0]['number']}/comments")
    require(attested(api, comments, reviewers, marker),
            f"designated release reviewer must attest the entire policy on PR #{prs[0]['number']}: {marker}")
    return data


def check_current(data, expected, checked, now):
    require(0 <= now - checked < 86400, "release security assessment expired")
    require(hashlib.sha256(data).hexdigest() == expected, "release security policy changed")


if __name__ == "__main__":
    policy = load_policy(PolicyAPI())
    if len(sys.argv) == 4 and sys.argv[1] == "check":
        check_current(policy, sys.argv[2], int(sys.argv[3]), time.time())
    elif len(sys.argv) == 2:
        Path(sys.argv[1]).write_bytes(policy)
    else:
        raise ValueError("expected output path or check POLICY CHECKED")
