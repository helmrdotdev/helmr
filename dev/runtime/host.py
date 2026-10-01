#!/usr/bin/env python3
"""Compose real Helmr services on one dedicated Linux/KVM host.

No provisioning, artifact publishing, or cloud destruction. See README.md.
"""
import argparse
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import pwd
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

CONFIG = Path('/etc/helmr/verification')
DATA = Path('/var/lib/helmr-verification')
USER = 'helmr-services'
SERVICES = ['postgres', 'redis', 'clickhouse', 'control-plane', 'dispatcher']
WORKER_DATA = Path('/var/lib/helmr/verification-worker')
JAILER_DATA = Path('/var/lib/helmr/jailer')


def run(*args, **kwargs):
    return subprocess.run(args, check=True, timeout=kwargs.pop('timeout', 120), **kwargs)


def unit(name):
    return f'helmr-verification-{name}.service'


def environment(values):
    lines = []
    for key, value in sorted(values.items()):
        if not re.fullmatch(r'[A-Z][A-Z0-9_]*', key) or not isinstance(value, str) or any(c in value for c in '\n\r\0'):
            raise ValueError('environment must contain uppercase names and single-line string values')
        # EnvironmentFile is data, never shell source. Percent is literal here.
        lines.append(key + '="' + value.replace('\\', '\\\\').replace('"', '\\"') + '"')
    return '\n'.join(lines) + '\n'


def merge_owned(supplied, owned):
    if supplied.keys() & owned.keys():
        raise ValueError('profile-owned environment keys cannot be overridden: ' + ', '.join(sorted(supplied.keys() & owned.keys())))
    return supplied | owned


def compile_config(raw):
    """Generate stable per-scope credentials once, without accessing a host."""
    if set(raw) != {'binaries', 'control_plane', 'worker', 'services_candidate', 'worker_host_receipt', 'worker_runtime_receipt'}:
        raise ValueError('expected binaries, control_plane, worker, services_candidate, worker_host_receipt, worker_runtime_receipt')
    binaries = raw['binaries']
    if set(binaries) != {'postgres', 'initdb', 'psql', 'redis-server', 'clickhouse'}:
        raise ValueError('binaries must name all backing-service tools')
    for value in binaries.values():
        if not re.fullmatch(r'/[A-Za-z0-9_./+-]+', value) or '..' in Path(value).parts:
            raise ValueError('binary paths must be absolute, without whitespace or systemd expansions')
    binaries = binaries | {name: str(CONFIG / 'bin' / name) for name in ['control-plane', 'dispatcher']}
    cp = raw['control_plane'].copy()
    for key in ['CAS_URI', 'PLATFORM_STORE_URI', 'DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH',
                'GITHUB_OAUTH_CLIENT_ID', 'GITHUB_OAUTH_CLIENT_SECRET', 'BOOTSTRAP_WORKER_TOKEN']:
        if not cp.get(key):
            raise ValueError(f'{key} is required')
    for key in ['CAS_URI', 'PLATFORM_STORE_URI']:
        if not cp[key].startswith('s3://'):
            raise ValueError(f'{key} must use real S3')
    if cp['CAS_URI'] == cp['PLATFORM_STORE_URI']:
        raise ValueError('CAS and platform stores must be distinct')
    db_password = secrets.token_hex(32)
    owned = {
        'DEPLOYMENT_MODE': 'self-hosted', 'DATABASE_URL': f'postgres://helmr:{db_password}@127.0.0.1:55432/helmr?sslmode=disable',
        'REDIS_URL': 'redis://127.0.0.1:56379/0', 'CLICKHOUSE_URL': 'http://127.0.0.1:58123',
        'CONTROL_PLANE_ADDR': '127.0.0.1:58080', 'PUBLIC_URL': 'http://127.0.0.1:58080',
        'API_ORIGIN': 'http://127.0.0.1:58080', 'BOOTSTRAP_ENABLED': 'true',
        'BOOTSTRAP_REGION_ID': 'default', 'BOOTSTRAP_WORKER_GROUP_NAME': 'default',
        'SETUP_TOKEN': secrets.token_urlsafe(32), 'COMPUTER_WRAPPING_KEY_ID': 'verification',
        'EMAIL_PROVIDER': 'none',
    }
    for key in ['AUTH_KEY', 'TOKEN_CREDENTIAL_KEY', 'COMPUTER_FENCING_KEY', 'ENCRYPTION_KEY',
                'WORKER_HOST_CREDENTIAL_SIGNING_KEY', 'COMPUTER_WRAPPING_KEY']:
        owned[key] = base64.b64encode(secrets.token_bytes(32)).decode()
    ch = {}
    for role in ['BOOTSTRAP', 'READER', 'INGESTER', 'MIGRATION']:
        ch[f'CLICKHOUSE_{role}_USER'] = role.lower()
        ch[f'CLICKHOUSE_{role}_PASSWORD'] = secrets.token_hex(32)
    cp = merge_owned(cp, owned | {'CLICKHOUSE_USER': ch['CLICKHOUSE_READER_USER'], 'CLICKHOUSE_PASSWORD': ch['CLICKHOUSE_READER_PASSWORD']})
    worker = raw['worker'].copy()
    for key in ['WORKER_RESOURCE_ID', 'WORKER_COMPUTER_DEVICES', 'WORKER_NETWORK_LINK_POOL',
                'WORKER_NETWORK_TRANSLATION_POOL', 'WORKER_NETWORK_RESOLVER_IPV4', 'WORKER_NETWORK_BLOCKED_IPV4_CIDRS']:
        if not worker.get(key):
            raise ValueError(f'{key} is required')
    devices = worker['WORKER_COMPUTER_DEVICES'].split()
    if not devices or len(set(devices)) != len(devices) or any(not re.fullmatch(r'/dev/nbd[0-9]+', d) for d in devices):
        raise ValueError('supply distinct operator-owned /dev/nbdN devices')
    worker = merge_owned(worker, {
        'CONTROL_PLANE_URL': 'http://127.0.0.1:58080', 'WORKER_POOL_NAME': 'default',
        'CAS_URI': cp['CAS_URI'], 'PLATFORM_STORE_URI': cp['PLATFORM_STORE_URI'],
        'WORKER_ENROLLMENT_TOKEN_FILE': str(CONFIG / 'enrollment-token'),
        'WORKER_WORK_DIR': str(WORKER_DATA), 'WORKER_IMAGES_DIR': '/var/lib/helmr/images',
        'WORKER_HOST_SECRET_PATH': str(WORKER_DATA / 'worker-host-secret.json'),
        'JAILER_CHROOT_DIR': '/var/lib/helmr/jailer', 'JAILER_UID': '1001', 'JAILER_GID': '1001',
        'FIRECRACKER_PATH': '/usr/local/bin/firecracker', 'JAILER_PATH': '/usr/local/bin/jailer',
        'CPU_TEMPLATE_HELPER_PATH': '/usr/local/bin/cpu-template-helper',
        'CHECKPOINT_ENCRYPTION_KEY': base64.b64encode(secrets.token_bytes(32)).decode(),
    })
    worker.setdefault('WORKER_COMPUTER_SAVE_EVERY', '30s')
    dispatcher = {key: cp[key] for key in ['DATABASE_URL', 'CLICKHOUSE_URL', 'COMPUTER_FENCING_KEY', 'ENCRYPTION_KEY']}
    dispatcher.update(CLICKHOUSE_USER=ch['CLICKHOUSE_INGESTER_USER'], CLICKHOUSE_PASSWORD=ch['CLICKHOUSE_INGESTER_PASSWORD'])
    for values in [cp, worker, dispatcher, ch]:
        environment(values)
    return dict(binaries=binaries, control_plane=cp, worker=worker, dispatcher=dispatcher,
                clickhouse=ch, database_password=db_password,
                services_candidate=raw['services_candidate'], worker_host_receipt=raw['worker_host_receipt'],
                worker_runtime_receipt=raw['worker_runtime_receipt'])


