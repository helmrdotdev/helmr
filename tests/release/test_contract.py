import copy
import io
import json
from pathlib import Path
import subprocess
import shutil
import sys
import tarfile
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
from contract import cli_checksums, ASSETS, canonical, descriptor, digest, preview_version, read, safe_extract, signer, validate, verify_files, write
from admission import ci_success, docs_only, pr_head, relevant
from publish import newest_eligible
from transport import freeze, same_selection

SOURCE = '01234567' + 'a' * 32

def selection(mode='main'):
    return dict(version=preview_version('0.1.0', SOURCE, '123'), sourceCommit=SOURCE,
                sourceRef='refs/heads/main' if mode == 'main' else 'refs/pull/7/head',
                build=dict(runId='123', attempt='1', workflowCommit='b'*40, workflowRef='refs/heads/main', ciRun='100', pr=None if mode == 'main' else 7, mode=mode))


def archive(path, files):
    with tarfile.open(path, 'w:gz' if path.suffix in ('.tgz', '.gz') else 'w') as out:
        root = tarfile.TarInfo('.')
        root.type = tarfile.DIRTYPE
        out.addfile(root)
        for name, value in files.items():
            data = canonical(value) if isinstance(value, dict) else value
            item = tarfile.TarInfo(name)
            item.size = len(data)
            out.addfile(item, io.BytesIO(data))


def assets(root):
    s = selection()
    runtime = dict(formatVersion=0, digest='sha256:'+'1'*64)
    for name in ASSETS:
        (root/name).write_bytes(b'bytes')
    for name, image in (('bundle-builder.json','bundle-builder'), ('controlplane.json','control-plane')):
        write(root/name, dict(formatVersion=0, sourceCommit=SOURCE, image=f'ghcr.io/helmrdotdev/{image}@sha256:'+ '2'*64, runtime=runtime))
    archive(root/'platform-release.tar', {'platform-release.json':dict(formatVersion=0,runtime=runtime)})
    write(root/'platform-release-provenance.json', dict(sourceCommit=SOURCE, sourceRef=s['sourceRef'],archive=descriptor(root/'platform-release.tar')))
    for name,bundle,manifest,key in (
        ('worker-host-bundle.json','worker-host-artifacts.tar','worker-host-artifacts.json','manifest'),
        ('worker-runtime-bundle.json','runtime-artifacts.tar','runtime-artifacts.json','runtimeArtifactsManifest')):
        write(root/name, dict(sourceCommit=SOURCE,bundle=dict(path=bundle,digest=digest(root/bundle)), **{key:dict(path=manifest,digest=digest(root/manifest))}))
    for filename, package in (('sdk.tgz','@helmr/sdk'),('proto.tgz','@helmr/proto')):
        metadata=dict(name=package,version=s['version'][1:],helmr=dict(formatVersion=0,sourceCommit=SOURCE,buildId='123'),dependencies={'@helmr/proto':s['version'][1:]})
        archive(root/filename, {'package/package.json':metadata})
    (root/'checksums.txt').write_bytes(cli_checksums(root))
    return dict(s,schema='helmr.release.v0',assets={name:descriptor(root/name) for name in ASSETS})


