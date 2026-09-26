from contextlib import contextmanager
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from test_profile import host, config
from test_updates import candidate


class RecoveryTests(unittest.TestCase):
    @contextmanager
    def interrupted(self, reset=False):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary); cfg_dir=root/'config';cfg_dir.mkdir();(cfg_dir/'bin').mkdir()
            old=candidate(root/'old',cp=b'old-cp');new=candidate(root/'new',cp=b'new-cp')
            new['source_commit']='f'*40;(root/'new/candidate.json').write_text(json.dumps(new))
            for name in ['control-plane','dispatcher']:
                (cfg_dir/'bin'/name).write_bytes((root/'old'/name).read_bytes())
            for filename,value in [('installed-candidate.json',old),('binary-digests.json',old['binaries']),('data-generation.json',dict(id='original-generation',schema=old['inputs']['schema']))]:
                (cfg_dir/filename).write_text(json.dumps(value))
            cfg=host.compile_config(config());cfg['binaries'].update({n:str(cfg_dir/'bin'/n) for n in ['control-plane','dispatcher']})
            def identity(name):
                if name==host.unit('control-plane'):
                    return dict(MainPID='2',InvocationID='cp',executable_sha256=host.digest(cfg_dir/'bin/control-plane'))
                return dict(MainPID='1',InvocationID=name,executable_sha256=host.digest(cfg_dir/'bin/dispatcher') if name==host.unit('dispatcher') else 'retained')
            def interrupt(*args,**kwargs):
                if args==('systemctl','start',host.unit('control-plane')): raise KeyboardInterrupt()
            with patch.object(host,'CONFIG',cfg_dir),patch.object(host,'require_active_services'),patch.object(host,'service_identity',side_effect=identity),patch.object(host,'http_ready',return_value=True),patch.object(host,'run',side_effect=interrupt):
                with self.assertRaises(KeyboardInterrupt):host.apply_services(cfg,root/'new',False)
            pending=json.loads((cfg_dir/'pending-update.json').read_text());attempt=Path(pending['attempt'])
            if reset:
                pending['reset_data']=True;(cfg_dir/'pending-update.json').write_text(json.dumps(pending))
            with patch.object(host,'CONFIG',cfg_dir),patch.object(host,'require_active_services'),patch.object(host,'service_identity',side_effect=identity),patch.object(host,'http_ready',return_value=True),patch.object(host,'run') as run:
                yield cfg_dir,attempt,cfg,old,new,run

    def test_interrupted_new_cp_restores_previous_bytes_and_metadata(self):
        with self.interrupted() as (root,attempt,cfg,old,new,run):
            update_evidence=(attempt/'result.json').read_bytes()
            host.recover_services(cfg)
            self.assertEqual((root/'bin/control-plane').read_bytes(),b'old-cp')
            self.assertEqual(json.loads((root/'installed-candidate.json').read_text()),old)
            self.assertEqual(json.loads((root/'binary-digests.json').read_text()),old['binaries'])
            self.assertFalse((root/'pending-update.json').exists())
            self.assertEqual((attempt/'result.json').read_bytes(),update_evidence)
            self.assertEqual([c.args for c in run.call_args_list],[('systemctl','stop',host.unit('control-plane')),('systemctl','start',host.unit('control-plane'))])

    def test_partial_receipt_commit_restores_original_identities(self):
        with self.interrupted() as (root,attempt,cfg,old,new,run):
            (root/'installed-candidate.json').write_text(json.dumps(new))
            host.recover_services(cfg)
            self.assertEqual(json.loads((root/'installed-candidate.json').read_text()),old)

    def test_missing_or_changed_evidence_blocks_before_mutation(self):
        for kind in ['snapshot','binary','generation','dispatcher','unknown-cp','processes','candidate']:
            with self.subTest(kind=kind),self.interrupted() as (root,attempt,cfg,old,new,run):
                if kind=='snapshot': (attempt/'previous-state.json').unlink()
                if kind=='binary': (attempt/'previous-control-plane').write_bytes(b'tampered')
                if kind=='generation': (root/'data-generation.json').write_text(json.dumps(dict(id='other',schema=old['inputs']['schema'])))
                if kind=='dispatcher': (root/'bin/dispatcher').write_bytes(b'tampered')
                if kind=='unknown-cp': (root/'bin/control-plane').write_bytes(b'tampered')
                if kind in ['processes','candidate']:
                    p=root/'pending-update.json';v=json.loads(p.read_text());v['before' if kind=='processes' else 'candidate']={} if kind=='processes' else '0'*40;p.write_text(json.dumps(v))
                with self.assertRaises((ValueError,RuntimeError,OSError)):host.recover_services(cfg)
                run.assert_not_called();self.assertTrue((root/'pending-update.json').exists())

    def test_reset_is_never_rolled_back(self):
        with self.interrupted(reset=True) as (root,attempt,cfg,old,new,run):
            with self.assertRaisesRegex(RuntimeError,'data reset'):host.recover_services(cfg)
            run.assert_not_called();self.assertTrue((root/'pending-update.json').exists())

    def test_interrupted_recovery_can_be_repeated_without_erasing_evidence(self):
        with self.interrupted() as (root,attempt,cfg,old,new,run):
            def interrupt(*args,**kwargs):
                if args==('systemctl','start',host.unit('control-plane')):raise KeyboardInterrupt()
            run.side_effect=interrupt
            with self.assertRaises(KeyboardInterrupt):host.recover_services(cfg)
            self.assertTrue((root/'pending-update.json').exists())
            first=list(attempt.glob('recovery-*.json'));self.assertEqual(len(first),1)
            prior=first[0].read_bytes()
            run.side_effect=None;host.recover_services(cfg)
            self.assertEqual(first[0].read_bytes(),prior)
            self.assertEqual(len(list(attempt.glob('recovery-*.json'))),2)

    def test_readiness_failure_keeps_pending_and_failure_evidence(self):
        with self.interrupted() as (root,attempt,cfg,old,new,run):
            with patch.object(host,'wait_for',side_effect=RuntimeError('not ready')):
                with self.assertRaises(RuntimeError):host.recover_services(cfg)
            self.assertTrue((root/'pending-update.json').exists())
            evidence=json.loads(next(attempt.glob('recovery-*.json')).read_text())
            self.assertEqual(evidence['phase'],'recovery-failed')


if __name__=='__main__':unittest.main()
