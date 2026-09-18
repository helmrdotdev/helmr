"""Actual shell entrypoint; signature and final store call are local test boundaries."""
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1]/'scripts/release'))
from contract import ASSETS, canonical, descriptor

ROOT = Path(__file__).resolve().parents[1]

class Platform(unittest.TestCase):
    def test_indexed_platform_entrypoint(self):
        for case in ('stable','preview','signature','archive','provenance','source','workflow','ref','tag','unsafe'):
            with self.subTest(case=case),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);repo=root/'repo';(repo/'scripts').mkdir(parents=True);binpath=root/'bin';binpath.mkdir()
                shutil.copytree(ROOT/'scripts/release',repo/'scripts/release',ignore=shutil.ignore_patterns('__pycache__'))
                shutil.copyfile(ROOT/'scripts/publish-platform-release.sh',repo/'scripts/publish-platform-release.sh')
                stub=repo/'scripts/publish-materialized-platform-release.sh'
                stub.write_text('#!/bin/sh\n[ -f "$2/platform-release.json" ] || exit 1\nprintf published > "$EVENTS"\n');stub.chmod(0o755)
                source='a'*40;version='v0.1.0-preview.gaaaaaaaa.b1' if case=='preview' else 'v0.1.0'
                # Repository state is stubbed: no source commit/tag mutation needed.
                (binpath/'git').write_text('#!/bin/sh\ncase "$*" in *status*) exit 0;; *check-ref*) exit 0;; *refs/tags*) printf "%s\\n" "${TAG_SOURCE}";; *) printf "%s\\n" "'+source+'";; esac\n');(binpath/'git').chmod(0o755)
                signer='https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@'+('refs/heads/main' if case=='preview' else 'refs/tags/'+version)
                (binpath/'cosign').write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$COSIGN_ARGS"\n[ "${FAIL_SIGNATURE}" != 1 ]\n');(binpath/'cosign').chmod(0o755)
                archive=root/'platform-release.tar'
                with tarfile.open(archive,'w') as t:
                    item=tarfile.TarInfo('../escape' if case=='unsafe' else 'platform-release.json');item.size=2;t.addfile(item,io.BytesIO(b'{}'))
                ref='refs/heads/main' if case=='preview' else 'refs/tags/'+version
                provenance=root/'platform-release-provenance.json';provenance.write_bytes(canonical(dict(formatVersion=0,sourceCommit=source,sourceRef='refs/heads/foreign' if case=='ref' else ref,archive=descriptor(archive))))
                index=dict(schema='helmr.release.v0',version=version,sourceCommit='b'*40 if case=='source' else source,sourceRef=ref,build=dict(runId='1',workflowCommit='c'*40,workflowRef='refs/heads/foreign' if case=='workflow' else signer.split('@')[1],ciRun='1',pr=None,mode='main' if case=='preview' else 'tag'),assets={n:dict(digest='sha256:'+'0'*64,sizeBytes=1) for n in ASSETS})
                for p in (archive,provenance):index['assets'][p.name]=descriptor(p)
                indexpath=root/'release-index.json';indexpath.write_bytes(canonical(index));sig=root/'release-index.sigstore.json';sig.write_text('fixture only')
                if case=='archive':archive.write_bytes(b'tamper')
                if case=='provenance':provenance.write_bytes(b'tamper')
                events=root/'events';args=root/'cosign-args'
                env=dict(os.environ,PATH=str(binpath)+os.pathsep+os.environ['PATH'],EVENTS=str(events),COSIGN_ARGS=str(args),FAIL_SIGNATURE='1' if case=='signature' else '0',TAG_SOURCE='b'*40 if case=='tag' else source,PYTHONDONTWRITEBYTECODE='1')
                result=subprocess.run(['bash',repo/'scripts/publish-platform-release.sh','s3://fixture.invalid/store',version,indexpath,sig,archive,provenance],env=env,capture_output=True,text=True)
                if case in ('stable','preview'):
                    self.assertEqual(result.returncode,0,result.stderr);self.assertEqual(events.read_text(),'published')
                    argv=args.read_text().splitlines();self.assertEqual(argv[argv.index('--certificate-identity')+1],signer);self.assertEqual(argv[-1],str(indexpath))
                else:self.assertNotEqual(result.returncode,0);self.assertFalse(events.exists())
                self.assertFalse((root/'escape').exists())

if __name__ == "__main__": unittest.main()