def service(command, env_file=None):
    text = '[Unit]\nDescription=Helmr verification service\nAfter=network-online.target\n\n[Service]\n'
    text += f'Type=exec\nUser={USER}\nGroup={USER}\nWorkingDirectory={DATA}\nUMask=0077\n'
    if env_file:
        text += f'EnvironmentFile={CONFIG}/{env_file}\n'
    return text + f'ExecStart={command}\nRestart=no\nTimeoutStopSec=120\n'


def files(cfg):
    b = cfg['binaries']
    ch = cfg['clickhouse']
    result = {
        'control-plane.env': environment(cfg['control_plane']),
        'dispatcher.env': environment(cfg['dispatcher']),
        'worker.env': environment(cfg['worker']),
        'enrollment-token': cfg['control_plane']['BOOTSTRAP_WORKER_TOKEN'],
        'config.json': json.dumps(cfg, indent=2) + '\n',
        'clickhouse.xml': f'''<clickhouse>
<logger><level>warning</level><console>true</console></logger>
<listen_host>127.0.0.1</listen_host><http_port>58123</http_port>
<path>{DATA}/clickhouse/</path><tmp_path>{DATA}/clickhouse/tmp/</tmp_path>
<user_files_path>{DATA}/clickhouse/user_files/</user_files_path>
<format_schema_path>{DATA}/clickhouse/format_schemas/</format_schema_path>
<user_directories><users_xml><path>{CONFIG}/clickhouse-users.xml</path></users_xml>
<local_directory><path>{DATA}/clickhouse/access/</path></local_directory></user_directories>
</clickhouse>\n''',
        'clickhouse-users.xml': f'''<clickhouse><profiles><default/></profiles>
<users><bootstrap><password_sha256_hex>{hashlib.sha256(ch['CLICKHOUSE_BOOTSTRAP_PASSWORD'].encode()).hexdigest()}</password_sha256_hex>
<networks><ip>127.0.0.1</ip></networks><profile>default</profile><quota>default</quota>
<access_management>1</access_management></bootstrap></users>
<quotas><default/></quotas></clickhouse>\n''',
    }
    commands = {
        'postgres': f'{b["postgres"]} -D {DATA}/postgres -h 127.0.0.1 -p 55432 -k {DATA}',
        'redis': f'{b["redis-server"]} --bind 127.0.0.1 --port 56379 --dir {DATA}/redis --appendonly yes --daemonize no',
        'clickhouse': f'{b["clickhouse"]} server --config-file={CONFIG}/clickhouse.xml',
        'control-plane': b['control-plane'], 'dispatcher': b['dispatcher'],
    }
    for name, command in commands.items():
        result[unit(name)] = service(command, name + '.env' if name in ['control-plane', 'dispatcher'] else None)
        if name == 'clickhouse':
            # The pinned ClickHouse launcher returns 128 + SIGTERM on normal stop.
            result[unit(name)] += 'SuccessExitStatus=143\n'
    # Keep the canonical Worker service's delegation/kill semantics.
    result['worker-override.conf'] = f'[Service]\nEnvironmentFile=\nEnvironmentFile={CONFIG}/worker.env\nRestart=no\n'
    return result


