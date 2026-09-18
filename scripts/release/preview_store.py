"""Fixed-origin S3 preview transport. No ListBucket; writes are conditional-create only."""
import json
import os
import re
import shutil
import subprocess
import tempfile
import urllib.error
import urllib.request
from pathlib import Path
from contract import ASSETS, DIGEST, canonical, descriptor, digest, read, require, validate, verify_files, write
from admission import git, relevant

DEFAULT_ORIGIN = 'https://helmr-previews-879980497511-us-east-1.s3.us-east-1.amazonaws.com'
DEFAULT_BUCKET = 'helmr-previews-879980497511-us-east-1'
LOCAL_BUILD_INDEX = 'release-build.json'
COMPLETION_INDEX = 'release-index.json'
COMPLETION_SIG = 'release-index.sigstore.json'
POINTER_KEY = 'channels/preview.json'
MAX_PUT_BYTES = 5 * 1024**3
STREAM_BLOCK = 1024 * 1024
OPTIONAL_ABSENT = frozenset({'NoSuchKey', '404', '403', 'AccessDenied'})


class NoPreviewRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError('preview downloads must not redirect')


def preview_mode(selection):
    return selection['build']['mode'] in ('main', 'pr')


def config():
    base = os.environ.get('PREVIEW_BASE_URL', DEFAULT_ORIGIN).rstrip('/')
    bucket = os.environ.get('PREVIEW_BUCKET', DEFAULT_BUCKET)
    require(base.startswith('https://'), 'preview origin must be HTTPS')
    return base, bucket


def object_key(version, name):
    return f'previews/{version}/{name}'


def public_url(version, name):
    base, _ = config()
    return f'{base}/{object_key(version, name)}'


def pointer_url():
    base, _ = config()
    return f'{base}/{POINTER_KEY}'


def public_open(url, *, timeout=60):
    return urllib.request.build_opener(NoPreviewRedirect()).open(url, timeout=timeout)


def aws(*args, check=True):
    result = subprocess.run(['aws', *args], capture_output=True, text=True)
    if check and result.returncode != 0:
        raise ValueError((result.stderr or result.stdout or 'aws command failed').strip())
    return result


def aws_failure(result):
    return ((result.stderr or '') + (result.stdout or '')).strip()


def aws_error_code(text):
    match = re.search(r'\(([A-Za-z0-9]+)\)', text)
    return match.group(1) if match else ''


def is_precondition_failed(result):
    return result.returncode != 0 and aws_error_code(aws_failure(result)) in ('PreconditionFailed', '412')


def is_request_conflict(result):
    return result.returncode != 0 and aws_error_code(aws_failure(result)) in ('ConditionalRequestConflict', '409')


def is_optional_absent(result):
    return result.returncode != 0 and aws_error_code(aws_failure(result)) in OPTIONAL_ABSENT


def ensure_put_size(path):
    size = Path(path).stat().st_size
    require(0 < size <= MAX_PUT_BYTES, 'preview object exceeds single PutObject limit')


def put_object(bucket, key, path, *, if_none=False, if_match=None):
    ensure_put_size(path)
    args = ['s3api', 'put-object', '--bucket', bucket, '--key', key, '--body', str(path)]
    if if_none:
        args.extend(['--if-none-match', '*'])
    if if_match is not None:
        args.extend(['--if-match', if_match])
    result = aws(*args, check=False)
    if result.returncode == 0:
        return 'created'
    if is_precondition_failed(result):
        return 'exists'
    if is_request_conflict(result):
        raise ValueError('conditional put conflict; re-run the release workflow: ' + aws_failure(result))
    raise ValueError(aws_failure(result) or 'conditional put failed')


def publisher_get_object(bucket, key, *, optional=False):
    """Download an object to a temp file; return (path, ETag). Caller deletes the path."""
    fd, name = tempfile.mkstemp()
    os.close(fd)
    destination = Path(name)
    try:
        result = aws('s3api', 'get-object', '--bucket', bucket, '--key', key, str(destination), check=False)
        if optional and is_optional_absent(result):
            destination.unlink(missing_ok=True)
            return None
        if result.returncode != 0:
            raise ValueError(aws_failure(result) or 'get-object failed')
        meta = json.loads(result.stdout)
        return destination, meta['ETag'].strip('"')
    except Exception:
        destination.unlink(missing_ok=True)
        raise


