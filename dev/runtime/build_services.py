#!/usr/bin/env python3
"""Build an API-only Linux service candidate from a clean, archived Product revision."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

COMPONENTS = ['control-plane', 'dispatcher', 'worker', 'guestd']
SCHEMA_DIRS = ['internal/db/schema', 'internal/clickhouse/schema']


def digest(path):
    with open(path, 'rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def tree_digest(root, paths):
    entries = []
    for relative in sorted(set(paths)):
        path = root / relative
        if path.is_file():
            entries.append([relative, digest(path)])
    return hashlib.sha256(json.dumps(entries, separators=(',', ':')).encode()).hexdigest()


def json_stream(text):
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        yield value
        text = text[end:]


def inputs(source, env):
    def go(*args):
        return subprocess.check_output(['go', *args], cwd=source, env=env, text=True)
    result = {}
    # Go's actual selected source/embedded files, not a manually maintained CI
    # path classifier. External dependency bytes are pinned by go.mod/go.sum.
    for name in COMPONENTS:
        paths = ['go.mod', 'go.sum']
        for package in json_stream(go('list', '-deps', '-json', './cmd/' + name)):
            directory = Path(package['Dir'])
            if package.get('Standard'):
                continue
            if not directory.is_relative_to(source):
                if package.get('Module', {}).get('Replace'):
                    raise ValueError('external module replacements are not candidate inputs')
                continue
            for field in ['GoFiles', 'CgoFiles', 'CFiles', 'CXXFiles', 'MFiles', 'HFiles', 'FFiles', 'SFiles', 'SysoFiles', 'EmbedFiles']:
                paths.extend(str((directory / file).relative_to(source)) for file in package.get(field, []))
        result[name] = tree_digest(source, paths)
    # Non-Go runtime/build inputs must also remain unchanged for host reuse.
    prefixes = ['internal/runtime/', 'internal/compiler/', 'internal/version/', 'nix/', 'images/', 'runtime/', 'sdk/', 'proto/', 'compiler/', 'scripts/materialize-']
    support = []
    for path in source.rglob('*'):
        relative = path.relative_to(source)
        if not path.is_file():
            continue
        name = str(relative)
        if (name.startswith(tuple(prefixes)) or name.endswith('/package.json') or name in [
                'flake.nix', 'flake.lock', 'bun.lock', 'bunfig.toml', 'package.json', 'tsconfig.json',
                'scripts/build-platform-entries.ts', 'scripts/node-version.mjs']):
            support.append(name)
    result['runtime-support'] = tree_digest(source, support)
    schema = [str(p.relative_to(source)) for directory in SCHEMA_DIRS for p in (source / directory).rglob('*')
              if p.is_file() and not p.name.endswith('_test.go')]
    if not schema:
        raise ValueError('schema source is missing')
    result['schema'] = tree_digest(source, schema)
    result['toolchain'] = go('env', 'GOVERSION').strip()
    return result


def build(source, output):
    source = source.resolve()
    if subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=all'], cwd=source):
        raise ValueError('candidate requires a clean source revision; preserve edits in a commit first')
    revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=source, text=True).strip()
    output.mkdir(mode=0o700)
    archive = output / 'source.tar'
    subprocess.run(['git', 'archive', '--format=tar', '-o', str(archive.resolve()), revision], cwd=source, check=True)
    # Ignore ambient flags/workspaces/toolchain fetching in the build contract.
    env = dict(os.environ, GOOS='linux', GOARCH='amd64', GOAMD64='v1', CGO_ENABLED='0',
               GOFLAGS='', GOWORK='off', GOTOOLCHAIN='local', GOENV='off', GOEXPERIMENT='')
    with tempfile.TemporaryDirectory(prefix='helmr-services-') as directory:
        root = Path(directory).resolve()
        with tarfile.open(archive) as tar:
            tar.extractall(root, filter='data')
        evidence = inputs(root, env)
        for name in COMPONENTS[:2]:
            subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags',
                            '-X github.com/helmrdotdev/helmr/internal/version.SourceCommit=' + revision,
                            '-o', str((output / name).resolve()), './cmd/' + name], cwd=root, env=env, check=True)
    receipt = {'source_commit': revision, 'source_sha256': digest(archive), 'inputs': evidence,
               'binaries': {name: digest(output / name) for name in COMPONENTS[:2]}}
    (output / 'candidate.json').write_text(json.dumps(receipt, indent=2) + '\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    parser.add_argument('output', type=Path, help='new directory, outside source checkout')
    args = parser.parse_args()
    build(args.source, args.output.resolve())