def wait_for(check, label, seconds=120, unit_name=None):
    end = time.monotonic() + seconds
    while True:
        if unit_name is not None and state(unit_name) == 'failed':
            raise RuntimeError(f'{unit_name} failed while waiting for {label}; inspect retained journal and state')
        try:
            if check():
                return
        except (subprocess.CalledProcessError, urllib.error.URLError, OSError):
            pass
        if time.monotonic() >= end:
            raise RuntimeError(f'timed out waiting for {label}; inspect retained journal and state')
        time.sleep(1)


def http_ready(url):
    with urllib.request.urlopen(url, timeout=2) as response:
        return response.status == 200


def redis_ready():
    with socket.create_connection(('127.0.0.1', 56379), timeout=2) as connection:
        connection.sendall(b'*1\r\n$4\r\nPING\r\n')
        with connection.makefile('rb') as response:
            return response.readline(64) == b'+PONG\r\n'


def require_active_services():
    for name in [unit(n) for n in SERVICES] + ['helmr-worker.service']:
        if state(name) != 'active':
            raise RuntimeError(f'{name} is not active; inspect its retained journal')


def service_run(*args, **kwargs):
    return run('runuser', '-u', USER, '--', *args, **kwargs)


def install(raw):
    cfg = compile_config(raw)
    if not Path('/dev/kvm').is_char_device() or not Path('/sys/fs/cgroup/cgroup.controllers').is_file():
        raise RuntimeError('KVM and cgroup v2 are required')
    if DATA.exists() or WORKER_DATA.exists():
        raise RuntimeError('service data already exists; installation requires a fresh dedicated host')
    if CONFIG.exists() or Path('/etc/systemd/system/helmr-worker.service.d').exists():
        raise RuntimeError('profile or Worker overrides already exist; use a fresh dedicated host')
    if not Path('/etc/systemd/system/helmr-worker.service').is_file():
        raise RuntimeError('install the verified Worker payload with the shared installer first')
    if state('helmr-worker.service') != 'inactive':
        raise RuntimeError('Worker must be inactive')
    candidate = read_candidate(Path(cfg['services_candidate']))
    for receipt_key, manifest_key, installed in [
        ('worker_host_receipt', 'manifest', Path('/usr/share/helmr/worker-host-artifacts.json')),
        ('worker_runtime_receipt', 'runtimeArtifactsManifest', Path('/var/lib/helmr/images/guest/out/runtime-artifacts.json')),
    ]:
        receipt = json.loads(Path(cfg[receipt_key]).read_text())
        if receipt['sourceCommit'] != candidate['source_commit'] or receipt[manifest_key]['digest'] != 'sha256:' + digest(installed):
            raise ValueError('Worker artifact receipt does not match service source and installed manifest')
    for name, path in cfg['binaries'].items():
        if name in ['control-plane', 'dispatcher']:
            continue
        if not os.access(path, os.X_OK):
            raise ValueError('an executable binary path is unavailable')
    if not run(cfg['binaries']['postgres'], '--version', capture_output=True, text=True).stdout.split()[-1].startswith('18.'):
        raise ValueError('PostgreSQL 18 is required')
    descriptor = Path(cfg['control_plane']['DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH'])
    if not descriptor.is_file():
        raise ValueError('materialized runtime descriptor is required')
    # The whole host belongs to this scope; no partitioning or stealing devices.
    for device in cfg['worker']['WORKER_COMPUTER_DEVICES'].split():
        if not Path(device).is_block_device() or Path('/sys/block', Path(device).name, 'pid').exists():
            raise ValueError('an owned NBD device is missing or connected')
    try:
        pwd.getpwnam(USER)
    except KeyError:
        run('useradd', '--system', '--user-group', '--no-create-home', '--home-dir', str(DATA), '--shell', '/usr/sbin/nologin', USER)
    CONFIG.mkdir(mode=0o750)
    shutil.chown(CONFIG, group=USER)
    (CONFIG / 'bin').mkdir(mode=0o755)
    for name in ['control-plane', 'dispatcher']:
        destination = CONFIG / 'bin' / name
        shutil.copyfile(Path(cfg['services_candidate']) / name, destination)
        destination.chmod(0o755)
        cfg['binaries'][name] = str(destination)
    shutil.copyfile(descriptor, CONFIG / 'runtime-descriptor.json')
    (CONFIG / 'runtime-descriptor.json').chmod(0o640)
    shutil.chown(CONFIG / 'runtime-descriptor.json', group=USER)
    cfg['control_plane']['DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH'] = str(CONFIG / 'runtime-descriptor.json')
    DATA.mkdir(mode=0o700)
    shutil.chown(DATA, user=USER, group=USER)
    for name in ['redis', 'clickhouse']:
        path = DATA / name
        path.mkdir(mode=0o700)
        shutil.chown(path, user=USER, group=USER)
    for name, content in files(cfg).items():
        path = CONFIG / name
        path.write_text(content)
        path.chmod(0o600 if name == 'config.json' or name.endswith('.env') or name == 'enrollment-token' else 0o640)
        shutil.chown(path, group=USER)
        if name.endswith('.service'):
            shutil.copyfile(path, Path('/etc/systemd/system') / name)
    # systemd reads EnvironmentFile as root before switching service identities.
    override = Path('/etc/systemd/system/helmr-worker.service.d')
    override.mkdir()
    shutil.copyfile(CONFIG / 'worker-override.conf', override / 'verification.conf')
    service_run(cfg['binaries']['initdb'], '-D', str(DATA / 'postgres'), '--auth-local=peer', '--auth-host=scram-sha-256')
    # Capture actual service bytes; never infer their identity from a branch label.
    identity = {name: hashlib.sha256(Path(path).read_bytes()).hexdigest() for name, path in cfg['binaries'].items()}
    write_json(CONFIG / 'installed-candidate.json', candidate)
    # Keep the source archive so a laptop-only commit is not the only evidence.
    shutil.copyfile(Path(cfg['services_candidate']) / 'source.tar', CONFIG / 'initial-source.tar')
    write_json(CONFIG / 'data-generation.json', {'id': str(uuid.uuid4()), 'schema': candidate['inputs']['schema']})
    (CONFIG / 'binary-digests.json').write_text(json.dumps(identity, indent=2) + '\n')
    for name in ['control-plane', 'dispatcher']:
        if digest(CONFIG / 'bin' / name) != candidate['binaries'][name]:
            raise ValueError('copied service binary digest mismatch')
    if digest(CONFIG / 'initial-source.tar') != candidate['source_sha256']:
        raise ValueError('copied source archive digest mismatch')
    run('systemctl', 'daemon-reload')