def verify_remote_bytes(bucket, key, path):
    remote_path, _ = publisher_get_object(bucket, key)
    try:
        require(descriptor(remote_path) == descriptor(path), 'existing preview object has different bytes: ' + key)
    finally:
        remote_path.unlink(missing_ok=True)


def verify_or_create(bucket, key, path):
    state = put_object(bucket, key, path, if_none=True)
    if state == 'exists':
        verify_remote_bytes(bucket, key, path)


def parse_pointer(raw):
    record = json.loads(raw.decode() if isinstance(raw, (bytes, bytearray)) else raw)
    for field in ('version', 'sourceCommit', 'indexDigest'):
        require(field in record, 'preview pointer missing field: ' + field)
    require(re.fullmatch(DIGEST, record['indexDigest']), 'preview pointer digest invalid')
    return record


def public_fetch(url, destination=None):
    """Return bytes without destination, True when written, None when absent (403/404)."""
    try:
        with public_open(url) as response:
            require(response.url == url, 'unexpected preview download URL')
            require(getattr(response, 'status', 200) == 200, f'preview fetch failed: HTTP {getattr(response, "status", "?")}')
            if destination is None:
                return response.read()
            dest = Path(destination)
            dest.parent.mkdir(parents=True, exist_ok=True)
            with dest.open('xb') as out:
                while True:
                    block = response.read(STREAM_BLOCK)
                    if not block:
                        break
                    out.write(block)
            return True
    except urllib.error.HTTPError as error:
        if error.code in (403, 404):
            return None
        raise ValueError(f'preview fetch failed: HTTP {error.code}') from error


def fetch_pointer():
    raw = public_fetch(pointer_url())
    return None if raw is None else parse_pointer(raw)


def pointer_snapshot(bucket, *, optional=False):
    got = publisher_get_object(bucket, POINTER_KEY, optional=optional)
    if got is None:
        return None
    path, etag = got
    try:
        body = path.read_bytes()
        return parse_pointer(body), etag, body
    finally:
        path.unlink(missing_ok=True)


def cas_pointer(record, snapshot):
    """Conditional pointer write bound to the discover-validated snapshot ETag."""
    _, bucket = config()
    body = canonical(record)
    if snapshot is not None and snapshot[0]['version'] == record['version'] and snapshot[0] != record:
        require(False, 'preview pointer must bind the completed cohort')
    with tempfile.NamedTemporaryFile(delete=False) as temporary:
        path = Path(temporary.name)
    try:
        path.write_bytes(body)
        args = dict(if_none=True) if snapshot is None else dict(if_match=snapshot[1])
        state = put_object(bucket, POINTER_KEY, path, **args)
        require(state == 'created', 'preview pointer CAS conflict')
        return record
    finally:
        path.unlink(missing_ok=True)


def build_index(selection, directory):
    directory = Path(directory)
    return dict(selection, schema='helmr.release.v0',
                assets={name: descriptor(directory / name) for name in sorted(ASSETS)})


def stage(api, selection, directory):
    from publish import publish_images, npm_publish
    from admission import recheck_pr
    directory = Path(directory)
    recheck_pr(api, selection)
    index = build_index(selection, directory)
    verify_files(index, directory)
    _, bucket = config()
    write(directory / LOCAL_BUILD_INDEX, index)
    publish_images(directory, index)
    for package, filename in (('@helmr/proto', 'proto.tgz'), ('@helmr/sdk', 'sdk.tgz')):
        npm_publish(directory / filename, package, index['version'][1:], selection)
    for name in sorted(ASSETS):
        verify_or_create(bucket, object_key(index['version'], name), directory / name)
    return digest(directory / LOCAL_BUILD_INDEX)


