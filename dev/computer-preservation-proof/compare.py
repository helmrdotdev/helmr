"""Local storage comparison; not a VM, remote upload, or production benchmark."""
import argparse
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import random
import statistics
import subprocess
import threading
import time

MIB = 1024 * 1024
SIZE = 256 * MIB


def run(*args):
    return subprocess.run(args, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)


def digest(path):
    with path.open('rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


class Probe:
    """Separate same-filesystem 4 KiB write+fdatasync latency, no guest stack."""
    def __init__(self, root):
        self.path = root / 'latency-probe'
        self.stop = threading.Event()
        self.samples = []
        self.error = None
        self.thread = threading.Thread(target=self.work)

    def work(self):
        try:
            with self.path.open('w+b', buffering=0) as f:
                f.truncate(MIB)
                n = 0
                while not self.stop.is_set():
                    t = time.monotonic()
                    f.seek((n % 256) * 4096)
                    f.write(bytes([n % 251]) * 4096)
                    os.fdatasync(f.fileno())
                    self.samples.append((t, (time.monotonic() - t) * 1000))
                    n += 1
                    self.stop.wait(.005)
        except BaseException as e:
            self.error = e

    def stats(self, start, end):
        xs = sorted(v for t, v in self.samples if start <= t < end)
        return dict(n=len(xs), p50_ms=round(statistics.median(xs), 3) if len(xs) >= 20 else None,
                    p95_ms=round(xs[min(len(xs)-1, int(len(xs)*.95))], 3) if len(xs) >= 20 else None,
                    max_ms=round(max(xs), 3) if xs else None)


def copy_clone(source, dest):
    with source.open('rb') as s, dest.open('wb') as d:
        fcntl.ioctl(d.fileno(), 0x40049409, s.fileno())


def main():
    p = argparse.ArgumentParser()
    p.add_argument('kind', choices=['xfs', 'btrfs', 'btrfs-nocow', 'btrfs-chunks'])
    p.add_argument('source', type=Path)
    p.add_argument('receiver', type=Path)
    p.add_argument('archive', type=Path)
    a = p.parse_args()
    a.archive.mkdir()
    is_btrfs = a.kind.startswith('btrfs')
    native_send = is_btrfs and a.kind != 'btrfs-chunks'
    live = a.source / 'live'
    if is_btrfs:
        run('btrfs', 'subvolume', 'create', str(live))
    else:
        live.mkdir()
    if a.kind == 'btrfs-nocow':
        run('chattr', '+C', str(live))
    disk = live / 'disk'
    original = hashlib.sha256()
    rng = random.Random(4301)
    with disk.open('wb') as f:
        for _ in range(SIZE // MIB):
            data = rng.randbytes(MIB)
            f.write(data)
            original.update(data)
        f.flush()
        os.fsync(f.fileno())
    probe = Probe(a.source)
    objects = a.archive / 'objects'
    objects.mkdir()
    link_behavior = 'supported'
    try:
        os.link(disk, a.source / 'cross-subvolume-link')
        (a.source / 'cross-subvolume-link').unlink()
    except OSError as e:
        assert is_btrfs and e.errno == errno.EXDEV
        link_behavior = 'EXDEV across subvolume boundary'
    known = set()
    rows, snaps, hashes = [], [], []
    streams = []
    manifests = []
    used_before = os.statvfs(a.source)
    probe.thread.start()
    try:
        base_start = time.monotonic()
        time.sleep(1)
        base_end = time.monotonic()
        # Base + repeated sparse changes + a 32 MiB rewrite.
        for generation in range(4):
            write_ms, changed = 0, 0
            if generation:
                rewrite = rng.randbytes(32 * MIB) if generation == 3 else None
                begin = time.monotonic()
                with disk.open('r+b', buffering=0) as f:
                    if generation < 3:
                        for index in range(32):
                            f.seek(index * 8 * MIB + generation * 8192)
                            f.write(bytes([generation + index]) * 4096)
                        changed = 32 * 4096
                    else:
                        f.seek(64 * MIB)
                        f.write(rewrite)
                        changed = 32 * MIB
                    if generation < 3:
                        os.fsync(f.fileno())
                write_ms = (time.monotonic() - begin) * 1000
            start = time.monotonic()
            snap = a.source / f'snap{generation}'
            with disk.open('rb') as f:
                os.fsync(f.fileno())
            if is_btrfs:
                run('btrfs', 'subvolume', 'snapshot', '-r', str(live), str(snap))
                snap_disk = snap / 'disk'
            else:
                snap.mkdir()
                snap_disk = snap / 'disk'
                copy_clone(disk, snap_disk)
            fixed = time.monotonic()
            pending_sync = []
            if native_send:
                stream = a.archive / f'{generation}.stream'
                args = ['btrfs', 'send', '--proto', '2']
                if snaps:
                    args += ['-p', str(snaps[-1])]
                args += ['-f', str(stream), str(snap)]
                run(*args)
                streams.append(stream)
                pending_sync.append(stream)
                saved = stream.stat().st_size
                scanned = None  # Native send's read I/O is not instrumented here.
                object_count = 1
            else:
                refs = []
                saved = scanned = object_count = 0
                with snap_disk.open('rb') as f:
                    while data := f.read(MIB):
                        scanned += len(data)
                        key = hashlib.sha256(data).hexdigest()
                        refs.append(key)
                        if key not in known:
                            (objects / key).write_bytes(data)
                            pending_sync.append(objects / key)
                            known.add(key)
                            saved += len(data)
                            object_count += 1
                manifests.append(refs)
                manifest_path = a.archive / f'{generation}.json'
                manifest_path.write_text(json.dumps(refs))
                pending_sync.append(manifest_path)
            # Flush archive file contents; directory durability/publication is not tested.
            for file in pending_sync:
                if file.is_file():
                    with file.open('rb') as f:
                        os.fsync(f.fileno())
            end = time.monotonic()
            # Hash verification excluded from phase timing, with probe still active.
            expected = digest(disk)
            assert digest(snap_disk) == expected
            snaps.append(snap)
            hashes.append(expected)
            rows.append(dict(generation=generation, changed_bytes=changed,
                             foreground_write_ms=round(write_ms, 2),
                             pre_capture_fsync=(generation < 3),
                             stable_view_ms=round((fixed-start)*1000, 2),
                             archive_ms=round((end-fixed)*1000, 2),
                             saved_bytes=saved, new_objects=object_count,
                             explicit_scan_bytes=scanned,
                             probe=probe.stats(start, end),
                             view_probe=probe.stats(start, fixed),
                             archive_probe=probe.stats(fixed, end)))
        # Ensure earlier snapshots still match despite subsequent source writes.
        for snap, expected in zip(snaps, hashes):
            assert digest(snap / 'disk') == expected
        assert hashes[0] == original.hexdigest()
    finally:
        probe.stop.set()
        probe.thread.join()
    if probe.error:
        raise probe.error
    used_after = os.statvfs(a.source)
    missing_parent_ms = None
    if native_send:
        missing_parent_start = time.monotonic()
        # First show a delta alone cannot cold-restore on the empty receiver.
        result = subprocess.run(['btrfs', 'receive', '-f', str(streams[-1]), str(a.receiver)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        assert result.returncode != 0, 'delta unexpectedly restored without its parent'
        missing_parent_ms = round((time.monotonic()-missing_parent_start)*1000, 2)
    restore_start = time.monotonic()
    if native_send:
        for stream in streams:
            run('btrfs', 'receive', '-f', str(stream), str(a.receiver))
        restored = a.receiver / 'snap3' / 'disk'
        restore_objects = len(streams)
        restore_input_bytes = sum(x.stat().st_size for x in streams)
    else:
        restored = a.receiver / 'disk'
        with restored.open('wb') as f:
            for key in manifests[-1]:
                data = (objects / key).read_bytes()
                assert hashlib.sha256(data).hexdigest() == key
                f.write(data)
            f.flush()
            os.fsync(f.fileno())
        restore_objects = len(set(manifests[-1]))
        restore_input_bytes = len(manifests[-1]) * MIB
    restore_ms = (time.monotonic()-restore_start)*1000
    assert digest(restored) == hashes[-1]
    received_attributes = subprocess.check_output(['lsattr', str(restored)], text=True).split()[0]
    print(json.dumps(dict(kind=a.kind, disk_bytes=SIZE, baseline_probe=probe.stats(base_start,base_end),
                         generations=rows, local_available_decrease_bytes=(used_before.f_bavail-used_after.f_bavail)*used_before.f_frsize,
                         cross_subvolume_hardlink=link_behavior, received_attributes=received_attributes,
                         restore_ms=round(restore_ms,2), missing_parent_check_ms=missing_parent_ms,
                         restore_input_bytes=restore_input_bytes,
                         restore_distinct_objects=restore_objects,
                         checks='all snapshots immutable; final digest restored on separate filesystem; native send rejects missing parent',
                         scope='buffered host-file fixture; local archives; NO VM/network/encryption; single trial'), indent=2))


if __name__ == '__main__':
    main()
