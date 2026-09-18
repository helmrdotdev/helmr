"""Main preview adoption: producer bytes, publisher readback, CI skip signal."""
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


def ci_run(run_id=100, attempt=1, head=SOURCE, event='push', branch='main', workflow_id=2, repo_id=1):
    return dict(id=run_id, run_attempt=attempt, head_sha=head, event=event, head_branch=branch,
                status='completed', conclusion='success', path='.github/workflows/ci.yaml',
                repository=dict(id=repo_id), workflow_id=workflow_id, pull_requests=[])


def job_entries(*names):
    return [dict(name=name, status='completed', conclusion='success') for name in names]


class API:
    def __init__(self, *, jobs=None, run=None, artifacts=None, zips=None):
        self.job_list = jobs or job_entries('source-ci-complete', 'preview-ready')
        self.run = run or ci_run()
        self.artifacts = artifacts or {}
        self.zips = zips or {}

    def request(self, path, missing=False, destination=None):
        if path == '':
            return dict(id=1)
        if path == 'actions/workflows/ci.yaml':
            return dict(id=2, path='.github/workflows/ci.yaml')
        if path == 'pulls/7':
            return dict(number=7, state='open', base=dict(ref='main', repo=dict(id=1)),
                        head=dict(sha=SOURCE, repo=dict(id=1)))
        if path.startswith('actions/runs/'):
            return self.run
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
        if path.endswith('/jobs'):
            return self.job_list
        if '/runs?' in path:
            return [self.run]
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


class MainAdmit(unittest.TestCase):
    def test_main_adopts_ci_producer_identity(self):
        run = ci_run(run_id=555, attempt=2)
        env = dict(GITHUB_REPOSITORY='helmrdotdev/helmr', GITHUB_EVENT_NAME='workflow_run',
                   GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='999', GITHUB_RUN_ATTEMPT='1',
                   GITHUB_WORKFLOW_SHA=ADVANCED)
        event = dict(repository=dict(id=1), workflow_run=dict(id=555))
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            subprocess.run(['git', '-C', str(root), 'init', '-q'], check=True)
            subprocess.run(['git', '-C', str(root), 'config', 'user.email', 'x@y.invalid'], check=True)
            subprocess.run(['git', '-C', str(root), 'config', 'user.name', 'fixture'], check=True)
            (root / 'sdk/typescript').mkdir(parents=True)
            (root / 'sdk/typescript/package.json').write_text('{"version":"0.1.0"}')
            subprocess.run(['git', '-C', str(root), 'add', '-A'], check=True)
            subprocess.run(['git', '-C', str(root), '-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture'], check=True)
            head = subprocess.check_output(['git', '-C', str(root), 'rev-parse', 'HEAD'], text=True).strip()
            run['head_sha'] = head
            with patch.object(admission, 'git', return_value='{"version":"0.1.0"}'), \
                 patch.object(admission.subprocess, 'run'):
                selected, _ = admission.admit(API(run=run), env, event, root)
        self.assertEqual(selected['build']['runId'], '555')
        self.assertEqual(selected['build']['workflowCommit'], head)
        self.assertNotEqual(selected['build']['workflowCommit'], ADVANCED)
        self.assertEqual(selected['build']['workflowRef'], signer(selected['version']).split('@', 1)[1])

    def test_wrong_ci_producer_fails_closed(self):
        with self.assertRaisesRegex(ValueError, 'wrong CI producer'):
            admission.ci_success(API(), ci_run(workflow_id=9), 2, 1, SOURCE, 'push')

    def test_admit_returns_skip_from_admitted_ci_jobs(self):
        jobs = job_entries('source-ci-complete', 'preview-ready', 'artifact build skipped')
        run = ci_run(run_id=555, attempt=2)
        env = dict(GITHUB_REPOSITORY='helmrdotdev/helmr', GITHUB_EVENT_NAME='workflow_run',
                   GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='999', GITHUB_RUN_ATTEMPT='1',
                   GITHUB_WORKFLOW_SHA=ADVANCED)
        event = dict(repository=dict(id=1), workflow_run=dict(id=555))
        with patch.object(admission, 'git', return_value='{"version":"0.1.0"}'), \
             patch.object(admission.subprocess, 'run'), \
             patch.object(admission, 'ci_success', return_value=(555, jobs)):
            selected, skip = admission.admit(API(run=run), env, event, Path('.'))
        self.assertTrue(skip)
        self.assertEqual(selected['build']['runId'], '555')


