"""Exercise the real lane scheduler with small processes, without Docker."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parent
LANES = ("package-workspace", "native", "agentic")


class BundleBuilderLanes(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        shutil.copy(ROOT / "bundle-builder/run-lanes.py", self.root)
        for lane in LANES:
            (self.root / f"{lane}.sh").write_text(r'''#!/usr/bin/env bash
set -eu
work=$1
lane=$(basename "$0" .sh)
trap 'touch "$work/$lane.cleaned"' EXIT
trap 'exit 143' TERM
printf '%s\n' "$$" >"$work/$lane.pid"
if [ "${CANCEL:-0}" = 1 ]; then
  sleep 30 &
  echo "$!" >"$work/$lane.child"
  wait
elif [ "${MODE}" = parallel ]; then
  # A barrier proves all independent lanes overlap; a serial launch times out.
  for ignored in $(seq 1 200); do
    [ "$(find "$work" -name '*.pid' | wc -l)" -eq 3 ] && break
    sleep 0.01
  done
  [ "$(find "$work" -name '*.pid' | wc -l)" -eq 3 ]
elif [ "$lane" = native ]; then
  [ -f "$work/package-workspace.cleaned" ]
elif [ "$lane" = agentic ]; then
  [ -f "$work/native.cleaned" ]
fi
if [ "$lane" = "${FAIL_LANE:-}" ]; then exit 17; fi
echo "passed $lane"
''')

    def command(self, mode):
        return [sys.executable, str(self.root / "run-lanes.py"), str(self.root), mode]

    def test_all_lanes_run_and_failures_propagate(self):
        for mode in ("serial", "parallel"):
            for failed in ("", *LANES):
                with self.subTest(mode=mode, failed=failed):
                    for pattern in ("*.pid", "*.cleaned", "*.outcome"):
                        for path in self.root.glob(pattern):
                            path.unlink()
                    result = subprocess.run(self.command(mode), env=dict(os.environ, MODE=mode, FAIL_LANE=failed),
                                            text=True, capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 17 if failed else 0, result.stdout + result.stderr)
                    for lane in LANES:
                        self.assertTrue((self.root / f"{lane}.cleaned").exists())
                        self.assertIn(f"lane={lane} exit={17 if lane == failed else 0}", result.stdout)

    def test_cancellation_reaps_lane_processes_and_runs_cleanup(self):
        process = subprocess.Popen(self.command("parallel"), env=dict(os.environ, MODE="parallel", CANCEL="1"),
                                   text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 10
            while len(list(self.root.glob("*.child"))) != 3:
                self.assertLess(time.monotonic(), deadline)
                time.sleep(0.01)
            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=10)
            self.assertEqual(process.returncode, 143, stdout + stderr)
            for lane in LANES:
                self.assertTrue((self.root / f"{lane}.cleaned").exists())
                for suffix in ("pid", "child"):
                    pid = int((self.root / f"{lane}.{suffix}").read_text())
                    with self.assertRaises(ProcessLookupError):
                        os.kill(pid, 0)
        finally:
            if process.poll() is None:
                process.terminate()
                process.communicate(timeout=10)


if __name__ == "__main__":
    unittest.main()
