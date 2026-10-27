"""Real helper transitions with an in-memory native release surface; no service writes."""
import copy
import hashlib
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
from contract import canonical, descriptor, digest, read, verify_files, write
from transport import freeze, restore
from test_contract import assets, selection
import test_contract

class Releases:
    """Draft visibility is publisher-only; tag lookup is published-only for all roles."""
    def __init__(self):
        self.releases=[];self.blobs={};self.writes=[];self.fail=None
        self.writer=True;self.asset_keys={}
    def visible(self, release):
        return self.writer or not release['draft']
    def request(self,path,*,data=None,method=None,destination=None,missing=False,accept='application/vnd.github+json'):
        if path.startswith('releases/tags/'):
            release=next((r for r in self.releases if r['tag_name']==path[14:] and not r['draft']),None)
            if release is None and not missing:raise ValueError('GitHub request failed: HTTP 404')
            return release
        if data is not None:
            if not self.writer:raise ValueError('GitHub request failed: HTTP 403')
            if path=='releases':
                r=dict({'draft':False,**data},id=len(self.releases)+1,upload_url='upload/'+str(len(self.releases)+1));self.releases.append(r);return r
            if path.startswith('upload/'):
                rid=int(path.split('/')[1].split('?')[0]);name=data.name
                self.writes.append(name)
                if self.fail==name:self.fail=None;raise OSError('interrupted upload')
                self.blobs[rid,name]=data.read_bytes();return {}
            if method=='PATCH':
                self.writes.append('PATCH')
                if self.fail=='pointer':self.fail=None;raise OSError('discovery unavailable')
                r=next(r for r in self.releases if r['id']==int(path.split('/')[1]));r.update(data)
                if self.fail=='response':self.fail=None;raise OSError('PATCH response lost')
                return r
        if path.startswith('releases/assets/'):
            key=self.asset_keys[int(path.removeprefix('releases/assets/'))]
            self.request('releases/'+str(key[0]))
            with Path(destination).open('xb') as out:out.write(self.blobs[key])
            return
        if path.startswith('releases/'):
            release=next((r for r in self.releases if str(r['id'])==path.split('/')[1] and self.visible(r)),None)
            if release is None:raise ValueError('GitHub request failed: HTTP 404')
            return release
        raise AssertionError(path)
    def pages(self,path,*_):
        if path=='releases':return iter(r for r in self.releases if self.visible(r))
        rid=int(path.split('/')[1]);self.request('releases/'+str(rid))
        for key in self.blobs:
            if key not in self.asset_keys.values():self.asset_keys[len(self.asset_keys)+1]=key
        return iter(dict(id=identifier,name=key[1]) for identifier,key in self.asset_keys.items() if key[0]==rid)


def seed_draft(root):
    selected = selection('tag')
    index = assets(root, selected)
    api = Releases()
    release=api.request('releases',data=dict(tag_name=index['version'],target_commitish=index['sourceCommit'],draft=True))
    write(root/'release-build.json',index);fake_sign(root/'release-build.json')
    for name in (*index['assets'],'release-build.json','release-build.sigstore.json'):
        api.blobs[release['id'],name]=(root/name).read_bytes()
    return api,release,index


def public_download(api, url, **_):
    version,name=url.rsplit('/',2)[-2:]
    release=api.request('releases/tags/'+version)
    response=io.BytesIO(api.blobs[release['id'],name]);response.url=url
    return response


def fake_sign(path):
    p=Path(str(path).removesuffix('.json')+'.sigstore.json');p.write_text(digest(path));return p

def fake_verify(path,signature,version):
    if Path(signature).read_text()!=digest(path):raise ValueError('bad fixture signature')

