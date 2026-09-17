"""Real urllib requests/redirects against loopback HTTP; TLS is not under test."""
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch
import urllib.request
import urllib.parse
import publish
import transport
import test_publication
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts/release'))
from api import GitHub
from contract import digest
from publish import release_asset
from transport import freeze, restore
from test_contract import selection


class GitHubDownloads(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        built = self.root / 'built'
        built.mkdir()
        (built / 'sdk.tgz').write_bytes(b'sdk fixture bytes')
        (built / 'proto.tgz').write_bytes(b'proto fixture bytes')
        freeze('sdk', built, selection(), self.root / 'frozen')
        self.archive = self.root / 'native.zip'
        with zipfile.ZipFile(self.archive, 'w') as archive:
            for path in (self.root / 'frozen').iterdir():
                archive.write(path, path.name)
        self.item = dict(id=4, name='build-artifacts-123-sdk', expired=False,
                         workflow_run=dict(id=123), digest=digest(self.archive))
        self.requests = []
        self.releases = None
        self.redirect_asset = False
        self.location = 'https://fixture-download.invalid/artifact.zip'
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                fixture.requests.append((self.path, self.headers))
                status, body, location = 200, b'', None
                endpoint = self.path.split('?', 1)[0]
                if endpoint == '/repos/helmrdotdev/helmr':
                    body = b'{"id":123}'
                elif fixture.releases is not None and '/releases' in endpoint:
                    surface = fixture.releases
                    surface.writer = self.headers.get('Authorization') == 'Bearer local-fixture-token'
                    path = endpoint.removeprefix('/repos/helmrdotdev/helmr/')
                    try:
                        if path == 'releases':
                            page = int(urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)['page'][0])
                            # Force production pagination past a full unrelated page.
                            records = [dict(id=100+i, tag_name='other-'+str(i), draft=False) for i in range(100)] + list(surface.pages(path))
                            body = json.dumps(records[(page-1)*100:page*100]).encode()
                        elif path.endswith('/assets'):
                            body = json.dumps(list(surface.pages(path))).encode()
                        elif path.startswith('releases/assets/'):
                            if self.headers.get('Accept') != 'application/octet-stream':
                                status = 415
                            else:
                                key = surface.asset_keys[int(path.rsplit('/', 1)[1])]
                                surface.request('releases/' + str(key[0]))
                                # Exercise the authenticated asset redirect too.
                                status, location = 302, 'https://fixture-download.invalid/release-asset/' + path.rsplit('/', 1)[1]
                        else:
                            body = json.dumps(surface.request(path)).encode()
                    except ValueError:
                        status = 404
                elif endpoint.startswith('/release-asset/'):
                    key = fixture.releases.asset_keys[int(endpoint.rsplit('/', 1)[1])]
                    body = fixture.releases.blobs[key]
                elif endpoint.endswith('/actions/artifacts/4'):
                    body = json.dumps(fixture.item).encode()
                elif endpoint.endswith('/actions/runs/123/artifacts'):
                    body = json.dumps(dict(artifacts=[fixture.item])).encode()
                elif endpoint.endswith('/actions/artifacts/4/zip'):
                    if self.headers.get('Accept') != 'application/vnd.github+json':
                        status = 415
                    else:
                        status, location = 302, fixture.location
                elif endpoint.endswith('/releases/1/assets'):
                    body = b'[{"id":5,"name":"sdk.tgz"}]'
                elif endpoint.endswith('/releases/assets/5'):
                    if self.headers.get('Accept') != 'application/octet-stream':
                        status = 415
                    elif fixture.redirect_asset:
                        status, location = 302, fixture.location
                    else:
                        body = fixture.archive.read_bytes()
                elif endpoint == '/artifact.zip':
                    body = fixture.archive.read_bytes()
                else:
                    status = 404
                self.send_response(status)
                if location:
                    self.send_header('Location', location)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        server = HTTPServer(('127.0.0.1', 0), Handler)
        self.addCleanup(server.server_close)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.shutdown)

        class LoopbackHTTPS(urllib.request.HTTPSHandler):
            def https_open(self, request):
                # Route only the socket to loopback. URL admission, HTTP parsing,
                # media headers, redirect processing and body streaming stay real.
                return self.do_open(lambda *_args, **_kwargs: http.client.HTTPConnection(
                    '127.0.0.1', server.server_port), request)

        build_opener = urllib.request.build_opener
        opener = patch('api.urllib.request.build_opener', side_effect=lambda *handlers:
                       build_opener(urllib.request.ProxyHandler({}), LoopbackHTTPS(), *handlers))
        opener.start()
        self.addCleanup(opener.stop)
        token = patch.dict(os.environ, GH_TOKEN='local-fixture-token')
        token.start()
        self.addCleanup(token.stop)
        self.api = GitHub()

    def test_publisher_draft_readback_to_read_only_native_artifact(self):
        built = self.root / 'cohort'
        built.mkdir()
        surface, release, index = test_publication.seed_draft(built)
        self.releases = surface
        with patch.object(publish, 'verify_signature', side_effect=test_publication.fake_verify):
            # Even the writer cannot see drafts by tag; writer list/ID can.
            self.assertIsNone(self.api.request('releases/tags/' + index['version'], missing=True))
            selected = publish.find_release(self.api, index['version'])
            self.assertEqual(selected['id'], release['id'])
            self.assertTrue(any('releases?per_page=100&page=2' in path for path, _ in self.requests))
            self.assertEqual(self.api.request('releases/1')['id'], 1)
            readback = self.root / 'publisher-readback'
            publish.download_build(self.api, selection(), readback, selected)
            self.assertEqual({p.name for p in readback.iterdir()}, set(index['assets']) | {'release-build.json', 'release-build.sigstore.json'})
            with zipfile.ZipFile(self.archive, 'w') as archive:
                for path in readback.iterdir():
                    archive.write(path, path.name)
            self.item.update(name='release-readback-123-2', digest=digest(self.archive))
            action_digest = digest(self.archive).removeprefix('sha256:')
            build_digest = digest(readback / 'release-build.json')
            with patch.dict(os.environ, GH_TOKEN='read-only-fixture-token'):
                self.assertIsNone(self.api.request('releases/tags/' + index['version'], missing=True))
                with self.assertRaisesRegex(ValueError, '404'):
                    self.api.request('releases/1')
                with self.assertRaisesRegex(ValueError, '404'):
                    self.api.request('releases/assets/1', destination=self.root / 'forbidden', accept='application/octet-stream')
                self.requests.clear()
                retry = selection()
                retry['build']['attempt'] = '3'
                accepted = self.root / 'consumer-readback'
                self.assertEqual(transport.download_readback(self.api, retry, accepted, '4', action_digest, '2', build_digest), index)
                self.assertEqual([path for path, _ in self.requests], [
                    '/repos/helmrdotdev/helmr/actions/artifacts/4',
                    '/repos/helmrdotdev/helmr/actions/artifacts/4/zip', '/artifact.zip'])
                self.assertEqual(self.requests[1][1]['Accept'], 'application/vnd.github+json')
                self.assertEqual(self.requests[1][1]['Authorization'], 'Bearer read-only-fixture-token')
                self.assertIsNone(self.requests[2][1]['Authorization'])
                for path in readback.iterdir():
                    self.assertEqual((accepted / path.name).read_bytes(), path.read_bytes())
                # The following candidate step has no token environment.
                with patch.dict(os.environ, GH_TOKEN=''):
                    self.api.request('')
                    self.assertIsNone(self.requests[-1][1]['Authorization'])

    def test_repository_root_relative_and_absolute(self):
        for path in ('', 'https://api.github.com/repos/helmrdotdev/helmr'):
            with self.subTest(path=path):
                self.assertEqual(self.api.request(path), {'id': 123})
                self.assertEqual(self.requests[-1][0], '/repos/helmrdotdev/helmr')
                self.assertEqual(self.requests[-1][1]['Authorization'], 'Bearer local-fixture-token')

    def assert_headers_and_redirect(self, accept):
        initial, redirected = self.requests[-2:]
        self.assertEqual(initial[1]['Accept'], accept)
        self.assertEqual(initial[1]['Authorization'], 'Bearer local-fixture-token')
        self.assertEqual(initial[1]['X-GitHub-Api-Version'], '2022-11-28')
        self.assertEqual(redirected[1]['Host'], 'fixture-download.invalid')
        self.assertIsNone(redirected[1]['Authorization'])

    def test_artifact_zip_restore_headers_redirect_bytes_and_digest_rejection(self):
        self.assertTrue(restore(self.api, 'sdk', selection(), self.root / 'restored'))
        self.assert_headers_and_redirect('application/vnd.github+json')
        self.assertEqual((self.root / 'restored/sdk.tgz').read_bytes(), b'sdk fixture bytes')
        self.assertEqual((self.root / 'restored/proto.tgz').read_bytes(), b'proto fixture bytes')
        self.item['digest'] = 'sha256:' + '0' * 64
        with self.assertRaisesRegex(ValueError, 'native artifact ZIP digest differs'):
            restore(self.api, 'sdk', selection(), self.root / 'corrupt')
        self.assertFalse((self.root / 'corrupt').exists())

    def test_release_asset_direct_and_redirected_file_downloads(self):
        for redirect in (False, True):
            with self.subTest(redirect=redirect):
                self.redirect_asset = redirect
                destination = self.root / f'release-{redirect}.zip'
                self.assertTrue(release_asset(self.api, dict(id=1), 'sdk.tgz', destination))
                self.assertEqual(destination.read_bytes(), self.archive.read_bytes())
                if redirect:
                    self.assert_headers_and_redirect('application/octet-stream')
                else:
                    self.assertEqual(self.requests[-1][1]['Accept'], 'application/octet-stream')
                with self.assertRaises(FileExistsError):
                    release_asset(self.api, dict(id=1), 'sdk.tgz', destination)

    def test_wrong_media_415_and_insecure_redirect_do_not_create_file(self):
        destination = self.root / 'bad.zip'
        with self.assertRaisesRegex(ValueError, 'GitHub request failed: HTTP 415'):
            self.api.request('actions/artifacts/4/zip', destination=destination,
                             accept='application/octet-stream')
        self.assertEqual(len(self.requests), 1)
        self.assertFalse(destination.exists())
        self.location = 'http://fixture-download.invalid/artifact.zip'
        with self.assertRaisesRegex(ValueError, 'non-HTTPS redirect'):
            self.api.request('actions/artifacts/4/zip', destination=destination)
        self.assertEqual(len(self.requests), 2)
        self.assertFalse(destination.exists())

    def test_foreign_api_roots_rejected_before_request(self):
        for url in ('https://api.github.com/repos/helmrdotdev/helmr-other',
                    'https://api.github.com/repos/helmrdotdev/helmr-other/releases',
                    'https://api.github.com/repos/foreign/repo/actions/artifacts/4/zip',
                    'https://api.github.com.evil.invalid/repos/helmrdotdev/helmr/',
                    'https://uploads.github.com/repos/foreign/repo/releases/1/assets'):
            with self.subTest(url=url), self.assertRaisesRegex(ValueError, 'foreign GitHub API'):
                self.api.request(url, destination=self.root / 'foreign.zip')
        self.assertEqual(self.requests, [])


class StandardLibraryImports(unittest.TestCase):
    def test_release_script_directory_does_not_shadow_platform(self):
        subprocess.run([sys.executable, '-c',
                        'import platform; assert platform.system(); '
                        'assert callable(platform.freedesktop_os_release)'],
                       cwd=Path(__file__).resolve().parents[2] / 'scripts/release', check=True)
