"""S3 preview transport: conditional writes, pointer CAS, completion readback."""
import copy
import io
import json
import os
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
import main
import preview_store
from contract import ASSETS, canonical, digest, validate, write
from s3_fixture import S3Fixture
from test_contract import assets, selection


def fake_verify(path, signature, version):
    if Path(signature).read_text() != digest(path):
        raise ValueError('bad fixture signature')


def fake_sign(path):
    signature = Path(str(path).removesuffix('.json') + '.sigstore.json')
    signature.write_text(digest(path))
    return signature


def stage_digest(root, selected):
    write(root / preview_store.LOCAL_BUILD_INDEX, preview_store.build_index(selected, root))
    return digest(root / preview_store.LOCAL_BUILD_INDEX)


class PreviewTransport(unittest.TestCase):
    def setUp(self):
        self.fixture = S3Fixture()
        os.environ['PREVIEW_BASE_URL'] = 'https://preview.example.invalid'
        os.environ['PREVIEW_BUCKET'] = self.fixture.bucket

    def tearDown(self):
        os.environ.pop('PREVIEW_BASE_URL', None)
        os.environ.pop('PREVIEW_BUCKET', None)

    def test_verify_or_create_reuses_existing_bytes(self):
        with tempfile.NamedTemporaryFile(delete=False) as tmp:
            path = Path(tmp.name)
        try:
            path.write_bytes(b'fixed')
            key = preview_store.object_key('v0.1.0-preview.gabc.b1', 'checksums.txt')
            with patch.object(preview_store, 'aws', side_effect=self.fixture.aws):
                preview_store.verify_or_create(self.fixture.bucket, key, path)
                preview_store.verify_or_create(self.fixture.bucket, key, path)
            self.assertEqual(self.fixture.objects[key]['body'], b'fixed')
        finally:
            path.unlink(missing_ok=True)

    def test_put_object_rejects_oversized_assets(self):
        with tempfile.NamedTemporaryFile(delete=False) as tmp:
            path = Path(tmp.name)
        try:
            with path.open('wb') as stream:
                stream.truncate(preview_store.MAX_PUT_BYTES + 1)
            with self.assertRaisesRegex(ValueError, 'PutObject limit'):
                preview_store.ensure_put_size(path)
        finally:
            path.unlink(missing_ok=True)

    def test_public_fetch_treats_403_and_404_as_unavailable(self):
        for code in (403, 404):
            with self.subTest(code=code):
                class Opener:
                    def open(self, url, timeout=60):
                        raise urllib.error.HTTPError(url, code, 'missing', None, io.BytesIO(b''))
                with patch('urllib.request.build_opener', return_value=Opener()):
                    self.assertIsNone(preview_store.public_fetch(preview_store.pointer_url()))

    def test_public_fetch_writes_destination_and_returns_true(self):
        body = b'payload'
        payload = io.BytesIO(body)

        class Response:
            url = 'https://preview.example.invalid/x'
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self, n=-1):
                return payload.read(n)

        class Opener:
            def open(self, url, timeout=60):
                payload.seek(0)
                return Response()
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / 'out'
            with patch('urllib.request.build_opener', return_value=Opener()):
                self.assertTrue(preview_store.public_fetch('https://preview.example.invalid/x', dest))
            self.assertEqual(dest.read_bytes(), body)

    def test_complete_returns_none_for_absent_index_or_signature(self):
        selected = selection('main')
        with tempfile.TemporaryDirectory() as tmp:
            public = Path(tmp)
            with patch.object(preview_store, 'public_fetch', return_value=None):
                self.assertIsNone(preview_store.complete(selected, public))

            def index_only(url, destination=None):
                if destination is not None and destination.name == preview_store.COMPLETION_INDEX:
                    destination.write_bytes(b'{}')
                    return True
                return None

            with patch.object(preview_store, 'public_fetch', side_effect=index_only):
                self.assertIsNone(preview_store.complete(selected, public))

    def test_main_admit_allows_unpublished_preview(self):
        selected = selection('main')
        output = Path(tempfile.mkdtemp()) / 'outputs'
        with patch.object(main, 'GitHub'), patch.object(main, 'admit', return_value=selected), \
             patch.object(main, 'complete', return_value=None) as complete, \
             patch.dict(os.environ, RELEASE_SELECTION='{}', GITHUB_OUTPUT=str(output),
                        GITHUB_EVENT_PATH=str(output.with_suffix('.event.json'))):
            output.with_suffix('.event.json').write_text('{}')
            with patch('sys.argv', ['release', 'admit']):
                main.main()
        complete.assert_called_once()
        lines = dict(line.split('=', 1) for line in output.read_text().splitlines())
        self.assertEqual(lines['complete'], 'false')

    def test_changed_asset_bytes_rejected_on_conditional_retry(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            selected = selection('main')
            assets(root, selected)
            key = preview_store.object_key(selected['version'], 'checksums.txt')
            with patch.object(preview_store, 'aws', side_effect=self.fixture.aws):
                preview_store.verify_or_create(self.fixture.bucket, key, root / 'checksums.txt')
                (root / 'checksums.txt').write_bytes(b'changed')
                with self.assertRaisesRegex(ValueError, 'different bytes'):
                    preview_store.verify_or_create(self.fixture.bucket, key, root / 'checksums.txt')

    def test_local_build_index_not_uploaded_by_stage(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            selected = selection('main')
            assets(root, selected)
            calls = []
            def tracking_aws(*args, **kwargs):
                calls.append(list(args))
                return self.fixture.aws(*args, **kwargs)
            with patch.object(preview_store, 'aws', side_effect=tracking_aws), \
                 patch('publish.publish_images'), patch('publish.npm_publish'):
                preview_store.stage(object(), selected, root)
            uploaded_keys = [cmd[cmd.index('--key') + 1] for cmd in calls if cmd[:2] == ['s3api', 'put-object']]
            self.assertNotIn(preview_store.object_key(selected['version'], preview_store.LOCAL_BUILD_INDEX), uploaded_keys)

    def test_identical_index_digest_across_admitted_ci_attempts(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            admitted = selection('main')
            assets(root, admitted)
            first = stage_digest(root, admitted)
            second = stage_digest(root, copy.deepcopy(admitted))
            self.assertEqual(first, second)

    def test_pointer_cas_rejects_same_version_cohort_mismatch(self):
        record = dict(version='v0.1.0-preview.gbbb.b2', sourceCommit='b' * 40, indexDigest='sha256:' + '1' * 64)
        snapshot = (record, 'etag', canonical(record))
        stale = dict(record, indexDigest='sha256:' + '2' * 64)
        with patch.object(preview_store, 'aws', side_effect=self.fixture.aws):
            with self.assertRaisesRegex(ValueError, 'must bind the completed cohort'):
                preview_store.cas_pointer(stale, snapshot)

    def test_cas_pointer_fails_when_snapshot_etag_stale(self):
        first = dict(version='v0.1.0-preview.gaaa.b1', sourceCommit='a' * 40, indexDigest='sha256:' + '1' * 64)
        second = dict(version='v0.1.0-preview.gbbb.b2', sourceCommit='b' * 40, indexDigest='sha256:' + '2' * 64)
        self.fixture.put(preview_store.POINTER_KEY, canonical(first))
        with patch.object(preview_store, 'aws', side_effect=self.fixture.aws):
            snapshot = preview_store.pointer_snapshot(self.fixture.bucket, optional=True)
            self.fixture.put(preview_store.POINTER_KEY, canonical(second))
            with self.assertRaisesRegex(ValueError, 'CAS conflict'):
                preview_store.cas_pointer(first, snapshot)
        self.assertEqual(json.loads(self.fixture.objects[preview_store.POINTER_KEY]['body']), second)

    def test_stale_completion_keeps_newer_pointer(self):
        newer = dict(version='v0.1.0-preview.gccc.b3', sourceCommit='c' * 40, indexDigest='sha256:' + '3' * 64)
        self.fixture.put(preview_store.POINTER_KEY, canonical(newer))
        older = selection('main')
        older['sourceCommit'] = 'a' * 40
        with patch.object(preview_store, 'aws', side_effect=self.fixture.aws), \
             patch.object(preview_store.subprocess, 'run', return_value=type('R', (), {'returncode': 0})()), \
             patch.object(preview_store, 'git', side_effect=lambda root, *args: {'rev-parse FETCH_HEAD': 'd' * 40}.get(' '.join(args), 'd' * 40)):
            result = preview_store.discover(object(), Path('.'), older, 'sha256:' + '2' * 64)
        self.assertEqual(result, newer)

    def test_discover_rejects_regressing_pointer(self):
        current = dict(version='v0.1.0-preview.gccc.b3', sourceCommit='c' * 40, indexDigest='sha256:' + '3' * 64)
        self.fixture.put(preview_store.POINTER_KEY, canonical(current))
        selected = selection('main')
        selected['sourceCommit'] = 'f' * 40

        def merge_base(cmd, **kwargs):
            args = cmd if isinstance(cmd, list) else []
            if len(args) >= 6 and args[-4] == 'merge-base':
                left, right = args[-2], args[-1]
                if left == selected['sourceCommit'] and right == current['sourceCommit']:
                    return type('R', (), {'returncode': 1})()
                if left == current['sourceCommit'] and right == selected['sourceCommit']:
                    return type('R', (), {'returncode': 1})()
            return type('R', (), {'returncode': 0})()

        with patch.object(preview_store, 'aws', side_effect=self.fixture.aws), \
             patch.object(preview_store.subprocess, 'run', side_effect=merge_base), \
             patch.object(preview_store, 'git', return_value='d' * 40):
            with self.assertRaisesRegex(ValueError, 'must not regress'):
                preview_store.discover(object(), Path('.'), selected, 'sha256:' + '2' * 64)

    def test_optional_pointer_absence_treats_access_denied_as_absent(self):
        class DenyFixture(S3Fixture):
            def get(self, key, destination, *, absent_code='AccessDenied'):
                return super().get(key, destination, absent_code=absent_code)
        fixture = DenyFixture()
        with patch.object(preview_store, 'aws', side_effect=fixture.aws):
            self.assertIsNone(preview_store.pointer_snapshot(fixture.bucket, optional=True))

    def test_required_object_access_denied_is_hard_error(self):
        class DenyFixture(S3Fixture):
            def get(self, key, destination, *, absent_code='AccessDenied'):
                return super().get(key, destination, absent_code=absent_code)
        fixture = DenyFixture()
        with tempfile.NamedTemporaryFile(delete=False) as tmp:
            local = Path(tmp.name)
        try:
            local.write_bytes(b'local')
            with patch.object(preview_store, 'aws', side_effect=fixture.aws):
                with self.assertRaisesRegex(ValueError, 'AccessDenied'):
                    preview_store.verify_remote_bytes(fixture.bucket, 'missing', local)
        finally:
            local.unlink(missing_ok=True)

    def test_signature_collision_reuses_valid_remote_bundle(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            selected = selection('main')
            index = assets(root, selected)
            completion = root / preview_store.COMPLETION_INDEX
            write(completion, index)
            signature = fake_sign(completion)
            remote_sig = b'remote-signature-bytes'
            key = preview_store.object_key(index['version'], preview_store.COMPLETION_SIG)
            self.fixture.put(key, remote_sig)
            with patch.object(preview_store, 'aws', side_effect=self.fixture.aws), \
                 patch('publish.verify_signature') as verify:
                path = preview_store.signature_or_create(self.fixture.bucket, key, completion, signature)
                verify.assert_called_once()
                self.assertEqual(path.read_bytes(), remote_sig)

    def test_finalize_resumes_when_signature_exists_before_index(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            selected = selection('main')
            assets(root, selected)
            expected = stage_digest(root, selected)
            version = selected['version']
            completion = preview_store.build_index(selected, root)
            write(root / preview_store.COMPLETION_INDEX, completion)
            sig = fake_sign(root / preview_store.COMPLETION_INDEX)
            self.fixture.put(preview_store.object_key(version, preview_store.COMPLETION_SIG), sig.read_bytes())
            for name in ASSETS:
                self.fixture.put(preview_store.object_key(version, name), (root / name).read_bytes())
            with patch.object(preview_store, 'aws', side_effect=self.fixture.aws), \
                 patch.object(preview_store, 'public_fetch', side_effect=self._public_get), \
                 patch('publish.verify_signature', side_effect=fake_verify), patch('publish.sign', side_effect=fake_sign):
                preview_store.finalize(object(), selected, root, expected)
            self.assertIn(preview_store.object_key(version, preview_store.COMPLETION_INDEX), self.fixture.objects)

    def test_complete_mkdirs_and_reconstructs_from_public_objects(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            selected = selection('main')
            index = assets(root, selected)
            write(root / preview_store.COMPLETION_INDEX, index)
            sig = fake_sign(root / preview_store.COMPLETION_INDEX)
            version = selected['version']
            self.fixture.put(preview_store.object_key(version, preview_store.COMPLETION_INDEX),
                             (root / preview_store.COMPLETION_INDEX).read_bytes())
            self.fixture.put(preview_store.object_key(version, preview_store.COMPLETION_SIG), sig.read_bytes())
            for name in ASSETS:
                self.fixture.put(preview_store.object_key(version, name), (root / name).read_bytes())
            public = root / 'public'
            with patch.object(preview_store, 'public_fetch', side_effect=self._public_get), \
                 patch('publish.verify_signature', side_effect=fake_verify):
                completed = preview_store.complete(selected, public)
            self.assertEqual(completed['version'], version)
            self.assertTrue((public / preview_store.COMPLETION_INDEX).is_file())

    def _public_get(self, url, destination=None):
        version = url.split('/previews/')[1].split('/', 1)[0]
        name = url.rsplit('/', 1)[1]
        key = preview_store.object_key(version, name)
        if key not in self.fixture.objects:
            return None
        body = self.fixture.objects[key]['body']
        if destination is None:
            return body
        dest = Path(destination)
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(body)
        return True


if __name__ == '__main__':
    unittest.main()
