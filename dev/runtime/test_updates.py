import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from test_host import host, config, ROOT

spec = importlib.util.spec_from_file_location('build_services', ROOT / 'dev/runtime/build_services.py')
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)


def receipt():
    return {'source_commit': 'a' * 40, 'source_sha256': 'b' * 64,
            'inputs': {**{n: 'c' * 64 for n in ['control-plane', 'dispatcher', 'worker', 'guestd', 'schema', 'runtime-support']}, 'toolchain': 'go1.27.1'},
            'binaries': {'control-plane': 'd' * 64, 'dispatcher': 'e' * 64}}


def candidate(directory, cp=b'new-cp', dispatcher=b'dispatcher'):
    directory.mkdir()
    for name, contents in [('control-plane', cp), ('dispatcher', dispatcher), ('source.tar', b'source archive fixture')]:
        (directory / name).write_bytes(contents)
    value = receipt()
    value['binaries'] = {n: host.digest(directory / n) for n in ['control-plane', 'dispatcher']}
    value['source_sha256'] = host.digest(directory / 'source.tar')
    (directory / 'candidate.json').write_text(json.dumps(value))
    return value


class UpdateTests(unittest.TestCase):
    def test_existing_migration_edit_requires_explicit_reset(self):
        old = receipt()
        changed = copy.deepcopy(old)
        changed['inputs']['schema'] = 'f' * 64
        with self.assertRaisesRegex(ValueError, 'schema content'):
            host.update_kind(old, changed, False)
        self.assertEqual(host.update_kind(old, changed, True), ['control-plane', 'dispatcher'])

    def test_reset_cannot_hide_runtime_change(self):
        for component in ['worker', 'guestd', 'runtime-support', 'toolchain']:
            old = receipt()
            changed = copy.deepcopy(old)
            changed['inputs'][component] = 'different'
            with self.subTest(component=component), self.assertRaises(ValueError):
                host.update_kind(old, changed, True)

    def exercise_cp_update(self, fail_receipt=False, fail_result=False):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            cfg_dir = directory / 'config'
            cfg_dir.mkdir()
            (cfg_dir / 'bin').mkdir()
            old = candidate(directory / 'old', cp=b'old-cp')
            new = candidate(directory / 'new')
            new['source_commit'] = 'f' * 40
            (directory / 'new/candidate.json').write_text(json.dumps(new))
            for name in ['control-plane', 'dispatcher']:
                (cfg_dir / 'bin' / name).write_bytes((directory / 'old' / name).read_bytes())
            (cfg_dir / 'installed-candidate.json').write_text(json.dumps(old))
            (cfg_dir / 'data-generation.json').write_text(json.dumps({'id': 'generation', 'schema': old['inputs']['schema']}))
            (cfg_dir / 'binary-digests.json').write_text(json.dumps(old['binaries']))
            cfg = host.compile_config(config())
            cfg['binaries'].update({name: str(cfg_dir / 'bin' / name) for name in ['control-plane', 'dispatcher']})
            def identity(name):
                if name == host.unit('control-plane'):
                    return {'executable_sha256': host.digest(cfg_dir / 'bin/control-plane')}
                return {'InvocationID': name, 'MainPID': '1', 'executable_sha256': 'unchanged'}
            original_write = host.write_json
            injected = False
            def write(path, value):
                nonlocal injected
                if not injected and ((fail_receipt and path.name == 'binary-digests.json') or
                                     (fail_result and path.name == 'result.json' and value.get('phase') == 'service-ready')):
                    injected = True
                    raise OSError('injected receipt write failure')
                original_write(path, value)
            with patch.object(host, 'write_json', side_effect=write), patch.object(host, 'CONFIG', cfg_dir), patch.object(host, 'require_active_services'), patch.object(host, 'service_identity', side_effect=identity), patch.object(host, 'http_ready', return_value=True), patch.object(host, 'state', return_value='active'), patch.object(host, 'run') as run:
                if fail_receipt or fail_result:
                    with self.assertRaises(OSError):
                        host.apply_services(cfg, directory / 'new', False)
                else:
                    host.apply_services(cfg, directory / 'new', False)
                expected = [('systemctl', 'stop', host.unit('control-plane')), ('systemctl', 'start', host.unit('control-plane'))]
                self.assertEqual([c.args for c in run.call_args_list], expected)
            accepted = json.loads((cfg_dir / 'installed-candidate.json').read_text())
            if fail_receipt or fail_result:
                self.assertEqual((cfg_dir / 'bin/control-plane').read_bytes(), b'new-cp')
                with patch.object(host, 'CONFIG', cfg_dir), self.assertRaisesRegex(RuntimeError, 'recreate'):
                    host.apply_services(cfg, directory / 'new', False)
            else:
                self.assertEqual(accepted['component_sources']['dispatcher'], old['source_commit'])
                self.assertEqual(accepted['component_sources']['control-plane'], new['source_commit'])
            self.assertEqual((cfg_dir / 'bin/dispatcher').read_bytes(), b'dispatcher')
            self.assertEqual((cfg_dir / 'pending-update.json').exists(), fail_receipt or fail_result)

    def test_cp_route_does_not_relabel_dispatcher(self):
        self.exercise_cp_update()

    def test_receipt_failure_blocks_reuse_without_rollback(self):
        self.exercise_cp_update(fail_receipt=True)

    def test_final_result_failure_keeps_incomplete_marker(self):
        self.exercise_cp_update(fail_result=True)

    def test_bad_binary_is_rejected_before_mutation(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / 'candidate'
            candidate(directory)
            (directory / 'control-plane').write_bytes(b'tampered')
            with self.assertRaisesRegex(ValueError, 'binary digest'):
                host.read_candidate(directory)

    def test_reset_keeps_installed_images_and_removes_old_fixtures(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            data, worker, jailer = [base / name for name in ['data', 'worker', 'jailer']]
            image = base / 'images/rootfs'
            image.parent.mkdir()
            image.write_bytes(b'immutable')
            for path in [data / n for n in ['postgres', 'redis', 'clickhouse']] + [worker, jailer]:
                path.mkdir(parents=True)
                (path / 'old').write_text('old fixture')
            with patch.object(host, 'DATA', data), patch.object(host, 'WORKER_DATA', worker), patch.object(host, 'JAILER_DATA', jailer), patch.object(host, 'state', return_value='inactive'), patch.object(Path, 'read_text', return_value=''), patch.object(host.shutil, 'chown'), patch.object(host, 'service_run') as initialize:
                host.reset_private_data(host.compile_config(config()))
                initialize.assert_called_once()
            self.assertEqual(image.read_bytes(), b'immutable')
            self.assertFalse(worker.exists())
            self.assertFalse((data / 'postgres').exists())  # native initdb is mocked
            self.assertEqual(list(jailer.iterdir()), [])

    def test_failed_old_worker_drain_never_resets_data(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            cfg_dir = directory / 'config'
            cfg_dir.mkdir()
            (cfg_dir / 'bin').mkdir()
            old = candidate(directory / 'old', cp=b'old-cp')
            candidate(directory / 'new')
            for name in ['control-plane', 'dispatcher']:
                (cfg_dir / 'bin' / name).write_bytes((directory / 'old' / name).read_bytes())
            (cfg_dir / 'installed-candidate.json').write_text(json.dumps(old))
            (cfg_dir / 'data-generation.json').write_text(json.dumps({'id': 'generation', 'schema': old['inputs']['schema']}))
            (cfg_dir / 'binary-digests.json').write_text(json.dumps(old['binaries']))
            cfg = host.compile_config(config())
            cfg['binaries'].update({name: str(cfg_dir / 'bin' / name) for name in ['control-plane', 'dispatcher']})
            with patch.object(host, 'CONFIG', cfg_dir), patch.object(host, 'require_active_services'), patch.object(host, 'service_identity', return_value={'MainPID': '1'}), patch.object(host, 'stop', side_effect=RuntimeError('drain failed')), patch.object(host, 'reset_private_data') as reset:
                with self.assertRaisesRegex(RuntimeError, 'drain failed'):
                    host.apply_services(cfg, directory / 'new', True)
                reset.assert_not_called()
            self.assertTrue((cfg_dir / 'pending-update.json').exists())
            self.assertEqual((cfg_dir / 'bin/control-plane').read_bytes(), b'old-cp')
            self.assertEqual(json.loads((cfg_dir / 'installed-candidate.json').read_text()), old)

    def test_reset_refuses_running_services(self):
        with patch.object(host, 'state', return_value='active'), patch.object(host.shutil, 'rmtree') as remove:
            with self.assertRaises(RuntimeError):
                host.reset_private_data(host.compile_config(config()))
            remove.assert_not_called()

    def test_persistence_stops_on_terminal_failure_without_checkpoint(self):
        for action in ['wait-parked', 'verify-restored']:
            for status in ['failed', 'system_failed', 'cancelled', 'expired']:
                row = {'run_status': status, 'run_failure': {'code': 'checkpoint_failed', 'message': 'capacity exceeded'}}
                result = subprocess.CompletedProcess([], 0, stdout=json.dumps(row))
                with self.subTest(action=action, status=status), patch.object(host, 'service_run', return_value=result) as query:
                    with self.assertRaisesRegex(RuntimeError, 'capacity exceeded'):
                        host.observe_persistence({'binaries': {'psql': '/psql'}}, '01900000-0000-7000-8000-000000000001', action)
                    self.assertEqual(query.call_count, 1)

    def test_persistence_requires_reclaimed_vm_not_just_waiting(self):
        row = {'attempt_number': 1, 'checkpoint_id': 'checkpoint', 'run_status': 'waiting',
               'condition': 'pending', 'suspension': 'parked', 'checkpoint_status': 'ready',
               'prior_runtime_state': 'closed', 'prior_runtime_reclaimed': True}
        self.assertTrue(host.persistence_matches(row, 'wait-parked'))
        for change in [{'prior_runtime_reclaimed': False}, {'prior_runtime_state': 'ready'}, {'attempt_number': 2}, {'checkpoint_status': 'creating'}]:
            self.assertFalse(host.persistence_matches(row | change, 'wait-parked'))
        self.assertFalse(host.persistence_matches(row | {'run_status': 'succeeded'}, 'verify-restored'))
        self.assertTrue(host.persistence_matches(row | {'run_status': 'succeeded', 'restored_runtime_ids': ['new-vm']}, 'verify-restored'))


class SourceInputTests(unittest.TestCase):
    def test_actual_go_dependencies_and_same_number_sql_edit(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary).resolve()
            (source / 'go.mod').write_text('module example.test/profile\n\ngo 1.27.1\n')
            (source / 'go.sum').write_text('')
            schema = source / 'internal/db/schema'
            schema.mkdir(parents=True)
            (schema / 'schema.go').write_text('package schema\nimport _ "embed"\n//go:embed 000001.sql\nvar SQL string\n')
            sql = schema / '000001.sql'
            sql.write_text('SELECT 1;')
            for name in build.COMPONENTS:
                directory = source / 'cmd' / name
                directory.mkdir(parents=True)
                imports = 'import _ "example.test/profile/internal/db/schema"\n' if name in ['control-plane', 'dispatcher'] else ''
                (directory / 'main.go').write_text('package main\n' + imports + 'func main() {}\n')
            env = dict(os.environ, GOOS='linux', GOARCH='amd64', CGO_ENABLED='0', GOWORK='off', GOFLAGS='', GOTOOLCHAIN='local')
            first = build.inputs(source, env)
            sql.write_text('SELECT 2;')
            second = build.inputs(source, env)
            self.assertNotEqual(first['schema'], second['schema'])
            self.assertNotEqual(first['control-plane'], second['control-plane'])
            self.assertNotEqual(first['dispatcher'], second['dispatcher'])
            self.assertEqual(first['worker'], second['worker'])
            self.assertEqual(first['guestd'], second['guestd'])
            for relative in ['internal/runtime/entry.mjs', 'internal/runtime/module-preload.mjs', 'internal/moduleloader/loader.mjs', 'internal/compiler/program-compiler.mjs', 'internal/version/runtime-dependencies.json']:
                path = source / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('old')
                before = build.inputs(source, env)
                path.write_text('new')
                after = build.inputs(source, env)
                self.assertNotEqual(before['runtime-support'], after['runtime-support'], relative)


if __name__ == '__main__':
    unittest.main()
