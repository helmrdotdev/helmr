"""Fast PR source feedback; full distribution acceptance belongs to main."""
import json
import os
from pathlib import PurePosixPath
import re
import subprocess
import sys


# Keep this inventory and the coverage table in README.md together. Matrix names
# remain stable for native job evidence; the policy job always runs.
REPO_CHECKS = {
    'ci-policy': 'repo policy',
    'ci-generated': 'generated',
    'ci-typescript': 'typescript',
    'ci-go-lint': 'go lint',
    'ci-go-build': 'go build',
    'ci-go-race': 'go race',
    'ci-linux-compile': 'linux compile',
    'ci-firecracker-probe': 'Firecracker probe',
    'ci-linux-lint': 'linux lint',
    'ci-infra-test': 'infrastructure',
    'ci-clickhouse': 'ClickHouse',
}
SOURCE_JOBS = ('nix-flake', 'postgres', 'browser', 'release-contracts')
ALL_CHECKS = frozenset(REPO_CHECKS) | frozenset(SOURCE_JOBS)
PR_CHECKS = frozenset(REPO_CHECKS) | {'postgres', 'browser'}
WEB_CHECKS = frozenset(('ci-policy', 'ci-typescript'))
CONSOLE_CHECKS = WEB_CHECKS | frozenset(('ci-generated', 'ci-go-build', 'browser'))
BACKEND_CHECKS = frozenset((
    'ci-policy', 'ci-generated', 'ci-go-lint', 'ci-go-build', 'ci-go-race',
    'ci-linux-compile', 'ci-linux-lint', 'ci-clickhouse', 'postgres', 'browser',
))


def path_checks(path):
    """Return affected source checks, or None for broad PR source coverage.

    Backend logic/SQL is exercised by Go, real databases and browser acceptance.
    Builder fixtures do not run a Control Plane or query its database. Shared
    runtime/client packages, build wiring and new file types keep broad source tests.
    """
    parts = PurePosixPath(path).parts
    if not parts or path.startswith('/') or '..' in parts:
        return None
    suffix = PurePosixPath(path).suffix
    if path == 'README.md':
        return frozenset(('ci-policy',))
    if path.startswith('packages/web/src/content/docs/') and suffix in ('.md', '.mdx'):
        return WEB_CHECKS
    if path.startswith('packages/console/src/') and suffix in ('.ts', '.tsx', '.css'):
        return CONSOLE_CHECKS
    if path.startswith('packages/web/src/') and suffix in ('.ts', '.astro', '.css'):
        return WEB_CHECKS
    if path.startswith('packages/web/public/') and suffix in ('.svg', '.png', '.ico', '.webmanifest'):
        return WEB_CHECKS
    if path.startswith(('internal/controlplane/', 'internal/email/')) and suffix == '.go':
        return BACKEND_CHECKS
    if path.startswith('internal/db/') and suffix in ('.go', '.sql'):
        return BACKEND_CHECKS
    return None


def full_checks():
    """Main and the explicit PR override keep all deep and packaging checks."""
    return dict(artifacts=True, bundle_builder=True, source_checks=sorted(ALL_CHECKS))


def classify(paths):
    # Unknown/missing inputs broaden source coverage, never silently opt a PR
    # into release construction. Main proves distribution and deep E2E behavior.
    checks = set()
    for path in paths:
        selected = path_checks(path)
        checks.update(PR_CHECKS if selected is None else selected)
    if not paths:
        checks.update(PR_CHECKS)
    return dict(artifacts=False, bundle_builder=False, source_checks=sorted(checks))


def repo_matrix(checks):
    return dict(include=[dict(name=name, app=app) for app, name in REPO_CHECKS.items() if app in checks])


def pr_checks(root, event):
    pr = event['pull_request']
    if any(label['name'] == 'ci:full' for label in pr['labels']):
        return full_checks()
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
    try:
        checks = json.loads(outputs['source_checks'])
    except (KeyError, TypeError, ValueError) as error:
        raise ValueError('missing or invalid source_checks') from error
    if (not isinstance(checks, list) or any(not isinstance(check, str) for check in checks)
            or len(set(checks)) != len(checks) or not set(checks) <= ALL_CHECKS
            or 'ci-policy' not in checks):
        raise ValueError('invalid source_checks')
    # Full packaging and builder validation must never accompany reduced source
    # coverage; main also retains all checks for documentation-only pushes.
    if ((outputs['skip_artifacts'] == 'false' or outputs['run_bundle_builder'] == 'true')
            and set(checks) != ALL_CHECKS):
        raise ValueError('full work requires complete source CI')
    if aggregate == 'source':
        if event not in ('push', 'pull_request'):
            raise ValueError('unsupported CI event')
        if event == 'push' and (outputs['run_bundle_builder'] != 'true' or set(checks) != ALL_CHECKS):
            raise ValueError('main requires complete source CI')
        result('repo')
        for job in SOURCE_JOBS:
            result(job, 'success' if job in checks else 'skipped')
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
