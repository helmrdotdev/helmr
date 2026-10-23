#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
bash -n images/guest/init.sh
# Boot functions below are probed rather than executed. Keep the policy guard
# over the whole init so a controller setting in any boot function is rejected.
if grep -Eq 'cgroup\.subtree_control|pids\.max|\+cpu|\+memory|\+pids' images/guest/init.sh; then
  printf 'guest init Program cgroups unexpectedly define a resource controller policy\n' >&2
  exit 1
fi
python3 - <<'PYTHON'
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile

source = Path('images/guest/init.sh').read_text()
definitions, separator, startup = source.partition('\nmount_base\n')
assert separator, 'guest init startup was not found'
startup = 'mount_base\n' + startup

with tempfile.TemporaryDirectory(prefix='helmr-init-cgroup-') as directory:
    root = Path(directory)
    cgroup = root / 'cgroup'
    placement = root / 'placement'
    log = root / 'commands'
    violations = root / 'violations'
    fixture = root / 'init.sh'
    guestd = root / 'guestd'
    guestd.write_text('''#!/usr/bin/env bash
set -eu
printf 'guestd %s\\n' "$*" >> "$COMMAND_LOG"
if [ "$*" = --boot-gate ]; then exit "${GATE_EXIT:-0}"; fi
''')
    guestd.chmod(0o755)
    # Execute the production function, redirecting only kernel paths. Other boot
    # functions are inert probes when executing the real startup command sequence.
    adapted = definitions.replace('/sys/fs/cgroup', str(cgroup)).replace('/proc/self/cgroup', str(placement))
    probes = r'''
record() {
    printf '%s\n' "$*" >> "$COMMAND_LOG"
    [ "$*" != "${FAIL_COMMAND:-}" ] || return 42
}
is_mounted() { record "is_mounted $*"; [ "${ALREADY_MOUNTED:-0}" = 1 ]; }
mount() { record "mount $*"; }
mkdir() {
    record "mkdir $*" || return $?
    command mkdir "$@" || return $?
    case "$*" in
        "$CGROUP/helmr") printf '%s' "${ROOT_PROCESSES:-}" > "$CGROUP/helmr/cgroup.procs" ;;
        "$CGROUP/helmr/supervisor")
            if [ "${FAIL_MOVE:-0}" = 1 ]; then
                command mkdir "$CGROUP/helmr/supervisor/cgroup.procs"
            fi ;;
    esac
}
cat() {
    record "cat $*"
    # Observe the redirection before the parent is inspected, without replacing it.
    moved=
    IFS= read -r moved < "$CGROUP/helmr/supervisor/cgroup.procs" || true
    if [ "$moved" != "$$" ]; then printf 'supervisor was not moved before root validation\n' >> "$VIOLATIONS"; fi
    command cat "$@"
}
grep() { record "grep $*"; command grep "$@"; }
'''
    boot_functions = ['mount_base', 'enable_user_namespaces', 'mount_scratch', 'load_vsock',
                      'load_session_program_devices', 'mount_computer', 'mount_program',
                      'configure_network', 'configure_vm_platform']
    probes += ''.join(f'{name}() {{ record {name}; }}\n' for name in boot_functions)
    fixture.write_text(adapted + '\n' + probes)
    base_env = dict(os.environ, CGROUP=str(cgroup), COMMAND_LOG=str(log), VIOLATIONS=str(violations),
                    ALREADY_MOUNTED='0', FAIL_COMMAND='', FAIL_MOVE='0', ROOT_PROCESSES='', GATE_EXIT='0')
    mount_events = [f'mkdir -p {cgroup}', f'is_mounted {cgroup}', f'mount -t cgroup2 cgroup2 {cgroup}']
    topology_events = [f'mkdir {cgroup}/helmr', f'mkdir {cgroup}/helmr/supervisor',
                       f'cat {cgroup}/helmr/cgroup.procs', f'grep -qx 0::/helmr/supervisor {placement}']
    bootstrap_events = mount_events + topology_events
    boot_events = ['mount_base'] + bootstrap_events + boot_functions[1:5] + ['guestd --boot-gate'] + boot_functions[5:] + ['guestd --vsock-port 5000 --health-port 5001']

    def run(name, expected_events, *, boot=False, failure=False, message=None, **environment):
        shutil.rmtree(cgroup, ignore_errors=True)
        log.write_text('')
        violations.unlink(missing_ok=True)
        placement.write_text(environment.pop('PLACEMENT', '0::/helmr/supervisor\n'))
        body = startup.replace('/usr/bin/guestd', str(guestd)) if boot else 'configure_program_cgroups\n'
        execution = subprocess.run(['bash', '--posix', '-eu', '-c', '. ' + shlex.quote(str(fixture)) + '\n' + body],
                                   env=dict(base_env, **environment), capture_output=True, text=True)
        assert (execution.returncode != 0) == failure, f'{name}: exit {execution.returncode}: {execution.stderr}'
        assert log.read_text().splitlines() == expected_events, f'{name}: unexpected order: {log.read_text()}'
        assert not violations.exists(), f'{name}: {violations.read_text()}'
        # The bootstrap may create only its process topology, never controller
        # settings such as subtree_control, pids.max, cpu.max or memory.max.
        allowed = {'helmr', 'helmr/supervisor', 'helmr/cgroup.procs', 'helmr/supervisor/cgroup.procs'}
        unexpected = {str(path.relative_to(cgroup)) for path in cgroup.rglob('*')} - allowed
        assert not unexpected, f'{name}: unexpected cgroup policy writes: {unexpected}'
        if message:
            assert message in execution.stderr, f'{name}: missing diagnostic: {execution.stderr}'
        print(f'ok - {name}')

    run('bootstrap creates process-free parent and moves supervisor before validation', bootstrap_events)
    run('mounted hierarchy is reused', mount_events[:2] + topology_events, ALREADY_MOUNTED='1')
    for index, name in [(2, 'mount'), (3, 'parent creation'), (4, 'supervisor creation')]:
        run(f'{name} failure stops bootstrap', bootstrap_events[:index + 1], failure=True,
            FAIL_COMMAND=bootstrap_events[index])
    run('move failure stops before validation', bootstrap_events[:5], failure=True, FAIL_MOVE='1')
    run('populated parent is rejected', bootstrap_events[:6], failure=True, ROOT_PROCESSES='123\n',
        message='Helmr Program cgroup root is not process-free')
    run('wrong supervisor identity is rejected', bootstrap_events, failure=True, PLACEMENT='0::/wrong\n',
        message='Helmr supervisor cgroup placement failed')
    run('startup orders bootstrap and gate before tenant mounts and network', boot_events, boot=True)
    gate_index = boot_events.index('guestd --boot-gate')
    run('rejected boot gate prevents tenant mounts network and guestd', boot_events[:gate_index + 1],
        boot=True, failure=True, GATE_EXIT='43')
    run('bootstrap failure prevents later startup', ['mount_base'] + bootstrap_events[:6],
        boot=True, failure=True, ROOT_PROCESSES='123\n')
PYTHON
