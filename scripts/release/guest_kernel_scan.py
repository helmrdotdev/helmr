"""Check the kernel in the packaged guest against OSV's Linux database."""
import argparse
import datetime
import hashlib
import io
import json
import re
import sys
import tarfile
import urllib.request
import zipfile
from email.utils import parsedate_to_datetime
from pathlib import Path

from cvss import CVSS2, CVSS3, CVSS4
from cvss.exceptions import CVSSError

DATABASE = "https://storage.googleapis.com/osv-vulnerabilities/Linux/all.zip"
BANNER = re.compile(rb"Linux version ([0-9]+\.[0-9]+\.[0-9]+)-[0-9]+-virt \(")


def guest_kernel(directory):
    descriptor = json.loads((directory / "runtime-artifacts.json").read_bytes())["kernel"]
    with tarfile.open(directory / "runtime-artifacts.tar") as archive:
        members = [member for member in archive if member.name == "vmlinuz"]
        if len(members) != 1 or not members[0].isfile() or not 0 < members[0].size <= 256 * 1024**2:
            raise ValueError("expected one regular guest kernel")
        data = archive.extractfile(members[0]).read()
    digest = "sha256:" + hashlib.sha256(data).hexdigest()
    if descriptor != dict(path="vmlinuz", digest=digest, size_bytes=len(data)):
        raise ValueError("kernel differs from runtime manifest")
    versions = set(BANNER.findall(data))
    if not data.startswith(b"\x7fELF") or len(versions) != 1:
        raise ValueError("cannot identify the Alpine virt kernel")
    return versions.pop().decode(), digest


def version(value):
    if not re.fullmatch(r"[0-9]+(?:\.[0-9]+)*", value):
        raise ValueError(f"unsupported Linux version {value!r}")
    parts = tuple(map(int, value.split(".")))
    return parts + (0,) * max(0, 3 - len(parts))


def affected_range(events, target):
    active = False
    for index, event in enumerate(events):
        (kind, value), = event.items()
        if event == {"limit": "*"}:
            return active, None
        boundary = version(value)
        if kind == "introduced":
            active = target >= boundary
        elif kind in ("fixed", "last_affected", "limit"):
            # Some Linux records list successive stable-branch fixes after a
            # single introduction. An earlier branch's fix cannot close ours.
            if (kind == "fixed" and boundary[:2] < target[:2] and index + 1 < len(events)
                    and "fixed" in events[index + 1]):
                continue
            if active and (target < boundary or (kind == "last_affected" and target == boundary)):
                return True, value if kind == "fixed" else None
            active = False
        else:
            raise ValueError(f"unsupported OSV range event {kind}")
    return active, None


def fixed_ranges(intervals, target):
    # Inferred OSV intervals for different stable branches can overlap. When
    # this branch has a known fix, a later branch's fix does not supersede it.
    branch_fixes = [version(e["fixed"]) for r in intervals for e in r["events"]
                    if "fixed" in e and version(e["fixed"])[:2] == target[:2]]
    for interval in intervals:
        events = interval["events"]
        hit, fixed = affected_range(events, target)
        if not hit:
            continue
        if branch_fixes and fixed and version(fixed)[:2] > target[:2]:
            # A later introduction on this branch can reopen the vulnerability.
            if not any(max(branch_fixes) < version(e["introduced"]) <= target
                       for e in events if "introduced" in e):
                continue
        yield fixed


