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
from admission import admit, relevant
from transport import assemble, freeze, restore
from publish import complete, discover, download_build, fetch_index, finalize, stage


def output(key, value):
    with open(os.environ['GITHUB_OUTPUT'], 'a') as stream:
        stream.write(f'{key}={value}\n')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('command', choices=('admit', 'restore', 'freeze', 'assemble', 'stage', 'download', 'finalize', 'discover'))
    parser.add_argument('--part')
    parser.add_argument('--directory')
    parser.add_argument('--output')
    args = parser.parse_args()
    # Failed-job reruns may skip admission; enforce formal-tag safety here too.
    if os.environ.get('GITHUB_EVENT_NAME') == 'push' and os.environ.get('GITHUB_REF', '').startswith('refs/tags/v'):
        require(os.environ.get('GITHUB_RUN_ATTEMPT') == '1', 'human tag cohorts are single-attempt')
    api = GitHub()
    selection = json.loads(os.environ.get('RELEASE_SELECTION', '{}'))
    if args.command == 'admit':
        selection = admit(api, os.environ, read(os.environ['GITHUB_EVENT_PATH']), Path.cwd())
        with tempfile.TemporaryDirectory() as temporary:
            done = complete(api, selection, Path(temporary) / 'complete')
            skip = False
            if selection['build']['mode'] == 'main' and not done:
                pointer = api.request('releases/tags/preview', missing=True)
                if pointer:
                    record = json.loads(pointer['body'])
                    release = api.request('releases/tags/' + record['version'])
                    index = fetch_index(api, release, Path(temporary) / 'pointer')
                    from contract import digest
                    require(digest(Path(temporary) / 'pointer/release-index.json') == record['indexDigest'], 'pointer index bytes differ')
                    skip = not relevant(Path.cwd(), index['sourceCommit'], selection['sourceCommit'])
        output('selection', json.dumps(selection, separators=(',', ':')))
        output('complete', str(bool(done)).lower())
        output('skip', str(skip).lower())
    elif args.command == 'restore':
        output('restored', str(restore(api, args.part, selection, args.directory)).lower())
    elif args.command == 'freeze':
        freeze(args.part, args.directory, selection, args.output)
    elif args.command == 'assemble':
        assemble(api, selection, args.directory)
        index = dict(selection, schema='helmr.release.v0', assets={name: descriptor(Path(args.directory) / name) for name in ASSETS})
        # PR CI retains its truthful merge-workflow identity; only publication
        # requires the main/tag certificate identity. All common bytes are checked.
        verify_files(index, args.directory, publishing=False)
    elif args.command == 'stage':
        stage(api, selection, args.directory)
    elif args.command == 'download':
        download_build(api, selection, args.directory)
    elif args.command == 'finalize':
        finalize(api, selection, args.directory)
        with tempfile.TemporaryDirectory() as temporary:
            require(complete(api, selection, Path(temporary) / 'complete') is not None, 'public completion readback failed')
    elif args.command == 'discover':
        discover(api, Path.cwd())


if __name__ == '__main__':
    main()
