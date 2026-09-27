#!/usr/bin/env python3
"""Prepare a self-contained case project using local SDK packages, without publishing."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess

SOURCE = Path(__file__).resolve().parent
ROOT = SOURCE.parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path, help='new project directory outside the checkout')
    output = parser.parse_args().output.resolve()
    if output.is_relative_to(ROOT):
        parser.error('output must be outside the source checkout')
    output.mkdir(mode=0o700)
    subprocess.run([str(ROOT / 'scripts/build-npm-packages.sh')], cwd=ROOT, check=True)
    packages = output / '.packages'
    subprocess.run([str(ROOT / 'scripts/pack-npm-packages.sh'), str(packages)], cwd=ROOT, check=True)
    for name in ['tasks', 'cases']:
        shutil.copytree(SOURCE / name, output / name)
    for name in ['helmr.config.ts', 'tsconfig.json', 'runtime-host.py', 'persistence.sql']:
        shutil.copyfile(SOURCE / name, output / name)
    manifest = json.loads((SOURCE / 'package.json').read_text())
    for name, section in [('sdk', 'dependencies'), ('proto', 'overrides')]:
        archive, = packages.glob(f'helmr-{name}-*.tgz')
        manifest[section][f'@helmr/{name}'] = f'file:.packages/{archive.name}'
    (output / 'package.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (output / '.helmrignore').write_text('node_modules/\ncases/\nruntime-host.py\npersistence.sql\n')
    subprocess.run(['bun', 'install'], cwd=output, check=True)
    print(output)


if __name__ == '__main__':
    main()
