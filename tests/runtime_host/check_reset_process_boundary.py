#!/usr/bin/env python3
"""Explicit Linux/systemd qualification; never part of workstation unit discovery.

Run as root on an authorized disposable host. Creates only a transient test unit
and private temporary evidence. Does not run Helmr, reset data or call AWS.
"""
import importlib.util
import json
import os
from pathlib import Path
import signal
import pwd
import shutil
import subprocess
import sys
import tempfile
import time
import uuid
from unittest.mock import patch


def wait_until(check):
    end = time.monotonic() + 15
    while time.monotonic() < end:
        if check(): return
        time.sleep(0.1)
    raise RuntimeError('native process boundary did not reach expected state')


def fixture(directory):
    # Even a failed cgroup-containment check must not leave endless test processes.
    deadline = time.monotonic() + 180
    pid = os.fork()
    if pid == 0:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        (directory/'child.pid').write_text(str(os.getpid()))
    while time.monotonic() < deadline: time.sleep(0.1)


def main():
    if len(sys.argv) == 3 and sys.argv[1] == '--fixture':
        fixture(Path(sys.argv[2]))
        return
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise SystemExit('requires an authorized root Linux/systemd test host')
    script = Path(__file__).resolve()
    spec = importlib.util.spec_from_file_location('runtime_host', script.parents[2]/'dev/verification/runtime-host.py')
    host = importlib.util.module_from_spec(spec); spec.loader.exec_module(host)
    # Exercise production supervision properties with a harmless initializer stand-in.
    with patch.object(host.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)) as invoke:
        host.supervise_reset(['apply-services', '--reset-data', '--candidate', '/private/candidate'])
        command = invoke.call_args.args[0]
    name = 'helmr-reset-boundary-test-' + str(uuid.uuid4()) + '.service'
    account = pwd.getpwnam(host.USER)
    runuser = shutil.which('runuser')
    if runuser is None: raise RuntimeError('native runuser is required')
    directory = Path(tempfile.mkdtemp(prefix='helmr-reset-boundary-'))
    os.chown(directory, account.pw_uid, account.pw_gid)
    command[command.index('--unit='+host.RESET_UNIT)] = '--unit='+name
    command = command[:command.index(sys.executable)] + [runuser, '-u', host.USER, '--', sys.executable, str(script), '--fixture', str(directory)]
    runner = None
    stopped = False
    with (directory/'runner.log').open('w') as log:
        try:
            runner = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT)
            wait_until(lambda: (directory/'child.pid').exists())
            child = int((directory/'child.pid').read_text())
            subprocess.run(['systemctl','kill','--kill-whom=main','--signal=KILL',name],check=True,timeout=10)
            os.kill(child, 0)  # the initializer outlives the killed main process
            duplicate = subprocess.run(['systemd-run','--unit='+name,'--service-type=exec',sys.executable,'-c','pass'],capture_output=True,timeout=10)
            if duplicate.returncode == 0:
                raise RuntimeError('a second operation started while the old child survived')
            subprocess.run(['systemctl','stop',name],check=True,timeout=45)
            stopped = True
            def child_finished():
                try: return Path(f'/proc/{child}/stat').read_text().split(') ',1)[1].split()[0] == 'Z'
                except FileNotFoundError: return True
            wait_until(child_finished)
            runner.wait(timeout=10)
            (directory/'result.json').write_text(json.dumps(dict(status='passed',unit=name,child_terminated=True,duplicate_rejected=True))+'\n')
            print(f'Native process-boundary evidence: {directory}')
        finally:
            # Keep evidence even on failure. Never kill by a guessed process name.
            if not stopped:
                subprocess.run(['systemctl','stop',name],check=True,timeout=45)
            if runner is not None: runner.wait(timeout=10)


if __name__ == '__main__': main()
