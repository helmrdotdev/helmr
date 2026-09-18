"""Faithful in-memory AWS CLI v2 backend for preview_store tests."""
import hashlib
import json
import subprocess
from pathlib import Path


class S3Fixture:
    def __init__(self, bucket='preview-bucket'):
        self.bucket = bucket
        self.objects = {}

    def etag(self, body):
        return hashlib.md5(body).hexdigest()

    def put(self, key, body, *, if_none=False, if_match=None):
        current = self.objects.get(key)
        if if_none and current is not None:
            return subprocess.CompletedProcess([], 254, '', 'An error occurred (PreconditionFailed) when calling the PutObject operation')
        if if_match is not None and (current is None or current['etag'] != if_match):
            return subprocess.CompletedProcess([], 254, '', 'An error occurred (PreconditionFailed) when calling the PutObject operation')
        etag = self.etag(body)
        self.objects[key] = dict(body=body, etag=etag)
        return subprocess.CompletedProcess([], 0, '', '')

    def get(self, key, destination, *, absent_code='NoSuchKey'):
        current = self.objects.get(key)
        if current is None:
            return subprocess.CompletedProcess([], 254, '', f'An error occurred ({absent_code}) when calling the GetObject operation')
        Path(destination).write_bytes(current['body'])
        meta = dict(ETag=f'"{current["etag"]}"', ContentLength=len(current['body']))
        return subprocess.CompletedProcess([], 0, json.dumps(meta), '')

    def aws(self, *args, check=True):
        cmd = list(args)
        if cmd[:2] != ['s3api', 'put-object'] and cmd[:2] != ['s3api', 'get-object']:
            raise AssertionError('unexpected aws command: ' + ' '.join(cmd))
        bucket = cmd[cmd.index('--bucket') + 1]
        if bucket != self.bucket:
            raise AssertionError('unexpected bucket: ' + bucket)
        key = cmd[cmd.index('--key') + 1]
        if cmd[:2] == ['s3api', 'put-object']:
            body = Path(cmd[cmd.index('--body') + 1]).read_bytes()
            result = self.put(key, body, if_none='--if-none-match' in cmd,
                              if_match=cmd[cmd.index('--if-match') + 1] if '--if-match' in cmd else None)
        else:
            result = self.get(key, cmd[-1])
        if check and result.returncode != 0:
            raise ValueError((result.stderr or result.stdout or 'aws command failed').strip())
        return result