class Publication(unittest.TestCase):
    def test_tag_releases_request_generated_notes_while_still_drafts(self):
        for version, prerelease in (('v0.2.0-rc.1', True), ('v0.2.0', False)):
            with self.subTest(version=version), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                selected = selection('tag')
                selected['version'] = version
                selected['sourceRef'] = selected['build']['workflowRef'] = 'refs/tags/' + version
                assets(root, selected)
                api = Releases()
                with patch.object(publish, 'publish_images'), patch.object(publish, 'npm_publish'), \
                     patch.object(publish, 'sign', side_effect=fake_sign):
                    release = publish.stage(api, selected, root)
                self.assertTrue(release['generate_release_notes'])
                self.assertEqual(release['name'], version)
                self.assertTrue(release['draft'])
                self.assertEqual(release['prerelease'], prerelease)
                self.assertEqual(release['make_latest'], 'false')

    def test_flat_product_image_publication_and_foreign_destination_rejection(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            index = assets(root)
            manifest = b'fixture OCI manifest bytes'
            image_digest = 'sha256:' + hashlib.sha256(manifest).hexdigest()
            images = (('bundle-builder.json', 'bundle-builder', 'builder-image'),
                      ('controlplane.json', 'control-plane', 'controlplane-image'))
            for name, repository, _ in images:
                record = read(root / name)
                record['image'] = f'ghcr.io/helmrdotdev/{repository}@{image_digest}'
                write(root / name, record)
                index['assets'][name] = descriptor(root / name)
            verify_files(index, root)
            absent = subprocess.CompletedProcess([], 1, b'', b'manifest unknown')
            with patch.object(publish.subprocess, 'run', return_value=absent) as inspect, \
                    patch.object(publish, 'run') as copy_image, \
                    patch.object(publish.subprocess, 'check_output', return_value=manifest) as readback:
                publish.publish_images(root, index)
                self.assertEqual(copy_image.call_count, 2)
                for _, repository, image_dir in images:
                    base = f'ghcr.io/helmrdotdev/{repository}'
                    inspect.assert_any_call(['skopeo', 'inspect', '--raw', 'docker://' + base + ':' + index['version']], capture_output=True)
                    copy_image.assert_any_call('skopeo', '--insecure-policy', 'copy', '--preserve-digests', 'dir:' + str(root / image_dir), 'docker://' + base + ':' + index['version'])
                    readback.assert_any_call(['skopeo', 'inspect', '--raw', 'docker://' + base + '@' + image_digest])
            for name, repository, _ in images:
                rejected = (
                    f'ghcr.io/helmrdotdev/helmr/{repository}',
                    f'ghcr.io/helmrdotdev/helmr-{repository}',
                    f'foreign.io/helmrdotdev/{repository}',
                    f'ghcrXio/helmrdotdev/{repository}',
                    f'ghcr.io/helmrdotdev/{repository}-other',
                    f'ghcr.io/foreign/{repository}',
                )
                for destination in rejected:
                    with self.subTest(repository=repository, destination=destination):
                        record = read(root / name)
                        original = record['image']
                        record['image'] = f'{destination}@{image_digest}'
                        write(root / name, record)
                        index['assets'][name] = descriptor(root / name)
                        with self.assertRaisesRegex(ValueError, 'Product image digest required'):
                            verify_files(index, root)
                        record['image'] = original
                        write(root / name, record)
                        index['assets'][name] = descriptor(root / name)

    def test_final_index_last_and_interrupted_completion_retry(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);api,r,index=seed_draft(root);s=selection('tag')
            build_digest=digest(root/'release-build.json')
            with patch.object(publish,'sign',side_effect=fake_sign) as sign,patch.object(publish,'verify_signature',side_effect=fake_verify),patch.object(publish.urllib.request,'urlopen',side_effect=lambda url,**kw:public_download(api,url,**kw)):
                api.fail='release-index.json'
                with self.assertRaises(OSError):publish.finalize(api,s,root/'first','1',build_digest)
                self.assertTrue(r['draft']);self.assertNotIn((1,'release-index.json'),api.blobs)
                signature=api.blobs[1,'release-index.sigstore.json']
                publish.finalize(api,s,root/'second','1',build_digest)
                self.assertFalse(r['draft']);self.assertEqual(api.writes[-2:],['release-index.json','PATCH'])
                self.assertEqual(api.blobs[1,'release-index.sigstore.json'],signature)
                self.assertEqual(api.blobs[1,'release-index.json'],api.blobs[1,'release-build.json'])
                self.assertEqual(sign.call_count,1)
                writes=list(api.writes)
                retry=copy.deepcopy(s)
                publish.finalize(api,retry,root/'third','1',build_digest)
                self.assertEqual(api.writes,writes);self.assertEqual(sign.call_count,1)
                # A full rerun admits public completed bytes without publisher outputs.
                api.writer=False
                self.assertEqual(publish.complete(api,retry,root/'admission'),index)

    def test_published_retry_binds_id_raw_build_and_completed_bytes_before_any_write(self):
        for failure in ('response','public-readback'):
            with self.subTest(failure=failure),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);api,r,index=seed_draft(root);s=selection('tag');build_digest=digest(root/'release-build.json')
                with patch.object(publish,'sign',side_effect=fake_sign) as sign,patch.object(publish,'verify_signature',side_effect=fake_verify),patch.object(publish.urllib.request,'urlopen',side_effect=OSError('public unavailable')) as public:
                    api.fail='response' if failure=='response' else None
                    with self.assertRaises(OSError):publish.finalize(api,s,root/'first','1',build_digest)
                    self.assertFalse(r['draft'])
                    writes=list(api.writes);signed=sign.call_count
                    public.side_effect=lambda url,**kw:public_download(api,url,**kw)
                    publish.finalize(api,s,root/'retry','1',build_digest)
                    self.assertEqual(api.writes,writes);self.assertEqual(sign.call_count,signed)
                    for identifier,expected,message in [('2',build_digest,'release ID'),('1','sha256:'+'0'*64,'build digest')]:
                        with self.assertRaisesRegex(ValueError,message):
                            publish.finalize(api,s,root/('bad'+identifier+expected[-1]),identifier,expected)
                    # Valid same-selection signed JSON, different raw bytes: no writes.
                    changed=json.dumps(index,indent=2).encode()
                    api.blobs[1,'release-index.json']=changed
                    api.blobs[1,'release-index.sigstore.json']=('sha256:'+hashlib.sha256(changed).hexdigest()).encode()
                    with self.assertRaisesRegex(ValueError,'completed index differs'):
                        publish.finalize(api,s,root/'different-complete','1',build_digest)
                    self.assertEqual(api.writes,writes);self.assertEqual(sign.call_count,signed)

    def test_draft_visibility_duplicate_version_and_formal_tag_collision(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);api,r,_=seed_draft(root);s=selection('tag')
            self.assertIsNone(api.request('releases/tags/'+s['version'],missing=True))
            self.assertEqual(publish.find_release(api,s['version']),r)
            api.writer=False
            self.assertEqual(list(api.pages('releases')),[])
            with self.assertRaisesRegex(ValueError,'404'):api.request('releases/1')
            self.assertIsNone(publish.complete(api,s,root/'admit'))
            api.writer=True
            api.releases.append(dict(r,id=2))
            with patch.object(publish,'publish_images') as images,patch.object(publish,'npm_publish') as npm:
                with self.assertRaisesRegex(ValueError,'duplicate release version'):publish.stage(api,s,root)
                with self.assertRaisesRegex(ValueError,'duplicate release version'):publish.finalize(api,s,root/'final','1',digest(root/'release-build.json'))
                images.assert_not_called();npm.assert_not_called()
            api.releases.pop();s['build']['mode']='tag'
            with patch.object(publish,'verify_files'),self.assertRaisesRegex(ValueError,'human tag release already exists'):
                publish.stage(api,s,root)

    def test_npm_retry_compares_bytes_and_human_tag_never_resumes(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'sdk.tgz';p.write_bytes(b'original')
            version='0.1.0-preview.g01234567.b1'
            metadata=json.dumps(dict(versions={version:dict(name='@helmr/sdk',version=version,dist=dict(tarball='https://registry.npmjs.org/sdk.tgz'))})).encode()
            for selected,remote,success in (
                (dict(build=dict(mode='main', pr=None), version='v0.1.0-preview.g01234567.b1'), b'original', True),
                (dict(build=dict(mode='pr', pr=7), version='v0.1.0-preview.g01234567.b1'), b'changed', False),
                (dict(build=dict(mode='tag', pr=None), version='v1.0.0'), b'original', False),
            ):
                with patch.object(publish.urllib.request,'urlopen',side_effect=[io.BytesIO(b'{}'),io.BytesIO(metadata),io.BytesIO(remote)]),patch.object(publish,'run') as command:
                    if success:
                        publish.npm_publish(p,'@helmr/sdk','0.1.0-preview.g01234567.b1',selected)
                    else:
                        with self.assertRaises(ValueError):publish.npm_publish(p,'@helmr/sdk','0.1.0-preview.g01234567.b1',selected)
                    command.assert_not_called()

    def test_npm_publication_waits_for_resolver_and_tarball_visibility(self):
        import urllib.error
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'sdk.tgz';p.write_bytes(b'original')
            version='0.1.0-preview.g01234567.b1'
            metadata=json.dumps(dict(versions={version:dict(name='@helmr/sdk',version=version,
                dist=dict(tarball='https://registry.npmjs.org/sdk.tgz'))})).encode()
            missing=lambda:urllib.error.HTTPError('https://registry.npmjs.org/',404,'missing',{},None)
            responses=[missing(),io.BytesIO(b'{"versions":{}}'),io.BytesIO(metadata),missing(),
                       io.BytesIO(metadata),io.BytesIO(b'original')]
            with patch.object(publish.urllib.request,'urlopen',side_effect=responses) as fetch, \
                    patch.object(publish,'run') as command,patch.object(publish.time,'sleep') as sleep:
                publish.npm_publish(p,'@helmr/sdk',version,selection('main'))
                command.assert_called_once()
                self.assertEqual(command.call_args.args[:2],('npm','publish'))
                self.assertEqual(sleep.call_count,2)
                self.assertEqual(fetch.call_args_list[1].args[0].get_header('Accept'),'application/vnd.npm.install-v1+json')

    def test_npm_readback_times_out_without_republishing(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'sdk.tgz';p.write_bytes(b'original')
            with patch.object(publish.urllib.request,'urlopen',return_value=io.BytesIO(b'{"versions":{}}')), \
                    patch.object(publish.time,'monotonic',side_effect=(0,601)),patch.object(publish,'run') as command:
                with self.assertRaisesRegex(ValueError,'deadline'):
                    publish.npm_readback(p,'@helmr/sdk','0.1.0')
                command.assert_not_called()

    def test_npm_readback_does_not_fetch_after_sleep_exhausts_deadline(self):
        with patch.object(publish.urllib.request,'urlopen',return_value=io.BytesIO(b'{"versions":{}}')) as fetch, \
                patch.object(publish.time,'monotonic',side_effect=(0,0,599,600)),patch.object(publish.time,'sleep') as sleep:
            with self.assertRaisesRegex(ValueError,'deadline'):
                publish.npm_readback(Path('unused'),'@helmr/sdk','0.1.0')
            fetch.assert_called_once()
            sleep.assert_called_once_with(1)

    def test_npm_readback_rejects_late_bytes_and_bounds_fetch_timeout(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=Path(tmp)/'sdk.tgz';p.write_bytes(b'original')
            metadata=json.dumps(dict(versions={'0.1.0':dict(name='@helmr/sdk',version='0.1.0',
                dist=dict(tarball='https://registry.npmjs.org/sdk.tgz'))})).encode()
            with patch.object(publish.urllib.request,'urlopen',side_effect=[io.BytesIO(metadata),io.BytesIO(b'original')]) as fetch, \
                    patch.object(publish.time,'monotonic',side_effect=(0,590,595,601)):
                with self.assertRaisesRegex(ValueError,'deadline'):
                    publish.npm_readback(p,'@helmr/sdk','0.1.0')
                self.assertEqual([c.kwargs['timeout'] for c in fetch.call_args_list],[10,5])

    def test_npm_readback_rejects_registry_errors_without_retry(self):
        import urllib.error
        for status in (401,403,429,500):
            with self.subTest(status=status),patch.object(publish.urllib.request,'urlopen',
                    side_effect=urllib.error.HTTPError('https://registry.npmjs.org/',status,'failure',{},None)), \
                    patch.object(publish.time,'sleep') as sleep:
                with self.assertRaisesRegex(ValueError,'HTTP '+str(status)):
                    publish.npm_readback(Path('unused'),'@helmr/sdk','0.1.0')
                sleep.assert_not_called()

    def test_npm_readback_rejects_foreign_package_or_tarball(self):
        for change in (dict(name='@other/sdk'),dict(version='0.2.0'),
                       dict(dist=dict(tarball='https://foreign.example/sdk.tgz'))):
            with self.subTest(change=change):
                package=dict(name='@helmr/sdk',version='0.1.0',dist=dict(tarball='https://registry.npmjs.org/sdk.tgz'))
                package.update(change)
                metadata=json.dumps(dict(versions={'0.1.0':package})).encode()
                with patch.object(publish.urllib.request,'urlopen',return_value=io.BytesIO(metadata)) as fetch, \
                        patch.object(publish.time,'sleep') as sleep:
                    with self.assertRaises(ValueError):publish.npm_readback(Path('unused'),'@helmr/sdk','0.1.0')
                    fetch.assert_called_once()
                    sleep.assert_not_called()

    def test_native_artifact_retry_and_corruption(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);built=root/'built';built.mkdir();(built/'sdk.tgz').write_bytes(b'sdk');(built/'proto.tgz').write_bytes(b'proto')
            freeze('sdk',built,selection(),root/'frozen')
            z=root/'native.zip'
            with zipfile.ZipFile(z,'w') as archive:
                for p in (root/'frozen').iterdir():archive.write(p,p.name)
            item=dict(id=4,name='build-artifacts-123-sdk',expired=False,workflow_run=dict(id=123),digest=digest(z))
            class API:
                def pages(self,*_):return [item]
                def request(self,path,*,destination):shutil.copyfile(z,destination)
            retry=copy.deepcopy(selection())
            with patch.dict('os.environ', GITHUB_EVENT_NAME='pull_request', GITHUB_RUN_ATTEMPT='2'):
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
