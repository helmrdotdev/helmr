from contextlib import contextmanager, ExitStack
import json
from pathlib import Path
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

from test_host import host, config

SOURCE = 'a' * 40
GENERATION = '12345678-1234-4234-8234-123456789abc'


class RetireTests(unittest.TestCase):
    @contextmanager
    def environment(self):
        with tempfile.TemporaryDirectory() as temporary, ExitStack() as stack:
            root = Path(temporary).resolve()
            for name, value in {'CONFIG': 'config', 'DATA': 'data', 'WORKER_DATA': 'worker-data',
                                'JAILER_DATA': 'jailer', 'SYSTEMD': 'systemd', 'RETIRED': 'retired'}.items():
                stack.enter_context(patch.object(host, name, root / value))
                if name != 'RETIRED': (root / value).mkdir()
            (root / 'proc/self').mkdir(parents=True)
            (root / 'proc/self/mountinfo').write_text('')
            (root / 'sys/block').mkdir(parents=True)
            def path(*args):
                p = Path(*args)
                return root / str(p).lstrip('/') if str(p).startswith(('/proc', '/sys')) else p
            stack.enter_context(patch.object(host, 'Path', side_effect=path))
            stack.enter_context(patch.object(host.pwd, 'getpwnam', return_value=types.SimpleNamespace(pw_uid=87654)))
            stack.enter_context(patch.object(host, 'state', return_value='inactive'))
            def run(*args, **kwargs):
                output = 'disabled' if 'UnitFileState' in args else 'not-found' if 'LoadState' in args else ''
                return subprocess.CompletedProcess(args, 0, stdout=output)
            runner = stack.enter_context(patch.object(host, 'run', side_effect=run))
            cfg = host.compile_config(config())
            cfg['worker']['JAILER_CHROOT_DIR'] = str(host.JAILER_DATA)
            for name, value in [('installed-candidate.json', {'source_commit': SOURCE}),
                                ('data-generation.json', {'id': GENERATION}), ('config.json', cfg)]:
                (host.CONFIG / name).write_text(json.dumps(value))
            for name in host.SERVICES:
                unit = host.unit(name)
                (host.CONFIG / unit).write_text('owned ' + name)
                (host.SYSTEMD / unit).write_text('owned ' + name)
            override = host.SYSTEMD / 'helmr-worker.service.d'
            override.mkdir()
            (override / 'verification.conf').write_text('owned override')
            (host.CONFIG / 'worker-override.conf').write_text('owned override')
            (host.DATA / 'private-data').write_text('preserve me')
            yield root, cfg, runner

    def test_moves_private_data_and_config_without_deleting_shared_artifacts(self):
        with self.environment() as (root, cfg, runner):
            shared = root / 'shared-artifact'; shared.write_text('keep')
            host.retire_profile(cfg, SOURCE, GENERATION)
            archive = host.RETIRED / GENERATION
            self.assertEqual((archive / 'data/private-data').read_text(), 'preserve me')
            self.assertEqual((archive / 'config/retired-config.json').read_text(), json.dumps(cfg))
            self.assertEqual(archive.stat().st_mode & 0o777, 0o700)
            self.assertFalse(host.CONFIG.exists())
            self.assertFalse(host.WORKER_DATA.exists())
            self.assertEqual(shared.read_text(), 'keep')
            self.assertIn(('systemctl', 'daemon-reload'), [c.args for c in runner.call_args_list])

    def test_retires_failed_start_without_worker_data(self):
        with self.environment() as (root, cfg, runner):
            host.WORKER_DATA.rmdir()
            host.retire_profile(cfg, SOURCE, GENERATION)
            archive = host.RETIRED / GENERATION
            self.assertFalse(host.CONFIG.exists())
            self.assertFalse((archive / 'worker-data').exists())
            self.assertEqual((archive / 'data/private-data').read_text(), 'preserve me')
            record = json.loads((archive / 'config/pending-retirement.json').read_text())
            self.assertEqual(record['absent_data_paths'], [str(host.WORKER_DATA)])

    def test_refuses_before_mutation_for_identity_activity_and_foreign_paths(self):
        cases = ['source', 'generation', 'pending', 'active', 'enabled', 'process', 'nbd', 'mount', 'symlink', 'dangling-worker', 'missing-data', 'override', 'unit', 'worker-path', 'destination']
        for case in cases:
            with self.subTest(case=case), self.environment() as (root, cfg, runner), ExitStack() as stack:
                source, generation = SOURCE, GENERATION
                if case == 'source': source = 'b' * 40
                if case == 'generation': generation = '22345678-1234-4234-8234-123456789abc'
                if case == 'pending': (host.CONFIG / 'pending-update.json').write_text('{}')
                if case == 'active': stack.enter_context(patch.object(host, 'state', return_value='active'))
                if case == 'enabled': runner.side_effect = lambda *a, **k: subprocess.CompletedProcess(a, 0, stdout='enabled')
                if case == 'process':
                    (root / 'proc/123').mkdir(); (root / 'proc/123/comm').write_text('firecracker')
                if case == 'nbd':
                    (root / 'sys/block/nbd0').mkdir(); (root / 'sys/block/nbd0/pid').write_text('1')
                if case == 'mount': (root / 'proc/self/mountinfo').write_text(f'1 0 0:1 / {host.DATA}/mounted rw - tmpfs tmpfs rw\n')
                if case == 'symlink':
                    host.WORKER_DATA.rmdir(); host.WORKER_DATA.symlink_to(host.DATA)
                if case == 'dangling-worker':
                    host.WORKER_DATA.rmdir(); host.WORKER_DATA.symlink_to(root / 'missing')
                if case == 'missing-data':
                    (host.DATA / 'private-data').unlink(); host.DATA.rmdir()
                if case == 'override': (host.SYSTEMD / 'helmr-worker.service.d/foreign.conf').write_text('foreign')
                if case == 'unit': (host.SYSTEMD / host.unit('postgres')).write_text('foreign')
                if case == 'worker-path': cfg['worker']['WORKER_WORK_DIR'] = '/foreign'
                if case == 'destination': (host.RETIRED / generation).mkdir(parents=True)
                with self.assertRaises((RuntimeError, ValueError)):
                    host.retire_profile(cfg, source, generation)
                self.assertTrue(host.CONFIG.exists())
                if case != 'missing-data':
                    self.assertEqual((host.DATA / 'private-data').read_text(), 'preserve me')
                self.assertFalse((host.CONFIG / 'pending-retirement.json').exists())

    def test_reload_failure_leaves_install_blocker_and_preserved_data(self):
        with self.environment() as (root, cfg, runner):
            original = runner.side_effect
            def fail_reload(*args, **kwargs):
                if args == ('systemctl', 'daemon-reload'): raise RuntimeError('reload failed')
                return original(*args, **kwargs)
            runner.side_effect = fail_reload
            with self.assertRaisesRegex(RuntimeError, 'reload failed'):
                host.retire_profile(cfg, SOURCE, GENERATION)
            self.assertTrue((host.CONFIG / 'pending-retirement.json').exists())
            self.assertEqual((host.RETIRED / GENERATION / 'data/private-data').read_text(), 'preserve me')
            with self.assertRaisesRegex(RuntimeError, 'incomplete'):
                host.retire_profile(cfg, SOURCE, GENERATION)

    def test_marker_write_failure_cannot_restart_through_old_config_entrypoint(self):
        with self.environment() as (root, cfg, runner), patch.object(host, 'write_json', side_effect=OSError('write failed')):
            with self.assertRaisesRegex(OSError, 'write failed'):
                host.retire_profile(cfg, SOURCE, GENERATION)
            self.assertTrue(host.CONFIG.exists())
            self.assertEqual((host.CONFIG / 'retired-config.json').read_text(), json.dumps(cfg))
            with self.assertRaises(FileNotFoundError):
                (host.CONFIG / 'config.json').read_text()
            self.assertEqual((host.DATA / 'private-data').read_text(), 'preserve me')
            self.assertTrue((host.SYSTEMD / host.unit('control-plane')).exists())


if __name__ == '__main__': unittest.main()
