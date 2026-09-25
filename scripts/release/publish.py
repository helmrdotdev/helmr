"""Fixed create-only publication. Invoked from trusted source, never PR source."""
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import tempfile
import urllib.error
import urllib.request
from contract import DIGEST, ASSETS, REPOSITORY, ISSUER, canonical, descriptor, digest, read, require, signer, validate, verify_files, write
from admission import git
from preview_store import preview_mode


def run(*args, **kwargs):
    return subprocess.check_output(list(map(str, args)), text=True, **kwargs).strip()


def verify_signature(path, signature, version):
    run('cosign', 'verify-blob', '--bundle', signature, '--certificate-identity', signer(version),
        '--certificate-oidc-issuer', ISSUER, path)


def sign(path):
    signature = Path(str(path).removesuffix('.json') + '.sigstore.json')
    run('cosign', 'sign-blob', '--yes', '--new-bundle-format=true', '--bundle', signature, path)
    return signature


def release_asset(api, release, name, destination, missing=False):
    assets = [a for a in api.pages(f'releases/{release["id"]}/assets') if a['name'] == name]
    if not assets and missing:
        return False
    require(len(assets) == 1, 'missing/duplicate release asset: ' + name)
    api.request(f'releases/assets/{assets[0]["id"]}', destination=destination, accept='application/octet-stream')
    return True


def upload_identical(api, release, path):
    path = Path(path)
    with tempfile.TemporaryDirectory() as temporary:
        old = Path(temporary) / path.name
        if release_asset(api, release, path.name, old, missing=True):
            require(descriptor(old) == descriptor(path), 'existing release asset has different bytes: ' + path.name)
            return
    api.request(release['upload_url'].split('{', 1)[0] + '?name=' + path.name, data=path)


def fetch_index(api, release, directory, completion='release-index'):
    directory = Path(directory)
    directory.mkdir()
    index_path = directory / (completion + '.json')
    release_asset(api, release, index_path.name, index_path)
    release_asset(api, release, completion + '.sigstore.json', directory / (completion + '.sigstore.json'))
    verify_signature(index_path, directory / (completion + '.sigstore.json'), release['tag_name'])
    index = validate(read(index_path))
    require(index['version'] == release['tag_name'], 'signed version differs from release')
    return index


def find_release(api, version):
    # Tags only expose published releases. This lookup requires publisher access.
    matches = [r for r in api.pages('releases') if r['tag_name'] == version]
    require(len(matches) <= 1, 'duplicate release version')
    return matches[0] if matches else None


def bind_release(release, selection):
    require(release['tag_name'] == selection['version'], 'release version differs')
    # Metadata consistency, not proof of the target of an already-existing Git tag.
    require(release['target_commitish'] == selection['sourceCommit'], 'release target differs')


def complete(api, selection, directory, *, release=None, expected_build=None):
    if preview_mode(selection):
        from preview_store import complete as preview_complete
        index = preview_complete(selection, directory)
        if index is None:
            return None
        if expected_build is not None:
            require((Path(directory) / 'release-index.json').read_bytes() == expected_build,
                    'completed preview index differs from verified build bytes')
        return index
    if release is None:
        release = api.request('releases/tags/' + selection['version'], missing=True)
    if release is None or release['draft']:
        return None
    index = fetch_index(api, release, directory)
    from transport import same_selection
    same_selection({k: index[k] for k in selection}, selection)
    if expected_build is not None:
        require((Path(directory) / 'release-index.json').read_bytes() == expected_build,
                'completed index differs from verified build bytes')
    # Public readback has no GitHub/npm/registry token. A successful signed draft
    # is insufficient if ordinary consumers cannot retrieve the final bytes.
    import shutil
    for name in (*index['assets'], 'release-index.json', 'release-index.sigstore.json'):
        target = Path(directory) / (name + '.public' if name.startswith('release-index.') else name)
        url = f'https://github.com/{REPOSITORY}/releases/download/{index["version"]}/{name}'
        with urllib.request.urlopen(url, timeout=60) as source, target.open('xb') as out:
            require(source.url.startswith('https://'), 'non-HTTPS public download')
            shutil.copyfileobj(source, out, 1024 * 1024)
        expected = index['assets'].get(name) or descriptor(Path(directory) / name)
        require(descriptor(target) == expected, 'public completed asset differs')
    verify_files(index, directory)
    return index


