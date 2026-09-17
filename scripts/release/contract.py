"""Fixed Product release v0 contract. No network or executable artifact loading."""
import hashlib
import json
from pathlib import Path
import re
import tarfile

REPOSITORY = 'helmrdotdev/helmr'
IMAGES = {
    'bundle-builder': 'ghcr.io/helmrdotdev/bundle-builder',
    'control-plane': 'ghcr.io/helmrdotdev/control-plane',
}
WORKFLOW = f'https://github.com/{REPOSITORY}/.github/workflows/release.yaml'
ISSUER = 'https://token.actions.githubusercontent.com'
SHA = r'[0-9a-f]{40}'
DIGEST = r'sha256:[0-9a-f]{64}'
SEMVER = r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?'
PLATFORMS = ('linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64')
ASSETS = frozenset({*(f'helmr-{p}.tar.gz' for p in PLATFORMS),
    'checksums.txt', 'proto.tgz', 'sdk.tgz', 'bundle-builder.json', 'controlplane.json',
    'control-plane-linux-amd64.tar.gz', 'platform-release.tar',
    'platform-release-provenance.json', 'worker-host-artifacts.tar',
    'worker-host-artifacts.json', 'worker-host-bundle.json', 'runtime-artifacts.tar',
    'runtime-artifacts.json', 'worker-runtime-bundle.json'})


def require(ok, message):
    if not ok:
        raise ValueError(message)


def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, f'duplicate JSON key: {key}')
        result[key] = value
    return result


def read(path):
    return json.loads(Path(path).read_bytes(), object_pairs_hook=unique)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()


def write(path, value):
    Path(path).write_bytes(canonical(value))


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    return 'sha256:' + h.hexdigest()


def descriptor(path):
    path = Path(path)
    require(path.is_file() and not path.is_symlink(), 'regular artifact required')
    return dict(digest=digest(path), sizeBytes=path.stat().st_size)


