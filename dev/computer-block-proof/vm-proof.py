"""Dev-only x86_64 KVM paired RAM/Computer/scratch proof. Never production wiring."""
import argparse, hashlib, http.client, importlib.util, json, os, pathlib, queue
import shutil, signal, socket, subprocess, sys, tempfile, threading, time

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('kernel_proof', HERE / 'kernel-proof.py')
nbd = importlib.util.module_from_spec(spec)
spec.loader.exec_module(nbd)
SIZE = 16 * 1024 * 1024


def digest(path):
    with open(path, 'rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def oracle_matches(result, nonce, challenge, computer, scratch):
    return (result.get('nonce') == nonce and result.get('challenge') == challenge
            and result.get('phase') == 'resumed' and result.get('computer') is computer
            and result.get('scratch') is scratch
            and 'computer_error' not in result and 'scratch_error' not in result)


def check_assets(directory):
    manifest = json.loads((directory / 'runtime-artifacts.json').read_text())
    if manifest['arch'] != 'amd64':
        raise RuntimeError('requires existing amd64 guest assets')
    assets = {}
    for name in ('kernel', 'initramfs', 'rootfs'):
        item = manifest[name]
        path = directory / item['path']
        if path.parent != directory or path.stat().st_size != item['size_bytes']:
            raise RuntimeError('invalid guest asset path or size')
        if 'sha256:' + digest(path) != item['digest']:
            raise RuntimeError('guest asset digest mismatch: ' + name)
        assets[name] = str(path)
    return assets, manifest


class API(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=30)
        self.path = path
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def api(path, method, route, body=None):
    c = API(path)
    try:
        c.request(method, route, None if body is None else json.dumps(body),
                  {'Content-Type': 'application/json'})
        r = c.getresponse()
        data = r.read(65537)
        if r.status not in (200, 204) or len(data) > 65536:
            raise RuntimeError(f'{method} {route}: {r.status}: {data[:512]!r}')
        return json.loads(data) if data else None
    finally:
        c.close()


def stop(process):
    if process.poll() is None:
        process.kill()
    process.wait(timeout=10)


class VM:
    def __init__(self, binary, directory):
        self.socket = str(directory / 'api.sock')
        self.lines = queue.Queue(maxsize=4096)
        self.process = subprocess.Popen([binary, '--api-sock', self.socket],
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=subprocess.STDOUT)
        self.buffer = ""
        def reader():
            while True:
                chunk = os.read(self.process.stdout.fileno(), 4096)
                if not chunk: break
                try: self.lines.put_nowait(chunk.decode(errors='replace'))
                except queue.Full: pass
            try: self.lines.put_nowait(None)
            except queue.Full: pass
        self.reader = threading.Thread(target=reader, daemon=True)
        self.reader.start()
        try:
            deadline = time.monotonic() + 10
            while not os.path.exists(self.socket):
                if self.process.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError('Firecracker API unavailable')
                time.sleep(.025)
        except BaseException:
            self.close()
            raise
    def send(self, line):
        self.process.stdin.write((line + '\n').encode())
        self.process.stdin.flush()
    def marker(self, prefix, seconds=30, newline=True):
        deadline = time.monotonic() + seconds
        while True:
            index = self.buffer.find(prefix)
            if index >= 0:
                start = index + len(prefix)
                end = self.buffer.find('\n', start)
                if not newline or end >= 0:
                    if not newline: end = start
                    result = self.buffer[start:end].strip()
                    self.buffer = self.buffer[end:]
                    return result
            try: chunk = self.lines.get(timeout=max(.01, deadline-time.monotonic()))
            except queue.Empty: raise RuntimeError(f'guest marker {prefix} timeout: {self.buffer[-512:]}')
            if chunk is None: raise RuntimeError(f'guest exited before {prefix}: {self.buffer[-512:]}')
            self.buffer = (self.buffer + chunk)[-65536:]
            if time.monotonic() >= deadline:
                raise RuntimeError(f'guest marker {prefix} timeout: {self.buffer[-512:]}')
    def close(self):
        stop(self.process)
        self.reader.join(timeout=2)


def fsync_path(path):
    fd = os.open(path, os.O_RDWR)
    try: os.fsync(fd)
    finally: os.close(fd)


def seed(path, directory=None):
    with open(path, 'wb') as f: f.truncate(SIZE)
    command = ['mkfs.ext4', '-q', '-F', '-E',
               'nodiscard,lazy_itable_init=0,lazy_journal_init=0']
    if directory: command += ['-d', str(directory)]
    subprocess.run(command + [str(path)], check=True, timeout=30)


def paired_capture(vm, attachment, scratch, scratch_fd, directory):
    api(vm.socket, 'PATCH', '/vm', {'state': 'Paused'})
    config = api(vm.socket, 'GET', '/vm/config')
    drives = config['drives']
    if (len(drives) != 3 or any(d.get('io_engine') != 'Sync' for d in drives)
            or {d['drive_id'] for d in drives if not d['is_read_only']} != {'computer', 'scratch'}):
        raise RuntimeError('unexpected VM disk topology or IO engine')
    # Pause alone is not a complete barrier. Retained NBD fd predates guest IO;
    # snapshot/create also drains/fsyncs devices. Check explicit host errors too.
    os.fsync(attachment.fd)
    os.fsync(scratch_fd)
    api(vm.socket, 'PUT', '/snapshot/create', {
        'snapshot_type': 'Full', 'snapshot_path': str(directory / 'vmstate'),
        'mem_file_path': str(directory / 'memory'), 'sync_snapshot_files': True})
    os.fsync(attachment.fd)
    os.fsync(scratch_fd)
    shutil.copyfile(scratch, directory / 'scratch.capture')
    fsync_path(directory / 'scratch.capture')


def attach(path, root):
    try:
        return nbd.Attachment(path)
    finally:
        # Persist known claims even when attachment setup subsequently fails.
        # SIGKILL during an ioctl can precede this journal: postflight must also
        # inspect the entire disposable candidate set; never clear by name.
        claims = [a.dev for a in nbd.owned if hasattr(a, 'dev')]
        (root / 'owned-devices.json').write_text(json.dumps(claims))


def execute_in_arena(args, root, runner=None):
    print('proof arena: ' + str(root), flush=True)
    try:
        report = (runner or run)(args, root)
        for attachment in nbd.owned:
            if hasattr(attachment, 'dev') and os.path.exists('/sys/block/' + os.path.basename(attachment.dev) + '/pid'):
                raise RuntimeError('owned NBD device still active')
    except BaseException:
        print('proof failed; retained arena: ' + str(root), file=sys.stderr, flush=True)
        raise
    # runner returns only after every VMM/backend was reaped and owned attachment
    # cleanup succeeded. Preserve the arena on any error, including cleanup errors.
    shutil.rmtree(root)
    return report


def run(args, root):
    assets, manifest = check_assets(args.assets)
    version = subprocess.check_output([args.firecracker, '--version'], timeout=5).decode()
    if not version.startswith('Firecracker v1.17.0\n'):
        raise RuntimeError('requires Firecracker 1.17.0')
    initial = root / 'seed'; initial.mkdir()
    shutil.copyfile(args.oracle, initial / 'guest-oracle')
    (initial / 'guest-oracle').chmod(0o755)
    (initial / 'oracle.data').write_bytes(bytes(4096))
    computer_files = root / 'computer-files'; computer_files.mkdir()
    (computer_files / 'oracle.data').write_bytes(bytes(4096))
    scratch, computer = root / 'scratch', root / 'computer.seed'
    seed(scratch, initial); seed(computer, computer_files)
    shutil.copyfile(scratch, root / 'scratch.baseline')
    state = root / 'state'; link = root / 'computer'
    processes, attachments, vms = [], [], []
    scratch_fd = os.open(scratch, os.O_RDWR)
    try:
        server = nbd.start_server(args.backend, str(state), str(root / 'backend.sock'), True)
        processes.append(server)
        attachment = attach(str(root / 'backend.sock'), root); attachments.append(attachment)
        with open(computer, 'rb') as f:
            offset = 0
            while data := f.read(4096):
                if os.pwrite(attachment.fd, data, offset) != len(data):
                    raise RuntimeError('short seed write')
                offset += len(data)
        os.fsync(attachment.fd)
        baseline = (state / 'root').read_text()
        link.symlink_to(attachment.dev)
        source_dir = root / 'source'; source_dir.mkdir()
        vm = VM(args.firecracker, source_dir); vms.append(vm)
        api(vm.socket, 'PUT', '/machine-config', {'vcpu_count': 1, 'mem_size_mib': 256})
        api(vm.socket, 'PUT', '/boot-source', {
            'kernel_image_path': assets['kernel'], 'initrd_path': assets['initramfs'],
            'boot_args': 'console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=squashfs ro init=/bin/sh'})
        for name, path, readonly in [('rootfs', assets['rootfs'], True),
                                     ('scratch', str(scratch), False), ('computer', str(link), False)]:
            api(vm.socket, 'PUT', '/drives/' + name, {'drive_id': name, 'path_on_host': path,
                'is_root_device': name == 'rootfs', 'is_read_only': readonly,
                'io_engine': 'Sync', 'cache_type': 'Writeback'})
        api(vm.socket, 'PUT', '/actions', {'action_type': 'InstanceStart'})
        # The pinned Alpine initramfs honors init= and switch_root into /bin/sh.
        # Wait for the shell prompt; early serial bytes may be lost during boot.
        vm.marker("# ", newline=False)
        vm.send('mount -t proc proc /proc 2>/dev/null; mount -t sysfs sysfs /sys 2>/dev/null; '
                'mount -t devtmpfs devtmpfs /dev 2>/dev/null; mount -t tmpfs tmpfs /run && '
                'mkdir -p /run/scratch && mount -t ext4 /dev/vdb /run/scratch && '
                'exec /run/scratch/guest-oracle')
        nonce = vm.marker('HELMR_ORACLE_READY ')
        if len(nonce) != 64 or any(c not in '0123456789abcdef' for c in nonce):
            raise RuntimeError('invalid RAM nonce')
        paired_capture(vm, attachment, scratch, scratch_fd, root)
        captured = (state / 'root').read_text()
        if captured == baseline: raise RuntimeError('Computer generation did not change')
        # Entire source VM must be reaped before any restored writer starts.
        vm.close(); vms.remove(vm)
        stop(server); processes.remove(server)
        attachment.close(); attachments.remove(attachment)
        link.unlink()
        results = []
        for name, pin, scratch_pin, expected in [
                ('wrong-computer', baseline, 'scratch.capture', (False, True)),
                ('wrong-scratch', captured, 'scratch.baseline', (True, False)),
                ('paired', captured, 'scratch.capture', (True, True))]:
            case = root / name; case.mkdir()
            branch = case / 'state'; shutil.copytree(state, branch)
            (branch / 'root').write_text(pin)
            shutil.copyfile(root / scratch_pin, scratch)
            shutil.copyfile(root / 'memory', case / 'memory')
            server = nbd.start_server(args.backend, str(branch), str(case / 'backend.sock'))
            processes.append(server)
            # A distinct kernel device avoids reusing the prior attachment cache.
            attachment = attach(str(case / 'backend.sock'), root); attachments.append(attachment)
            link.symlink_to(attachment.dev)
            vm = VM(args.firecracker, case); vms.append(vm)
            api(vm.socket, 'PUT', '/snapshot/load', {'snapshot_path': str(root / 'vmstate'),
                'mem_backend': {'backend_type': 'File', 'backend_path': str(case / 'memory')},
                'enable_diff_snapshots': False, 'resume_vm': False})
            api(vm.socket, 'PATCH', '/vm', {'state': 'Resumed'})
            challenge = os.urandom(32).hex()
            vm.send('verify ' + challenge)
            result = json.loads(vm.marker('HELMR_ORACLE_RESULT '))
            if not oracle_matches(result, nonce, challenge, *expected):
                raise RuntimeError(f'{name} oracle mismatch: {result}')
            results.append({'case': name, **result})
            vm.close(); vms.remove(vm)
            stop(server); processes.remove(server)
            attachment.close(); attachments.remove(attachment); link.unlink()
        return {'paired_vm_proof': 'pass', 'results': results, 'guest_assets': manifest,
                'firecracker_sha256': digest(args.firecracker), 'backend_sha256': digest(args.backend),
                'oracle_sha256': digest(args.oracle), 'power_loss': False, 'remote_store': False}
    finally:
        errors = []
        os.close(scratch_fd)
        for vm in reversed(vms):
            try: vm.close()
            except BaseException as e: errors.append(str(e))
        for process in reversed(processes):
            try: stop(process)
            except BaseException as e: errors.append(str(e))
        for attachment in reversed(nbd.owned):
            try: attachment.close()
            except BaseException as e: errors.append(str(e))
        if errors: raise RuntimeError('cleanup incomplete: ' + repr(errors))


def main():
    parser = argparse.ArgumentParser()
    for name in ('firecracker', 'backend', 'oracle', 'assets'):
        parser.add_argument('--' + name, required=True, type=pathlib.Path)
    parser.add_argument('--arena', type=pathlib.Path, help='existing private empty directory; retained on failure')
    args = parser.parse_args()
    args.firecracker, args.backend, args.oracle = map(str, [args.firecracker.resolve(), args.backend.resolve(), args.oracle.resolve()])
    args.assets = args.assets.resolve()
    if (os.environ.get('HELMR_DISPOSABLE_VM_PROOF') != '1' or os.uname().machine != 'x86_64'
            or not os.access('/dev/kvm', os.R_OK | os.W_OK)):
        raise RuntimeError('authorized disposable x86_64 KVM host required')
    available = [n for n in range(16) if os.path.exists(f'/dev/nbd{n}')
                 and not os.path.exists(f'/sys/block/nbd{n}/pid')]
    if len(available) < 4:
        raise RuntimeError('four unused NBD devices required; each restore uses a distinct device')
    def interrupted(signum, frame): raise RuntimeError('proof interrupted; cleaning owned resources')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(240) # Under the backend connection deadline of five minutes.
    root = args.arena.resolve() if args.arena else pathlib.Path(tempfile.mkdtemp(prefix='helmr-vm-'))
    st = root.stat()
    if not root.is_dir() or st.st_uid != os.getuid() or st.st_mode & 0o077 or any(root.iterdir()):
        raise RuntimeError('arena must be an owned private empty directory')
    report = execute_in_arena(args, root)
    signal.alarm(0)
    print(json.dumps(report))

if __name__ == '__main__': main()