def npm_channel(selection):
    mode = selection['build']['mode']
    if mode == 'main':
        return 'preview'
    if mode == 'tag':
        return 'next' if '-' in selection['version'] else 'latest'
    raise ValueError('invalid release mode')


def npm_publish(path, package, version, selection):
    url = f'https://registry.npmjs.org/{package}/{version}'
    try:
        with urllib.request.urlopen(url, timeout=60) as response:
            metadata = json.load(response)
    except urllib.error.HTTPError as error:
        require(error.code == 404, f'npm lookup failed: HTTP {error.code}')
        metadata = None
    if metadata is not None:
        require(selection['build']['mode'] != 'tag', 'human tag package already exists')
        with tempfile.TemporaryDirectory() as temporary:
            remote = Path(temporary) / 'package.tgz'
            require(metadata['dist']['tarball'].startswith('https://registry.npmjs.org/'), 'foreign npm tarball')
            with urllib.request.urlopen(metadata['dist']['tarball'], timeout=60) as source, remote.open('wb') as target:
                import shutil
                shutil.copyfileobj(source, target)
            require(descriptor(remote) == descriptor(path), 'existing npm version has different bytes')
        return
    run('npm', 'publish', path, '--access', 'public', '--provenance', '--ignore-scripts', '--tag', npm_channel(selection))


def publish_images(directory, index):
    directory = Path(directory)
    for name, image_dir in (('bundle-builder.json', 'builder-image'), ('controlplane.json', 'controlplane-image')):
        image = read(directory / name)['image']
        tag = image.split('@')[0] + ':' + index['version']
        # Inspect failure other than not-found is not permission to replace remote data.
        result = subprocess.run(['skopeo', 'inspect', '--raw', 'docker://' + tag], capture_output=True)
        if result.returncode == 0:
            require('sha256:' + hashlib.sha256(result.stdout).hexdigest() == image.split('@')[1], 'OCI version collision')
        else:
            require(b'manifest unknown' in result.stderr.lower() or b'name unknown' in result.stderr.lower(), 'OCI lookup failed')
            run('skopeo', '--insecure-policy', 'copy', '--preserve-digests', 'dir:' + str(directory / image_dir), 'docker://' + tag)
        actual = subprocess.check_output(['skopeo', 'inspect', '--raw', 'docker://' + image])
        require('sha256:' + hashlib.sha256(actual).hexdigest() == image.split('@')[1], 'published image digest differs')


