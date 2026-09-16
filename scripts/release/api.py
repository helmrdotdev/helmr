"""Native GitHub release/run transport, with authenticated redirects stripped."""
import json
import os
from pathlib import Path
import urllib.error
import urllib.request
from contract import REPOSITORY, require


class Redirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        require(newurl.startswith('https://'), 'non-HTTPS redirect')
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        redirected.remove_header('Authorization')
        return redirected


class GitHub:
    def __init__(self):
        self.root = f'https://api.github.com/repos/{REPOSITORY}/'

    def request(self, path, *, data=None, method=None, destination=None, missing=False, accept='application/vnd.github+json'):
        url = path if path.startswith('https://') else self.root + path
        require(url.startswith((self.root, f'https://uploads.github.com/repos/{REPOSITORY}/')), 'foreign GitHub API')
        # Endpoint media negotiation is independent of saving the response to a file.
        headers = {'Accept': accept,
                   'X-GitHub-Api-Version': '2022-11-28'}
        token = os.environ.get('GH_TOKEN', '')
        if token:
            headers['Authorization'] = 'Bearer ' + token
        upload = None
        if data is not None:
            if isinstance(data, Path):
                headers['Content-Type'] = 'application/octet-stream'
                headers['Content-Length'] = str(data.stat().st_size)
                upload = data.open('rb')
                data = upload
            else:
                headers['Content-Type'] = 'application/json'
                data = json.dumps(data).encode()
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.build_opener(Redirect()).open(request, timeout=60) as response:
                if destination:
                    import shutil
                    with Path(destination).open('xb') as output:
                        shutil.copyfileobj(response, output)
                    return None
                raw = response.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as error:
            if missing and error.code == 404:
                return None
            raise ValueError(f'GitHub request failed: HTTP {error.code}') from None
        finally:
            if upload is not None:
                upload.close()

    def pages(self, path, key=None):
        page = 1
        while True:
            result = self.request(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            items = result[key] if key else result
            yield from items
            if len(items) < 100:
                break
            page += 1
