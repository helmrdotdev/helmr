"""Admission checks for exact native CI identity and PR head recheck."""
import copy
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
import admission
import publish
from contract import REPOSITORY, signer
from test_contract import assets, selection


class Fixture:
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.counter = 0
        self.git('init', '-q')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.git('config', 'user.name', 'fixture')
        (self.root / '.github/workflows').mkdir(parents=True)
        (self.root / '.github/workflows/ci.yaml').write_text('name: ci\n')
        self.original = self.commit()
        self.api = self.API(self.original)

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def commit(self):
        self.counter += 1
        (self.root / 'code.txt').write_text(f'change-{self.counter}')
        self.git('add', '-A')
        self.git('-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    class API:
        def __init__(self, head, main=None):
            self.head = head
            self.main = main or head
            self.calls = []

        def request(self, path, missing=False, destination=None):
            self.calls.append(path)
            if path == '':
                return dict(id=1)
            if path == 'actions/workflows/ci.yaml':
                return dict(id=2, path='.github/workflows/ci.yaml')
            if path == 'pulls/7':
                return dict(number=7, state='open', base=dict(ref='main', repo=dict(id=1)),
                            head=dict(sha=self.head, repo=dict(id=1)))
            if path == 'git/ref/heads/main':
                return dict(ref='refs/heads/main', object=dict(type='commit', sha=self.main))
            raise AssertionError(path)

        def pages(self, path, key=None):
            if '/runs?' in path:
                return [dict(id=10)]
            raise AssertionError(path)


class Admission(Fixture, unittest.TestCase):
    def test_behind_pr_and_main_movement_rechecked_without_mutating_selection(self):
        (self.root / 'code.txt').write_text('PR change')
        head = self.commit()
        self.api.head = head
        s = selection()
        s['sourceCommit'] = head
        s['build'].update(mode='pr', pr=7)
        original = copy.deepcopy(s)
        admission.recheck_pr(self.api, s)
        self.git('reset', '--hard', self.original)
        (self.root / '.github/workflows/ci.yaml').write_text('name: newer main\n')
        self.api.main = self.commit()
        self.api.head = head
        admission.recheck_pr(self.api, s)
        self.assertEqual(s, original)

    def test_manual_admission_accepts_workflow_differences(self):
        (self.root / '.github/workflows/ci.yaml').write_text('name: changed\n')
        self.api.head = self.commit()
        env = dict(GITHUB_REPOSITORY=REPOSITORY, GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main',
                   GITHUB_RUN_ID='123', GITHUB_RUN_ATTEMPT='1', GITHUB_WORKFLOW_SHA=self.original)
        event = dict(repository=dict(id=1), inputs=dict(pr='7', commit=self.api.head))
        with patch.object(self.api, 'pages', return_value=[dict(id=10)]), \
             patch.object(admission, 'ci_success', return_value=(10, [])), \
             patch.object(admission, 'git', return_value='{"version":"0.1.0"}'), \
             patch.object(admission, 'subprocess', wraps=subprocess) as commands:
            commands.run.return_value = subprocess.CompletedProcess([], 0)
            selected, _ = admission.admit(self.api, env, event, self.root)
        self.assertEqual(selected['sourceCommit'], self.api.head)

    def test_normal_exact_head_manual_admission(self):
        (self.root / 'code.txt').write_text('PR change')
        self.api.head = self.commit()
        env = dict(GITHUB_REPOSITORY=REPOSITORY, GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main',
                   GITHUB_RUN_ID='123', GITHUB_RUN_ATTEMPT='1', GITHUB_WORKFLOW_SHA=self.original)
        event = dict(repository=dict(id=1), inputs=dict(pr='7', commit=self.api.head))
        with patch.object(self.api, 'pages', return_value=[dict(id=10)]), patch.object(admission, 'ci_success', return_value=(10, [])), \
             patch.object(admission, 'git', return_value='{"version":"0.1.0"}'), patch.object(admission, 'subprocess', wraps=subprocess) as commands:
            commands.run.return_value = subprocess.CompletedProcess([], 0)
            selected, _ = admission.admit(self.api, env, event, self.root)
        self.assertEqual(selected['sourceCommit'], self.api.head)
        self.assertEqual(selected['sourceRef'], 'refs/pull/7/head')
        self.assertEqual(selected['build']['mode'], 'pr')
        commands.run.assert_called_once_with(['git', '-C', str(self.root), 'fetch', '--no-tags', 'origin', self.api.head], check=True)

    def test_pr_recheck_at_both_publication_boundaries(self):
        s = selection()
        s['sourceCommit'] = self.original
        s['build'].update(mode='pr', pr=7)
        (self.root / 'code.txt').write_text('stale head')
        self.api.head = self.commit()
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            assets(directory)
            with patch.object(publish, 'verify_files'), patch.object(publish, 'publish_images') as images:
                with self.assertRaisesRegex(ValueError, 'current PR head'):
                    publish.stage(self.api, s, directory)
                images.assert_not_called()
            with patch.object(publish, 'download_build') as download:
                with self.assertRaisesRegex(ValueError, 'current PR head'):
                    publish.finalize(self.api, s, directory, '1', 'sha256:' + '0' * 64)
                download.assert_not_called()

    def test_main_and_stable_do_not_acquire_pr_restriction(self):
        for mode in ('main', 'tag'):
            s = selection()
            s['build']['mode'] = mode
            admission.recheck_pr(self.api, s)
        self.assertEqual(self.api.calls, [])

    def test_human_tag_single_attempt_guard(self):
        import main
        with patch.dict('os.environ', {'GITHUB_EVENT_NAME': 'push', 'GITHUB_REF': 'refs/tags/v1.0.0',
                                       'GITHUB_RUN_ATTEMPT': '2', 'RELEASE_SELECTION': '{}'}):
            with patch.object(main, 'GitHub'), patch('sys.argv', ['release', 'precheck']):
                with self.assertRaisesRegex(ValueError, 'single-attempt'):
                    main.main()


if __name__ == '__main__':
    unittest.main()
