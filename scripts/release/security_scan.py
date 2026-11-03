"""Collect release evidence without executing files from the target."""
import argparse
import io
import datetime as dt
import gzip
import hashlib
import os
import stat
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


def collect(rows, scope, identity, aws_templates=None, ssh_digest=None):
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
                if (scope == "bundle-builder" and kind == "Vulnerabilities"
                        and row.get("Class") == "os-pkgs" and row["Type"] == "debian"):
                    # The reviewed Debian binary package and advisory can remain
                    # identical when a later image layer changes unrelated tools.
                    purl = (entry.get("PkgIdentifier") or {}).get("PURL", "")
                    layer = (entry.get("Layer") or {}).get("DiffID", "")
                    if purl.startswith("pkg:deb/debian/") and re.fullmatch(r"sha256:[0-9a-f]{64}", layer):
                        items[-1]["debian_package_key"] = fingerprint([purl, layer, evidence])
                        if ssh_digest and is_ssh_backport(entry):
                            items[-1]["binary_digest"] = ssh_digest
                            items[-1]["debian_package_key"] = fingerprint([purl, ssh_digest, evidence])
                if (aws_templates and kind == "Misconfigurations" and advisory == "AWS-0104"
                        and scope == "source" and row["Type"] == "terraform"
                        and row["Target"].startswith("infra/aws/") and location.get("Occurrences")):
                    items[-1]["template_key"] = fingerprint([aws_templates, row["Target"], evidence])
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


def scan_source(work, source, context, aws_templates):
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
        items.extend(collect([row], "source", identity, aws_templates))
    return items


def module_names(paths):
    return sorted({m[1].replace("-", "_") for path in paths
                   if (m := re.search(r"(?:^|/)([^/]+)\.ko(?:\.[^/]*)?$", path))})


def initramfs_inventory(path):
    # The guest builder emits gzip-compressed newc. Read every entry without
    # extracting links or running guest code. Refuse uninspected trailing data.
    names, mlx5 = [], set()
    with gzip.open(path, "rb") as stream:
        while True:
            header = stream.read(110)
            require(re.fullmatch(rb"070701[0-9a-fA-F]{104}", header), "unsupported guest initramfs")
            mode = int(header[14:22], 16)
            size = int(header[54:62], 16)
            length = int(header[94:102], 16)
            require(0 < length <= 4096, "invalid cpio name length")
            raw = stream.read(length)
            require(len(raw) == length and raw.endswith(b"\0") and b"\0" not in raw[:-1],
                    "invalid cpio name")
            name = os.fsdecode(raw[:-1])
            stream.read(-(110 + length) % 4)
            if name == "TRAILER!!!":
                require(size == 0 and not any(stream.read(512)) and not stream.read(1),
                        "unexpected trailing initramfs data")
                return names, mlx5
            names.append(name)
            if is_mlx5_module(name):
                require(size <= 64 * 1024**2, "oversized mlx5 module")
                data = stream.read(size)
                require(len(data) == size, "truncated mlx5 module")
                mlx5.add("sha256:" + hashlib.sha256(data).hexdigest() if stat.S_ISREG(mode) else "nonregular")
            else:
                stream.seek(size, 1)
            stream.read(-size % 4)


def is_mlx5_module(name):
    return re.fullmatch(r"mlx5[_-]core\.ko(?:\..+)?", Path(name).name) is not None


def perf_name(name):
    return re.search(r"(?:^|[-_.])perf(?:$|[-_.])", name.lower()) is not None


