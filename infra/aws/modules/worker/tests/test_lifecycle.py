"""Exercise the rendered lifecycle shell with local provider and worker substitutes."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

TEMPLATE = Path(__file__).resolve().parents[1] / "templates/user-data.sh.tftpl"


class TerminationLifecycle(unittest.TestCase):
    def run_termination(self, state="Terminated", after_state=None, drain_ok=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "state").write_text(state)
            (root / "identity").write_text("i-source")
            (root / "calls").touch()
            script = TEMPLATE.read_text().split("cat >/usr/local/bin/helmr-asg-lifecycle <<'EOF'\n", 1)[1].split("\nEOF", 1)[0]
            script = script.split('case "$${1:-watch}" in', 1)[0]
            values = {
                "autoscaling_group_name": "source-asg", "launch_lifecycle_hook_name": "launch",
                "launch_readiness_timeout_seconds": "10", "termination_lifecycle_hook_name": "terminate",
                "termination_wait_timeout_seconds": "1", "lifecycle_heartbeat_interval_seconds": "0",
                "worker_binary_path": str(root / "worker"), "worker_work_dir": str(root),
                "worker_service_name": "worker.service",
            }
            script = re.sub(r"\$\{([a-z_]+)\}", lambda m: values[m[1]], script)
            script = script.replace("/var/log/helmr-worker-drain.log", str(root / "drain.log"))
            # Override provider transport, retaining the production identity/state
            # checks and complete/fence control flow unchanged.
            script += '''
instance_id() { cat "$TEST_ROOT/identity"; }
target_lifecycle_state() { cat "$TEST_ROOT/state"; }
drain_and_complete_termination
'''
            executables = {
                "curl": '#!/bin/sh\ncase "$*" in *instance-id*) echo i-source;; *) echo \'{"region":"us-east-1"}\';; esac\n',
                "aws": '#!/bin/sh\nprintf "%s\\n" "$*" >> "$TEST_ROOT/calls"\n',
                "worker": '''#!/bin/sh
printf '%s\\n' "$*" >> "$TEST_ROOT/calls"
if [ "$1" = drain ]; then
 [ -z "$AFTER_STATE" ] || printf '%s' "$AFTER_STATE" > "$TEST_ROOT/state"
 exit "$DRAIN_EXIT"
fi
''',
            }
            for name, contents in executables.items():
                path = root / name
                path.write_text(contents)
                path.chmod(0o755)
            result = subprocess.run(["sh", "-c", script], env={**os.environ,
                "PATH": str(root) + os.pathsep + os.environ["PATH"], "TEST_ROOT": str(root),
                "AFTER_STATE": after_state or "", "DRAIN_EXIT": "0" if drain_ok else "1",
            }, capture_output=True, text=True, timeout=10)
            return result, (root / "calls").read_text()

    def test_planned_drain_does_not_enter_loss_path(self):
        result, calls = self.run_termination(state="InService")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, "")

    def test_verified_termination_fences_after_wait_expires(self):
        result, calls = self.run_termination()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("drain --wait-timeout 1s", calls)
        self.assertIn("fence\n", calls)
        self.assertIn("--instance-id i-source --lifecycle-action-result CONTINUE", calls)

    def test_lost_termination_evidence_does_not_fence(self):
        for changes in ({"after_state": "InService"},):
            with self.subTest(changes=changes):
                result, calls = self.run_termination(**changes)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("fence\n", calls)
                self.assertNotIn("complete-lifecycle-action", calls)

    def test_completed_cleanup_does_not_fence(self):
        result, calls = self.run_termination(drain_ok=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("fence\n", calls)
        self.assertIn("complete-lifecycle-action", calls)


if __name__ == "__main__":
    unittest.main()
