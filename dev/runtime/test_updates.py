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

    def exercise_cp_update(self, fail_receipt=False, fail_result=False, include_dispatcher=False, fail_stop=False, fail_config=False, fail_start=False):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            cfg_dir = directory / 'config'
            cfg_dir.mkdir()
            (cfg_dir / 'bin').mkdir()
            old = candidate(directory / 'old', cp=b'old-cp')
            new = candidate(directory / 'new', dispatcher=b'new-dispatcher' if include_dispatcher else b'dispatcher')
            if include_dispatcher:
                new['inputs']['dispatcher'] = 'f' * 64
            new['source_commit'] = 'f' * 40
            (directory / 'new/candidate.json').write_text(json.dumps(new))
            for name in ['control-plane', 'dispatcher']:
                (cfg_dir / 'bin' / name).write_bytes((directory / 'old' / name).read_bytes())
            (cfg_dir / 'installed-candidate.json').write_text(json.dumps(old))
            (cfg_dir / 'data-generation.json').write_text(json.dumps({'id': 'generation', 'schema': old['inputs']['schema']}))
            (cfg_dir / 'binary-digests.json').write_text(json.dumps(old['binaries']))
            cfg = host.compile_config(config())
            cfg['binaries'].update({name: str(cfg_dir / 'bin' / name) for name in ['control-plane', 'dispatcher']})
            generation = (cfg_dir / 'data-generation.json').read_bytes()
            def identity(name):
                for component in ['control-plane'] + (['dispatcher'] if include_dispatcher else []):
                    if name == host.unit(component):
                        return {'executable_sha256': host.digest(cfg_dir / 'bin' / component)}
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
            with patch.object(host, 'write_json', side_effect=write), patch.object(host, 'CONFIG', cfg_dir), patch.object(host, 'require_active_services'), patch.object(host, 'service_identity', side_effect=identity), patch.object(host, 'http_ready', return_value=True), patch.object(host, 'state', return_value='active'), patch.object(host, 'candidate_config') as check_config, patch.object(host, 'dispatcher_started', return_value=True), patch.object(host, 'reset_private_data') as reset, patch.object(host, 'service_run') as service_run, patch.object(host, 'run') as run:
                if fail_config:
                    check_config.side_effect = RuntimeError('config invalid')
                    with self.assertRaisesRegex(RuntimeError, 'config invalid'):
                        host.apply_services(cfg, directory / 'new', False, include_dispatcher)
                elif fail_start:
                    def fail_dispatcher_start(*args, **kwargs):
                        if args == ('systemctl', 'start', host.unit('dispatcher')):
                            raise RuntimeError('start failed')
                    run.side_effect = fail_dispatcher_start
                    with self.assertRaisesRegex(RuntimeError, 'start failed'):
                        host.apply_services(cfg, directory / 'new', False, include_dispatcher)
                elif fail_stop:
                    run.side_effect = RuntimeError('stop failed')
                    with self.assertRaisesRegex(RuntimeError, 'stop failed'):
                        host.apply_services(cfg, directory / 'new', False, include_dispatcher)
                elif fail_receipt or fail_result:
                    with self.assertRaises(OSError):
                        host.apply_services(cfg, directory / 'new', False, include_dispatcher)
                else:
                    host.apply_services(cfg, directory / 'new', False, include_dispatcher)
                expected = [('systemctl', 'stop', host.unit('control-plane')), ('systemctl', 'start', host.unit('control-plane'))]
                if include_dispatcher:
                    expected = [('systemctl', 'stop', host.unit('dispatcher'))] + expected + [('systemctl', 'start', host.unit('dispatcher'))]
                self.assertEqual([c.args for c in run.call_args_list], [] if fail_config else expected[:1] if fail_stop else expected)
                check_config.assert_called_once()
                reset.assert_not_called()
                service_run.assert_not_called()
                self.assertEqual((cfg_dir / 'data-generation.json').read_bytes(), generation)
            accepted = json.loads((cfg_dir / 'installed-candidate.json').read_text())
            if fail_stop or fail_config:
                self.assertEqual(accepted, old)
                self.assertEqual((cfg_dir / 'bin/control-plane').read_bytes(), b'old-cp')
            elif fail_receipt or fail_result or fail_start:
                if fail_start:
                    self.assertEqual(accepted, old)
                    self.assertEqual(json.loads((cfg_dir / 'binary-digests.json').read_text()), old['binaries'])
                self.assertEqual((cfg_dir / 'bin/control-plane').read_bytes(), b'new-cp')
                with patch.object(host, 'CONFIG', cfg_dir), self.assertRaisesRegex(RuntimeError, 'recreate'):
                    host.apply_services(cfg, directory / 'new', False, include_dispatcher)
            else:
                self.assertEqual(accepted['component_sources']['dispatcher'], new['source_commit'] if include_dispatcher else old['source_commit'])
                self.assertEqual(accepted['component_sources']['control-plane'], new['source_commit'])
            self.assertEqual((cfg_dir / 'bin/dispatcher').read_bytes(), b'new-dispatcher' if include_dispatcher and not (fail_stop or fail_config) else b'dispatcher')
            self.assertEqual((cfg_dir / 'pending-update.json').exists(), fail_receipt or fail_result or fail_stop or fail_start)

    def test_copied_candidate_can_execute_native_config_preflight(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            cfg_dir = directory / 'config'
            cfg_dir.mkdir()
            script = b'#!/bin/sh\n[ "$1" = check-config ] || exit 17\n'
            old = candidate(directory / 'old', cp=script, dispatcher=script)
            candidate(directory / 'new', cp=script, dispatcher=script)
            for name in ['control-plane', 'dispatcher']:
                (directory / 'new' / name).chmod(0o755)
            (cfg_dir / 'installed-candidate.json').write_text(json.dumps(old))
            (cfg_dir / 'binary-digests.json').write_text(json.dumps(old['binaries']))
            (cfg_dir / 'data-generation.json').write_text(json.dumps({'id': 'generation', 'schema': old['inputs']['schema']}))
            cfg = host.compile_config(config())
            cfg['binaries'].update({name: str(directory / 'old' / name) for name in ['control-plane', 'dispatcher']})
            with patch.object(host, 'CONFIG', cfg_dir), patch.object(host, 'require_active_services'), patch.object(host, 'service_identity', return_value={'MainPID': '1'}), patch.object(host, 'write_json', side_effect=RuntimeError('preflight finished')):
                with self.assertRaisesRegex(RuntimeError, 'preflight finished'):
                    host.apply_services(cfg, directory / 'new', False, True)
            self.assertFalse((cfg_dir / 'pending-update.json').exists())

    def test_candidate_config_failure_precedes_stop_and_marker(self):
        self.exercise_cp_update(include_dispatcher=True, fail_config=True)

    def test_dispatcher_start_failure_keeps_marker_and_old_receipts(self):
        self.exercise_cp_update(include_dispatcher=True, fail_start=True)

    def test_candidate_config_uses_exact_environment_and_binaries(self):
        cfg = host.compile_config(config())
        with patch.object(host, 'run') as run:
            host.candidate_config(cfg, Path('/private/attempt'), ['control-plane', 'dispatcher'])
        for call, name, key in zip(run.call_args_list, ['control-plane', 'dispatcher'], ['control_plane', 'dispatcher']):
            self.assertEqual(call.args, ('/private/attempt/' + name, 'check-config'))
            self.assertEqual(call.kwargs['env'], {'PATH': os.environ.get('PATH', os.defpath)} | cfg[key])

    def test_dispatcher_startup_log_is_bound_to_current_invocation(self):
        identity = {'InvocationID': 'exact-invocation', 'MainPID': '9'}
        message = json.dumps({'MESSAGE': json.dumps({'msg': 'Helmr dispatcher running'})})
        with patch.object(host, 'service_identity', return_value=identity), patch.object(host, 'run', return_value=subprocess.CompletedProcess([], 0, stdout=message)) as run:
            self.assertTrue(host.dispatcher_started(identity))
            self.assertIn('_SYSTEMD_INVOCATION_ID=exact-invocation', run.call_args.args)
            run.return_value.stdout = json.dumps({'MESSAGE': 'unrelated text'})
            self.assertFalse(host.dispatcher_started(identity))
        with patch.object(host, 'service_identity', return_value=identity | {'MainPID': '10'}), patch.object(host, 'run') as run:
            with self.assertRaisesRegex(RuntimeError, 'process changed'):
                host.dispatcher_started(identity)
            run.assert_not_called()

    def test_dispatcher_update_requires_explicit_selection(self):
        old = receipt()
        changed = copy.deepcopy(old)
        changed['inputs']['dispatcher'] = 'f' * 64
        with self.assertRaisesRegex(ValueError, 'include-dispatcher'):
            host.update_kind(old, changed, False)
        self.assertEqual(host.update_kind(old, changed, False, True), ['control-plane', 'dispatcher'])
        for component in ['schema', 'worker', 'guestd', 'runtime-support', 'toolchain']:
            incompatible = copy.deepcopy(changed)
            incompatible['inputs'][component] = 'different'
            with self.subTest(component=component), self.assertRaises(ValueError):
                host.update_kind(old, incompatible, False, True)

    def test_dispatcher_update_retains_data_and_records_both_sources(self):
        self.exercise_cp_update(include_dispatcher=True)

    def test_dispatcher_receipt_failure_blocks_reuse(self):
        self.exercise_cp_update(include_dispatcher=True, fail_receipt=True)

    def test_dispatcher_stop_failure_keeps_old_binaries_and_pending_marker(self):
        self.exercise_cp_update(include_dispatcher=True, fail_stop=True)

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

    def test_persistence_stops_on_terminal_session_without_checkpoint(self):
        for action in ['wait-parked', 'verify-restored', 'wait-aborted']:
            for status in ['closed', 'cancelled']:
                row = {'session_status': status}
                result = subprocess.CompletedProcess([], 0, stdout=json.dumps(row))
                with self.subTest(action=action, status=status), patch.object(host, 'service_run', return_value=result) as query:
                    with self.assertRaisesRegex(RuntimeError, 'Session ended with ' + status):
                        host.observe_persistence({'binaries': {'psql': '/psql'}}, '01900000-0000-7000-8000-000000000001', action)
                    self.assertEqual(query.call_count, 1)

    def test_persistence_requires_fenced_source_and_confirmed_target(self):
        row = {'checkpoint_id': 'checkpoint', 'session_status': 'open', 'checkpoint_status': 'ready',
               'source_fenced': True, 'target_lease_epoch': None}
        self.assertTrue(host.persistence_matches(row, 'wait-parked'))
        for change in [{'source_fenced': False}, {'checkpoint_status': 'capturing'}, {'target_lease_epoch': 2}]:
            self.assertFalse(host.persistence_matches(row | change, 'wait-parked'))
        restored = row | {'checkpoint_status': 'consumed', 'target_lease_epoch': 2,
                          'controls_reconciled': True, 'target_current': True, 'target_runtime_id': 'new-vm'}
        self.assertTrue(host.persistence_matches(restored, 'verify-restored'))
        for change in [{'source_fenced': False}, {'controls_reconciled': False}, {'target_current': False}, {'target_runtime_id': None}, {'checkpoint_status': 'restoring'}]:
            self.assertFalse(host.persistence_matches(restored | change, 'verify-restored'))


class SourceInputTests(unittest.TestCase):
    def test_console_assets_are_inputs_only_when_actually_embedded(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary).resolve()
            (source / 'go.mod').write_text('module example.test/profile\n\ngo 1.27.1\n')
            (source / 'go.sum').write_text('')
            schema = source / 'internal/db/schema'
            schema.mkdir(parents=True)
            (schema / '000001.sql').write_text('SELECT 1;')
            assets = source / 'internal/console/out'
            assets.mkdir(parents=True)
            (assets.parent / 'console.go').write_text('//go:build !embed_console\n\npackage console\n')
            (assets.parent / 'console_embed.go').write_text(
                '//go:build embed_console\n\npackage console\nimport "embed"\n//go:embed out\nvar Files embed.FS\n')
            for name in build.COMPONENTS:
                directory = source / 'cmd' / name
                directory.mkdir(parents=True)
                imports = 'import _ "example.test/profile/internal/console"\n' if name == 'control-plane' else ''
                (directory / 'main.go').write_text('package main\n' + imports + 'func main() {}\n')
            env = dict(os.environ, GOOS='linux', GOARCH='amd64', CGO_ENABLED='0', GOWORK='off', GOFLAGS='', GOTOOLCHAIN='local')
            (assets / 'index.html').write_text('first production Console')
            api_before = build.inputs(source, env)
            console_before = build.inputs(source, env, console=True)
            (assets / 'index.html').write_text('changed production Console')
            self.assertEqual(api_before, build.inputs(source, env))
            console_after = build.inputs(source, env, console=True)
            self.assertNotEqual(console_before['control-plane'], console_after['control-plane'])
            self.assertNotEqual(api_before['control-plane'], console_after['control-plane'])
            for name in ['dispatcher', 'worker', 'guestd', 'schema', 'runtime-support', 'toolchain']:
                self.assertEqual(console_before[name], console_after[name])

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
            # Installed Console dependencies must not change the Worker reuse
            # decision; authored manifests and embedded assets still do.
            for relative in ['node_modules/vendor/package.json', 'packages/console/node_modules/vendor/package.json']:
                dependency = source / relative
                dependency.parent.mkdir(parents=True, exist_ok=True)
                dependency.write_text('generated dependency manifest')
            installed = build.inputs(source, env)
            self.assertEqual(second['runtime-support'], installed['runtime-support'])
            for relative in [
                    'internal/version/runtime-dependencies.json', 'tsconfig.json', 'bunfig.toml',
                    'bun.lock', 'package.json', 'scripts/build-platform-entries.ts', 'scripts/node-version.mjs',
                    'compiler/typescript/src/config-evaluator.ts', 'runtime/typescript/src/entry.ts',
                    'sdk/typescript/src/index.ts', 'proto/typescript/src/gen/agent_pb.ts',
                    'packages/console/package.json', 'examples/hello-world/package.json']:
                path = source / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('old')
                before = build.inputs(source, env)
                path.write_text('new')
                after = build.inputs(source, env)
                self.assertNotEqual(before['runtime-support'], after['runtime-support'], relative)


if __name__ == '__main__':
    unittest.main()
