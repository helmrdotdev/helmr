"""Exercise the rendered lifecycle shell with local provider and worker substitutes."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

TEMPLATE = Path(__file__).resolve().parents[1] / "templates/user-data.sh.tftpl"


class TerminationLifecycle(unittest.TestCase):
    def run_watch(self, state="InService", fail_credentials=False, expired=False, transient_imds=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "state").write_text(state)
            (root / "calls").touch()
            (root / "clock").write_text("100" if expired else "0")
            if expired:
                (root / "launch-started").write_text("0")
            if fail_credentials:
                (root / "credentials-fail").touch()
            if transient_imds:
                (root / "imds-fail").touch()
            script = TEMPLATE.read_text().split("cat >/usr/local/bin/helmr-asg-lifecycle <<'EOF'\n", 1)[1].split("\nEOF", 1)[0]
            values = {
                "autoscaling_group_name": "source-asg", "launch_lifecycle_hook_name": "launch",
                "launch_readiness_timeout_seconds": "10", "termination_lifecycle_hook_name": "terminate",
                "termination_wait_timeout_seconds": "1", "lifecycle_heartbeat_interval_seconds": "0",
                "worker_binary_path": str(root / "worker"), "worker_work_dir": str(root),
                "worker_service_name": "worker",
            }
            script = re.sub(r"\$\{([a-z_]+)\}", lambda m: values[m[1]], script).replace("$${", "${")
            script = script.replace("/run/helmr-launch-started-at", str(root / "launch-started"))
            commands = {
                "curl": '''#!/bin/sh
case "$*" in
 *target-lifecycle-state*)
  if [ -f "$TEST_ROOT/imds-fail" ]; then rm "$TEST_ROOT/imds-fail"; exit 22; fi
  cat "$TEST_ROOT/state";;
 *instance-id*) echo i-source;;
 *) echo '{"region":"us-east-1"}';;
esac
''',
                "systemctl": '''#!/bin/sh
printf 'systemctl %s\\n' "$*" >>"$TEST_ROOT/calls"
case "$1" in
 start)
  if [ -f "$TEST_ROOT/credentials-fail" ]; then
   rm "$TEST_ROOT/credentials-fail"
   echo credentials-failed >>"$TEST_ROOT/calls"
  else
   echo credentials-ready >>"$TEST_ROOT/calls"
   touch "$TEST_ROOT/ready"
  fi;;
 is-active) test -f "$TEST_ROOT/ready";;
esac
''',
                "worker": '#!/bin/sh\necho worker-status >>"$TEST_ROOT/calls"\ntest -f "$TEST_ROOT/ready"\n',
                "aws": '''#!/bin/sh
printf '%s\\n' "$*" >>"$TEST_ROOT/calls"
case "$*" in
 *complete-lifecycle-action*CONTINUE*) echo Terminated >"$TEST_ROOT/state";;
esac
''',
                "date": '#!/bin/sh\ncat "$TEST_ROOT/clock"\n',
                "sleep": '''#!/bin/sh
clock=$(cat "$TEST_ROOT/clock")
test "$clock" -lt 15 || exit 1
echo $((clock + 1)) >"$TEST_ROOT/clock"
''',
            }
            for name, contents in commands.items():
                (root / name).write_text(contents)
                (root / name).chmod(0o755)
            result = subprocess.run(["sh", "-c", script], env={**os.environ,
                "PATH": str(root) + os.pathsep + os.environ["PATH"], "TEST_ROOT": str(root),
            }, capture_output=True, text=True, timeout=10)
            return result, (root / "calls").read_text()

    def test_watch_retries_credential_failure_before_completing_launch(self):
        result, calls = self.run_watch(fail_credentials=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls.count("systemctl start --no-block worker.service"), 2)
        self.assertLess(calls.index("credentials-failed"), calls.index("record-lifecycle-action-heartbeat"))
        self.assertLess(calls.index("credentials-ready"), calls.index("complete-lifecycle-action"))

    def test_watch_retries_transient_metadata_without_starting_early(self):
        result, calls = self.run_watch(transient_imds=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertLess(calls.index("record-lifecycle-action-heartbeat"), calls.index("systemctl start"))

    def test_watch_reuses_boot_deadline_and_abandons_expired_launch(self):
        result, calls = self.run_watch(state="", expired=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--lifecycle-action-result ABANDON", calls)
        self.assertNotIn("systemctl start", calls)

    def test_watch_warm_entry_does_not_start_worker(self):
        result, calls = self.run_watch(state="Warmed:Stopped")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("systemctl", calls)
        self.assertIn("--lifecycle-hook-name launch", calls)

    def test_watch_observes_termination_after_boot_launch_deadline(self):
        for state in ("Terminated", "Warmed:Terminated"):
            with self.subTest(state=state):
                result, calls = self.run_watch(state=state, expired=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("--lifecycle-hook-name terminate", calls)
                self.assertNotIn("--lifecycle-hook-name launch", calls)
                self.assertNotIn("systemctl start", calls)

    def test_worker_restart_keeps_ami_failure_retry_and_nonfatal_credentials_dependency(self):
        template = TEMPLATE.read_text()
        self.assertIn("Wants=helmr-worker-enrollment-token.service", template)
        self.assertNotIn("Requires=helmr-worker-enrollment-token.service", template)
        self.assertNotIn("ExecCondition=", template)
        self.assertIn("systemctl disable '${worker_service_name}'", template)
        image = TEMPLATE.parents[2] / "worker-image/templates/install-worker-host.sh"
        self.assertIn("Restart=on-failure", image.read_text())

    def run_termination(self, state="Terminated", after_state=None, drain_ok=False, fence_ok=True, enrolled=True, action="drain_and_complete_termination"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "state").write_text(state)
            (root / "identity").write_text("i-source")
            (root / "calls").touch()
            if enrolled:
                (root / "worker-host-secret.json").touch()
            script = TEMPLATE.read_text().split("cat >/usr/local/bin/helmr-asg-lifecycle <<'EOF'\n", 1)[1].split("\nEOF", 1)[0]
            script = script.split('case "$${1:-watch}" in', 1)[0]
            values = {
                "autoscaling_group_name": "source-asg", "launch_lifecycle_hook_name": "launch",
                "launch_readiness_timeout_seconds": "10", "termination_lifecycle_hook_name": "terminate",
                "termination_wait_timeout_seconds": "1", "lifecycle_heartbeat_interval_seconds": "0",
                "worker_binary_path": str(root / "worker"), "worker_work_dir": str(root),
                "worker_service_name": "worker",
            }
            script = re.sub(r"\$\{([a-z_]+)\}", lambda m: values[m[1]], script)
            script = script.replace("/var/log/helmr-worker-drain.log", str(root / "drain.log"))
            # Override provider transport, retaining the production identity/state
            # checks and complete/fence control flow unchanged.
            script += '''
instance_id() { cat "$TEST_ROOT/identity"; }
target_lifecycle_state() { cat "$TEST_ROOT/state"; }
'''+action+'\n'
            executables = {
                "curl": '#!/bin/sh\ncase "$*" in *instance-id*) echo i-source;; *) echo \'{"region":"us-east-1"}\';; esac\n',
                "systemctl": '#!/bin/sh\nprintf "systemctl %s\\n" "$*" >> "$TEST_ROOT/calls"\n',
                "aws": '#!/bin/sh\nprintf "%s\\n" "$*" >> "$TEST_ROOT/calls"\n',
                "worker": '''#!/bin/sh
printf '%s\\n' "$*" >> "$TEST_ROOT/calls"
if [ "$1" = drain ]; then
 [ -z "$AFTER_STATE" ] || printf '%s' "$AFTER_STATE" > "$TEST_ROOT/state"
 exit "$DRAIN_EXIT"
fi
if [ "$1" = fence ]; then exit "$FENCE_EXIT"; fi
''',
            }
            for name, contents in executables.items():
                path = root / name
                path.write_text(contents)
                path.chmod(0o755)
            result = subprocess.run(["sh", "-c", script], env={**os.environ,
                "PATH": str(root) + os.pathsep + os.environ["PATH"], "TEST_ROOT": str(root),
                "AFTER_STATE": after_state or "", "DRAIN_EXIT": "0" if drain_ok else "1",
                "FENCE_EXIT": "0" if fence_ok else "1",
            }, capture_output=True, text=True, timeout=10)
            return result, (root / "calls").read_text()

    def test_warm_entry_completes_without_enrollment(self):
        result, calls = self.run_termination(state="Warmed:Stopped", enrolled=False, action="complete_launch")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--lifecycle-hook-name launch", calls)
        self.assertNotIn("systemctl", calls)
        self.assertNotIn("status", calls)

    def test_resumed_launch_starts_worker_before_completing_hook(self):
        result, calls = self.run_termination(state="InService", action="complete_launch")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertLess(calls.index("systemctl start --no-block worker.service"), calls.index("status"))
        self.assertLess(calls.index("status"), calls.index("complete-lifecycle-action"))

    def test_unknown_boot_target_does_not_start_or_complete(self):
        result, calls = self.run_termination(state="", enrolled=False, action="complete_launch")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, "")

    def test_unused_warm_termination_does_not_drain_or_fence(self):
        result, calls = self.run_termination(state="Warmed:Terminated", enrolled=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("drain", calls)
        self.assertNotIn("fence", calls)
        self.assertIn("--lifecycle-hook-name terminate", calls)

    def test_boot_credentials_are_refreshed_after_stopped_standby(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            script=TEMPLATE.read_text().split("cat >/usr/local/sbin/helmr-worker-enrollment-token <<'EOF'\n",1)[1].split("\nEOF",1)[0]
            script=script.replace("${aws_region}","us-east-1").replace("${checkpoint_key_secret_arn}","checkpoint").replace("${worker_enrollment_token_secret_arn}","enrollment")
            script=script.replace("-o root -g root ","").replace("/etc/helmr",str(root / "etc")).replace("/run/helmr",str(root / "run"))
            (root / "etc").mkdir();(root / "run").mkdir()
            (root / "etc/worker-base.env").write_text("WORKER_POOL_NAME=pool\n")
            (root / "aws").write_text('#!/bin/sh\nif [ -f "$TEST_ROOT/secret-fail" ]; then rm "$TEST_ROOT/secret-fail"; exit 1; fi\ncase "$*" in *checkpoint*) echo "key-$ROTATION";; *) echo "token-$ROTATION";; esac\n')
            (root / "curl").write_text('#!/bin/sh\ncase "$*" in *instance-id*) echo i-current;; *) echo imds-token;; esac\n')
            for name in ("aws","curl"):(root / name).chmod(0o755)
            environment={**os.environ,"PATH":str(root)+os.pathsep+os.environ["PATH"],"TEST_ROOT":str(root)}
            (root / "secret-fail").touch()
            failed=subprocess.run(["sh","-c",script],env={**environment,"ROTATION":"first-attempt"},capture_output=True,text=True)
            self.assertNotEqual(failed.returncode,0)
            self.assertFalse((root / "etc/worker.env").exists())
            for rotation in ("before-stop","after-wake"):
                result=subprocess.run(["sh","-c",script],env={**environment,"ROTATION":rotation},capture_output=True,text=True)
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertIn("CHECKPOINT_ENCRYPTION_KEY=key-"+rotation,(root / "etc/worker.env").read_text())
                self.assertEqual((root / "run/worker-enrollment-token").read_text(),"token-"+rotation)
                self.assertEqual((root / "etc/worker.env").stat().st_mode & 0o777,0o600)

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

    def test_failed_fence_stops_watcher_restarts_without_completing_termination(self):
        result, calls = self.run_termination(fence_ok=False)
        self.assertIn("fence\n", calls)
        self.assertNotIn("complete-lifecycle-action", calls)
        self.assertEqual(result.returncode, 42, result.stderr)
        service = TEMPLATE.read_text().split("cat >/etc/systemd/system/helmr-asg-lifecycle.service <<'EOF'\n", 1)[1].split("\nEOF", 1)[0]
        self.assertIn("RestartPreventExitStatus=42", service)
        self.assertIn("Restart=always", service)

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