def state(name):
    return run('systemctl', 'show', name, '-p', 'ActiveState', '--value', capture_output=True, text=True).stdout.strip()


def start(cfg):
    if state('helmr-worker.service') != 'inactive':
        raise RuntimeError('start expects an inactive Worker; inspect a running or failed attempt')
    for name in SERVICES[3:]:
        if state(unit(name)) not in ['inactive', 'failed']:
            raise RuntimeError('stop application services before running migrations')
    b = cfg['binaries']
    for name in SERVICES[:3]:
        run('systemctl', 'start', unit(name))
    def sql(query):
        return service_run(b['psql'], '-h', str(DATA), '-p', '55432', '-d', 'postgres', '-X', '-v', 'ON_ERROR_STOP=1', '-At', input=query, capture_output=True, text=True).stdout.strip()
    wait_for(lambda: sql('SELECT 1') == '1', 'PostgreSQL', unit_name=unit('postgres'))
    if sql("SELECT count(*) FROM pg_roles WHERE rolname='helmr'") == '0':
        sql(f"CREATE ROLE helmr LOGIN PASSWORD '{cfg['database_password']}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS")
    if sql("SELECT count(*) FROM pg_database WHERE datname='helmr'") == '0':
        sql('CREATE DATABASE helmr OWNER helmr')
    wait_for(redis_ready, 'Redis', unit_name=unit('redis'))
    wait_for(lambda: http_ready('http://127.0.0.1:58123/ping'), 'ClickHouse', unit_name=unit('clickhouse'))
    cp = cfg['control_plane']
    # Commands run as the same service identity, against real backing services.
    # runuser preserves supplied environment except its own identity variables.
    service_run(b['control-plane'], 'clickhouse-bootstrap', env=dict(os.environ) | cp | cfg['clickhouse'])
    migration = cp | {'CLICKHOUSE_USER': cfg['clickhouse']['CLICKHOUSE_MIGRATION_USER'],
                      'CLICKHOUSE_PASSWORD': cfg['clickhouse']['CLICKHOUSE_MIGRATION_PASSWORD']}
    service_run(b['control-plane'], 'migrate', 'up', env=dict(os.environ) | migration)
    for name in SERVICES[3:]:
        run('systemctl', 'start', unit(name))
    wait_for(lambda: http_ready('http://127.0.0.1:58080/readyz'), 'Control Plane', unit_name=unit('control-plane'))
    run('systemctl', 'start', 'helmr-worker.service')
    wait_for(lambda: run('/usr/local/bin/worker', 'status', env=dict(os.environ) | cfg['worker'], capture_output=True).returncode == 0, 'Worker readiness', seconds=300, unit_name='helmr-worker.service')
    require_active_services()
    print('CP and Worker report ready; all service processes are active. No Task/Actor assertion has run.')


