#!/usr/bin/env python3
"""Prepare explicitly selected fixture directories with local SDK archives."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

SOURCE = Path(__file__).resolve().parent
ROOT = SOURCE.parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path, help='new directory outside the checkout')
    parser.add_argument('--fixtures', nargs='+', required=True, help='fixture directories relative to tests/e2e, e.g. cases/task')
    parser.add_argument('--sdk-packages', type=Path, help='already built SDK/proto tarballs; otherwise build them locally')
    args = parser.parse_args()
    output = args.output.resolve()
    if output.is_relative_to(ROOT):
        parser.error('output must be outside the source checkout')
    for name in args.fixtures:
        path = (SOURCE / name).resolve()
        if not path.is_relative_to(SOURCE) or not path.is_dir():
            parser.error('fixture must be an existing directory inside tests/e2e: ' + name)
    output.mkdir(mode=0o700)
    with tempfile.TemporaryDirectory(prefix='helmr-case-packages-') as tmp:
        packages = args.sdk_packages
        if packages is None:
            subprocess.run([str(ROOT / 'scripts/build-npm-packages.sh')], cwd=ROOT, check=True)
            packages = Path(tmp) / 'packages'
            subprocess.run([str(ROOT / 'scripts/pack-npm-packages.sh'), str(packages)], cwd=ROOT, check=True)
        shutil.copytree(SOURCE, output, dirs_exist_ok=True, ignore=shutil.ignore_patterns('node_modules', 'bun.lock', '__pycache__'))
        target = output / '.packages'
        target.mkdir()
        manifest = json.loads((SOURCE / 'package.json').read_text())
        for name, section in [('sdk', 'dependencies'), ('proto', 'overrides')]:
            archive, = packages.glob(f'helmr-{name}-*.tgz')
            shutil.copyfile(archive, target / archive.name)
            manifest.setdefault(section, {})[f'@helmr/{name}'] = f'file:.packages/{archive.name}'
        (output / 'package.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (output / 'helmr.config.ts').write_text('import { defineConfig } from "@helmr/sdk"\nexport default defineConfig(' + json.dumps({'dirs': args.fixtures, 'ignorePatterns': ['**/run.ts', '**/*.run.ts', '**/*.test.*']}) + ')\n')
    (output / '.helmrignore').write_text('node_modules/\n**/run.ts\n**/*.run.ts\n**/*.test.*\n**/observe.py\n')
    subprocess.run(['bun', 'install', '--ignore-scripts'], cwd=output, check=True)
    print(output)


if __name__ == '__main__':
    main()
