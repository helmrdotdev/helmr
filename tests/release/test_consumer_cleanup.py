"""A failed native create does not confer ownership of a context name."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from test_contract import assets
import consumer

ROOT = Path(__file__).resolve().parents[2]


class ContextCleanup(unittest.TestCase):
    def test_shell_failed_create_preserves_context_and_error(self):
        for message in ('context already exists', 'daemon unavailable'):
            with self.subTest(message=message), tempfile.TemporaryDirectory() as directory:
                result = subprocess.run(['bash', '-c', r'''
set -eu
source tests/buildx-fixture.sh
# Record calls independently of cleanup's suppressed stdout/stderr.
env() { printf 'original\n'; }
docker() {
  printf '%s\n' "$*" >> "$WORK/docker-calls"
  case "$1 $2" in
    'context inspect') printf 'unix:///fixture.sock\n' ;;
    'context create') printf '%s\n' "$FAILURE" >&2; return 17 ;;
    *) printf 'UNEXPECTED MUTATION: %s\n' "$*" >&2; return 99 ;;
  esac
}
trap cleanup_buildx_fixture EXIT
start_buildx_fixture "$WORK"
'''], cwd=ROOT, env=dict(os.environ, DOCKER_CONTEXT='original', FAILURE=message, WORK=directory), capture_output=True, text=True)
                self.assertEqual(result.returncode, 17)
                self.assertEqual(result.stderr, message + '\n')
                calls = (Path(directory) / 'docker-calls').read_text().splitlines()
                self.assertEqual([call.split()[:2] for call in calls],
                                 [['context', 'inspect'], ['context', 'create']])

    def test_public_failed_create_preserves_context_and_error(self):
        for message in ('context already exists', 'daemon unavailable'):
            with self.subTest(message=message), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                assets(root)
                error = subprocess.CalledProcessError(17, ['docker', 'context', 'create'], stderr=message)
                def metadata(argv, **kwargs):
                    return 'unix:///fixture.sock\n' if 'inspect' in argv else 'original\n'
                with patch.dict(os.environ, HELMR_PUBLIC_CONSUMER='1', BUILDX_BUILDER=''), \
                     patch.object(consumer.subprocess, 'check_output', side_effect=metadata), \
                     patch.object(consumer, 'run', side_effect=error) as create, \
                     patch.object(consumer.subprocess, 'run') as cleanup:
                    with self.assertRaises(subprocess.CalledProcessError) as raised:
                        consumer.consumer(root, root / 'helmr-darwin-arm64.tar.gz', root / 'work')
                    self.assertIs(raised.exception, error)
                    self.assertEqual(create.call_args.args[:3], ('docker', 'context', 'create'))
                    create.assert_called_once()
                    cleanup.assert_not_called()