def stage(api, selection, directory):
    directory = Path(directory)
    index = dict(selection, schema='helmr.release.v0', assets={name: descriptor(directory / name) for name in sorted(ASSETS)})
    verify_files(index, directory)
    release = find_release(api, index['version'])
    if release is not None:
        require(index['build']['mode'] != 'tag', 'human tag release already exists')
    if release is None:
        release = api.request('releases', data=dict(tag_name=index['version'], target_commitish=index['sourceCommit'],
                    name=index['version'], draft=True, prerelease='-' in index['version'], make_latest='false'))
    bind_release(release, selection)
    require(release['draft'], 'release is already complete; use completed-byte admission')
    # Frozen index bytes from an earlier publishing attempt are reused, not re-signed.
    old = directory / 'old-build.json'
    if release_asset(api, release, 'release-build.json', old, missing=True):
        if release_asset(api, release, 'release-build.sigstore.json', directory / 'release-build.sigstore.json', missing=True):
            verify_signature(old, directory / 'release-build.sigstore.json', index['version'])
        previous = read(old)
        from transport import same_selection
        same_selection({k: previous[k] for k in selection}, selection)
        require(previous['assets'] == index['assets'], 'retry built different bytes')
        index = previous
    if old.exists():
        (directory / 'release-build.json').write_bytes(old.read_bytes())
    else:
        write(directory / 'release-build.json', index)
    # Freeze original publication metadata before the first OCI/npm writes.
    upload_identical(api, release, directory / 'release-build.json')
    publish_images(directory, index)
    for package, filename in (('@helmr/proto', 'proto.tgz'), ('@helmr/sdk', 'sdk.tgz')):
        npm_publish(directory / filename, package, index['version'][1:], selection)
    for name in sorted(ASSETS):
        upload_identical(api, release, directory / name)
    # This is pre-completion evidence. Freeze bytes before obtaining its signature;
    # an interrupted upload can then finish signing exactly the original bytes.
    if not (directory / 'release-build.sigstore.json').exists():
        sign(directory / 'release-build.json')
    upload_identical(api, release, directory / 'release-build.sigstore.json')
    return release


def download_build(api, selection, directory, release, expected_digest=None):
    bind_release(release, selection)
    index = fetch_index(api, release, directory, 'release-build')
    if expected_digest is not None:
        require(digest(Path(directory) / 'release-build.json') == expected_digest, 'signed build digest differs')
    from transport import same_selection
    same_selection({k: index[k] for k in selection}, selection)
    for name in ASSETS:
        release_asset(api, release, name, Path(directory) / name)
    return verify_files(index, directory)


def finalize(api, selection, directory, release_id, build_digest):
    if preview_mode(selection):
        from preview_store import finalize as preview_finalize
        return preview_finalize(api, selection, directory, build_digest)
    require(re.fullmatch(r'[1-9][0-9]*', str(release_id)), 'release ID required')
    require(re.fullmatch(DIGEST, build_digest or ''), 'build digest required')
    release = find_release(api, selection['version'])
    require(release is not None and str(release['id']) == str(release_id), 'selected release ID differs')
    release = api.request(f'releases/{release_id}')
    require(str(release['id']) == str(release_id), 'release ID differs')
    index = download_build(api, selection, directory, release, build_digest)
    build_bytes = (Path(directory) / 'release-build.json').read_bytes()
    if not release['draft']:
        # PATCH may have succeeded before public readback failed. Never write again.
        with tempfile.TemporaryDirectory() as temporary:
            require(complete(api, selection, Path(temporary) / 'public', release=release,
                             expected_build=build_bytes) is not None, 'public completion readback failed')
        return index
    # The successful consumer job is a dependency in the trusted workflow.
    path = Path(directory) / 'release-index.json'
    path.write_bytes(build_bytes)
    signature = Path(directory) / 'release-index.sigstore.json'
    old = Path(directory) / 'existing-index.sigstore.json'
    if release_asset(api, release, signature.name, old, missing=True):
        verify_signature(path, old, index['version'])
        old.rename(signature)
    else:
        sign(path)
    upload_identical(api, release, signature)
    upload_identical(api, release, path)  # Completion asset last.
    release = api.request(f'releases/{release["id"]}', method='PATCH', data=dict(draft=False, make_latest='false'))
    with tempfile.TemporaryDirectory() as temporary:
        require(complete(api, selection, Path(temporary) / 'public', release=release,
                         expected_build=build_bytes) is not None, 'public completion readback failed')
    return index


def discover(api, root, selection=None, index_digest=None):
    if selection is not None and selection['build']['mode'] == 'main':
        from preview_store import discover as preview_discover
        if not index_digest:
            with tempfile.TemporaryDirectory() as temporary:
                completed = complete(api, selection, Path(temporary) / 'public')
                if completed is None:
                    return None
                index_digest = digest(Path(temporary) / 'public' / 'release-index.json')
        return preview_discover(api, root, selection, index_digest)
    return None