class MainOrdering(unittest.TestCase):
    def api_with_main(self, head):
        class MainAPI(API):
            def request(self, path, missing=False, destination=None):
                if path == 'git/ref/heads/main':
                    return dict(ref='refs/heads/main', object=dict(type='commit', sha=head))
                return super().request(path, missing=missing, destination=destination)
        return MainAPI()

    def test_source_at_head_allowed(self):
        selected = base_selection()
        selected['build']['mode'] = 'main'
        selected['sourceCommit'] = SOURCE
        self.assertFalse(admission.main_superseded(self.api_with_main(SOURCE), selected))

    def test_moved_head_supersedes_older_automatic_main(self):
        selected = base_selection()
        selected['build']['mode'] = 'main'
        selected['sourceCommit'] = SOURCE
        self.assertTrue(admission.main_superseded(self.api_with_main(ADVANCED), selected))

    def test_pr_mode_never_superseded_by_main_head(self):
        selected = base_selection()
        selected['build'].update(mode='pr', pr=7)
        with self.assertRaisesRegex(ValueError, 'main-only'):
            admission.main_superseded(self.api_with_main(ADVANCED), selected)

    def test_preview_channel_uses_native_queue_max_without_pending_eviction(self):
        release = (Path(__file__).resolve().parents[2] / '.github/workflows/release.yaml').read_text()
        top = release.split('\njobs:\n', 1)[0]
        publish_preview = release.split('\n  publish-preview:\n', 1)[1].split('\n  publish-tag:\n', 1)[0]
        discovery = release.split('\n  discovery:\n', 1)[1]
        for block in (top, publish_preview, discovery):
            self.assertIn('queue: max', block)
            self.assertIn('cancel-in-progress: false', block)
        self.assertIn('release-producer-', top)
        self.assertIn('release-preview-channel', publish_preview)
        self.assertIn('main.py verify', release)
        self.assertIn('PREVIEW_PUBLISHER_ROLE_ARN', publish_preview)
        self.assertIn('build.mode != \'tag\'', publish_preview)

    def test_precheck_command_skips_before_publication(self):
        selected = base_selection()
        selected['build']['mode'] = 'main'
        output = Path(tempfile.mkdtemp()) / 'outputs'
        with patch.object(main, 'GitHub', return_value=self.api_with_main(ADVANCED)), \
             patch.dict(os.environ, RELEASE_SELECTION=json.dumps(selected), GITHUB_OUTPUT=str(output)), \
             patch('sys.argv', ['release', 'precheck']):
            main.main()
        self.assertEqual(dict(line.split('=', 1) for line in output.read_text().splitlines()),
                         dict(superseded='true'))

    def test_publish_workflow_gates_superseded_candidates(self):
        release = (Path(__file__).resolve().parents[2] / '.github/workflows/release.yaml').read_text()
        publish_preview = release.split('\n  publish-preview:\n', 1)[1].split('\n  publish-tag:\n', 1)[0]
        verify = release.split('\n  verify:\n', 1)[1].split('\n  complete-preview:\n', 1)[0]
        complete_preview = release.split('\n  complete-preview:\n', 1)[1].split('\n  complete-tag:\n', 1)[0]
        self.assertIn('main.py precheck', publish_preview)
        self.assertIn('superseded: ${{ steps.precheck.outputs.superseded }}', publish_preview)
        self.assertIn("steps.precheck.outputs.superseded != 'true'", publish_preview)
        self.assertIn('needs.publish-preview.outputs.superseded != \'true\'', verify)
        self.assertIn('needs.publish-preview.outputs.superseded != \'true\'', complete_preview)


class ArtifactSkip(RepositoryFixture, unittest.TestCase):
    def test_docs_only_skips_artifact_build(self):
        self.assertFalse(admission.relevant(self.root, self.b, self.c))

    def test_unknown_paths_remain_relevant(self):
        self.assertTrue(admission.relevant(self.root, self.a, self.c))


class NpmChannels(unittest.TestCase):
    def test_publish_command_uses_explicit_channels(self):
        import urllib.error
        cases = [
            (dict(build=dict(mode='main', pr=None), version='v0.1.0-preview.g01234567.b1'), 'preview'),
            (dict(build=dict(mode='pr', pr=42), version='v0.1.0-preview.g01234567.b1'), 'pr-42'),
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
