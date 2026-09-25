"""Frozen release bytes and publisher readback remain bound to native run identity."""
import copy
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
import admission
import main
import publish
import transport
from contract import preview_version, signer
from test_contract import RepositoryFixture, assets, selection as base_selection
import test_publication


SOURCE = '01234567' + 'a' * 32
ADVANCED = '89abcdef' + 'b' * 32


class API:
    def __init__(self, *, artifacts=None, zips=None):
        self.artifacts = artifacts or {}
        self.zips = zips or {}

    def request(self, path, missing=False, destination=None):
        if path.endswith('/zip'):
            artifact_id = path.split('/')[-2]
            shutil.copyfile(self.zips[artifact_id], destination)
            return None
        if path.startswith('actions/artifacts/'):
            item = self.artifacts.get(path.split('/')[-1])
            if item is None and missing:
                return None
            if item is None:
                raise AssertionError(path)
            return item
        raise AssertionError(path)

    def pages(self, path, key=None):
        if path.endswith('/artifacts'):
            return list(self.artifacts.values())
        raise AssertionError(path)


class ProducerRestore(unittest.TestCase):
    def test_restore_accepts_producer_workflow_commit_after_main_advances(self):
        producer = base_selection()
        producer['build'].update(runId='555', ciRun='555', mode='main', workflowCommit=SOURCE)
        adopted = copy.deepcopy(producer)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            built = root / 'built'
            built.mkdir()
            (built / 'sdk.tgz').write_bytes(b'sdk')
            (built / 'proto.tgz').write_bytes(b'proto')
            transport.freeze('sdk', built, producer, root / 'frozen')
            z = root / 'native.zip'
            with zipfile.ZipFile(z, 'w') as archive:
                for part in (root / 'frozen').iterdir():
                    archive.write(part, part.name)
            item = dict(id=4, name='build-artifacts-555-sdk', expired=False,
                          workflow_run=dict(id=555), digest=transport.digest(z))
            api = API(artifacts={'4': item}, zips={'4': z})
            self.assertTrue(transport.restore(api, 'sdk', adopted, root / 'restored'))
            self.assertEqual((root / 'restored/sdk.tgz').read_bytes(), b'sdk')


class ReadbackPublisherRun(unittest.TestCase):
    def test_readback_binds_publisher_run_not_producer(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            producer = base_selection()
            producer['build'].update(runId='555', ciRun='555')
            index = assets(root, producer)
            publish.write(root / 'release-build.json', index)
            test_publication.fake_sign(root / 'release-build.json')
            publisher_run = '999'
            archive = root / 'readback.zip'
            members = [p.name for p in root.iterdir() if p.is_file()]
            with zipfile.ZipFile(archive, 'w') as bundle:
                for name in members:
                    bundle.write(root / name, name)
            build_digest = transport.digest(root / 'release-build.json')
            item = dict(id=9, name=f'release-readback-{publisher_run}-2', expired=False,
                        workflow_run=dict(id=int(publisher_run)), digest=transport.digest(archive))
            api = API(artifacts={'9': item}, zips={'9': archive})
            with patch.object(publish, 'verify_signature', side_effect=test_publication.fake_verify):
                transport.download_readback(api, producer, root / 'accepted', '9',
                                            transport.digest(archive)[7:], publisher_run, '2', build_digest)
            item['workflow_run'] = dict(id=555)
            with patch.object(publish, 'verify_signature', side_effect=test_publication.fake_verify):
                with self.assertRaisesRegex(ValueError, 'foreign artifact ID/run'):
                    transport.download_readback(api, producer, root / 'rejected', '9',
                                                transport.digest(archive)[7:], publisher_run, '2', build_digest)

    def test_foreign_producer_artifact_rejected_on_restore(self):
        producer = base_selection()
        producer['build'].update(runId='555', ciRun='555')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            built = root / 'built'
            built.mkdir()
            (built / 'sdk.tgz').write_bytes(b'sdk')
            (built / 'proto.tgz').write_bytes(b'proto')
            transport.freeze('sdk', built, producer, root / 'frozen')
            z = root / 'native.zip'
            with zipfile.ZipFile(z, 'w') as archive:
                for part in (root / 'frozen').iterdir():
                    archive.write(part, part.name)
            item = dict(id=4, name='build-artifacts-555-sdk', expired=False,
                        workflow_run=dict(id=124), digest=transport.digest(z))
            api = API(artifacts={'4': item}, zips={'4': z})
            with self.assertRaisesRegex(ValueError, 'foreign artifact run'):
                transport.restore(api, 'sdk', producer, root / 'out')


class NpmChannels(unittest.TestCase):
    def test_publish_command_uses_explicit_channels(self):
        import urllib.error
        cases = [
            (dict(build=dict(mode='main', pr=None), version='v0.1.0-preview.g01234567.b1'), 'preview'),
            (dict(build=dict(mode='tag', pr=None), version='v1.0.0'), 'latest'),
            (dict(build=dict(mode='tag', pr=None), version='v1.0.0-rc.1'), 'next'),
        ]
        for selected, expected in cases:
            with self.subTest(tag=expected), tempfile.NamedTemporaryFile() as tmp:
                version = selected['version'][1:] if selected['version'].startswith('v') else selected['version']
                with patch.object(publish.urllib.request, 'urlopen', side_effect=urllib.error.HTTPError('u', 404, 'm', None, None)), \
                     patch.object(publish, 'run') as command:
                    publish.npm_publish(Path(tmp.name), '@helmr/sdk', version, selected)
                    self.assertEqual(command.call_args[0][-1], expected)


if __name__ == '__main__':
    unittest.main()
