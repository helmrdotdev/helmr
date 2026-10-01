import importlib.util
import json
from pathlib import Path
import subprocess
import socket
import threading
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('runtime_host', ROOT / 'dev/runtime/host.py')
host = importlib.util.module_from_spec(spec)
spec.loader.exec_module(host)


def config():
    return {
        'binaries': {name: '/opt/bin/' + name for name in ['postgres', 'initdb', 'psql', 'redis-server', 'clickhouse']},
        'services_candidate': '/opt/candidate',
        'worker_host_receipt': '/opt/worker-host-bundle.json',
        'worker_runtime_receipt': '/opt/worker-runtime-bundle.json',
        'control_plane': {
            'CAS_URI': 's3://scope-cas', 'PLATFORM_STORE_URI': 's3://scope-platform',
            'DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH': '/opt/runtime.json',
            'GITHUB_OAUTH_CLIENT_ID': 'test-id', 'GITHUB_OAUTH_CLIENT_SECRET': 'test-secret',
            'BOOTSTRAP_WORKER_TOKEN': 'hlmr_wgt_' + 'A' * 43,
        },
        'worker': {
            'WORKER_RESOURCE_ID': 'scope-host', 'WORKER_COMPUTER_DEVICES': '/dev/nbd0 /dev/nbd1',
            'WORKER_NETWORK_LINK_POOL': '172.30.0.0/16', 'WORKER_NETWORK_TRANSLATION_POOL': '172.31.0.0/16',
            'WORKER_NETWORK_RESOLVER_IPV4': '1.1.1.1', 'WORKER_NETWORK_BLOCKED_IPV4_CIDRS': '["169.254.0.0/16"]',
        },
    }


