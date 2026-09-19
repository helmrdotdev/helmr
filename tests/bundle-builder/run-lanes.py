"""Run all builder lanes on one host with isolated, cancellable process groups."""
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
import os
import signal
import subprocess
import sys
import time

LANES = ("package-workspace", "native", "agentic")


def main(shared, mode):
    if mode not in ("serial", "parallel"):
        raise SystemExit("expected serial or parallel")
    shared = Path(shared)
    lane_dir = Path(__file__).resolve().parent
    running = []
    waiters = ThreadPoolExecutor(max_workers=len(LANES))
    status = 0

    def interrupted(signum, _frame):
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)

    def wait_lane(process):
        return process.wait(), time.monotonic()

    def finish(entry):
        nonlocal status
        lane, _, started, completion = entry
        code, ended = completion.result()
        outcome = f"bundle builder lane={lane} exit={code} duration={ended-started:.2f}s\n"
        (shared / f"{lane}.outcome").write_text(outcome)
        log = (shared / f"{lane}.log").read_text(errors="replace")
        print(log if code == 0 else "\n".join(log.splitlines()[-120:]),
              file=sys.stdout if code == 0 else sys.stderr, flush=True)
        print(outcome, end="", flush=True)
        if code:
            status = code if code > 0 else 128 - code

    try:
        for lane in LANES:
            started = time.monotonic()
            with (shared / f"{lane}.log").open("w") as log:
                process = subprocess.Popen(["bash", str(lane_dir / f"{lane}.sh"), str(shared)],
                                           stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            entry = (lane, process, started, waiters.submit(wait_lane, process))
            running.append(entry)
            if mode == "serial":
                finish(entry)
                running.remove(entry)
        for entry in running:
            finish(entry)
    finally:
        # Signal the whole group, then reap its shell only after its EXIT trap
        # has removed the owned BuildKit builder/context and harness container.
        for _, process, _, _ in running:
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
        for _, process, _, _ in running:
            process.wait()
        waiters.shutdown()
    return status


if __name__ == "__main__":
    raise SystemExit(main(*sys.argv[1:]))
