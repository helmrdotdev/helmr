"""Frozen build parts use native Actions artifacts, never a resume database."""
from pathlib import Path
import shutil
import tarfile
import tempfile
import zipfile
from contract import canonical, descriptor, digest, read, require, safe_extract, write

PARTS = ('sdk', 'builder', 'platform', 'host', 'guest', 'controlplane', 'cli')
FILES = {
    'sdk': {'proto.tgz', 'sdk.tgz'},
    'builder': {'bundle-builder.json', 'builder-image'},
    'platform': {'platform-release.tar', 'platform-release-provenance.json'},
    'host': {'worker-host-artifacts.tar', 'worker-host-artifacts.json', 'worker-host-bundle.json'},
    'guest': {'runtime-artifacts.tar', 'runtime-artifacts.json', 'worker-runtime-bundle.json'},
    'controlplane': {'controlplane.json', 'control-plane-linux-amd64.tar.gz', 'controlplane-image'},
    'cli': {'checksums.txt', *(f'helmr-{p}.tar.gz' for p in ('linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64'))},
}


def same_selection(a, b):
    # Publishing attempt changes; original source/workflow/build identity does not.
    a, b = dict(a, build=dict(a['build'])), dict(b, build=dict(b['build']))
    a['build'].pop('attempt'); b['build'].pop('attempt')
    require(a == b, 'frozen source/workflow/build identity differs')


def freeze(part, directory, selection, output):
    require(part in PARTS, 'unknown part')
    directory, output = Path(directory), Path(output)
    require({p.name for p in directory.iterdir()} == FILES[part], 'build output coverage differs')
    files = {str(p.relative_to(directory)): descriptor(p) for p in directory.rglob('*') if p.is_file()}
    require(not any(p.is_symlink() for p in directory.rglob('*')), 'build output contains symlink')
    output.mkdir()
    archive = output / 'part.tar'
    with tarfile.open(archive, 'w') as bundle:
        for path in sorted(directory.iterdir()):
            bundle.add(path, arcname=path.name)
    write(output / 'part.json', dict(part=part, selection=selection, archive=descriptor(archive), files=files))


def restore(api, part, selection, destination):
    run_id = selection['build']['runId']
    name = f'build-artifacts-{run_id}-{part}'
    items = [a for a in api.pages(f'actions/runs/{run_id}/artifacts', 'artifacts') if a['name'] == name]
    if not items:
        return False
    require(len(items) == 1 and not items[0]['expired'], 'ambiguous or expired frozen artifact; use a new build identity')
    item = items[0]
    require(str(item['workflow_run']['id']) == str(run_id), 'foreign artifact run')
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        archive = root / 'native.zip'
        api.request(f'actions/artifacts/{item["id"]}/zip', destination=archive)
        require(digest(archive) == item['digest'], 'native artifact ZIP digest differs')
        with zipfile.ZipFile(archive) as zipped:
            require(sorted(zipped.namelist()) == ['part.json', 'part.tar'], 'native artifact layout differs')
            for name in zipped.namelist():
                with zipped.open(name) as source, (root / name).open('xb') as target:
                    shutil.copyfileobj(source, target)
        manifest = read(root / 'part.json')
        same_selection(manifest['selection'], selection)
        require(manifest['part'] == part and descriptor(root / 'part.tar') == manifest['archive'], 'part archive differs')
        safe_extract(root / 'part.tar', destination)
        actual = {str(p.relative_to(destination)): descriptor(p) for p in Path(destination).rglob('*') if p.is_file()}
        require(actual == manifest['files'], 'frozen part bytes differ')
        require({p.name for p in Path(destination).iterdir()} == FILES[part], 'frozen part coverage differs')
    return True


def assemble(api, selection, destination):
    destination = Path(destination)
    destination.mkdir()
    origins = []
    with tempfile.TemporaryDirectory() as temporary:
        for part in PARTS:
            directory = Path(temporary) / part
            require(restore(api, part, selection, directory), 'missing frozen part: ' + part)
            for path in directory.iterdir():
                require(not (destination / path.name).exists(), 'duplicate assembled output')
                shutil.move(path, destination / path.name)
            origins.append(part)
    return origins
