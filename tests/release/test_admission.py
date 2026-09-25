"""Native source CI and real Git ancestry for explicitly requested checkpoints."""
import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
import admission
import preview_store
from contract import REPOSITORY, preview_version


class Checkpoint(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.git('config', 'user.name', 'fixture')
        (self.root/'sdk/typescript').mkdir(parents=True)
        (self.root/'sdk/typescript/package.json').write_text('{"version":"0.1.0"}')
        self.source = self.commit('first')
        self.workflow = self.commit('second')
        self.git('remote', 'add', 'origin', str(self.root))
        self.env = dict(GITHUB_REPOSITORY=REPOSITORY, GITHUB_EVENT_NAME='workflow_dispatch',
                        GITHUB_REF='refs/heads/main', GITHUB_RUN_ID='123', GITHUB_RUN_ATTEMPT='1',
                        GITHUB_WORKFLOW_SHA=self.workflow, GITHUB_SHA=self.workflow)
        self.event = dict(repository=dict(id=1), inputs=dict(commit=self.source))
        self.run = dict(id=10, run_attempt=2, repository=dict(id=1), workflow_id=2,
                        path='.github/workflows/ci.yaml', event='push', head_branch='main',
                        head_sha=self.source, status='completed', conclusion='success')
        self.jobs = [dict(name='ci complete', status='completed', conclusion='success')]
        outer = self
        class API:
            def request(self, path, **kwargs):
                if path == '': return dict(id=1)
                if path == 'actions/workflows/ci.yaml': return dict(id=2, path='.github/workflows/ci.yaml')
                if path.startswith('releases/tags/'): return None
                raise AssertionError(path)
            def pages(self, path, key):
                if key == 'workflow_runs':
                    outer.assertIn('event=push&head_sha='+outer.source, path)
                    return [outer.run]
                outer.assertEqual(path, 'actions/runs/10/attempts/2/jobs')
                return outer.jobs
        self.api = API()

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def commit(self, text):
        (self.root/'code').write_text(text)
        self.git('add', '-A')
        self.git('-c', 'commit.gpgsign=false', 'commit', '-qm', text)
        return self.git('rev-parse', 'HEAD')

    def admit(self):
        return admission.admit(self.api, self.env, self.event, self.root)

    def test_checkpoint_owns_build_identity_even_after_main_advances(self):
        selected = self.admit()
        self.assertEqual(selected['sourceCommit'], self.source)
        self.assertEqual(selected['version'], preview_version('0.1.0', self.source, '123'))
        self.assertEqual(selected['build'], dict(runId='123', ciRun='10', workflowCommit=self.workflow,
                                                workflowRef='refs/heads/main', pr=None, mode='main'))
        self.commit('later main')
        self.assertEqual(self.admit(), selected)

    def test_non_main_source_is_rejected(self):
        self.git('checkout', '-qb', 'unmerged', self.source)
        self.event['inputs']['commit'] = self.commit('unmerged change')
        with self.assertRaisesRegex(ValueError, 'main history'): self.admit()

    def test_wrong_dispatch_ref_and_short_sha_are_rejected(self):
        self.env['GITHUB_REF'] = 'refs/heads/feature'
        with self.assertRaisesRegex(ValueError, 'execute main'): self.admit()
        self.env['GITHUB_REF'] = 'refs/heads/main'
        self.event['inputs']['commit'] = self.source[:8]
        with self.assertRaisesRegex(ValueError, 'full input SHA'): self.admit()

    def test_ci_must_bind_exact_main_run_and_attempt(self):
        for key, value in [('head_sha', self.workflow), ('event', 'pull_request'), ('head_branch', 'other'),
                           ('conclusion', 'failure'), ('workflow_id', 99), ('path', 'other.yaml')]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                admission.ci_success(self.api, dict(self.run, **{key:value}), 2, 1, self.source)
        for result in ('failure', 'cancelled', 'skipped', None):
            self.jobs[0]['conclusion'] = result
            with self.assertRaises(ValueError): admission.ci_success(self.api, self.run, 2, 1, self.source)
        self.jobs = []
        with self.assertRaises(ValueError): admission.ci_success(self.api, self.run, 2, 1, self.source)

    def test_old_automatic_release_event_is_rejected(self):
        self.env['GITHUB_EVENT_NAME'] = 'workflow_run'
        with self.assertRaisesRegex(ValueError, 'unsupported release event'): self.admit()

    def test_tag_uses_source_ci_without_preview_artifacts(self):
        self.env.update(GITHUB_EVENT_NAME='push', GITHUB_REF='refs/tags/v0.1.0', GITHUB_SHA=self.source)
        selected = self.admit()
        self.assertEqual(selected['build']['mode'], 'tag')
        self.assertEqual(selected['build']['runId'], '123')
        self.env['GITHUB_RUN_ATTEMPT'] = '2'
        with self.assertRaisesRegex(ValueError, 'single-attempt'): self.admit()

    def test_promotion_rejects_older_source_and_older_same_source_build(self):
        def record(source, run):
            return dict(sourceCommit=source, version=preview_version('0.1.0', source, str(run)))
        current = record(self.workflow, 120)
        self.assertFalse(preview_store.checkpoint_follows(self.root, record(self.source, 123), current))
        self.assertFalse(preview_store.checkpoint_follows(self.root, record(self.workflow, 119), current))
        self.assertTrue(preview_store.checkpoint_follows(self.root, record(self.workflow, 123), current))
        self.assertTrue(preview_store.checkpoint_follows(self.root, current, current))
        later = self.commit('later')
        self.assertTrue(preview_store.checkpoint_follows(self.root, record(later, 124), current))

    def test_older_checkpoint_cannot_mutate_npm_or_images(self):
        selected = dict(sourceCommit=self.source, version=preview_version('0.1.0', self.source, '123'))
        current = dict(sourceCommit=self.workflow, version=preview_version('0.1.0', self.workflow, '120'))
        with patch.object(preview_store, 'build_index', return_value={}), patch.object(preview_store, 'verify_files'), \
             patch.object(preview_store, 'config', return_value=('url', 'bucket')), \
             patch.object(preview_store, 'pointer_snapshot', return_value=(current, 'etag', b'')), \
             patch.object(preview_store, 'checkpoint_follows', return_value=False), \
             patch('publish.npm_publish') as npm, patch('publish.publish_images') as images:
            with self.assertRaisesRegex(ValueError, 'precedes'): preview_store.stage(self.api, selected, self.root)
            npm.assert_not_called()
            images.assert_not_called()
