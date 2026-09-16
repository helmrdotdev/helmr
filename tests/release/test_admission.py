"""Native Git object metadata fixtures for current-main PR publication authority."""
import copy
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
import admission
import publish
from contract import REPOSITORY
from test_contract import assets, selection


class GitAPI:
    def __init__(self, root, main, head):
        self.root, self.main, self.head = root, main, head
        self.calls = []

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def request(self, path):
        self.calls.append(path)
        if path == '':
            return dict(id=1)
        if path == 'actions/workflows/ci.yaml':
            return dict(id=2, path='.github/workflows/ci.yaml')
        if path == 'pulls/7':
            return dict(number=7, state='open', base=dict(ref='main', repo=dict(id=1)), head=dict(sha=self.head, repo=dict(id=1)))
        if path == 'git/ref/heads/main':
            return dict(ref='refs/heads/main', object=dict(type='commit', sha=self.main))
        if path.startswith('git/commits/'):
            sha = path.split('/')[-1]
            return dict(sha=sha, tree=dict(sha=self.git('rev-parse', sha + '^{tree}')))
        if path.startswith('git/trees/'):
            sha = path.split('/')[-1]
            entries = []
            for entry in self.git('ls-tree', '-z', sha).split('\0'):
                if not entry:
                    continue
                metadata, name = entry.split('\t')
                mode, kind, oid = metadata.split()
                entries.append(dict(path=name, mode=mode, type=kind, sha=oid))
            return dict(sha=sha, truncated=False, tree=entries)
        raise AssertionError('unexpected API operation: ' + path)

    def pages(self, *args):
        raise AssertionError('workflow mismatch must stop before CI/build selection')


