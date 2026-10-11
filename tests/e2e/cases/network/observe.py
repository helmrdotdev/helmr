#!/usr/bin/env python3
"""Read the exact Session's retained VM policy and denied-packet counter; no mutation."""
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

session_id = sys.argv[1]
assert str(uuid.UUID(session_id)) == session_id
assert os.geteuid() == 0, 'run on the dedicated host as root'
spec = importlib.util.spec_from_file_location('profile', Path(sys.argv[2]))
profile = importlib.util.module_from_spec(spec)
spec.loader.exec_module(profile)
cfg = json.loads((profile.CONFIG / 'config.json').read_text())
query = """SELECT json_build_object('session_id',s.id,'process_epoch',p.epoch,
 'lease_epoch',l.epoch,'runtime_id',l.computer_instance_id)::text FROM sessions s
 JOIN session_processes p ON p.environment_id=s.environment_id AND p.session_id=s.id
 JOIN computer_leases l ON l.environment_id=s.environment_id AND l.computer_id=s.computer_id AND l.epoch=p.computer_lease_epoch
 JOIN worker_hosts h ON h.id=l.worker_host_id
 WHERE s.id=:'session_id'::uuid AND p.status='ready' AND p.fenced_at IS NULL
 AND l.status='active' AND l.fenced_at IS NULL AND l.expires_at>now()
 AND h.current_epoch=l.worker_epoch AND h.status IN ('active','draining');"""
raw = profile.service_run(cfg['binaries']['psql'], '-h', str(profile.DATA), '-p', '55432', '-d', 'helmr',
    '-X', '-v', 'ON_ERROR_STOP=1', '-v', 'session_id=' + session_id, '-At', input=query,
    capture_output=True, text=True, timeout=10,
    env=dict(os.environ, PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=5000')).stdout
value = json.loads(raw)
runtime = value['runtime_id']
assert str(uuid.UUID(runtime)) == runtime
state = profile.WORKER_DATA / 'vms/guest' / runtime
assert (state / 'owner').read_text() == f'instance\n{runtime}\n'
manifest = json.loads((state / 'network.json').read_text())
assert manifest['owner_id'] == runtime and manifest['namespace_name'] == runtime and manifest['installed']
policy = json.loads(subprocess.check_output(['ip', 'netns', 'exec', runtime, 'nft', '-j', 'list', 'table',
    'inet', 'helmr_network_policy'], text=True, timeout=10))
sets = [x['set'] for x in policy['nftables'] if x.get('set', {}).get('name') == 'blocked_ipv4']
counters = [x['counter'] for x in policy['nftables'] if x.get('counter', {}).get('name') == 'run_denied']
assert len(sets) == len(counters) == 1
prefixes = []
for element in sets[0].get('elem', []):
    if isinstance(element, str):
        prefixes.append(ipaddress.ip_network(element))
    elif 'prefix' in element:
        prefixes.append(ipaddress.ip_network(f"{element['prefix']['addr']}/{element['prefix']['len']}"))
assert any(ipaddress.ip_address('169.254.169.254') in prefix for prefix in prefixes), 'metadata not in native deny set'
value.update(namespace=runtime, metadata_in_deny_set=True, denied_packets=counters[0]['packets'])
print(json.dumps(value))
