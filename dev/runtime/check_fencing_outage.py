#!/usr/bin/env python3
"""Qualify dead-Worker fencing across a real CP outage on the exclusive dev host.

Run only under the dedicated host's existing fault-injection authorization, after
ordinary case fixtures have been reclaimed. This kills the idle Worker, keeps
Dispatcher alive, and restores CP and Worker before returning.
"""
import argparse
from datetime import datetime
import fcntl
import json
import os
import signal
from pathlib import Path
import time

import host


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise RuntimeError('requires root on the dedicated runtime host')
    args.evidence.mkdir(mode=0o700)
    report = {'passed': False, 'samples': [], 'restored': False}
    cfg = json.loads((host.CONFIG / 'config.json').read_text())
    with open('/run/helmr-verification.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        inspected = host.inspect_profile(cfg)
        if inspected['blockers']:
            raise RuntimeError('profile is not ready: ' + ', '.join(inspected['blockers']))
        resource = cfg['worker']['WORKER_RESOURCE_ID']

        def observed():
            values = host.read_observation(cfg, 'worker-state', {'resource_ids': [resource]})
            if len(values) != 1:
                raise RuntimeError('expected exactly one current Host')
            return values[0]

        before = observed()
        if before['status'] != 'active' or before['active_leases'] or before['unfenced_preparations']:
            raise RuntimeError('requires an active idle Host with all fixtures reclaimed')
        report['before'] = before
        receipt = args.evidence / 'result.json'
        receipt.write_text(json.dumps(report, indent=2) + '\n')
        def interrupted(signum, frame):
            raise RuntimeError(f'interrupted by signal {signum}')
        signal.signal(signal.SIGTERM, interrupted)
        dispatcher = host.service_identity(host.unit('dispatcher'))['InvocationID']
        try:
            host.run('systemctl', 'stop', host.unit('control-plane'))
            host.run('systemctl', 'kill', '--kill-whom=all', '--signal=SIGKILL', 'helmr-worker.service')
            host.wait_for(lambda: host.state('helmr-worker.service') == 'failed', 'killed Worker', seconds=30)
            stopped = time.monotonic()
            report['outage_started_at'] = time.time()
            while time.monotonic() - stopped < 150:
                require_dispatcher(dispatcher)
                if host.state(host.unit('control-plane')) != 'inactive' or host.state('helmr-worker.service') != 'failed':
                    raise RuntimeError('fault state changed during outage')
                value = observed()
                if value['id'] != before['id'] or value['status'] != 'active':
                    raise RuntimeError('Host fenced while CP was unavailable')
                report['samples'].append({'phase': 'outage', 'elapsed': time.monotonic()-stopped, 'host': value})
                time.sleep(2)
            recovery_requested_at = time.time()
            host.run('systemctl', 'start', host.unit('control-plane'))
            recovered = time.monotonic()
            host.wait_for(lambda: host.http_ready('http://127.0.0.1:58080/readyz'), 'CP readiness', unit_name=host.unit('control-plane'))
            while time.monotonic() - recovered < 240:
                require_dispatcher(dispatcher)
                if host.state('helmr-worker.service') != 'failed':
                    raise RuntimeError('dead Worker restarted before fencing')
                value = observed()
                elapsed = time.monotonic() - recovered
                report['samples'].append({'phase': 'recovery', 'elapsed': elapsed, 'host': value})
                if value['id'] != before['id']:
                    raise RuntimeError('Host identity changed before fencing')
                if value['status'] == 'lost':
                    raw = host.run('journalctl', '_SYSTEMD_INVOCATION_ID=' + dispatcher,
                                   '--since', '@' + str(recovery_requested_at), '--no-pager', '-o', 'cat',
                                   capture_output=True, text=True).stdout
                    resumed = []
                    for line in raw.splitlines():
                        try:
                            entry = json.loads(line)
                        except json.JSONDecodeError:
                            continue
                        if isinstance(entry, dict) and entry.get('msg') == 'stale worker fencing resumed':
                            resumed.append(entry)
                    if len(resumed) != 1:
                        raise RuntimeError('missing or ambiguous Dispatcher recovery-window evidence')
                    healthy_since = datetime.fromisoformat(resumed[0]['healthy_since']).timestamp()
                    lost_at = datetime.fromisoformat(value['lost_at']).timestamp()
                    if healthy_since < recovery_requested_at or lost_at - healthy_since < 120:
                        raise RuntimeError('Host fenced before a new 120-second healthy observation window')
                    report['fencing_resumed'] = resumed[0]
                    report['healthy_window_seconds'] = lost_at - healthy_since
                    report['passed'] = True
                    break
                if value['status'] != 'active':
                    raise RuntimeError('unexpected Host state after recovery')
                time.sleep(2)
            if not report['passed']:
                raise RuntimeError('dead Host was not fenced after CP recovered')
        except Exception as error:
            report['failure'] = str(error)
            raise
        finally:
            try:
                host.run('systemctl', 'start', host.unit('control-plane'))
                host.wait_for(lambda: host.http_ready('http://127.0.0.1:58080/readyz'), 'CP restoration', unit_name=host.unit('control-plane'))
                host.run('systemctl', 'start', 'helmr-worker.service')
                host.wait_for(lambda: host.run('/usr/local/bin/worker', 'status', env=dict(os.environ) | cfg['worker'], capture_output=True).returncode == 0,
                              'Worker restoration', seconds=300, unit_name='helmr-worker.service')
                report['restored'] = True
            except Exception as error:
                report['passed'] = False
                report['restoration_failure'] = str(error)
                raise
            finally:
                receipt.write_text(json.dumps(report, indent=2) + '\n')


def require_dispatcher(invocation):
    if host.state(host.unit('dispatcher')) != 'active' or host.service_identity(host.unit('dispatcher'))['InvocationID'] != invocation:
        raise RuntimeError('Dispatcher did not remain continuously active')


if __name__ == '__main__':
    main()