class Contract(unittest.TestCase):
    def test_fixed_oci_script_interface(self):
        script = Path(__file__).resolve().parents[2] / 'scripts/release/contract.py'
        for role in ('bundle-builder', 'control-plane'):
            expected = 'ghcr.io/helmrdotdev/' + role
            name = subprocess.check_output([sys.executable, str(script), role], text=True).strip()
            self.assertEqual(name, expected)
            reference = name + '@sha256:' + 'a' * 64
            good = subprocess.run([sys.executable, str(script), role, '--verify', reference], capture_output=True)
            self.assertEqual(good.returncode, 0, good.stderr)
            self.assertEqual(good.stdout, b'')
            for bad in (reference.replace('ghcr.io', 'ghcrXio'),
                        reference.replace('helmrdotdev/', 'helmrdotdev/helmr/'),
                        reference.replace('ghcr.io', 'registry.example'),
                        name + ':latest', reference[:-1], reference + '0',
                        reference.replace(role, 'control-plane' if role == 'bundle-builder' else 'bundle-builder')):
                with self.subTest(role=role, reference=bad):
                    result = subprocess.run([sys.executable, str(script), role, '--verify', bad], capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(b'Product image digest required', result.stderr)

    def test_constructed_versions(self):
        for fragment in ('01234567','12345678','00000000','abc12345'):
            version=preview_version('0.1.0',fragment+'a'*32,'123')
            self.assertEqual(version, 'v0.1.0-preview.g'+fragment+'.b123')
            self.assertEqual(version, preview_version('0.1.0',fragment+'a'*32,'123'))
        with self.assertRaises(ValueError): preview_version('00.1.0',SOURCE,'123')

    def test_constructed_versions_with_npm_semver(self):
        npm=str(Path(shutil.which('npm')).resolve())
        for fragment in ('01234567','00000000','12345678','abc12345'):
            version=preview_version('0.1.0',fragment+'a'*32,'123')[1:]
            subprocess.run(['node','-e',"const s=require(require('node:module').createRequire(process.argv[1]).resolve('semver')); if(s.valid(process.argv[2])!==process.argv[2])process.exit(1)",npm,version],check=True)

    def test_exact_signer(self):
        self.assertEqual(signer(selection()['version']), 'https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/heads/main')
        self.assertEqual(signer('v0.1.0'), 'https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/tags/v0.1.0')

    def test_actual_archive_round_trip_and_corruption(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            index=assets(root)
            verify_files(index,root)
            safe_extract(root/'platform-release.tar',root/'extracted')
            self.assertTrue((root/'extracted/platform-release.json').is_file())
            (root/'sdk.tgz').write_bytes(b'corrupt')
            with self.assertRaisesRegex(ValueError,'bytes differ'): verify_files(index,root)

    def test_checksum_projection_is_bound_to_cli_assets(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);index=assets(root)
            (root/'checksums.txt').write_bytes(b'0'*64+b'  helmr-linux-amd64.tar.gz\n')
            index['assets']['checksums.txt']=descriptor(root/'checksums.txt')
            with self.assertRaisesRegex(ValueError,'checksum projection'):verify_files(index,root)

    def test_complete_coverage_and_source(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); index=assets(root)
            bad=copy.deepcopy(index); del bad['assets']['sdk.tgz']
            with self.assertRaises(ValueError):validate(bad)
            b=read(root/'bundle-builder.json');b['sourceCommit']='c'*40;write(root/'bundle-builder.json',b)
            index['assets']['bundle-builder.json']=descriptor(root/'bundle-builder.json')
            with self.assertRaisesRegex(ValueError,'source mismatch'): verify_files(index,root)

    def test_unsafe_archives(self):
        for name,kind in (('../escape',tarfile.REGTYPE),('/absolute',tarfile.REGTYPE),('link',tarfile.SYMTYPE),('device',tarfile.CHRTYPE)):
            with self.subTest(name=name),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp)
                with tarfile.open(root/'bad.tar','w') as out:
                    member=tarfile.TarInfo(name);member.type=kind;out.addfile(member)
                with self.assertRaises(ValueError):safe_extract(root/'bad.tar',root/'out')
                self.assertFalse((root/'out').exists())

    def test_duplicate_json(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'x';path.write_text('{"x":1,"x":2}')
            with self.assertRaises(ValueError):read(path)

    def test_original_byte_retry_identity(self):
        original=selection();retry=copy.deepcopy(original);retry['build']['attempt']='2'
        same_selection(original,retry)
        retry['build']['workflowCommit']='c'*40
        with self.assertRaises(ValueError):same_selection(original,retry)

    def test_current_pr_head(self):
        pr=dict(number=7,state='open',base=dict(ref='main',repo=dict(id=1)),head=dict(sha=SOURCE,repo=dict(id=1)))
        pr_head(pr,7,SOURCE,1)
        for change in ('closed','fork','stale','base'):
            bad=copy.deepcopy(pr)
            if change=='closed':bad['state']='closed'
            elif change=='fork':bad['head']['repo']['id']=2
            elif change=='stale':bad['head']['sha']='c'*40
            else:bad['base']['ref']='other'
            with self.assertRaises(ValueError):pr_head(bad,7,SOURCE,1)

    def test_native_ci_producer_and_aggregate(self):
        class API:
            def pages(self,*_):return [dict(name='ci complete',status='completed',conclusion='success')]
        run=dict(id=10,run_attempt=1,repository=dict(id=1),workflow_id=2,path='.github/workflows/ci.yaml',event='push',head_sha=SOURCE,head_branch='main',status='completed',conclusion='success')
        self.assertEqual(ci_success(API(),run,2,1,SOURCE,'push'),10)
        for key,value in [('path','.github/workflows/release.yaml'),('workflow_id',3),('event','pull_request'),('head_branch','other'),('conclusion','failure'),('head_sha','c'*40)]:
            with self.subTest(key=key),self.assertRaises(ValueError):ci_success(API(),dict(run,**{key:value}),2,1,SOURCE,'push')


class RepositoryFixture:
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name)
        self.git('init','-q');self.git('config','user.email','fixture@example.invalid');self.git('config','user.name','fixture')
        self.a=self.commit('code.go','a')
        self.b=self.commit('code.go','b')
        self.c=self.commit('README.md','docs')

    def tearDown(self):self.temp.cleanup()
    def git(self,*args):return subprocess.check_output(['git','-C',str(self.root),*args],text=True).strip()
    def commit(self,path,content):
        (self.root/path).write_text(content);self.git('add',path);self.git('-c','commit.gpgsign=false','commit','-qm',content);return self.git('rev-parse','HEAD')
    def index(self,source,run):
        s=selection();s['sourceCommit']=source;s['build']['runId']=str(run);return s


class Discovery(RepositoryFixture, unittest.TestCase):
    def test_pending_replacement_and_stale_finish_reconciles_newest(self):
        # updater occupied, B pending, late A replaces B. A reconciles both.
        completed=[self.index(self.a,1),self.index(self.b,2)]
        self.assertEqual(newest_eligible(completed,self.root,self.c)['sourceCommit'],self.b)
        self.assertEqual(newest_eligible(list(reversed(completed)),self.root,self.c)['sourceCommit'],self.b)

    def test_publication_complete_discovery_failure_retry(self):
        completed=[self.index(self.b,2)]
        first=newest_eligible(completed,self.root,self.c)
        # No upload/rebuild changes on discovery-only retry.
        self.assertEqual(first,newest_eligible(completed,self.root,self.c))
        self.assertFalse(relevant(self.root,self.b,self.c))
        self.assertTrue(relevant(self.root,self.a,self.c))

    def test_rename_and_delete_relevance(self):
        self.git('mv','README.md','shipped.md');self.git('-c','commit.gpgsign=false','commit','-qm','rename')
        self.assertTrue(relevant(self.root,self.c,self.git('rev-parse','HEAD')))
        self.assertFalse(docs_only(['sdk/README.md']))
        self.assertFalse(docs_only(['LICENSE']))
        self.assertFalse(docs_only(['README.md','go.sum']))
        self.assertTrue(docs_only(['README.md','packages/web/src/content/docs/test.md']))
        self.assertFalse(docs_only([]))

if __name__=='__main__':unittest.main()