class ProfileTests(unittest.TestCase):
    def test_abort_observation_rejects_replacement_and_unacknowledged_source(self):
        evidence = dict(attempt_number=1, acknowledged=True, source_reclaimed=False,
                        source_state='ready', other_instances=0, lease_on_source=True,
                        writer_generation=1, captured_writer_generation=1)
        self.assertTrue(host.persistence_matches(evidence, 'wait-aborted'))
        for key, value in [('acknowledged', False), ('source_reclaimed', True),
                           ('source_state', 'closed'), ('other_instances', 1),
                           ('lease_on_source', False), ('attempt_number', 2),
                           ('writer_generation', 2)]:
            with self.subTest(key=key):
                self.assertFalse(host.persistence_matches(evidence | {key: value}, 'wait-aborted'))

    def test_real_composition_preserves_native_worker_service(self):
        cfg = host.compile_config(config())
        files = host.files(cfg)
        self.assertIn(f'ExecStart={host.CONFIG}/bin/control-plane\n', files[host.unit('control-plane')])
        self.assertNotIn('dev-controlplane', ''.join(files.values()))
        self.assertNotIn('ExecStart=', files['worker-override.conf'])
        self.assertNotIn('Delegate=', files['worker-override.conf'])
        self.assertEqual(cfg['worker']['CAS_URI'], cfg['control_plane']['CAS_URI'])
        self.assertEqual(cfg['dispatcher']['COMPUTER_FENCING_KEY'], cfg['control_plane']['COMPUTER_FENCING_KEY'])
        self.assertEqual(cfg['dispatcher']['CONTROL_PLANE_URL'], cfg['worker']['CONTROL_PLANE_URL'])
        self.assertNotEqual(cfg['dispatcher']['CLICKHOUSE_USER'], cfg['control_plane']['CLICKHOUSE_USER'])

    def test_rejects_overrides_and_non_s3(self):
        for key, value in [('DATABASE_URL', 'postgres://other'), ('CAS_URI', 'file:///tmp/cas')]:
            raw = config()
            raw['control_plane'][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                host.compile_config(raw)

    def test_rejects_systemd_command_injection(self):
        for path in ['/opt/bin/cp --bad', '/opt/%i', '/opt/../bin/cp', '/opt/cp\nExecStart=/bin/sh']:
            raw = config()
            raw['binaries']['postgres'] = path
            with self.subTest(path=path), self.assertRaises(ValueError):
                host.compile_config(raw)

    def test_environment_is_data_and_rejects_multiline(self):
        self.assertEqual(host.environment({'TOKEN': 'a"b\\c$() %x'}), 'TOKEN="a\\"b\\\\c$() %x"\n')
        with self.assertRaises(ValueError):
            host.environment({'TOKEN': 'x\nINJECTED=yes'})

    def test_render_is_offline_private_and_non_overwriting(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            source = directory / 'input.json'
            source.write_text(json.dumps(config()))
            output = directory / 'render'
            command = ['python3', str(ROOT / 'dev/runtime/host.py'), 'render', '--config', str(source), '--output', str(output)]
            subprocess.run(command, check=True, capture_output=True)
            self.assertEqual(output.stat().st_mode & 0o777, 0o700)
            self.assertEqual((output / 'config.json').stat().st_mode & 0o777, 0o600)
            before = (output / 'config.json').read_bytes()
            self.assertNotEqual(subprocess.run(command, capture_output=True).returncode, 0)
            self.assertEqual(before, (output / 'config.json').read_bytes())

    def test_failed_drain_keeps_dependencies_running(self):
        with patch.object(host, 'state', return_value='active'), patch.object(host, 'run', side_effect=subprocess.CalledProcessError(1, 'drain')) as run:
            with self.assertRaises(subprocess.CalledProcessError):
                host.stop(host.compile_config(config()))
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[:4], ('/usr/local/bin/worker', 'drain', '--wait-timeout', '5m'))

    def test_failed_worker_requires_inspection_not_dependency_stop(self):
        with patch.object(host, 'state', return_value='failed'), patch.object(host, 'run') as run:
            with self.assertRaises(RuntimeError):
                host.stop(host.compile_config(config()))
            run.assert_not_called()

    def test_stop_drains_before_stopping_worker_and_control_plane(self):
        with patch.object(host, 'state', return_value='active'), patch.object(host, 'run') as run:
            host.stop(host.compile_config(config()))
            calls = [call.args for call in run.call_args_list]
            self.assertEqual(calls[0][:4], ('/usr/local/bin/worker', 'drain', '--wait-timeout', '5m'))
            self.assertEqual(calls[1], ('systemctl', 'stop', 'helmr-worker.service'))
            for call in run.call_args_list[1:]:
                self.assertGreater(call.kwargs['timeout'], 120)
            self.assertLess(calls.index(('systemctl', 'stop', host.unit('control-plane'))), calls.index(('systemctl', 'stop', host.unit('postgres'))))

    def test_start_does_not_migrate_under_live_application(self):
        with patch.object(host, 'state', side_effect=['inactive', 'active']), patch.object(host, 'run') as run:
            with self.assertRaises(RuntimeError):
                host.start(host.compile_config(config()))
            run.assert_not_called()

    def test_dead_dispatcher_prevents_success(self):
        with patch.object(host, 'state', side_effect=['active'] * 4 + ['failed']):
            with self.assertRaisesRegex(RuntimeError, 'dispatcher'):
                host.require_active_services()

    def test_redis_probe_requires_pong(self):
        for reply, expected in [(b'+PONG\r\n', True), (b'-ERR unavailable\r\n', False)]:
            client, server = socket.socketpair()
            def respond():
                with server:
                    self.assertEqual(server.recv(128), b'*1\r\n$4\r\nPING\r\n')
                    server.sendall(reply)
            thread = threading.Thread(target=respond)
            thread.start()
            with patch.object(host.socket, 'create_connection', return_value=client):
                self.assertEqual(host.redis_ready(), expected)
            thread.join(timeout=2)
            self.assertFalse(thread.is_alive())

    def test_wait_is_bounded(self):
        with patch.object(host.time, 'monotonic', side_effect=[0, 121]), patch.object(host.time, 'sleep') as sleep:
            with self.assertRaises(RuntimeError):
                host.wait_for(lambda: False, 'dependency')
            sleep.assert_not_called()

    def test_dead_service_stops_readiness_wait_without_sleep(self):
        with patch.object(host, 'state', return_value='failed'), patch.object(host.time, 'sleep') as sleep:
            with self.assertRaisesRegex(RuntimeError, 'helmr-worker.service failed'):
                host.wait_for(lambda: False, 'Worker readiness', seconds=300, unit_name='helmr-worker.service')
            sleep.assert_not_called()

    def test_live_service_can_become_ready_on_later_probe(self):
        ready = iter([False, True])
        with patch.object(host, 'state', return_value='active'), patch.object(host.time, 'sleep') as sleep:
            host.wait_for(lambda: next(ready), 'Control Plane', unit_name=host.unit('control-plane'))
            sleep.assert_called_once_with(1)


if __name__ == '__main__':
    unittest.main()
