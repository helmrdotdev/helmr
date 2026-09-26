from contextlib import contextmanager
import json
import os
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_profile import host, config, ROOT
from test_updates import candidate


class ResetResumeTests(unittest.TestCase):
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
            self.assertEqual(host.supervise_reset(['resume-reset']), 1)
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

    def test_resume_does_not_accept_a_replacement_candidate(self):
        result=subprocess.run([sys.executable,str(ROOT/'dev/verification/runtime-host.py'),
                               'resume-reset','--candidate','/unrelated'],capture_output=True,text=True)
        self.assertEqual(result.returncode,2)
        self.assertIn('--candidate is only valid',result.stderr)


    def test_restart_reset_after_each_mutating_phase(self):
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
                pending=json.loads((directory/'pending-update.json').read_text());attempt=Path(pending['attempt'])
                initial=(attempt/'result.json').read_bytes()
                self.assertTrue((directory/'pending-update.json').exists())
                if selected:selected.side_effect=None
                host.resume_reset(cfg)
                self.assertFalse((directory/'pending-update.json').exists())
                self.assertEqual(json.loads((directory/'data-generation.json').read_text()),pending['reset_generation'])
                self.assertEqual(json.loads((directory/'installed-candidate.json').read_text())['source_commit'],new['source_commit'])
                for name in ['control-plane','dispatcher']:self.assertEqual(host.digest(directory/'bin'/name),new['binaries'][name])
                self.assertEqual((attempt/'result.json').read_bytes(),initial)
                result=json.loads(next(attempt.glob('reset-retry-*.json')).read_text())
                self.assertEqual(result['phase'],'services-ready-fixtures-required')

    def test_repeated_interruption_keeps_same_candidate_generation_and_evidence(self):
        with self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
            erase.side_effect=KeyboardInterrupt()
            with self.assertRaises(KeyboardInterrupt):host.apply_services(cfg,root/'new',True)
            pending=(directory/'pending-update.json').read_bytes()
            for _ in range(2):
                with self.assertRaises(KeyboardInterrupt):host.resume_reset(cfg)
                self.assertEqual((directory/'pending-update.json').read_bytes(),pending)
            attempt=Path(json.loads(pending)['attempt'])
            self.assertEqual(len(list(attempt.glob('reset-retry-*.json'))),2)
            erase.side_effect=None;host.resume_reset(cfg)
            self.assertEqual(len(list(attempt.glob('reset-retry-*.json'))),3)

    def test_failed_new_worker_can_stop_only_after_old_quiescence(self):
        with self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
            start.side_effect=KeyboardInterrupt()
            with self.assertRaises(KeyboardInterrupt):host.apply_services(cfg,root/'new',True)
            calls=0
            def statuses(name):
                nonlocal calls
                calls+=1
                return 'failed' if calls==1 else 'inactive'
            state.side_effect=statuses;start.side_effect=None;stop.reset_mock()
            host.resume_reset(cfg)
            stop.assert_not_called()
            self.assertEqual(run.call_args_list[0].args,('systemctl','stop','helmr-worker.service'))

    def test_active_worker_drain_failure_never_forces_erasure(self):
        with self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
            start.side_effect=KeyboardInterrupt()
            with self.assertRaises(KeyboardInterrupt):host.apply_services(cfg,root/'new',True)
            state.return_value='active';stop.side_effect=RuntimeError('drain failure');erase.reset_mock()
            with self.assertRaises(RuntimeError):host.resume_reset(cfg)
            erase.assert_not_called();run.assert_not_called()
            self.assertTrue((directory/'pending-update.json').exists())

    def test_foreign_candidate_generation_or_corrupt_marker_blocks_before_mutation(self):
        for kind in ['candidate','generation','marker','missing-marker','binary','wrong-mode']:
            with self.subTest(kind=kind),self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
                erase.side_effect=KeyboardInterrupt()
                with self.assertRaises(KeyboardInterrupt):host.apply_services(cfg,root/'new',True)
                path=directory/'pending-update.json';pending=json.loads(path.read_text());attempt=Path(pending['attempt'])
                if kind=='candidate':pending['candidate']='0'*40;path.write_text(json.dumps(pending))
                if kind=='wrong-mode':pending['reset_data']=False;path.write_text(json.dumps(pending))
                if kind=='generation':(directory/'data-generation.json').write_text(json.dumps(dict(id='foreign',schema=new['inputs']['schema'])))
                if kind=='marker':(attempt/'reset-quiesced.json').write_text('{}')
                if kind=='missing-marker':(attempt/'reset-quiesced.json').unlink()
                if kind=='binary':(attempt/'dispatcher').write_bytes(b'tampered')
                stop.reset_mock();erase.reset_mock();erase.side_effect=None
                with self.assertRaises((ValueError,RuntimeError)):host.resume_reset(cfg)
                stop.assert_not_called();erase.assert_not_called();run.assert_not_called()

    def test_mount_or_device_refusal_retains_pending(self):
        with self.environment() as (root,directory,cfg,new,state,stop,erase,start,run):
            erase.side_effect=RuntimeError('device remains connected')
            with self.assertRaises(RuntimeError):host.apply_services(cfg,root/'new',True)
            with self.assertRaises(RuntimeError):host.resume_reset(cfg)
            self.assertTrue((directory/'pending-update.json').exists());start.assert_not_called()


if __name__=='__main__':unittest.main()
