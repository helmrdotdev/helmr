"""Conservative PR work selection; main release relevance stays in admission.py."""
import json
import os
from pathlib import PurePosixPath
import re
import subprocess
import sys


def source_only(path):
    """Paths covered by source CI without constructing the distribution set.

    Keep the coverage table in README.md in sync. Everything else is full CI,
    including locks, packaging/wiring, shared Go packages and new path classes.
    """
    parts = PurePosixPath(path).parts
    if not parts or path.startswith('/') or '..' in parts:
        return False
    if path == 'README.md' or path.startswith('packages/web/src/content/docs/'):
        return True
    suffix = PurePosixPath(path).suffix
    if path.startswith('packages/console/src/') and suffix in ('.ts', '.tsx', '.css'):
        return True
    if path.startswith('packages/web/src/') and suffix in ('.ts', '.astro', '.css'):
        return True
    if path.startswith('packages/web/public/') and suffix in ('.svg', '.png', '.ico', '.webmanifest'):
        return True
    if path.startswith('internal/controlplane/') and suffix == '.go':
        # Reviewed application CRUD only. Worker contracts also live in files
        # such as worker.go and run_lease_claim_response.go, not one prefix.
        name = path.removeprefix('internal/controlplane/')
        return name in ('project.go', 'organization.go', 'member.go') or (
            '/' not in name and name.endswith('_test.go')
            and name.startswith(('project_', 'organization_', 'member_')))
    return path.startswith('internal/email/') and suffix == '.go'


def classify(paths):
    # An empty/unavailable comparison is not evidence that expensive work is safe
    # to omit. Both names of renames are retained by the caller.
    ordinary = bool(paths) and all(source_only(path) for path in paths)
    # These packaging inputs do not reach the CLI/SDK/guest builder fixtures.
    packaging_only = {
        'internal/console/console.go', 'internal/console/console_embed.go',
        'packages/console/vite.config.ts', 'packages/console/index.html',
        'scripts/build-controlplane-image.sh', 'scripts/verify-controlplane-image-build.sh',
    }
    builder = not paths or any(not source_only(path) and path not in packaging_only for path in paths)
    return dict(artifacts=not ordinary, bundle_builder=builder)


def pr_checks(root, event):
    pr = event['pull_request']
    if any(label['name'] == 'ci:full' for label in pr['labels']):
        return classify([])
    base, head = pr['base']['sha'], pr['head']['sha']
    if not all(re.fullmatch(r'[0-9a-f]{40}', sha) for sha in (base, head)):
        return classify([])
    try:
        # Three-dot comparison covers the complete PR even when its head is behind
        # main. No rename detection means both old and new paths are classified.
        raw = subprocess.check_output([
            'git', '-C', str(root), 'diff', '--no-ext-diff', '--no-textconv',
            '--no-renames', '--name-only', '-z', f'{base}...{head}',
        ], stderr=subprocess.PIPE)
        return classify(raw.decode().rstrip('\0').split('\0') if raw else [])
    except (subprocess.CalledProcessError, UnicodeError):
        return classify([])


def check_jobs(needs, aggregate, event):
    def result(job, expected='success'):
        actual = needs.get(job, {}).get('result')
        if actual != expected:
            raise ValueError(f'{job}: expected {expected}, got {actual}')

    result('artifact-selection')
    outputs = needs['artifact-selection'].get('outputs', {})
    for key in ('skip_artifacts', 'run_bundle_builder'):
        if outputs.get(key) not in ('true', 'false'):
            raise ValueError(f'missing or invalid selection: {key}')
    if aggregate == 'source':
        if event not in ('push', 'pull_request'):
            raise ValueError('unsupported CI event')
        if event == 'push' and outputs['run_bundle_builder'] != 'true':
            raise ValueError('main requires complete source CI')
        for job in ('nix-flake', 'repo', 'postgres', 'browser', 'release-contracts'):
            result(job)
        result('bundle-builder', 'success' if outputs['run_bundle_builder'] == 'true' else 'skipped')
    elif aggregate == 'pr':
        if event != 'pull_request':
            raise ValueError('PR aggregate requires pull_request')
        result('source-ci-complete')
        result('build-artifacts', 'skipped' if outputs['skip_artifacts'] == 'true' else 'success')
    else:
        raise ValueError('unknown CI aggregate')


if __name__ == '__main__':
    check_jobs(json.loads(os.environ['CI_NEEDS']), sys.argv[1], os.environ['GITHUB_EVENT_NAME'])
