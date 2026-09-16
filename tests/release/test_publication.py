"""Real helper transitions with an in-memory native release surface; no service writes."""
import copy
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile
import publish
import transport
from contract import canonical, digest, read, write
from transport import freeze, restore
from test_contract import assets, selection
import test_contract

class Releases:
    def __init__(self):
        self.releases=[];self.blobs={};self.writes=[];self.fail=None
    def request(self,path,*,data=None,method=None,destination=None,missing=False):
        if path.startswith('releases/tags/'):
            return next((r for r in self.releases if r['tag_name']==path[14:]),None)
        if path=='releases' and data:
            r=dict({'draft':False,**data},id=len(self.releases)+1,upload_url='upload/'+str(len(self.releases)+1));self.releases.append(r);return r
        if path.startswith('upload/'):
            rid=int(path.split('/')[1].split('?')[0]);name=data.name
            self.writes.append(name)
            if self.fail==name:self.fail=None;raise OSError('interrupted upload')
            self.blobs[rid,name]=data.read_bytes();return {}
        if path.startswith('releases/assets/'):
            key=json.loads(path.removeprefix('releases/assets/'))
            Path(destination).write_bytes(self.blobs[tuple(key)]);return
        if method=='PATCH':
            if self.fail=='pointer':self.fail=None;raise OSError('discovery unavailable')
            r=next(r for r in self.releases if r['id']==int(path.split('/')[1]));r.update(data);return r
        raise AssertionError(path)
    def pages(self,path,*_):
        if path=='releases':return iter(self.releases)
        rid=int(path.split('/')[1])
        return iter(dict(id=json.dumps([r,n]),name=n) for r,n in self.blobs if r==rid)


def fake_sign(path):
    p=Path(str(path).removesuffix('.json')+'.sigstore.json');p.write_text(digest(path));return p

def fake_verify(path,signature,version):
    if Path(signature).read_text()!=digest(path):raise ValueError('bad fixture signature')