def match_record(record, kernel, target):
    stable_repo = "https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git"
    ranges_checked = 0
    fixes = set()
    scores = []
    matched = False
    explicit_match = False
    published_fixes = set()
    for entry in record.get("affected", []):
        package = entry.get("package", {})
        ranges = entry.get("ranges", [])
        named_kernel = (package.get("ecosystem"), package.get("name")) == ("Linux", "Kernel")
        stable_git = any(r.get("repo") == stable_repo and r["type"] == "GIT" for r in ranges)
        if not (named_kernel or (not package and stable_git)):
            continue
        explicit = entry.get("versions", [])
        hit = kernel in explicit or f"v{kernel}" in explicit
        explicit_match |= hit
        intervals = [r for r in ranges if r["type"] == "ECOSYSTEM"]
        ranges_checked += len(intervals)
        published_fixes.update(e["fixed"] for r in intervals for e in r["events"]
                               if "fixed" in e and version(e["fixed"]) > target)
        for fixed in fixed_ranges(intervals, target):
            hit = True
            if fixed:
                fixes.add(fixed)
        if hit:
            matched = True
            for severity in entry.get("severity") or record.get("severity", []):
                calculator = {"CVSS_V2": CVSS2, "CVSS_V3": CVSS3, "CVSS_V4": CVSS4}.get(severity["type"])
                if calculator:
                    scores.append(calculator(severity["score"]).scores()[0])
    if explicit_match and not fixes:
        # A stable git tag can be affected even when inferred numeric ranges
        # omit that branch. Report newer published fixes as upstream versions.
        fixes = published_fixes
    if matched:
        return dict(id=record["id"], score=max(scores, default=None),
                    fixes=sorted(fixes, key=version)), ranges_checked
    return None, ranges_checked


def findings(records, kernel):
    target = version(kernel)
    matches = []
    ranges_checked = 0
    for record in records:
        if record.get("withdrawn"):
            continue
        try:
            finding, checked = match_record(record, kernel, target)
        except (ValueError, TypeError, KeyError, CVSSError) as exc:
            raise ValueError(f"invalid advisory {record.get('id')}: {exc}") from exc
        if finding:
            matches.append(finding)
        ranges_checked += checked
    if not ranges_checked:
        raise ValueError("database contains no Linux kernel version ranges")
    return dict(kernel=kernel, findings=sorted(matches, key=lambda item: item["id"]))


def current_database():
    with urllib.request.urlopen(DATABASE, timeout=90) as response:
        modified = parsedate_to_datetime(response.headers["Last-Modified"])
        age = datetime.datetime.now(datetime.timezone.utc) - modified
        if not datetime.timedelta(0) <= age <= datetime.timedelta(days=2):
            raise ValueError(f"OSV Linux database timestamp outside freshness window: {modified}")
        return modified.isoformat(), response.read()


def blockers(report):
    return {item["id"]: item for item in report["findings"]
            if item["score"] is not None and item["score"] >= 9 and item["fixes"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--release", required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--previous", type=Path, help="previous daily report, for notification comparison only")
    args = parser.parse_args()
    kernel, digest = guest_kernel(args.directory)
    modified, data = current_database()
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        report = findings((json.loads(archive.read(name)) for name in archive.namelist()
                           if name.endswith(".json")), kernel)
    report.update(release=args.release, digest=digest, database_modified=modified)
    blocking = blockers(report)
    print(f"{args.release} Linux {kernel} {digest}: {len(report['findings'])} matches, {len(blocking)} critical with a fix")
    previous = {}
    if args.previous and args.previous.exists():
        previous = json.loads(args.previous.read_bytes())
    changes = changed_blockers(report, previous)
    prior = blockers(previous) if changes and previous.get("release") == args.release else {}
    for item in changes.values():
        old = prior.get(item["id"])
        score = f"{old['score']} -> {item['score']}" if old else str(item["score"])
        print(f"{item['id']} CVSS {score}, upstream fixed versions: {', '.join(item['fixes'])}")
    if blocking and not changes:
        print("No new or worsened blocking findings since the previous daily report.")
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    return bool(changes)


def changed_blockers(report, previous):
    current = blockers(report)
    if (report["digest"], report["release"]) != (previous.get("digest"), previous.get("release")):
        return current
    prior = blockers(previous)
    return {key: item for key, item in current.items()
            if key not in prior or item["score"] > prior[key]["score"]}


if __name__ == "__main__":
    try:
        result = main()
    except (OSError, ValueError, KeyError, TypeError, tarfile.TarError, zipfile.BadZipFile) as exc:
        print(f"Guest kernel scan incomplete: {exc}", file=sys.stderr)
        sys.exit(2)
    raise SystemExit(result)