class PRWorkflowAuthority(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git('init', '-q')
        self.git('config', 'user.name', 'fixture')
        self.git('config', 'user.email', 'fixture@example.invalid')
        (self.root / '.github/workflows').mkdir(parents=True)
        (self.root / '.github/workflows/ci.yaml').write_text('name: original\n')
        self.original = self.commit()
        self.api = GitAPI(self.root, self.original, self.original)

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def commit(self):
        self.git('add', '-A')
        self.git('-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    def test_complete_tree_add_modify_delete_rename_and_revert(self):
        workflow = self.root / '.github/workflows/ci.yaml'
        for change in ('addition', 'modification', 'deletion', 'rename', 'mode'):
            with self.subTest(change=change):
                self.git('reset', '--hard', self.original)
                if change == 'addition':
                    (workflow.parent / 'extra.yaml').write_text('name: extra\n')
                elif change == 'modification':
                    workflow.write_text('name: changed\n')
                elif change == 'deletion':
                    workflow.unlink()
                elif change == 'rename':
                    workflow.rename(workflow.parent / 'renamed.yaml')
                else:
                    workflow.chmod(0o755)
                changed = self.commit()
                with self.assertRaisesRegex(ValueError, 'workflow-write authority'):
                    admission.pr_workflows(self.api, changed)
                self.api.main = changed
                admission.pr_workflows(self.api, changed)
                self.api.main = self.original
                # Revert restores byte/tree equality despite workflow history changes.
                self.git('revert', '--no-commit', changed)
                reverted = self.commit()
                admission.pr_workflows(self.api, reverted)
        admission.pr_workflows(self.api, self.original)

    def test_behind_pr_and_main_movement_rechecked_without_mutating_selection(self):
        (self.root / 'code.txt').write_text('PR change')
        head = self.commit()
        self.api.head = head
        s = selection(); s['sourceCommit'] = head
        s['build'].update(mode='pr', pr=7)
        original = copy.deepcopy(s)
        admission.recheck_pr(self.api, s)
        # Main advances from the shared base; PR has no workflow changes vs base.
        self.git('reset', '--hard', self.original)
        (self.root / '.github/workflows/ci.yaml').write_text('name: newer main\n')
        self.api.main = self.commit()
        with self.assertRaisesRegex(ValueError, 'current Product main'):
            admission.recheck_pr(self.api, s)
        self.assertEqual(s, original)
        # A workflow change already present on main is allowed, regardless of history.
        self.api.main = head
        admission.recheck_pr(self.api, s)

    def test_manual_admission_rejects_before_fetch_or_build(self):
        (self.root / '.github/workflows/ci.yaml').write_text('name: changed\n')
        self.api.head = self.commit()
        env = dict(GITHUB_REPOSITORY=REPOSITORY, GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main')
        event = dict(repository=dict(id=1), inputs=dict(pr='7', commit=self.api.head))
        with patch.object(admission, 'subprocess', wraps=subprocess) as commands:
            with self.assertRaisesRegex(ValueError, 'current Product main'):
                admission.admit(self.api, env, event, self.root)
            commands.run.assert_not_called()

    def test_normal_exact_head_manual_admission(self):
        (self.root / 'code.txt').write_text('PR change')
        self.api.head = self.commit()
        env = dict(GITHUB_REPOSITORY=REPOSITORY, GITHUB_EVENT_NAME='workflow_dispatch', GITHUB_REF='refs/heads/main',
                   GITHUB_RUN_ID='123', GITHUB_RUN_ATTEMPT='1', GITHUB_WORKFLOW_SHA=self.original)
        event = dict(repository=dict(id=1), inputs=dict(pr='7', commit=self.api.head))
        with patch.object(self.api, 'pages', return_value=[dict(id=10)]), patch.object(admission, 'ci_success', return_value=10), \
             patch.object(admission, 'git', return_value='{"version":"0.1.0"}'), patch.object(admission, 'subprocess', wraps=subprocess) as commands:
            commands.run.return_value = subprocess.CompletedProcess([], 0)
            selected = admission.admit(self.api, env, event, self.root)
        self.assertEqual(selected['sourceCommit'], self.api.head)
        self.assertEqual(selected['sourceRef'], 'refs/pull/7/head')
        self.assertEqual(selected['build']['mode'], 'pr')
        commands.run.assert_called_once_with(['git', '-C', str(self.root), 'fetch', '--no-tags', 'origin', self.api.head], check=True)

    def test_pr_recheck_at_both_publication_boundaries(self):
        s = selection(); s['sourceCommit'] = self.original; s['build'].update(mode='pr', pr=7)
        (self.root / '.github/workflows/ci.yaml').write_text('name: newer main\n')
        self.api.main = self.commit()
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary); assets(directory)
            with patch.object(publish, 'verify_files'), patch.object(publish, 'publish_images') as images:
                with self.assertRaisesRegex(ValueError, 'current Product main'):
                    publish.stage(self.api, s, directory)
                images.assert_not_called()
            with patch.object(publish, 'download_build') as download:
                with self.assertRaisesRegex(ValueError, 'current Product main'):
                    publish.finalize(self.api, s, directory)
                download.assert_not_called()
        self.assertFalse(any(path.startswith('releases') for path in self.api.calls))

    def test_main_and_stable_do_not_acquire_pr_restriction(self):
        for mode in ('main', 'tag'):
            s = selection(); s['build']['mode'] = mode
            admission.recheck_pr(self.api, s)
        self.assertEqual(self.api.calls, [])

    def test_incomplete_or_foreign_metadata_fails_closed(self):
        real = self.api.request
        for fault in ('truncated', 'wrong-tree', 'wrong-commit'):
            def response(path):
                result = real(path)
                if path.startswith('git/trees/'):
                    if fault == 'truncated': result['truncated'] = True
                    if fault == 'wrong-tree': result['sha'] = 'f' * 40
                if path.startswith('git/commits/') and fault == 'wrong-commit': result['sha'] = 'f' * 40
                return result
            with self.subTest(fault=fault), patch.object(self.api, 'request', side_effect=response):
                with self.assertRaises(ValueError): admission.pr_workflows(self.api, self.original)


if __name__ == '__main__':
    unittest.main()