class Publication(unittest.TestCase):
    def test_interrupted_preview_pair_reuses_original_attempt_and_rejects_changed_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);s=selection();assets(root);api=Releases();api.fail='release-build.sigstore.json'
            with patch.object(publish,'recheck_pr'),patch.object(publish,'publish_images'),patch.object(publish,'npm_publish'),patch.object(publish,'sign',side_effect=fake_sign),patch.object(publish,'verify_signature',side_effect=fake_verify):
                with self.assertRaises(OSError):publish.stage(api,s,root)
                frozen=api.blobs[1,'release-build.json']
                retry=copy.deepcopy(s);retry['build']['attempt']='2'
                # Fresh publishing job receives the original frozen components.
                (root/'release-build.sigstore.json').unlink()
                result=publish.stage(api,retry,root)
                self.assertEqual(result['build']['attempt'],'1')
                self.assertEqual(api.blobs[1,'release-build.json'],frozen)
                (root/'helmr-linux-amd64.tar.gz').write_bytes(b'changed')
                (root/'old-build.json').unlink()
                (root/'checksums.txt').write_bytes(test_contract.cli_checksums(root))
                with self.assertRaisesRegex(ValueError,'retry built different bytes'):publish.stage(api,retry,root)

    def test_final_index_last_and_interrupted_completion_retry(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);s=selection();index=assets(root);api=Releases()
            r=api.request('releases',data=dict(tag_name=s['version'],draft=True))
            api.blobs[1,'release-build.json']=canonical(index);api.blobs[1,'release-build.sigstore.json']='fixture'.encode()
            def download(_api,_selection,directory):Path(directory).mkdir();return index
            with patch.object(publish,'download_build',side_effect=download),patch.object(publish,'sign',side_effect=fake_sign),patch.object(publish,'verify_signature',side_effect=fake_verify):
                api.fail='release-index.json'
                with self.assertRaises(OSError):publish.finalize(api,s,root/'first')
                self.assertTrue(r['draft']);self.assertNotIn((1,'release-index.json'),api.blobs)
                signature=api.blobs[1,'release-index.sigstore.json']
                publish.finalize(api,s,root/'second')
                self.assertFalse(r['draft']);self.assertEqual(api.writes[-1],'release-index.json')
                self.assertEqual(api.blobs[1,'release-index.sigstore.json'],signature)

    def test_npm_retry_compares_bytes_and_human_tag_never_resumes(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'sdk.tgz';p.write_bytes(b'original')
            metadata=json.dumps(dict(dist=dict(tarball='https://registry.npmjs.org/sdk.tgz'))).encode()
            for mode,remote,success in [('main',b'original',True),('pr',b'changed',False),('tag',b'original',False)]:
                with patch.object(publish.urllib.request,'urlopen',side_effect=[io.BytesIO(metadata),io.BytesIO(remote)]),patch.object(publish,'run') as command:
                    if success:publish.npm_publish(p,'@helmr/sdk','0.1.0',mode)
                    else:
                        with self.assertRaises(ValueError):publish.npm_publish(p,'@helmr/sdk','0.1.0',mode)
                    command.assert_not_called()

    def test_native_artifact_retry_and_corruption(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);built=root/'built';built.mkdir();(built/'sdk.tgz').write_bytes(b'sdk');(built/'proto.tgz').write_bytes(b'proto')
            freeze('sdk',built,selection(),root/'frozen')
            z=root/'native.zip'
            with zipfile.ZipFile(z,'w') as archive:
                for p in (root/'frozen').iterdir():archive.write(p,p.name)
            item=dict(id=4,name='common-123-sdk',expired=False,workflow_run=dict(id=123),digest=digest(z))
            class API:
                def pages(self,*_):return [item]
                def request(self,path,*,destination):shutil.copyfile(z,destination)
            retry=selection();retry['build']['attempt']='2'
            self.assertTrue(restore(API(),'sdk',retry,root/'restored'))
            self.assertEqual((root/'restored/sdk.tgz').read_bytes(),b'sdk')
            extract = transport.safe_extract
            def damaged_extract(archive, destination):
                extract(archive, destination)
                (Path(destination)/'sdk.tgz').write_bytes(b'local extraction corruption')
            with patch.object(transport,'safe_extract',side_effect=damaged_extract):
                with self.assertRaisesRegex(ValueError,'frozen part bytes differ'):restore(API(),'sdk',retry,root/'damaged')
            item['digest']='sha256:'+'0'*64
            with self.assertRaisesRegex(ValueError,'ZIP digest'):restore(API(),'sdk',retry,root/'bad')

class DiscoverySurface(test_contract.RepositoryFixture, unittest.TestCase):
    def test_actual_serial_updater_reconciles_replaced_pending_and_failure_retry(self):
        self.git('branch','-M','main');self.git('remote','add','origin',str(self.root))
        api=Releases()
        # Late A's updater replaces pending B. Invoking run is deliberately absent
        # from discover's interface; it reads every completed signed release.
        for source,run in [(self.b,2),(self.a,1)]:
            index=self.index(source,run);index['version']='v0.1.0-preview.g'+source[:8]+'.b'+str(run)
            r=api.request('releases',data=dict(tag_name=index['version'],draft=False))
            api.blobs[r['id'],'release-index.json']=canonical(index)
        def fetch(_api,release,directory):
            Path(directory).mkdir();raw=api.blobs[release['id'],'release-index.json'];(Path(directory)/'release-index.json').write_bytes(raw);return json.loads(raw)
        with patch.object(publish,'fetch_index',side_effect=fetch):
            result=publish.discover(api,self.root);self.assertEqual(result['sourceCommit'],self.b)
            pointer=api.request('releases/tags/preview');original=pointer['body']
            api.fail='pointer'
            with self.assertRaises(OSError):publish.discover(api,self.root)
            self.assertEqual(pointer['body'],original)
            self.assertEqual(publish.discover(api,self.root),result)
            # Same version/tag but different signed bytes cannot match old pointer.
            record=json.loads(pointer['body']);record['indexDigest']='sha256:'+'0'*64;pointer['body']=json.dumps(record)
            with self.assertRaisesRegex(ValueError,'signed bytes'):publish.discover(api,self.root)
