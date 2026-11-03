"""Collect release evidence without executing files from the target."""
import argparse
import io
import datetime as dt
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import time
import zipfile

from contract import IMAGES, PLATFORMS, digest, read, require
from guest_kernel_scan import current_database, findings, guest_kernel


def fingerprint(data):
    return hashlib.sha256(json.dumps(data, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def trivy(work, command, target, scanners):
    output = work / "trivy.json"
    run("trivy", command, "--quiet", "--disable-telemetry", "--skip-version-check",
        "--offline-scan", "--skip-db-update", *(["--include-dev-deps"] if command == "fs" else []), "--ignorefile", "/dev/null", "--format", "json",
        "--scanners", scanners, "--list-all-pkgs", "--output", output, *target, cwd=work)
    report = read(output)
    require(report["SchemaVersion"] == 2 and report.get("Results"), "missing Trivy scan results")
    return report["Results"]


def collect(rows, scope, identity):
    items = []
    for row in rows:
        for secret in row.get("Secrets") or []:
            # Trivy includes the matched secret and surrounding source. Never
            # persist either in the report or the workflow summary.
            fields = {k: secret[k] for k in ("RuleID", "Severity", "StartLine", "EndLine")}
            items.append(dict(id=fields["RuleID"], component=scope, target=row["Target"],
                              kind="secret", severity=fields["Severity"],
                              start=fields["StartLine"], end=fields["EndLine"],
                              key=fingerprint([scope, identity, row["Target"], fields])))
        for kind in ("Vulnerabilities", "Misconfigurations"):
            for entry in row.get(kind) or []:
                if kind == "Misconfigurations" and entry["Status"] != "FAIL":
                    continue
                advisory = entry.get("VulnerabilityID") or entry["ID"]
                if kind == "Vulnerabilities":
                    # Per-scan Trivy fingerprints, layer identifiers and package
                    # locations differ between registry, OCI and rootfs inputs.
                    evidence = {k: entry.get(k) for k in (
                        "VulnerabilityID", "PkgName", "InstalledVersion", "FixedVersion", "Status",
                        "Description", "Severity", "CVSS", "LastModifiedDate", "DataSource")}
                else:
                    evidence = {k: entry.get(k) for k in (
                        "ID", "Status", "Severity", "Description", "Message", "Resolution", "CauseMetadata")}
                location = entry.get("CauseMetadata", {})
                evidence_location = {k: location[k] for k in ("Resource", "StartLine", "EndLine", "Occurrences")
                                     if k in location}
                items.append(dict(id=advisory, component=scope, target=row["Target"],
                                  location=evidence_location, fixed=entry.get("FixedVersion", ""),
                                  package=entry.get("PkgName", ""), version=entry.get("InstalledVersion", ""),
                                  severity=entry["Severity"], title=entry.get("Title", ""),
                                  key=fingerprint([scope, identity, row["Type"], row["Target"], evidence])))
    return items


def check_terraform_inputs(source):
    # Inline ignores take effect before Trivy emits findings, even when an
    # empty ignore file is supplied. Applicability belongs to the review data.
    for path in source.rglob("*"):
        if path.name.endswith((".tf", ".tf.json", ".tfvars", ".tfvars.json")) and path.is_file():
            require(not path.is_symlink(), f"unverified Terraform input: {path.relative_to(source)}")
            require(not re.search(rb"(?:tfsec|trivy):ignore", path.read_bytes(), re.I),
                    f"inline suppression prevents Terraform assessment: {path.relative_to(source)}")


def fresh(timestamp, hours):
    if not timestamp:
        return False
    age = dt.datetime.now(dt.timezone.utc) - dt.datetime.fromisoformat(timestamp)
    return dt.timedelta(0) <= age < dt.timedelta(hours=hours)


def trivy_version():
    return json.loads(run("trivy", "version", "--format", "json", capture_output=True).stdout)


def scan_source(work, source, context):
    check_terraform_inputs(source)
    flags = ["--misconfig-scanners", "terraform", str(source)]
    if fresh((trivy_version().get("CheckBundle") or {}).get("DownloadedAt"), 24):
        flags.insert(0, "--skip-check-update")
    try:
        rows = trivy(work, "fs", flags, "vuln,misconfig,secret")
    except subprocess.CalledProcessError:
        time.sleep(4)
        rows = trivy(work, "fs", flags, "vuln,misconfig,secret")
    detected = {r["Target"] for r in rows if r.get("Packages")}
    locks = {str(p.relative_to(source)) for name in ("bun.lock", "go.mod", "package-lock.json")
             for p in source.rglob(name)}
    require({"bun.lock", "go.mod"} <= locks and locks <= detected,
            f"unscanned source dependencies: {sorted(locks - detected)}")
    require(any(r["Type"] == "terraform" for r in rows), "missing Terraform coverage")
    items = []
    for row in rows:
        identity = context
        if row["Target"] == ".github/security-assessments.json":
            identity = digest(source / row["Target"])
        items.extend(collect([row], "source", identity))
    return items


def module_names(paths):
    return sorted({m[1].replace("-", "_") for path in paths
                   if (m := re.search(r"(?:^|/)([^/]+)\.ko(?:\.[^/]*)?$", path))})


def guest(work, assets):
    kernel, kernel_digest = guest_kernel(assets)
    manifest = read(assets / "runtime-artifacts.json")
    with tarfile.open(assets / "runtime-artifacts.tar") as bundle:
        for key in ("rootfs", "initramfs"):
            name = {"rootfs": "rootfs.squashfs", "initramfs": "initramfs"}[key]
            members = [m for m in bundle if m.name == name]
            require(len(members) == 1 and members[0].isfile(), f"missing {name}")
            with (work / name).open("xb") as out:
                shutil.copyfileobj(bundle.extractfile(members[0]), out)
            require(manifest[key] == dict(path=name, digest=digest(work / name), size_bytes=members[0].size),
                    f"{name} differs from manifest")
    fs = work / "guest"
    run("unsquashfs", "-no-progress", "-d", fs, work / "rootfs.squashfs", stdout=subprocess.DEVNULL)
    rows = trivy(work, "rootfs", [str(fs)], "vuln")
    require(any(r["Type"] == "wolfi" and r["Packages"] for r in rows), "missing Wolfi package inventory")
    # Trivy includes the temporary scan directory in the OS Target label.
    for row in rows:
        if row["Class"] == "os-pkgs":
            row["Target"] = row["Type"]
    listing = run("bash", "-eo", "pipefail", "-c", 'gzip -dc "$1" | cpio -it',
                  "scan", work / "initramfs", capture_output=True, text=True).stdout.splitlines()
    shipped = module_names(listing + [str(p) for p in fs.rglob("*.ko*")])
    orders = [n for n in listing if re.fullmatch(r"(?:\./)?lib/modules/[^/]+/modules.order", n)]
    require(len(orders) == 1, "ambiguous guest module inventory")
    order_path = "usr/" + orders[0].removeprefix("./")
    ordered = run("unsquashfs", "-cat", work / "rootfs.squashfs", order_path,
                  capture_output=True, text=True).stdout.splitlines()
    builtin = run("unsquashfs", "-cat", work / "rootfs.squashfs", order_path.replace("modules.order", "modules.builtin"),
                  capture_output=True, text=True).stdout.splitlines()
    require(shipped and ordered and builtin, "incomplete guest module inventory")
    modified, data = current_database()
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        linux = findings((json.loads(archive.read(n)) for n in archive.namelist() if n.endswith(".json")), kernel)
    inventory = dict(kernel=kernel, digest=kernel_digest, modules=shipped,
                     optional=module_names(ordered), builtin=module_names(builtin), database_modified=modified)
    items = collect(rows, "guest-os", manifest["rootfs"]["digest"])
    for entry in linux["findings"]:
        score = entry["score"]
        items.append(dict(id=entry["id"], component="kernel", target=kernel, score=score, fixes=entry["fixes"],
                          severity="CRITICAL" if score is not None and score >= 9 else "unrated" if score is None else "scored",
                          advisory=entry["advisory"], key=fingerprint([kernel_digest, manifest, entry])))
    return inventory, items


def scan_image(work, assets, descriptor, scope, published):
    ref = read(assets / descriptor)["image"]
    require(re.fullmatch(re.escape(IMAGES[scope]) + r"@sha256:[0-9a-f]{64}", ref), "unexpected image identity")
    if published:
        args = ["--image-src", "remote", ref]
    else:
        image = assets / ("builder-image" if scope == "bundle-builder" else "controlplane-image")
        require(digest(image / "manifest.json") == ref.split("@")[1], "image manifest digest mismatch")
        layout = work / scope
        run("skopeo", "--insecure-policy", "copy", "dir:" + str(image), f"oci:{layout}:scan", stdout=subprocess.DEVNULL)
        args = ["--input", str(layout)]
    rows = trivy(work, "image", args, "vuln")
    require(any(r["Class"] == "os-pkgs" and r["Packages"] for r in rows), f"{scope} lacks OS inventory")
    for row in rows:
        if row["Class"] == "os-pkgs":
            row["Target"] = row["Type"]
    return collect(rows, scope, ref)


def scan_binaries(work, assets):
    directory = work / "binaries"
    directory.mkdir()
    subjects = {"worker": ("worker-host-artifacts.tar", "worker"),
                "control-plane": ("control-plane-linux-amd64.tar.gz", "control-plane")}
    for platform in PLATFORMS:
        name = "helmr-" + platform
        subjects[name] = (name + ".tar.gz", "helmr")
    identities = {}
    for name, (archive, member) in subjects.items():
        with tarfile.open(assets / archive) as bundle:
            entries = [m for m in bundle if m.name == member]
            require(len(entries) == 1 and entries[0].isfile(), f"missing {name} binary")
            target = directory / name
            with target.open("xb") as stream:
                shutil.copyfileobj(bundle.extractfile(entries[0]), stream)
            target.chmod(0o755)
            identities[name] = digest(target)
    rows = trivy(work, "rootfs", [str(directory)], "vuln")
    observed = {r["Target"] for r in rows if r["Type"] == "gobinary" and r.get("Packages")}
    require(observed == subjects.keys(), f"unexpected Go binary coverage: {sorted(observed)}")
    return [item for r in rows for item in collect([r], "go-binary", identities[r["Target"]])]


def tracked_source(work, source):
    # A repository-controlled export-ignore attribute must not remove inputs.
    commit = run("git", "-C", source, "rev-parse", "HEAD", capture_output=True, text=True).stdout.strip()
    entries = run("git", "-C", source, "ls-tree", "-r", "-z", commit, capture_output=True).stdout.split(b"\0")
    require(not any(e.startswith(b"160000 ") for e in entries), "unscanned Git submodule")
    tracked = b"\0".join(e for e in entries if e.split(b"\t", 1)[-1] != b".github/security-assessments.json")
    tree = work / "source"
    run("git", "-c", "core.hooksPath=/dev/null", "clone", "--quiet", "--shared", "--no-checkout", source, tree)
    run("git", "-C", tree, "-c", "core.hooksPath=/dev/null", "checkout", "--quiet", "--detach", commit)
    shutil.rmtree(tree / ".git")
    return tree, hashlib.sha256(tracked).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--assets", type=Path)
    parser.add_argument("--published", action="store_true")
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    source = args.source.resolve()
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        tree, source_digest = tracked_source(work, source)
        # Pin one database snapshot for every surface in this scan. A transient
        # registry failure can be retried before any findings are evaluated.
        update = ["trivy", "image", "--quiet", "--download-db-only", "--disable-telemetry"]
        if subprocess.run(update, cwd=work).returncode:
            time.sleep(4)
            subprocess.run(update, cwd=work)
        require(fresh((trivy_version().get("VulnerabilityDB") or {}).get("UpdatedAt"), 48), "no fresh Trivy database")
        items = scan_source(work, tree, source_digest)
        database = trivy_version()
        now = dt.datetime.now(dt.timezone.utc)
        require(fresh(database["VulnerabilityDB"]["UpdatedAt"], 48), "stale Trivy database")
        bundle = database.get("CheckBundle") or {}
        require(bundle.get("Digest") and fresh(bundle.get("DownloadedAt"), 48), "missing or stale IaC check bundle")
        report = dict(scanned=now.isoformat(), database=database, findings=items)
        if args.assets:
            assets = args.assets.resolve()
            items.extend(scan_binaries(work, assets))
            report["guest"], extra = guest(work, assets)
            items.extend(extra)
            for desc, scope in [("bundle-builder.json", "bundle-builder"), ("controlplane.json", "control-plane")]:
                items.extend(scan_image(work, assets, desc, scope, args.published))
        report["source"] = source_digest
        args.report.write_text(json.dumps(report, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
