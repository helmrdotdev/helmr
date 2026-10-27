#!/usr/bin/env python3
"""Package already acquired, pinned Linux x86_64 client tools for an offline image."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[4]
PREFIX = "opt/network-client-tools"
SDK_INTEGRITY = "sha512-N7aXtCvC5g6T1Y4V29lJjceu/zTkVkIZF0jdBvagr0TRFHuKeImffalGWEfqZKrvjH+IQbzJWw6TmSmUzrlMgg=="


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-archive", type=Path, required=True)
    parser.add_argument("--git-store-path", type=Path, required=True)
    parser.add_argument("--sdk-directory", type=Path, required=True,
                        help="isolated npm project containing package-lock.json and node_modules")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    if output.is_relative_to(ROOT):
        parser.error("binary payload must remain outside the checkout")
    release = json.loads((ROOT / "internal/version/runtime-dependencies.json").read_text())["node"]
    if release["version"] != "24.22.1" or digest(args.node_archive) != release["sha256"]["x86_64-linux"]:
        parser.error("expected the Product-pinned official Node 24.22.1 Linux x64 archive")
    git = args.git_store_path.resolve()
    if git.parent != Path("/nix/store") or not git.name.endswith("-git-2.51.2"):
        parser.error("expected pinned Nix Git 2.51.2 store output")
    header = (git / "bin/git").read_bytes()[:20]
    if header[:7] != bytes.fromhex("7f454c46020101") or header[18:20] != bytes.fromhex("3e00"):
        parser.error("expected a Linux x86_64 Git executable")
    closure = subprocess.check_output(["nix-store", "--query", "--requisites", str(git)], text=True).splitlines()
    sdk = args.sdk_directory.resolve()
    package = sdk / "node_modules/@anthropic-ai/sdk/package.json"
    locked = json.loads((sdk / "package-lock.json").read_text())["packages"].values()
    if json.loads(package.read_text())["version"] != "0.98.0" or not any(
        entry.get("version") == "0.98.0" and entry.get("integrity") == SDK_INTEGRITY for entry in locked
    ):
        parser.error("expected the previously integrity-verified, locked Anthropic SDK 0.98.0 closure")
    if list((sdk / "node_modules").rglob("*.node")):
        parser.error("SDK closure must contain no architecture-specific native addons")
    manifest = {"versions": {"node": "v24.22.1", "npm": "11.19.1", "git": "git version 2.51.2", "anthropic": "0.98.0"},
                "nodeArchiveSHA256": digest(args.node_archive), "gitStorePath": str(git),
                "sdkPackageSHA256": digest(package), "sdkLockSHA256": digest(sdk / "package-lock.json")}
    with output.open("xb") as raw, tarfile.open(fileobj=raw, mode="w:gz") as archive:
        with tarfile.open(args.node_archive) as node:
            root = "node-v24.22.1-linux-x64/"
            npm = json.load(node.extractfile(root + "lib/node_modules/npm/package.json"))
            if npm["version"] != "11.19.1":
                raise ValueError("official archive npm version is not 11.19.1")
            for member in node:
                if member.name.rstrip("/") == root.rstrip("/"):
                    continue
                if not member.name.startswith(root):
                    raise ValueError("unexpected official Node archive member")
                body = node.extractfile(member) if member.isfile() else None
                member.name = PREFIX + "/node/" + member.name[len(root):]
                if member.islnk():
                    member.linkname = PREFIX + "/node/" + member.linkname.removeprefix(root)
                archive.addfile(member, body)
        for store_path in sorted(closure):
            archive.add(store_path, arcname=store_path.lstrip("/"))
        link = tarfile.TarInfo(PREFIX + "/git")
        link.type, link.linkname = tarfile.SYMTYPE, str(git)
        archive.addfile(link)
        archive.add(sdk / "node_modules", arcname=PREFIX + "/sdk/node_modules")
        body = json.dumps(manifest, sort_keys=True).encode() + b"\n"
        info = tarfile.TarInfo(PREFIX + "/artifacts.json")
        info.size, info.mode = len(body), 0o644
        archive.addfile(info, io.BytesIO(body))
    print(json.dumps({"archive": str(output), "sha256": digest(output), **manifest}, sort_keys=True))


if __name__ == "__main__":
    main()
