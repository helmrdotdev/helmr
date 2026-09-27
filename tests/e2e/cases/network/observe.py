#!/usr/bin/env python3
"""Read the exact Run's retained VM policy and denied-packet counter; no mutation."""
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

run_id = sys.argv[1]
assert str(uuid.UUID(run_id)) == run_id
assert os.geteuid() == 0, 'run on the dedicated host as root'
spec = importlib.util.spec_from_file_location('profile', Path(sys.argv[2]))
profile = importlib.util.module_from_spec(spec)
spec.loader.exec_module(profile)
cfg = json.loads((profile.CONFIG / 'config.json').read_text())
query = """SELECT json_build_object('run_id',r.id,'attempt',r.current_attempt_number,
 'runtime_id',l.computer_instance_id)::text FROM runs r
 JOIN run_leases l ON l.id=r.current_run_lease_id AND l.run_id=r.id
 WHERE r.id=:'run_id'::uuid AND l.terminal_at IS NULL;"""
raw = profile.service_run(cfg['binaries']['psql'], '-h', str(profile.DATA), '-p', '55432', '-d', 'helmr',
    '-X', '-v', 'ON_ERROR_STOP=1', '-v', 'run_id=' + run_id, '-At', input=query,
    capture_output=True, text=True, timeout=10,
    env=dict(os.environ, PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=5000')).stdout
value = json.loads(raw)
assert value['attempt'] == 1
runtime = value['runtime_id']
assert str(uuid.UUID(runtime)) == runtime
state = profile.WORKER_DATA / 'vms/guest' / runtime
assert (state / 'owner').read_text() == f'runtime\n{runtime}\n'
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
