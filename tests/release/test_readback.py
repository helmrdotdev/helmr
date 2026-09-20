"""Exact native handoff identity, flat archive and signed-byte admission."""
import copy
import json
import os
from pathlib import Path
import shutil
import stat
import tempfile
import unittest
from unittest.mock import patch
import warnings
import zipfile
import publish
import main
import transport
from contract import digest
from test_contract import selection
import test_publication


class Readback(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.api, self.release, self.index = test_publication.seed_draft(self.root)
        self.archive = self.root / 'readback.zip'
        self.members = {name: value for (_, name), value in self.api.blobs.items()}
        self.pack()
        self.item = dict(id=9, name='release-readback-123-2', expired=False,
                         workflow_run=dict(id=123), digest=digest(self.archive))
        # Real action output is bare hex; REST metadata and Product are prefixed.
        self.args = dict(artifact_id='9', artifact_digest=digest(self.archive)[7:],
                         publisher_run='123', publisher_attempt='2',
                         build_digest=digest(self.root / 'release-build.json'))
        self.requests = []
        fixture = self

        class Actions:
            def request(self, path, *, destination=None):
                fixture.requests.append(path)
                if path == 'actions/artifacts/9':
                    return fixture.item
                if path == 'actions/artifacts/9/zip':
                    shutil.copyfile(fixture.archive, destination)
                    return
                raise AssertionError('unexpected route: ' + path)

        self.actions = Actions()
        self.signatures = patch.object(publish, 'verify_signature', side_effect=test_publication.fake_verify)
        self.signatures.start()
        self.addCleanup(self.signatures.stop)

    def pack(self, extra=None, symlink=None):
        with zipfile.ZipFile(self.archive, 'w') as archive:
            for name, value in self.members.items():
                if name == symlink:
                    member = zipfile.ZipInfo(name)
                    member.create_system = 3
                    member.external_attr = (stat.S_IFLNK | 0o777) << 16
                    archive.writestr(member, value)
                else:
                    archive.writestr(name, value)
            if extra is not None:
                with warnings.catch_warnings():
                    warnings.simplefilter('ignore', UserWarning)
                    archive.writestr(extra, b'not an asset')

    def accept(self, destination='accepted', **overrides):
        selected = selection('tag')
        return transport.download_readback(self.actions, selected, self.root / destination,
                                           **dict(self.args, **overrides))

    def refresh_zip_digest(self):
        self.item['digest'] = digest(self.archive)
        self.args['artifact_digest'] = digest(self.archive)[7:]

    def test_distinct_attempts_digest_forms_and_real_output_bytes(self):
        self.assertEqual(self.accept(), self.index)
        self.assertNotIn('attempt', self.index['build'])
        self.assertEqual(self.requests, ['actions/artifacts/9', 'actions/artifacts/9/zip'])
        for name, value in self.members.items():
            self.assertEqual((self.root / 'accepted' / name).read_bytes(), value)

    def test_stage_command_exports_readback_identity_and_actual_publisher_attempt(self):
        selected = selection('tag')
        api = test_publication.Releases()
        output = self.root / 'job-outputs'
        readback = self.root / 'command-readback'
        with patch.object(main, 'GitHub', return_value=api), \
                patch.object(publish, 'npm_publish'), patch.object(publish, 'publish_images'), \
                patch.object(publish, 'sign', side_effect=test_publication.fake_sign), \
                patch.dict(os.environ, RELEASE_SELECTION=json.dumps(selected),
                           GITHUB_OUTPUT=str(output), GITHUB_RUN_ATTEMPT='1',
                           GITHUB_EVENT_NAME='push', GITHUB_REF='refs/tags/v1.0.0'), \
                patch('sys.argv', ['release', 'stage', '--directory', str(self.root), '--output', str(readback)]):
            main.main()
        self.assertEqual(dict(line.split('=', 1) for line in output.read_text().splitlines()),
                         dict(release_id='1', build_digest=digest(readback / 'release-build.json'),
                              publisher_attempt='1'))
        self.assertNotIn('attempt', json.loads((readback / 'release-build.json').read_bytes())['build'])

    def test_missing_or_malformed_outputs_fail_before_network(self):
        for key, values in dict(artifact_id=[None, '', '0', 'other'], publisher_run=[None, '', '0'],
                                publisher_attempt=[None, '', '0'],
                                artifact_digest=[None, '', 'A' * 64, '0' * 63, 'sha256:' + '0' * 64],
                                build_digest=[None, '', '0' * 64]).items():
            for value in values:
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    self.accept(**{key: value})
                self.assertEqual(self.requests, [])
        self.assertFalse((self.root / 'accepted').exists())

    def test_wrong_native_metadata_rejected_before_zip(self):
        original = copy.deepcopy(self.item)
        for field, value in [('id', 10), ('workflow_run', dict(id=124)), ('expired', True),
                             ('name', 'release-readback-123-1'), ('name', 'release-readback-124-2'),
                             ('digest', 'sha256:' + '0' * 64), ('digest', self.args['artifact_digest'])]:
            with self.subTest(field=field, value=value):
                self.item = dict(original, **{field: value})
                self.requests.clear()
                with self.assertRaises(ValueError):
                    self.accept()
                self.assertEqual(self.requests, ['actions/artifacts/9'])
                self.assertFalse((self.root / 'accepted').exists())

    def test_download_digest_and_flat_member_safety(self):
        with self.archive.open('ab') as stream:
            stream.write(b'corruption')
        with self.assertRaisesRegex(ValueError, 'ZIP digest'):
            self.accept()
        for extra, symlink in [('unexpected', None), ('../escape', None), ('sdk.tgz', None),
                               ('subdirectory/', None), (None, 'sdk.tgz')]:
            with self.subTest(extra=extra, symlink=symlink):
                self.pack(extra, symlink)
                self.refresh_zip_digest()
                with self.assertRaisesRegex(ValueError, 'coverage|non-regular'):
                    self.accept()
                self.assertFalse((self.root / 'accepted').exists())
        self.assertFalse((self.root.parent / 'escape').exists())

    def test_signed_build_and_assets_checked_before_candidate_execution(self):
        original = dict(self.members)
        for failure in ('build-digest', 'signature', 'source', 'asset'):
            with self.subTest(failure=failure):
                self.members = dict(original)
                args = {}
                selected = selection()
                if failure == 'build-digest':
                    args['build_digest'] = 'sha256:' + '0' * 64
                elif failure == 'signature':
                    self.members['release-build.sigstore.json'] = b'invalid signature'
                elif failure == 'source':
                    selected['sourceCommit'] = 'b' * 40
                else:
                    self.members['sdk.tgz'] = b'changed package bytes'
                self.pack()
                self.refresh_zip_digest()
                with patch('subprocess.run') as candidate:
                    with self.assertRaises(ValueError):
                        transport.download_readback(self.actions, selected, self.root / failure,
                                                   **dict(self.args, **args))
                        candidate(['candidate-execution'])
                    candidate.assert_not_called()

    def test_non_pr_frozen_build_missing_part_semantics_remain(self):
        class Empty:
            def pages(self, *_):
                return []
        for event in ('push', 'workflow_run', 'workflow_dispatch'):
            with patch.dict('os.environ', GITHUB_EVENT_NAME=event, GITHUB_RUN_ATTEMPT='2'):
                self.assertFalse(transport.restore(Empty(), 'sdk', selection(), self.root / 'missing'))

    def test_missing_pr_part_on_retry_requires_new_identity(self):
        class Empty:
            def pages(self, *_):
                return []
        for part in transport.PARTS:
            destination = self.root / part
            with patch.dict('os.environ', GITHUB_EVENT_NAME='pull_request', GITHUB_RUN_ATTEMPT='1'):
                self.assertFalse(transport.restore(Empty(), part, selection(), destination))
            with patch.dict('os.environ', GITHUB_EVENT_NAME='pull_request', GITHUB_RUN_ATTEMPT='2'):
                with self.assertRaisesRegex(ValueError, 'start a new workflow run'):
                    transport.restore(Empty(), part, selection(), destination)
            self.assertFalse(destination.exists())


class WorkflowBoundary(unittest.TestCase):
    def test_handoff_outputs_and_execution_authority(self):
        workflow = (Path(__file__).resolve().parents[2] / '.github/workflows/release.yaml').read_text()
        publish_preview = workflow.split('\n  publish-preview:\n', 1)[1].split('\n  publish-tag:\n', 1)[0]
        publish_tag = workflow.split('\n  publish-tag:\n', 1)[1].split('\n  verify:\n', 1)[0]
        verifier = workflow.split('\n  verify:\n', 1)[1].split('\n  complete-preview:\n', 1)[0]
        complete_preview = workflow.split('\n  complete-preview:\n', 1)[1].split('\n  complete-tag:\n', 1)[0]
        complete_tag = workflow.split('\n  complete-tag:\n', 1)[1].split('\n  discovery:\n', 1)[0]
        for name in ('build_digest', 'publisher_attempt'):
            self.assertIn(name + ': ${{ steps.stage.outputs.' + name, publish_preview)
        for name in ('build_digest', 'publisher_attempt'):
            self.assertIn('steps.stage_tag.outputs.' + name, publish_tag)
        self.assertIn('release_id: ${{ steps.stage_tag.outputs.release_id }}', publish_tag)
        self.assertIn('publisher_run: ${{ github.run_id }}', publish_preview)
        self.assertIn('publisher_run: ${{ github.run_id }}', publish_tag)
        for name in ('artifact-id', 'artifact-digest'):
            self.assertIn('steps.readback.outputs.' + name, publish_tag)
        self.assertIn('name: release-readback-${{ github.run_id }}-${{ github.run_attempt }}', publish_tag)
        self.assertIn('overwrite: false', publish_tag)
        self.assertIn('retention-days: 7', publish_tag)
        self.assertIn('compression-level: 0', publish_tag)
        self.assertIn('environment: preview', publish_preview)
        self.assertNotIn('contents: write', publish_preview)
        self.assertIn('contents: read', publish_preview)
        self.assertIn('pull-requests: read', publish_preview)
        self.assertIn('environment: release', publish_tag)
        self.assertIn('contents: write', publish_tag)
        self.assertIn('contents: read', verifier)
        self.assertIn('actions: read', verifier)
        for forbidden in ('environment:', 'id-token:', 'contents: write', 'packages: write'):
            self.assertNotIn(forbidden, verifier)
        download, execution = verifier.split('      - name: Execute downloaded consumer', 1)
        self.assertIn('GH_TOKEN: ${{ github.token }}', download)
        self.assertNotIn('GH_TOKEN', execution)
        for name in ('artifact_id', 'artifact_digest', 'publisher_run', 'publisher_attempt', 'build_digest'):
            self.assertIn('needs.publish-tag.outputs.' + name, download)
        self.assertIn('needs.publish-preview.outputs.build_digest', verifier)
        self.assertIn('main.py verify', verifier)
        self.assertIn('main.py download', verifier)
        self.assertIn('needs: [admission, publish-preview, publish-tag, verify]', complete_preview)
        self.assertIn('needs: [admission, publish-preview, publish-tag, verify]', complete_tag)
        self.assertIn('GH_TOKEN: ${{ github.token }}', complete_preview)
        self.assertIn('contents: read', complete_preview)
        self.assertIn('pull-requests: read', complete_preview)
        self.assertIn('Assume preview publisher role', complete_preview)
        self.assertIn('main.py finalize', complete_preview)
        complete_after_assume = complete_preview.split('- name: Assume preview publisher role', 1)[1]
        complete_consumer = complete_after_assume.split('- name:', 1)[1]
        self.assertIn('main.py finalize', complete_consumer)
        self.assertNotIn('assume-preview-role.sh', complete_consumer)
        discovery = workflow.split('\n  discovery:\n', 1)[1]
        self.assertIn('contents: read', discovery)
        self.assertIn('Assume preview publisher role', discovery)
        discover_after_assume = discovery.split('- name: Assume preview publisher role', 1)[1]
        discover_consumer = discover_after_assume.split('- name:', 1)[1] if '- name:' in discover_after_assume else discover_after_assume
        self.assertIn('main.py discover', discover_consumer)
        self.assertNotIn('assume-preview-role.sh', discover_consumer)
        self.assertIn('needs.publish-preview.outputs.build_digest', complete_preview)
        self.assertIn('needs.publish-tag.outputs.release_id', complete_tag)
        self.assertIn('needs.publish-tag.outputs.build_digest', complete_tag)