def fetch_assets(selection, directory):
    directory = Path(directory)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    version = selection['version']
    for name in ASSETS:
        url = public_url(version, name)
        require(public_fetch(url, directory / name) is True, 'missing public preview asset: ' + name)
    return build_index(selection, directory)


def verify_public(selection, directory, expected_digest):
    require(re.fullmatch(DIGEST, expected_digest or ''), 'build digest required')
    index = fetch_assets(selection, directory)
    write(Path(directory) / LOCAL_BUILD_INDEX, index)
    require(digest(Path(directory) / LOCAL_BUILD_INDEX) == expected_digest, 'reconstructed preview index digest differs')
    verify_files(index, directory)
    return index


def signature_or_create(bucket, key, index_path, signature_path):
    from publish import verify_signature
    version = read(index_path)['version']
    state = put_object(bucket, key, signature_path, if_none=True)
    if state == 'created':
        return signature_path
    remote_path, _ = publisher_get_object(bucket, key)
    try:
        verify_signature(index_path, remote_path, version)
        shutil.copyfile(remote_path, signature_path)
        return signature_path
    finally:
        remote_path.unlink(missing_ok=True)


def finalize(api, selection, directory, build_digest):
    from admission import recheck_pr
    from publish import sign
    directory = Path(directory)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    recheck_pr(api, selection)
    require(re.fullmatch(DIGEST, build_digest or ''), 'build digest required')
    index = verify_public(selection, directory, build_digest)
    _, bucket = config()
    version = index['version']
    completion = directory / COMPLETION_INDEX
    write(completion, index)
    signature = directory / COMPLETION_SIG
    sign(completion)
    signature_or_create(bucket, object_key(version, COMPLETION_SIG), completion, signature)
    verify_or_create(bucket, object_key(version, COMPLETION_INDEX), completion)
    with tempfile.TemporaryDirectory() as temporary:
        require(complete(selection, Path(temporary) / 'public') is not None, 'public preview completion readback failed')
    return index


def complete(selection, directory):
    directory = Path(directory)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    version = selection['version']
    index_path = directory / COMPLETION_INDEX
    sig_path = directory / COMPLETION_SIG
    if public_fetch(public_url(version, COMPLETION_INDEX), index_path) is not True:
        return None
    if public_fetch(public_url(version, COMPLETION_SIG), sig_path) is not True:
        return None
    from publish import verify_signature
    verify_signature(index_path, sig_path, version)
    index = validate(read(index_path))
    from transport import same_selection
    same_selection({k: index[k] for k in selection}, selection)
    for name in ASSETS:
        require(public_fetch(public_url(version, name), directory / name) is True,
                'missing public completed preview asset: ' + name)
        require(descriptor(directory / name) == index['assets'][name], 'public completed asset differs')
    verify_files(index, directory)
    return index


def discover(api, root, selection, index_digest):
    require(selection['build']['mode'] == 'main', 'preview pointer update is main-only')
    require(re.fullmatch(DIGEST, index_digest or ''), 'index digest required')
    subprocess.run(['git', '-C', str(root), 'fetch', '--no-tags', 'origin', 'main'], check=True)
    main = git(root, 'rev-parse', 'FETCH_HEAD')
    record = dict(version=selection['version'], sourceCommit=selection['sourceCommit'], indexDigest=index_digest)
    _, bucket = config()
    current = pointer_snapshot(bucket, optional=True)
    if current:
        existing = current[0]
        if existing['version'] == selection['version']:
            require(existing == record, 'preview pointer must bind the completed cohort')
            return record
        if subprocess.run(['git', '-C', str(root), 'merge-base', '--is-ancestor',
                           selection['sourceCommit'], existing['sourceCommit']], capture_output=True).returncode == 0:
            return existing
        if subprocess.run(['git', '-C', str(root), 'merge-base', '--is-ancestor',
                           existing['sourceCommit'], selection['sourceCommit']], capture_output=True).returncode != 0:
            require(False, 'preview pointer must not regress')
        if relevant(root, selection['sourceCommit'], main):
            return existing
    return cas_pointer(record, current)
