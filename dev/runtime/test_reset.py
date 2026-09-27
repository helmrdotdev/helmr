from contextlib import contextmanager
import json
import os
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_host import host, config, ROOT
from test_updates import candidate


class ResetTests(unittest.TestCase):
    @contextmanager
    def environment(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);cfg_dir=root/'config';cfg_dir.mkdir();(cfg_dir/'bin').mkdir()
            old=candidate(root/'old',cp=b'old');new=candidate(root/'new',cp=b'new',dispatcher=b'new-dispatcher')
            new['source_commit']='f'*40;new['inputs']['schema']='e'*64
            (root/'new/candidate.json').write_text(json.dumps(new))
            generation=dict(id='old-generation',schema=old['inputs']['schema'])
            for name in ['control-plane','dispatcher']:(cfg_dir/'bin'/name).write_bytes((root/'old'/name).read_bytes())
            for name,value in [('installed-candidate.json',old),('binary-digests.json',old['binaries']),('data-generation.json',generation)]:
                (cfg_dir/name).write_text(json.dumps(value))
            cfg=host.compile_config(config());cfg['binaries'].update({n:str(cfg_dir/'bin'/n) for n in ['control-plane','dispatcher']})
            def identity(unit):
                name='dispatcher' if 'dispatcher' in unit else 'control-plane'
                return dict(MainPID='1',InvocationID=unit,executable_sha256=host.digest(cfg_dir/'bin'/name))
            with patch.object(host,'CONFIG',cfg_dir),patch.object(host,'require_active_services'),patch.object(host,'state',return_value='inactive') as state,patch.object(host,'service_identity',side_effect=identity),patch.object(host,'stop') as stop,patch.object(host,'reset_private_data') as erase,patch.object(host,'start') as start,patch.object(host,'run') as run:
                yield root,cfg_dir,cfg,new,state,stop,erase,start,run

    def test_native_boundary_propagates_failure_without_direct_retry(self):
        with patch.object(host.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1)) as run:
            self.assertEqual(host.supervise_reset(['apply-services', '--reset-data', '--candidate', '/private/candidate']), 1)
            self.assertEqual(run.call_count, 1)
            command=run.call_args.args[0]
            for required in ['--unit=helmr-verification-reset.service', '--property=ExitType=cgroup',
                             '--property=KillMode=control-group', '--property=SendSIGKILL=yes', '--wait']:
                self.assertIn(required, command)
            self.assertEqual(command[-1], '--reset-runner')

    def test_internal_runner_requires_actual_service_main_identity(self):
        with patch.dict(os.environ, {'INVOCATION_ID':'invocation'}):
            for output in [f'MainPID={os.getpid()+1}\nInvocationID=invocation',f'MainPID={os.getpid()}\nInvocationID=foreign']:
                with patch.object(host,'run',return_value=subprocess.CompletedProcess([],0,stdout=output)):
                    with self.assertRaises(RuntimeError):host.require_reset_runner()

    def test_successful_reset_installs_schema_and_invalidates_old_data(self):
        with self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
            host.apply_services(cfg,root/'new',True)
            stop.assert_called_once(); erase.assert_called_once(); start.assert_called_once()
            self.assertFalse((directory/'pending-update.json').exists())
            generation=json.loads((directory/'data-generation.json').read_text())
            self.assertNotEqual(generation['id'],'old-generation')
            self.assertEqual(generation['schema'],new['inputs']['schema'])
            self.assertEqual(json.loads((directory/'installed-candidate.json').read_text())['source_commit'],new['source_commit'])

    def test_final_result_failure_keeps_incomplete_marker(self):
        with self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
            original=host.write_json
            def write(path,value):
                if path.name=='result.json' and value.get('phase')=='services-ready-fixtures-required':
                    raise OSError('result write failed')
                original(path,value)
            with patch.object(host,'write_json',side_effect=write),self.assertRaises(OSError):
                host.apply_services(cfg,root/'new',True)
            self.assertTrue((directory/'pending-update.json').exists())

    def test_interrupted_reset_requires_recreation_and_never_restarts_itself(self):
        for point in ['drain','erase','start','receipts']:
            with self.subTest(point=point),self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
                original=host.write_json
                def write(path,value):
                    if point=='receipts' and path.name=='binary-digests.json':raise KeyboardInterrupt()
                    original(path,value)
                selected={'drain':stop,'erase':erase,'start':start}.get(point)
                if selected:selected.side_effect=KeyboardInterrupt()
                with patch.object(host,'write_json',side_effect=write):
                    with self.assertRaises(KeyboardInterrupt):host.apply_services(cfg,root/'new',True)
                self.assertTrue((directory/'pending-update.json').exists())
                stop.reset_mock();erase.reset_mock();start.reset_mock()
                with self.assertRaisesRegex(RuntimeError,'recreate'):
                    host.apply_services(cfg,root/'new',True)
                stop.assert_not_called();erase.assert_not_called();start.assert_not_called()


if __name__=='__main__':unittest.main()