def guest_files(fs, listing, mlx5, builtin, packages):
    # Path.walk propagates traversal failures via on_error. Include links and
    # versioned perf names; even an ambiguous matching entry requires review.
    def unreadable(error):
        raise error

    perf_present = any(perf_name(Path(path).name) for path in listing) or any(
        perf_name(name) or re.match(r"linux-tools(?:$|[-_.])", name) for name in packages)
    paths = list(listing)
    for directory, dirs, files in fs.walk(on_error=unreadable):
        for name in dirs + files:
            path = directory / name
            relative = path.relative_to(fs)
            paths.append(str(relative))
            if perf_name(name):
                # Linux headers such as perf_event.h are data, not the tool.
                mode = path.lstat().st_mode
                header = (relative.is_relative_to("usr/include") and name.endswith(".h")
                          and stat.S_ISREG(mode) and not mode & 0o111)
                if not header:
                    perf_present = True
            if is_mlx5_module(name):
                mlx5.add(digest(path) if stat.S_ISREG(path.lstat().st_mode) else "nonregular")
    shipped = module_names(paths)
    drivers = set(shipped) | set(builtin)
    return shipped, dict(
        perf_absent=not perf_present,
        ntfs_absent=drivers.isdisjoint({"ntfs", "ntfs3"}),
        airoha_ethernet_absent="airoha_eth" not in drivers,
        mlx5_core_hashes=sorted(mlx5) if "mlx5_core" not in builtin else [],
    )


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
    packages = {pkg["Name"] for row in rows if row["Type"] == "wolfi" for pkg in row["Packages"]}
    require(packages, "missing Wolfi package inventory")
    # Trivy includes the temporary scan directory in the OS Target label.
    for row in rows:
        if row["Class"] == "os-pkgs":
            row["Target"] = row["Type"]
    listing, mlx5 = initramfs_inventory(work / "initramfs")
    orders = [n for n in listing if re.fullmatch(r"(?:\./)?lib/modules/[^/]+/modules.order", n)]
    require(len(orders) == 1, "ambiguous guest module inventory")
    order_path = "usr/" + orders[0].removeprefix("./")
    ordered = run("unsquashfs", "-cat", work / "rootfs.squashfs", order_path,
                  capture_output=True, text=True).stdout.splitlines()
    builtin = run("unsquashfs", "-cat", work / "rootfs.squashfs", order_path.replace("modules.order", "modules.builtin"),
                  capture_output=True, text=True).stdout.splitlines()
    builtin_modules = module_names(builtin)
    shipped, facts = guest_files(fs, listing, mlx5, builtin_modules, packages)
    require(shipped and ordered and builtin, "incomplete guest module inventory")
    modified, data = current_database()
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        linux = findings((json.loads(archive.read(n)) for n in archive.namelist() if n.endswith(".json")), kernel)
    inventory = dict(kernel=kernel, digest=kernel_digest, modules=shipped,
                     optional=module_names(ordered), builtin=builtin_modules, facts=facts, database_modified=modified)
    items = collect(rows, "guest-os", manifest["rootfs"]["digest"])
    for entry in linux["findings"]:
        score = entry["score"]
        items.append(dict(id=entry["id"], component="kernel", target=kernel, score=score, fixes=entry["fixes"],
                          severity="CRITICAL" if score is not None and score >= 9 else "unrated" if score is None else "scored",
                          advisory=entry["advisory"], key=fingerprint([kernel_digest, manifest, entry])))
    return inventory, items


def is_ssh_backport(entry):
    return (entry.get("VulnerabilityID") == "CVE-2026-60002" and entry.get("PkgName") == "openssh-client"
            and entry.get("InstalledVersion", "").endswith("+helmr1"))


def ssh_executable_digest(layout, work):
    # Inspect the merged OCI filesystem, including replacements and whiteouts.
    # Never execute anything from the release being assessed.
    with tempfile.TemporaryDirectory(dir=work) as tmp:
        root = Path(tmp).resolve() / "rootfs"
        run("umoci", "--log", "error", "raw", "unpack", "--rootless", "--image", f"{layout}:scan", root)
        ssh = root / "usr/bin/ssh"
        require(ssh.resolve() == ssh and ssh.is_file(), "expected a regular Debian SSH executable")
        return digest(ssh)


def scan_image(work, assets, descriptor, scope, published):
    ref = read(assets / descriptor)["image"]
    require(re.fullmatch(re.escape(IMAGES[scope]) + r"@sha256:[0-9a-f]{64}", ref), "unexpected image identity")
    layout = work / scope
    if published:
        transport = "docker://" + ref
        args = ["--image-src", "remote", ref]
    else:
        image = assets / ("builder-image" if scope == "bundle-builder" else "controlplane-image")
        require(digest(image / "manifest.json") == ref.split("@")[1], "image manifest digest mismatch")
        transport = "dir:" + str(image)
        run("skopeo", "--insecure-policy", "copy", transport, f"oci:{layout}:scan", stdout=subprocess.DEVNULL)
        args = ["--input", str(layout)]
    rows = trivy(work, "image", args, "vuln")
    require(any(r["Class"] == "os-pkgs" and r["Packages"] for r in rows), f"{scope} lacks OS inventory")
    for row in rows:
        if row["Class"] == "os-pkgs":
            row["Target"] = row["Type"]
    ssh_digest = None
    if scope == "bundle-builder" and any(is_ssh_backport(v) for row in rows if row["Type"] == "debian"
                                         for v in row.get("Vulnerabilities", [])):
        if published:
            run("skopeo", "--insecure-policy", "copy", transport, f"oci:{layout}:scan", stdout=subprocess.DEVNULL)
        ssh_digest = ssh_executable_digest(layout, work)
    return collect(rows, scope, ref, ssh_digest=ssh_digest)


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
    aws_files = b"\0".join(e for e in entries if e.split(b"\t", 1)[-1].startswith(b"infra/aws/"))
    return tree, hashlib.sha256(tracked).hexdigest(), hashlib.sha256(aws_files).hexdigest()


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
        tree, source_digest, aws_templates = tracked_source(work, source)
        # Pin one database snapshot for every surface in this scan. A transient
        # registry failure can be retried before any findings are evaluated.
        update = ["trivy", "image", "--quiet", "--download-db-only", "--disable-telemetry"]
        if subprocess.run(update, cwd=work).returncode:
            time.sleep(4)
            subprocess.run(update, cwd=work)
        require(fresh((trivy_version().get("VulnerabilityDB") or {}).get("UpdatedAt"), 48), "no fresh Trivy database")
        items = scan_source(work, tree, source_digest, aws_templates)
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