def stop(cfg):
    active = state('helmr-worker.service')
    if active == 'active':
        # Keep CP and dispatcher alive until the native drain is acknowledged.
        run('/usr/local/bin/worker', 'drain', '--timeout', '5m', env=dict(os.environ) | cfg['worker'], timeout=310)
        run('systemctl', 'stop', 'helmr-worker.service', timeout=180)
    elif active != 'inactive':
        raise RuntimeError('Worker is not inactive or active; inspect failure before stopping dependencies')
    for name in reversed(SERVICES):
        run('systemctl', 'stop', unit(name), timeout=180)
    print('Services stopped; scope data, S3 objects and host remain allocated.')


def digest(path):
    with open(path, 'rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def write_json(path, value):
    temporary = path.with_suffix('.tmp')
    with open(temporary, 'w', opener=lambda p, flags: os.open(p, flags, 0o600)) as stream:
        json.dump(value, stream, indent=2)
        stream.write('\n')
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def read_candidate(directory):
    candidate = json.loads((directory / 'candidate.json').read_text())
    if not re.fullmatch('[0-9a-f]{40}', candidate['source_commit']):
        raise ValueError('candidate source revision is invalid')
    for name in ['control-plane', 'dispatcher']:
        if digest(directory / name) != candidate['binaries'][name]:
            raise ValueError('candidate binary digest mismatch')
    if digest(directory / 'source.tar') != candidate['source_sha256']:
        raise ValueError('candidate source archive digest mismatch')
    for name in ['control-plane', 'dispatcher', 'worker', 'guestd', 'runtime-support', 'schema']:
        if not re.fullmatch('[0-9a-f]{64}', candidate['inputs'][name]):
            raise ValueError('candidate input digest is invalid')
    if not candidate['inputs']['toolchain'].startswith('go'):
        raise ValueError('candidate toolchain is missing')
    return candidate


def update_kind(previous, candidate, reset):
    for name in ['worker', 'guestd', 'runtime-support', 'toolchain']:
        if previous['inputs'][name] != candidate['inputs'][name]:
            raise ValueError(f'{name} inputs changed; materialize matching Worker/runtime artifacts in a new profile')
    if not reset:
        if previous['inputs']['schema'] != candidate['inputs']['schema']:
            raise ValueError('schema content changed, including existing migration edits; --reset-data is required')
        if previous['inputs']['dispatcher'] != candidate['inputs']['dispatcher']:
            raise ValueError('Dispatcher inputs changed; CP-only update is insufficient')
    return ['control-plane', 'dispatcher'] if reset else ['control-plane']


def service_identity(name):
    raw = run('systemctl', 'show', name, '-p', 'InvocationID,MainPID', capture_output=True, text=True).stdout
    value = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
    if not value.get('InvocationID') or not value.get('MainPID', '0').isdigit() or int(value['MainPID']) == 0:
        raise RuntimeError(f'{name} has no active process identity')
    value['executable_sha256'] = digest(Path('/proc') / value['MainPID'] / 'exe')
    return value


def inspect_profile(cfg):
    """Read receipts and current CP/Dispatcher bytes; never repairs or admits work."""
    report = {'status': 'observed', 'observed_at': time.time(), 'blockers': [],
              'services': {}, 'components': {},
              'limits': ['not case acceptance', 'not scope ownership or expiry',
                         'not Worker/guest artifact verification', 'not an atomic snapshot']}
    blockers = report['blockers']

    def load(name, optional=False):
        try:
            value = json.loads((CONFIG / name).read_text())
            if not isinstance(value, dict):
                raise ValueError('object required')
            return value
        except FileNotFoundError:
            if not optional:
                blockers.append(str(name) + ': missing')
        except (OSError, ValueError):
            blockers.append(str(name) + ': unreadable or invalid')
        return None

    def hex_value(value, size):
        if not isinstance(value, str) or not re.fullmatch('[0-9a-f]{' + str(size) + '}', value):
            raise ValueError('invalid digest or revision')
        return value

    installed = load('installed-candidate.json')
    hashes = load('binary-digests.json')
    generation = load('data-generation.json')
    pending = load('pending-update.json', optional=True)
    if installed is not None:
        try:
            source = hex_value(installed['source_commit'], 40)
            schema = hex_value(installed['inputs']['schema'], 64)
            sources = installed.get('component_sources', {n: source for n in ['control-plane', 'dispatcher']})
            components = {n: {'source_commit': hex_value(sources[n], 40),
                              'expected_sha256': hex_value(installed['binaries'][n], 64)}
                          for n in ['control-plane', 'dispatcher']}
            report['installed'] = {'source_commit': source, 'schema_sha256': schema}
            report['components'] = components
        except (KeyError, TypeError, ValueError):
            blockers.append('installed-candidate.json: invalid identity')
    if generation is not None:
        try:
            identifier = str(uuid.UUID(generation['id']))
            schema = hex_value(generation['schema'], 64)
            report['data_generation'] = {'id': identifier, 'schema_sha256': schema}
            if schema != report.get('installed', {}).get('schema_sha256'):
                blockers.append('data generation schema differs from installed candidate')
        except (KeyError, TypeError, ValueError, AttributeError):
            blockers.append('data-generation.json: invalid identity')
    if pending is not None:
        blockers.append('incomplete update; recreate this disposable environment')
        try:
            attempt = Path(pending['attempt'])
            identifier = str(uuid.UUID(attempt.name))
            if attempt != CONFIG / 'updates' / identifier or attempt.is_symlink():
                raise ValueError('foreign attempt')
            report['pending_update'] = {'attempt_id': identifier,
                                        'candidate': hex_value(pending['candidate'], 40)}
            result = load(Path('updates') / identifier / 'result.json', optional=True)
            phases = {'prepared', 'quiescing-reset', 'discarding-private-data', 'reset-failed',
                      'services-ready-fixtures-required', 'service-ready', 'update-failed'}
            report['pending_update']['result_phase'] = (result.get('phase') if result and result.get('phase') in phases else 'unknown')
            report['pending_update']['failure_recorded'] = bool(result and (result.get('phase') in {'update-failed', 'reset-failed'} or 'failure' in result))
        except (KeyError, TypeError, ValueError):
            blockers.append('pending-update.json: invalid identity')
    for name in [unit(n) for n in SERVICES] + ['helmr-worker.service']:
        try:
            observed = state(name)
            report['services'][name] = observed
            if observed != 'active':
                blockers.append(name + ': not active')
        except (OSError, subprocess.SubprocessError):
            report['services'][name] = 'unknown'
            blockers.append(name + ': observation failed')
    for name, component in report['components'].items():
        try:
            expected = component['expected_sha256']
            if hashes is None or hashes.get(name) != expected:
                blockers.append(name + ': receipt digests differ')
            component['installed_sha256'] = digest(Path(cfg['binaries'][name]))
            if component['installed_sha256'] != expected:
                blockers.append(name + ': installed bytes differ')
            if report['services'].get(unit(name)) == 'active':
                identity = service_identity(unit(name))
                component['running_sha256'] = identity['executable_sha256']
                component['pid'] = identity['MainPID']
                component['invocation_id'] = identity['InvocationID']
                if component['running_sha256'] != expected:
                    blockers.append(name + ': running bytes differ')
        except (KeyError, TypeError, OSError, RuntimeError, subprocess.SubprocessError):
            blockers.append(name + ': byte/process observation failed')
    if blockers:
        report['status'] = 'blocked'
    return report


def replace_binary(source, destination):
    temporary = destination.with_suffix('.next')
    shutil.copyfile(source, temporary)
    temporary.chmod(0o755)
    os.replace(temporary, destination)


def reset_private_data(cfg):
    # stop() must have completed against the OLD schema before this function.
    if any(state(name) != 'inactive' for name in [unit(n) for n in SERVICES] + ['helmr-worker.service']):
        raise RuntimeError('all profile services must be inactive before data reset')
    for device in cfg['worker']['WORKER_COMPUTER_DEVICES'].split():
        if Path('/sys/block', Path(device).name, 'pid').exists():
            raise RuntimeError('a Worker NBD device remains connected; refusing data reset')
    paths = [DATA / n for n in ['postgres', 'redis', 'clickhouse']] + [WORKER_DATA, JAILER_DATA]
    mounts = [line.split()[4] for line in Path('/proc/self/mountinfo').read_text().splitlines()]
    for path in paths:
        if path.is_symlink() or any(m == str(path) or m.startswith(str(path) + '/') for m in mounts):
            raise RuntimeError('reset path is a symlink or still mounted')
    for path in paths:
        if path.exists():
            shutil.rmtree(path)
    for name in ['redis', 'clickhouse']:
        path = DATA / name
        path.mkdir(mode=0o700)
        shutil.chown(path, user=USER, group=USER)
    JAILER_DATA.mkdir(mode=0o755)
    service_run(cfg['binaries']['initdb'], '-D', str(DATA / 'postgres'), '--auth-local=peer', '--auth-host=scram-sha-256')


def apply_services(cfg, directory, reset):
    previous = json.loads((CONFIG / 'installed-candidate.json').read_text())
    previous_hashes = json.loads((CONFIG / 'binary-digests.json').read_text())
    candidate = read_candidate(directory)
    names = update_kind(previous, candidate, reset)
    if (CONFIG / 'pending-update.json').exists():
        raise RuntimeError('incomplete update; collect evidence and recreate this disposable environment')
    require_active_services()
    # Private copies are rechecked before any running service is touched.
    attempt = CONFIG / 'updates' / str(uuid.uuid4())
    attempt.mkdir(parents=True, mode=0o700)
    for name in ['control-plane', 'dispatcher', 'source.tar', 'candidate.json']:
        shutil.copyfile(directory / name, attempt / name)
    candidate = read_candidate(attempt)
    names = update_kind(previous, candidate, reset)
    for name in names:
        if digest(Path(cfg['binaries'][name])) != previous['binaries'][name]:
            raise ValueError('installed service bytes differ from last accepted candidate')
    untouched = [unit(n) for n in SERVICES if n not in names] + ['helmr-worker.service']
    before = {name: service_identity(name) for name in untouched}
    evidence = {'candidate': candidate['source_commit'], 'reset_data': reset, 'phase': 'prepared',
                'before': before, 'attempt': str(attempt), 'started_at': time.time()}
    generation = json.loads((CONFIG / 'data-generation.json').read_text())
    if generation['schema'] != previous['inputs']['schema']:
        raise ValueError('installed schema and data generation differ')
    # This marker blocks testing a partial update; it is not a resume journal.
    write_json(CONFIG / 'pending-update.json', evidence)
    write_json(attempt / 'result.json', evidence)
    if reset:
        try:
            evidence['phase'] = 'quiescing-reset'
            write_json(attempt / 'result.json', evidence)
            stop(cfg)  # Drain the old Worker against the old schema before erasure.
            write_json(CONFIG / 'data-generation.json',
                       {'id': str(uuid.uuid4()), 'schema': candidate['inputs']['schema']})
            evidence['phase'] = 'discarding-private-data'
            write_json(attempt / 'result.json', evidence)
            reset_private_data(cfg)
            for name in names:
                replace_binary(attempt / name, Path(cfg['binaries'][name]))
            start(cfg)
            for name in names:
                if service_identity(unit(name))['executable_sha256'] != candidate['binaries'][name]:
                    raise RuntimeError('reset service process differs from candidate')
            installed = dict(candidate)
            installed['component_sources'] = {n: candidate['source_commit'] for n in names}
            write_json(CONFIG / 'installed-candidate.json', installed)
            write_json(CONFIG / 'binary-digests.json', previous_hashes | candidate['binaries'])
            evidence['phase'] = 'services-ready-fixtures-required'
        except BaseException:
            evidence['phase'] = 'reset-failed'
            raise
        finally:
            evidence['finished_at'] = time.time()
            write_json(attempt / 'result.json', evidence)
        (CONFIG / 'pending-update.json').unlink()
        print('Reset services ready; repeat normal setup, API keys and case deployment. No behavior case has passed.')
        return
    try:
        run('systemctl', 'stop', unit('control-plane'), timeout=180)
        for name in names:
            replace_binary(attempt / name, Path(cfg['binaries'][name]))
        run('systemctl', 'start', unit('control-plane'))
        wait_for(lambda: http_ready('http://127.0.0.1:58080/readyz'), 'updated Control Plane', unit_name=unit('control-plane'))
        require_active_services()
        after = {name: service_identity(name) for name in untouched}
        evidence['after'] = after
        if before != after:
            raise RuntimeError('a retained service process changed during CP-only update')
        for name in names:
            if service_identity(unit(name))['executable_sha256'] != candidate['binaries'][name]:
                raise RuntimeError('running service bytes do not match candidate')
        installed = dict(candidate)
        installed['binaries'] = dict(previous['binaries']) | {name: candidate['binaries'][name] for name in names}
        sources = previous.get('component_sources', {name: previous['source_commit'] for name in ['control-plane', 'dispatcher']})
        installed['component_sources'] = sources | {name: candidate['source_commit'] for name in names}
        write_json(CONFIG / 'installed-candidate.json', installed)
        write_json(CONFIG / 'binary-digests.json', previous_hashes | installed['binaries'])
        evidence['phase'] = 'service-ready'
    except BaseException:
        evidence['phase'] = 'update-failed'
        # Keep the marker and failure evidence. Recreate instead of rolling back.
        raise
    finally:
        evidence['finished_at'] = time.time()
        write_json(attempt / 'result.json', evidence)
    (CONFIG / 'pending-update.json').unlink()
    print(f'Service update complete; run selected cases. Evidence: {attempt}/result.json')


def persistence_matches(value, action):
    if action == 'wait-aborted':
        return bool(value and value.get('attempt_number') == 1 and value.get('acknowledged')
                    and not value.get('source_reclaimed') and value.get('source_state') != 'closed'
                    and value.get('other_instances') == 0 and value.get('lease_on_source')
                    and value.get('writer_generation') == value.get('captured_writer_generation'))
    if not value or value.get('attempt_number') != 1 or not value.get('checkpoint_id'):
        return False
    if value.get('prior_runtime_state') != 'closed' or value.get('prior_runtime_reclaimed') is not True:
        return False
    if action == 'wait-parked':
        return (value.get('run_status') == 'waiting' and value.get('condition') == 'pending'
                and value.get('suspension') == 'parked' and value.get('checkpoint_status') == 'ready')
    return value.get('run_status') == 'succeeded' and bool(value.get('restored_runtime_ids'))


def observe_persistence(cfg, run_id, action):
    if not run_id or str(uuid.UUID(run_id)) != run_id:
        raise ValueError('a canonical Run UUID is required')
    query = Path(__file__).with_name('capture_abort.sql' if action == 'wait-aborted' else 'persistence.sql').read_text()
    observed = None
    def check():
        nonlocal observed
        raw = service_run(cfg['binaries']['psql'], '-h', str(DATA), '-p', '55432', '-d', 'helmr',
                          '-X', '-v', 'ON_ERROR_STOP=1', '-v', 'run_id=' + run_id, '-At',
                          input=query, capture_output=True, text=True,
                          env=dict(os.environ, PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=5000'),
                          timeout=10).stdout.strip()
        observed = json.loads(raw) if raw else None
        status = observed.get('run_status') if observed else None
        if status in ['failed', 'system_failed', 'cancelled', 'expired'] or (status == 'succeeded' and action == 'wait-parked'):
            raise RuntimeError(f"Run ended with {status} before {action}: {json.dumps(observed.get('run_failure'))}")
        return persistence_matches(observed, action)
    wait_for(check, action, seconds=180)
    print(json.dumps(observed))


RESET_UNIT = 'helmr-verification-reset.service'


def supervise_reset(argv):
    # The named native service outlives the caller. Its cgroup owns initdb,
    # runuser, migrations and their descendants; an occupied name rejects retry.
    return subprocess.run([
        'systemd-run', '--unit=' + RESET_UNIT, '--service-type=exec', '--wait',
        '--pipe', '--collect', '--property=ExitType=cgroup', '--property=KillMode=control-group',
        '--property=SendSIGKILL=yes', '--property=TimeoutStopSec=30s',
        '--property=Restart=no', '--working-directory=' + os.getcwd(),
        sys.executable, str(Path(__file__).resolve()), *argv, '--reset-runner',
    ], check=False).returncode


def require_reset_runner():
    raw = run('systemctl', 'show', RESET_UNIT, '-p', 'MainPID,InvocationID',
              capture_output=True, text=True).stdout
    fields = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
    if (fields.get('MainPID') != str(os.getpid()) or not os.environ.get('INVOCATION_ID')
            or fields.get('InvocationID') != os.environ['INVOCATION_ID']):
        raise RuntimeError('reset runner must be the current native reset service process')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['render', 'install', 'start', 'stop', 'inspect', 'apply-services', 'wait-parked', 'verify-restored', 'wait-aborted'])
    parser.add_argument('--config', type=Path, help='input JSON for render/install')
    parser.add_argument('--output', type=Path, help='new private directory for offline render')
    parser.add_argument('--candidate', type=Path, help='build_services.py output for apply-services')
    parser.add_argument('--reset-data', action='store_true', help='explicitly discard private scope fixtures and recreate schema')
    parser.add_argument('--run-id', help='Run UUID for persistence evidence')
    parser.add_argument('--reset-runner', action='store_true', help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.candidate is not None and args.action != 'apply-services':
        parser.error('--candidate is only valid with apply-services')
    reset_action = args.action == 'apply-services' and args.reset_data
    if args.reset_runner and not reset_action:
        parser.error('reset runner is only valid for a reset')
    if args.reset_data and args.action != 'apply-services':
        parser.error('--reset-data is only valid with apply-services')
    if args.action in ['render', 'install']:
        if not args.config:
            parser.error('--config is required')
        raw = json.loads(args.config.read_text())
    if args.action == 'render':
        if not args.output:
            parser.error('--output is required')
        content = files(compile_config(raw))
        args.output.mkdir(mode=0o700)
        for name, value in content.items():
            path = args.output / name
            path.write_text(value)
            path.chmod(0o600)
        print('Rendered private configuration; no host was changed.')
        return
    if platform.system() != 'Linux' or platform.machine() != 'x86_64' or os.geteuid() != 0:
        raise RuntimeError('requires root on a dedicated Linux x86_64 systemd host')
    if reset_action:
        if not args.reset_runner:
            sys.exit(supervise_reset(sys.argv[1:]))
        require_reset_runner()
    # Reject concurrent host operations.
    with open('/run/helmr-verification.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == 'install':
            install(raw)
            return
        cfg = json.loads((CONFIG / 'config.json').read_text())
        if args.action == 'start':
            if (CONFIG / 'pending-update.json').exists():
                raise RuntimeError('incomplete update; recreate this disposable environment before starting')
            start(cfg)
        elif args.action == 'apply-services':
            if args.candidate is None:
                parser.error('--candidate is required')
            apply_services(cfg, args.candidate, args.reset_data)
        elif args.action in ['wait-parked', 'verify-restored', 'wait-aborted']:
            observe_persistence(cfg, args.run_id, args.action)
        elif args.action == 'stop':
            stop(cfg)
        else:
            report = inspect_profile(cfg)
            print(json.dumps(report, sort_keys=True))
            if report['blockers']:
                sys.exit(1)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, TypeError, RuntimeError, OSError, subprocess.SubprocessError) as error:
        print(f'runtime host: {error}', file=sys.stderr)
        sys.exit(1)
