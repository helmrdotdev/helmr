"""Exercise PR diff selection and terminal aggregate results without GitHub access."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
from ci_policy import check_jobs, classify, pr_checks


class Groups(unittest.TestCase):
    def test_ordinary_source_keeps_only_source_checks(self):
        for path in ('README.md', 'packages/web/src/content/docs/cli.md',
                     'packages/console/src/routes/runs.tsx', 'packages/console/src/lib/runs.test.ts',
                     'packages/web/src/pages/index.astro', 'packages/web/public/favicon.svg',
                     'internal/controlplane/project.go', 'internal/controlplane/project_test.go',
                     'internal/email/resend/client.go'):
            with self.subTest(path=path):
                self.assertEqual(classify([path]), dict(artifacts=False, bundle_builder=False))

    def test_sensitive_and_unknown_paths_require_full_checks(self):
        for path in ('go.mod', 'go.sum', 'flake.lock', 'bun.lock', 'package.json',
                     'packages/console/package.json', 'scripts/release/ci_policy.py',
                     '.github/workflows/ci.yaml', 'nix/packages/worker.nix',
                     'internal/controlplane/worker_run.go', 'internal/controlplane/deployment_create.go',
                     'internal/controlplane/worker.go', 'internal/controlplane/run_lease_claim_response.go',
                     'internal/controlplane/run_start_contract.go', 'internal/controlplane/new_contract.go',
                     'internal/controlplane/runtime_descriptor.go', 'internal/console/new_embedding.go',
                     'cmd/helmr/build.go', 'sdk/typescript/src/work.ts', 'proto/typescript/new.ts',
                     'compiler/typescript/src/compiler.ts', 'runtime/typescript/src/main.ts',
                     'internal/builder/builder.go', 'internal/hostconfig/config.go',
                     'internal/guestd/guest.go', 'tests/fixtures/native-environment/probe/check.cjs',
                     'tests/fixtures/agentic-work/work/tool.mjs', 'LICENSE', 'unknown/new.go',
                     'packages/console/src/new.unknown', 'packages/console/src/../../package.json'):
            with self.subTest(path=path):
                self.assertEqual(classify([path]), dict(artifacts=True, bundle_builder=True))
        self.assertEqual(classify([]), dict(artifacts=True, bundle_builder=True))

    def test_packaging_and_builder_are_selected_independently(self):
        for path in ('internal/console/console_embed.go', 'packages/console/vite.config.ts',
                     'scripts/build-controlplane-image.sh'):
            with self.subTest(path=path):
                self.assertEqual(classify(['README.md', path]), dict(artifacts=True, bundle_builder=False))
        self.assertEqual(classify(['packages/console/src/App.tsx', 'go.sum']),
                         dict(artifacts=True, bundle_builder=True))


class GitDiff(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.name', 'fixture')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.write('README.md', 'base')
        self.write('runtime/old.ts', 'export const version = 0;')
        self.base = self.commit()

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.root), *args], text=True).strip()

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def commit(self):
        self.git('add', '-A')
        self.git('-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    def event(self, head, labels=()):
        return dict(action='synchronize', pull_request=dict(base=dict(sha=self.base, repo=dict(id=1)),
                    head=dict(sha=head, repo=dict(id=1)), labels=[dict(name=name) for name in labels]))

    def test_diff_uses_complete_pr_not_last_commit(self):
        self.write('runtime/old.ts', 'critical')
        self.commit()
        self.write('README.md', 'docs afterward')
        self.assertTrue(pr_checks(self.root, self.event(self.commit()))['artifacts'])

    def test_behind_main_uses_merge_base(self):
        self.write('packages/console/src/App.tsx', 'ui')
        head = self.commit()
        self.git('checkout', '-qb', 'main-moved', self.base)
        self.write('go.sum', 'main-only change')
        event = self.event(head)
        event['pull_request']['base']['sha'] = self.commit()
        self.assertEqual(pr_checks(self.root, event), dict(artifacts=False, bundle_builder=False))

    def test_rename_out_of_critical_path_keeps_deleted_name(self):
        (self.root / 'packages/console/src').mkdir(parents=True)
        self.git('mv', 'runtime/old.ts', 'packages/console/src/old.ts')
        self.assertTrue(pr_checks(self.root, self.event(self.commit()))['bundle_builder'])

    def test_deleted_critical_input_is_selected(self):
        (self.root / 'runtime/old.ts').unlink()
        self.assertTrue(pr_checks(self.root, self.event(self.commit()))['artifacts'])

    def test_missing_invalid_and_empty_comparison_are_full(self):
        for base in ('f' * 40, '--bad-ref', self.base):
            event = self.event(self.base)
            event['pull_request']['base']['sha'] = base
            self.assertEqual(pr_checks(self.root, event), dict(artifacts=True, bundle_builder=True))

    def test_labels_add_remove_and_persist_on_new_revision(self):
        self.write('README.md', 'docs')
        head = self.commit()
        for action, labels, expected in (
            ('labeled', ('ci:full',), True), ('unlabeled', (), False),
            ('synchronize', ('ci:full',), True), ('labeled', ('other',), False),
            ('unlabeled', ('ci:full',), True),
        ):
            with self.subTest(action=action, labels=labels):
                event = self.event(head, labels)
                event['action'] = action
                event['label'] = dict(name='other')
                self.assertEqual(pr_checks(self.root, event)['artifacts'], expected)

    def test_fork_uses_same_unprivileged_selection(self):
        self.write('packages/console/src/App.tsx', 'ui')
        event = self.event(self.commit())
        event['pull_request']['head']['repo']['id'] = 2
        self.assertFalse(pr_checks(self.root, event)['artifacts'])
        event['pull_request']['labels'] = [dict(name='ci:full')]
        self.assertTrue(pr_checks(self.root, event)['artifacts'])


class Aggregates(unittest.TestCase):
    def needs(self, artifacts=False, builder=False):
        needs = {name: dict(result='success') for name in (
            'nix-flake', 'repo', 'postgres', 'browser', 'release-contracts', 'source-ci-complete')}
        needs['artifact-selection'] = dict(result='success', outputs=dict(
            skip_artifacts='false' if artifacts else 'true', run_bundle_builder='true' if builder else 'false'))
        needs['bundle-builder'] = dict(result='success' if builder else 'skipped')
        needs['build-artifacts'] = dict(result='success' if artifacts else 'skipped')
        return needs

    def test_selected_and_intentionally_skipped_jobs(self):
        for artifacts, builder in ((False, False), (True, False), (True, True)):
            for aggregate in ('source', 'pr'):
                check_jobs(self.needs(artifacts, builder), aggregate, 'pull_request')

    def test_every_required_failure_cancellation_skip_or_absence_rejects(self):
        for aggregate, jobs in (
            ('source', ('nix-flake', 'repo', 'postgres', 'browser', 'release-contracts', 'bundle-builder')),
            ('pr', ('source-ci-complete', 'build-artifacts')),
        ):
            for job in (*jobs, 'artifact-selection'):
                for result in ('failure', 'cancelled', 'skipped', None):
                    with self.subTest(aggregate=aggregate, job=job, result=result):
                        needs = self.needs(True, True)
                        needs[job]['result'] = result
                        with self.assertRaises(ValueError):
                            check_jobs(needs, aggregate, 'pull_request')

    def test_missing_or_forged_selector_does_not_authorize_skips(self):
        for key in ('skip_artifacts', 'run_bundle_builder'):
            for value in ('', 'yes', True, False, None):
                needs = self.needs()
                needs['artifact-selection']['outputs'][key] = value
                for aggregate in ('source', 'pr'):
                    with self.subTest(key=key, value=value, aggregate=aggregate), self.assertRaises(ValueError):
                        check_jobs(needs, aggregate, 'pull_request')
        needs = self.needs()
        del needs['artifact-selection']
        with self.assertRaises(ValueError):
            check_jobs(needs, 'pr', 'pull_request')

    def test_unselected_job_failure_or_missing_is_not_an_intentional_skip(self):
        for job, aggregate in (('bundle-builder', 'source'), ('build-artifacts', 'pr')):
            for value in ('failure', 'cancelled', None):
                needs = self.needs()
                needs[job]['result'] = value
                with self.assertRaises(ValueError):
                    check_jobs(needs, aggregate, 'pull_request')

    def test_main_still_requires_all_source_checks(self):
        for artifacts in (True, False):
            check_jobs(self.needs(artifacts, True), 'source', 'push')
        with self.assertRaises(ValueError):
            check_jobs(self.needs(), 'source', 'push')

    def test_native_command_reports_failed_aggregate(self):
        script = Path(__file__).resolve().parents[2] / 'scripts/release/ci_policy.py'
        env = dict(os.environ, CI_NEEDS=json.dumps(self.needs()), GITHUB_EVENT_NAME='pull_request')
        subprocess.run([sys.executable, str(script), 'pr'], env=env, check=True)
        needs = self.needs()
        needs['source-ci-complete']['result'] = 'failure'
        env['CI_NEEDS'] = json.dumps(needs)
        result = subprocess.run([sys.executable, str(script), 'pr'], env=env, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b'source-ci-complete', result.stderr)


if __name__ == '__main__':
    unittest.main()
