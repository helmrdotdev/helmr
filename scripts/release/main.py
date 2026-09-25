#!/usr/bin/env python3
"""Commands for the fixed common Product release workflow."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
from contract import ASSETS, descriptor, read, require, verify_files, write
from api import GitHub
from admission import admit
from preview_store import preview_mode, stage as preview_stage, verify_public
from transport import assemble, freeze, restore, download_readback
from publish import complete, discover, download_build, finalize, stage


def output(key, value):
    with open(os.environ['GITHUB_OUTPUT'], 'a') as stream:
        stream.write(f'{key}={value}\n')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('command', choices=('admit', 'restore', 'freeze', 'assemble', 'stage', 'verify',
                                              'download', 'finalize', 'discover'))
    parser.add_argument('--part')
    parser.add_argument('--directory')
    parser.add_argument('--output')
    parser.add_argument('--artifact-id')
    parser.add_argument('--artifact-digest')
    parser.add_argument('--publisher-run')
    parser.add_argument('--publisher-attempt')
    parser.add_argument('--build-digest')
    parser.add_argument('--release-id')
    parser.add_argument('--index-digest')
    args = parser.parse_args()
    if os.environ.get('GITHUB_EVENT_NAME') == 'push' and os.environ.get('GITHUB_REF', '').startswith('refs/tags/v'):
        require(os.environ.get('GITHUB_RUN_ATTEMPT') == '1', 'human tag cohorts are single-attempt')
    api = GitHub()
    selection = json.loads(os.environ.get('RELEASE_SELECTION', '{}'))
    if args.command == 'admit':
        event = read(os.environ['GITHUB_EVENT_PATH'])
        selection = admit(api, os.environ, event, Path.cwd())
        with tempfile.TemporaryDirectory() as temporary:
            done = complete(api, selection, Path(temporary) / 'complete')
        output('selection', json.dumps(selection, separators=(',', ':')))
        output('complete', str(bool(done)).lower())
    elif args.command == 'restore':
        output('restored', str(restore(api, args.part, selection, args.directory)).lower())
    elif args.command == 'freeze':
        freeze(args.part, args.directory, selection, args.output)
    elif args.command == 'assemble':
        assemble(api, selection, args.directory)
        index = dict(selection, schema='helmr.release.v0', assets={name: descriptor(Path(args.directory) / name) for name in ASSETS})
        verify_files(index, args.directory, publishing=False)
    elif args.command == 'stage':
        if preview_mode(selection):
            from contract import digest
            output('build_digest', preview_stage(api, selection, args.directory))
            output('publisher_attempt', os.environ['GITHUB_RUN_ATTEMPT'])
        else:
            require(args.output is not None, 'readback output directory required')
            release = stage(api, selection, args.directory)
            download_build(api, selection, args.output, release)
            from contract import digest
            output('release_id', release['id'])
            output('build_digest', digest(Path(args.output) / 'release-build.json'))
            output('publisher_attempt', os.environ['GITHUB_RUN_ATTEMPT'])
    elif args.command == 'verify':
        verify_public(selection, args.directory, args.build_digest)
    elif args.command == 'download':
        download_readback(api, selection, args.directory, args.artifact_id, args.artifact_digest,
                          args.publisher_run, args.publisher_attempt, args.build_digest)
    elif args.command == 'finalize':
        index = finalize(api, selection, args.directory, args.release_id, args.build_digest)
        if preview_mode(selection):
            from contract import digest
            output('index_digest', digest(Path(args.directory) / 'release-index.json'))
    elif args.command == 'discover':
        discover(api, Path.cwd(), selection if selection else None, args.index_digest)


if __name__ == '__main__':
    main()