def preview_version(core, commit, run):
    require(re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', core), 'invalid package core')
    require(re.fullmatch(SHA, commit) and re.fullmatch(r'[1-9][0-9]*', str(run)), 'invalid build identity')
    version = f'{core}-preview.g{commit[:8]}.b{run}'
    require(re.fullmatch(SEMVER, version), 'invalid constructed version')
    return 'v' + version


def signer(version):
    require(re.fullmatch('v' + SEMVER, version), 'invalid release version')
    ref = 'refs/heads/main' if '-preview.' in version else 'refs/tags/' + version
    return WORKFLOW + '@' + ref


def safe_extract(archive, destination):
    destination = Path(destination)
    require(not destination.exists(), 'extraction requires a new directory')
    with tarfile.open(archive) as bundle:
        seen = set()
        for member in bundle.getmembers():
            name = member.name.removeprefix('./')
            if name in ('', '.'):
                require(member.isdir(), 'invalid archive root')
                name = '.'
            else:
                require(not name.startswith('/') and all(p not in ('', '.', '..') for p in name.split('/')), 'unsafe archive path')
            require(name not in seen and (member.isfile() or member.isdir()), 'duplicate/link/device archive member')
            seen.add(name)
        destination.mkdir(mode=0o700)
        for member in bundle.getmembers():
            name = member.name.removeprefix('./')
            if name in ('', '.'):
                continue
            target = destination / name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with bundle.extractfile(member) as source, target.open('xb') as output:
                    import shutil
                    shutil.copyfileobj(source, output)
                target.chmod(0o755 if member.mode & 0o111 else 0o644)


def validate(index, *, publishing=True):
    require(set(index) == {'schema', 'version', 'sourceCommit', 'sourceRef', 'build', 'assets'}, 'release index fields differ')
    require(index['schema'] == 'helmr.release.v0' and re.fullmatch('v' + SEMVER, index['version']), 'invalid release index/version')
    require(re.fullmatch(SHA, index['sourceCommit']), 'full source SHA required')
    require(re.fullmatch(r'refs/(heads/main|pull/[1-9][0-9]*/head|tags/v[^/]+)', index['sourceRef']), 'invalid selected source ref')
    b = index['build']
    require(set(b) == {'runId', 'attempt', 'workflowCommit', 'workflowRef', 'ciRun', 'pr', 'mode'}, 'build identity fields differ')
    require(all(re.fullmatch(r'[1-9][0-9]*', str(b[k])) for k in ('runId', 'attempt', 'ciRun')), 'native run/attempt required')
    require(re.fullmatch(SHA, b['workflowCommit']), 'workflow commit required')
    if publishing:
        require(b['workflowRef'] == signer(index['version']).split('@', 1)[1], 'workflow ref differs from signer policy')
    require(b['mode'] in ('main', 'pr', 'tag'), 'invalid release mode')
    if b['mode'] == 'pr':
        require(type(b['pr']) is int and b['pr'] > 0 and index['sourceRef'] == f'refs/pull/{b["pr"]}/head', 'PR identity differs')
    else:
        require(b['pr'] is None, 'unexpected PR identity')
        require(index['sourceRef'] == ('refs/heads/main' if b['mode'] == 'main' else 'refs/tags/' + index['version']), 'source ref differs')
    require(('-preview.' in index['version']) == (b['mode'] != 'tag'), 'generated tag/mode differs')
    require(set(index['assets']) == ASSETS, 'incomplete or unexpected release assets')
    for name, item in index['assets'].items():
        require(set(item) == {'digest', 'sizeBytes'} and re.fullmatch(DIGEST, item['digest']), f'invalid asset digest: {name}')
        require(type(item['sizeBytes']) is int and 0 < item['sizeBytes'] <= 2 * 1024**3, 'invalid asset size')
    return index


def cli_checksums(directory):
    directory = Path(directory)
    return ''.join(f"{digest(directory / ('helmr-' + p + '.tar.gz'))[7:]}  helmr-{p}.tar.gz\n" for p in sorted(PLATFORMS)).encode()


def verify_image(role, reference):
    require(re.fullmatch(re.escape(IMAGES[role]) + '@' + DIGEST, reference), 'Product image digest required')


def verify_files(index, directory, *, publishing=True):
    validate(index, publishing=publishing)
    directory = Path(directory)
    for name, expected in index['assets'].items():
        require(descriptor(directory / name) == expected, f'artifact bytes differ: {name}')
    require((directory / 'checksums.txt').read_bytes() == cli_checksums(directory), 'CLI checksum projection differs')
    builder = read(directory / 'bundle-builder.json')
    cp = read(directory / 'controlplane.json')
    for record, image in ((builder, 'bundle-builder'), (cp, 'control-plane')):
        require(record['formatVersion'] == 0 and record['sourceCommit'] == index['sourceCommit'], 'image source mismatch')
        verify_image(image, record['image'])
    with tarfile.open(directory / 'platform-release.tar') as archive:
        members = [m for m in archive.getmembers() if m.name.removeprefix('./') == 'platform-release.json']
        require(len(members) == 1 and members[0].isfile(), 'unique platform descriptor required')
        runtime = json.load(archive.extractfile(members[0]))['runtime']
    require(builder['runtime'] == runtime and cp['runtime'] == runtime, 'builder/CP/platform Runtime mismatch')
    provenance = read(directory / 'platform-release-provenance.json')
    require(provenance['sourceCommit'] == index['sourceCommit'] and provenance['sourceRef'] == index['sourceRef'], 'platform source/ref mismatch')
    require(provenance['archive']['digest'] == digest(directory / 'platform-release.tar'), 'platform archive mismatch')
    for name, bundle, manifest, key in (
        ('worker-host-bundle.json', 'worker-host-artifacts.tar', 'worker-host-artifacts.json', 'manifest'),
        ('worker-runtime-bundle.json', 'runtime-artifacts.tar', 'runtime-artifacts.json', 'runtimeArtifactsManifest')):
        receipt = read(directory / name)
        require(receipt['sourceCommit'] == index['sourceCommit'], 'Worker source mismatch')
        require(receipt['bundle'] == dict(path=bundle, digest=digest(directory / bundle)), 'Worker bundle mismatch')
        require(receipt[key] == dict(path=manifest, digest=digest(directory / manifest)), 'Worker manifest mismatch')
    for filename, package in (('proto.tgz', '@helmr/proto'), ('sdk.tgz', '@helmr/sdk')):
        with tarfile.open(directory / filename) as archive:
            members = [m for m in archive.getmembers() if m.name == 'package/package.json']
            require(len(members) == 1 and members[0].isfile(), 'unique npm metadata required')
            metadata = json.load(archive.extractfile(members[0]))
        require(metadata['name'] == package and metadata['version'] == index['version'][1:], 'npm name/version mismatch')
        require(metadata['helmr'] == dict(formatVersion=0, sourceCommit=index['sourceCommit'], buildId=str(index['build']['runId'])), 'npm source/build stamp mismatch')
        if package == '@helmr/sdk':
            require(metadata['dependencies']['@helmr/proto'] == index['version'][1:], 'SDK must pin exact sibling version')
    return index


if __name__ == '__main__':
    import argparse
    parser = argparse.ArgumentParser(description='Fixed Product OCI identity for artifact producers and gates')
    parser.add_argument('role', choices=IMAGES)
    parser.add_argument('--verify', metavar='REFERENCE')
    args = parser.parse_args()
    if args.verify is None:
        print(IMAGES[args.role])
    else:
        verify_image(args.role, args.verify)
