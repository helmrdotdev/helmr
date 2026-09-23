"""Bounded format experiment; no production snapshot/crypto/storage implementation."""
import fcntl
import hashlib
import json
import os
import pathlib
import shutil
import tempfile
import time

MIB = 1024 * 1024
SIZE = 128 * MIB


def manifest(path, chunk_size, objects):
    refs = []
    uploaded = scanned = 0
    begin = time.perf_counter()
    with path.open('rb') as source:
        while data := source.read(chunk_size):
            scanned += len(data)
            digest = hashlib.sha256(data).hexdigest()
            refs.append((digest, len(data)))
            if digest not in objects:
                objects[digest] = data
                uploaded += len(data)
    return refs, dict(scan_bytes=scanned, new_object_bytes=uploaded,
                      scan_ms=round((time.perf_counter() - begin) * 1000, 2))


def verify(refs, objects, expected):
    reconstructed = hashlib.sha256()
    for digest, length in refs:
        data = objects[digest]
        assert len(data) == length
        assert hashlib.sha256(data).hexdigest() == digest
        reconstructed.update(data)
    with expected.open('rb') as source:
        assert reconstructed.hexdigest() == hashlib.file_digest(source, 'sha256').hexdigest()


def snapshot(source, target):
    if os.environ.get('PROOF_REQUIRE_REFLINK') == '1':
        with source.open('rb') as src, target.open('wb') as dst:
            fcntl.ioctl(dst.fileno(), 0x40049409, src.fileno())  # Linux FICLONE
    else:
        shutil.copyfile(source, target)


def main():
    with tempfile.TemporaryDirectory(prefix='computer-chunks-') as directory:
        root = pathlib.Path(directory)
        live, baseline, changed = (root / name for name in ['live', 'baseline', 'changed'])
        original_hash = hashlib.sha256()
        with live.open('wb') as disk:
            for index in range(SIZE // 4096):
                data = hashlib.sha256(str(index).encode()).digest() * 128
                disk.write(data)
                original_hash.update(data)
            disk.flush()
            os.fsync(disk.fileno())
        # No concurrent writer at capture. Reflink mode must succeed, never fall back.
        begin = time.perf_counter()
        snapshot(live, baseline)
        stable_view_ms = (time.perf_counter() - begin) * 1000
        with live.open('r+b') as disk:
            for index in range(8):
                disk.seek(index * 16 * MIB + 8192)
                disk.write(bytes([index + 1]) * 4096)
            disk.flush()
            os.fsync(disk.fileno())
        snapshot(live, changed)
        with baseline.open('rb') as source:
            assert hashlib.file_digest(source, 'sha256').digest() == original_hash.digest()
        with changed.open('rb') as source:
            assert hashlib.file_digest(source, 'sha256').digest() != original_hash.digest()
        rows = []
        for chunk_size in [64 * 1024, MIB, 4 * MIB]:
            objects = {}
            first, initial = manifest(baseline, chunk_size, objects)
            second, delta = manifest(changed, chunk_size, objects)
            verify(first, objects, baseline)
            verify(second, objects, changed)
            # A partial candidate must never replace the published head.
            published = first
            new_digest = next(d for d, n in second if d not in {x[0] for x in first})
            missing = objects.pop(new_digest)
            try:
                verify(second, objects, changed)
                raise AssertionError('incomplete candidate accepted')
            except KeyError:
                pass
            verify(published, objects, baseline)
            objects[new_digest] = missing
            verify(second, objects, changed)
            published = second
            assert published != first
            rows.append(dict(chunk_bytes=chunk_size, changed_guest_bytes=8*4096,
                             manifest_entries=len(second), initial=initial, delta=delta))
        print(json.dumps(dict(logical_bytes=SIZE, stable_view_ms=round(stable_view_ms, 2),
                              snapshot_method=('FICLONE; writer stopped at capture; NO VM'
                                               if os.environ.get('PROOF_REQUIRE_REFLINK') == '1'
                                               else 'full copy; writer stopped; NOT COW'),
                              checks='snapshot survives source mutation; old/new reconstruction; incomplete candidate retains old head',
                              rows=rows), indent=2))


if __name__ == '__main__':
    main()
