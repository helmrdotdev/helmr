import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_profile import host
from test_updates import candidate


class InspectionTests(unittest.TestCase):
    def inspect(self, change=lambda root, installed: None, running=None, service='active'):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            installed = candidate(root/'bin')
            (root/'installed-candidate.json').write_text(json.dumps(installed))
            (root/'binary-digests.json').write_text(json.dumps(installed['binaries']))
            (root/'data-generation.json').write_text(json.dumps({'id': '12345678-1234-4234-8234-123456789012', 'schema': installed['inputs']['schema']}))
            (root/'config.json').write_text('secret-not-for-evidence')
            change(root, installed)
            before = {str(p): p.read_bytes() for p in root.rglob('*') if p.is_file()}
            def identity(unit):
                name = 'control-plane' if 'control-plane' in unit else 'dispatcher'
                return dict(MainPID='123', InvocationID='native-invocation', executable_sha256=running or installed['binaries'][name])
            with patch.object(host, 'CONFIG', root), patch.object(host, 'state', return_value=service), patch.object(host, 'service_identity', side_effect=identity):
                result = host.inspect_profile({'binaries': {n: str(root/'bin'/n) for n in ['control-plane','dispatcher']}})
            self.assertEqual(before, {str(p): p.read_bytes() for p in root.rglob('*') if p.is_file()})
            self.assertNotIn('secret-not-for-evidence', json.dumps(result))
            return result

    def test_observes_but_does_not_admit_or_mutate(self):
        value = self.inspect()
        self.assertEqual(value['status'], 'observed')
        self.assertEqual(value['blockers'], [])
        self.assertNotIn('ready', value)
        self.assertEqual(value['components']['dispatcher']['source_commit'], 'a'*40)

    def test_distinguishes_running_and_installed_bytes(self):
        value = self.inspect(running='f'*64)
        self.assertIn('control-plane: running bytes differ', value['blockers'])
        def tamper(root, installed): (root/'bin/control-plane').write_bytes(b'changed')
        value = self.inspect(tamper)
        self.assertIn('control-plane: installed bytes differ', value['blockers'])

    def test_corrupt_missing_and_schema_mismatch_block(self):
        for name in ['installed-candidate.json', 'binary-digests.json', 'data-generation.json']:
            for payload in [None, 'null', '{}', 'invalid']:
                def change(root, installed):
                    p=root/name
                    if payload is None: p.unlink()
                    else: p.write_text(payload)
                with self.subTest(name=name,payload=payload):
                    self.assertEqual(self.inspect(change)['status'], 'blocked')
        def schema(root, installed):
            p=root/'data-generation.json';v=json.loads(p.read_text());v['schema']='f'*64;p.write_text(json.dumps(v))
        self.assertIn('data generation schema differs from installed candidate', self.inspect(schema)['blockers'])

    def test_pending_even_success_result_blocks_and_redacts_failure(self):
        for result_phase in ['service-ready', 'update-failed', None]:
            def pending(root, installed):
                attempt=root/'updates/12345678-1234-4234-8234-123456789012';attempt.mkdir(parents=True)
                (root/'pending-update.json').write_text(json.dumps(dict(attempt=str(attempt),candidate='b'*40)))
                if result_phase:
                    (attempt/'result.json').write_text(json.dumps(dict(phase=result_phase,failure='private-token-value')))
            value=self.inspect(pending)
            self.assertEqual(value['status'],'blocked')
            self.assertNotIn('private-token-value',json.dumps(value))
            self.assertEqual(value['pending_update']['result_phase'], result_phase or 'unknown')

    def test_malformed_pending_and_foreign_attempt_never_resume(self):
        for payload in ['null', '{}', json.dumps(dict(attempt='/tmp/12345678-1234-4234-8234-123456789012',candidate='b'*40))]:
            self.assertEqual(self.inspect(lambda root, installed: (root/'pending-update.json').write_text(payload))['status'], 'blocked')

    def test_inactive_services_are_not_ready(self):
        value=self.inspect(service='inactive')
        self.assertEqual(value['status'],'blocked')
        self.assertNotIn('running_sha256', value['components']['control-plane'])

    def test_retained_dispatcher_source_is_not_restamped(self):
        def sources(root, installed):
            installed['component_sources']={'control-plane':'b'*40,'dispatcher':'a'*40}
            installed['source_commit']='b'*40
            (root/'installed-candidate.json').write_text(json.dumps(installed))
        value=self.inspect(sources)
        self.assertEqual(value['components']['dispatcher']['source_commit'],'a'*40)
        self.assertEqual(value['components']['control-plane']['source_commit'],'b'*40)


if __name__ == '__main__': unittest.main()
