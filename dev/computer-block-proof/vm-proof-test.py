"""Local fixture contract tests; these do not run Firecracker or establish KVM proof."""
import contextlib, importlib.util, io, os, pathlib, shutil, tempfile, types, unittest
from unittest.mock import patch

path = pathlib.Path(__file__).with_name('vm-proof.py')
spec = importlib.util.spec_from_file_location('vmproof', path)
proof = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proof)

class OracleContract(unittest.TestCase):
    def test_pair_requires_nonce_fresh_challenge_and_both_disks(self):
        valid = {'nonce': 'n', 'challenge': 'c', 'phase': 'resumed', 'computer': True, 'scratch': True}
        self.assertTrue(proof.oracle_matches(valid, 'n', 'c', True, True))
        for key, wrong in [('nonce', 'old'), ('challenge', 'old'), ('phase', 'boot'),
                           ('computer', False), ('scratch', False)]:
            changed = {**valid, key: wrong}
            self.assertFalse(proof.oracle_matches(changed, 'n', 'c', True, True))
    def test_negative_requires_readable_selected_mismatch(self):
        wrong_computer = {'nonce': 'n', 'challenge': 'c', 'phase': 'resumed', 'computer': False, 'scratch': True}
        self.assertTrue(proof.oracle_matches(wrong_computer, 'n', 'c', False, True))
        self.assertFalse(proof.oracle_matches({**wrong_computer, 'computer_error': 'missing'}, 'n', 'c', False, True))
        self.assertFalse(proof.oracle_matches({**wrong_computer, 'scratch': False}, 'n', 'c', False, True))
        wrong_scratch = {**wrong_computer, 'computer': True, 'scratch': False}
        self.assertTrue(proof.oracle_matches(wrong_scratch, 'n', 'c', True, False))
    def test_timeout_or_missing_result_is_not_negative_success(self):
        self.assertFalse(proof.oracle_matches({}, 'n', 'c', False, True))

class ArenaLifecycle(unittest.TestCase):
    def test_cleanup_failure_preserves_arena_and_original_exception(self):
        with tempfile.TemporaryDirectory() as parent:
            root = pathlib.Path(parent) / 'arena'; root.mkdir()
            def failed(args, arena):
                (arena / 'memory').write_bytes(b'evidence')
                raise RuntimeError('owned driver still alive')
            diagnostic = io.StringIO()
            with contextlib.redirect_stderr(diagnostic), contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(RuntimeError, 'owned driver still alive'):
                    proof.execute_in_arena(None, root, failed)
            self.assertEqual((root / 'memory').read_bytes(), b'evidence')
            self.assertIn(str(root), diagnostic.getvalue())
    def test_successful_verified_cleanup_removes_only_arena(self):
        with tempfile.TemporaryDirectory() as parent:
            root = pathlib.Path(parent) / 'arena'; root.mkdir()
            sibling = pathlib.Path(parent) / 'keep'; sibling.write_bytes(b'keep')
            with contextlib.redirect_stdout(io.StringIO()):
                report = proof.execute_in_arena(None, root, lambda args, arena: {'pass': True})
            self.assertTrue(report['pass'])
            self.assertFalse(root.exists())
            self.assertEqual(sibling.read_bytes(), b'keep')

class QueueBounds(unittest.TestCase):
    def fixture(self, parent, max_kb=32768, discard=2**31):
        queue = pathlib.Path(parent) / 'nbd15/queue'; queue.mkdir(parents=True)
        (queue/'max_sectors_kb').write_text(str(max_kb))
        (queue/'discard_max_bytes').write_text(str(discard))
        attachment = types.SimpleNamespace(claimed=True, fd=10, dev='/dev/nbd15')
        return queue, attachment
    def test_bounds_large_limits_and_preserves_smaller_or_disabled_limits(self):
        for max_kb, discard, expected in [(32768,2**31,(1024,1048576)),(128,0,(128,0)),(512,4096,(512,4096))]:
            with self.subTest(max_kb=max_kb), tempfile.TemporaryDirectory() as root:
                queue, attachment = self.fixture(root,max_kb,discard)
                with contextlib.redirect_stdout(io.StringIO()): proof.bound_queue(attachment,pathlib.Path(root))
                self.assertEqual((int((queue/'max_sectors_kb').read_text()),int((queue/'discard_max_bytes').read_text())),expected)
    def test_refuses_unowned_missing_and_invalid_limits(self):
        with tempfile.TemporaryDirectory() as root:
            queue, attachment = self.fixture(root)
            attachment.claimed=False
            with self.assertRaisesRegex(RuntimeError,'unowned'): proof.bound_queue(attachment,pathlib.Path(root))
            self.assertEqual((queue/'max_sectors_kb').read_text(),'32768')
            attachment.claimed=True; (queue/'max_sectors_kb').write_text('0')
            with self.assertRaisesRegex(RuntimeError,'invalid'): proof.bound_queue(attachment,pathlib.Path(root))
            (queue/'max_sectors_kb').unlink()
            with self.assertRaises(FileNotFoundError): proof.bound_queue(attachment,pathlib.Path(root))
    def test_ignored_or_rejected_write_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            _, attachment = self.fixture(root)
            with contextlib.redirect_stdout(io.StringIO()), patch.object(pathlib.Path,'write_text',return_value=0):
                with self.assertRaisesRegex(RuntimeError,'did not retain'): proof.bound_queue(attachment,pathlib.Path(root))
            with contextlib.redirect_stdout(io.StringIO()), patch.object(pathlib.Path,'write_text',side_effect=PermissionError('read-only sysfs')):
                with self.assertRaises(PermissionError): proof.bound_queue(attachment,pathlib.Path(root))

@unittest.skipUnless(os.environ.get('HELMR_DISPOSABLE_NBD_PROOF')=='1', 'requires explicit disposable NBD fixture')
class KernelBulk(unittest.TestCase):
    def test_buffered_seed_flush_readback(self):
        # Only the caller-exposed unused devices can be atomically claimed by
        # the reviewed helper. No filesystem is formatted or mounted here.
        binary=os.environ['BLOCK_PROOF_BINARY']
        payload=os.urandom(16*1024*1024)
        root=pathlib.Path(tempfile.mkdtemp(prefix='helmr-nbd-bulk-'))
        print('bulk proof arena: '+str(root),flush=True)
        server=None; attachment=None
        try:
            server=proof.nbd.start_server(binary,str(root/'state'),str(root/'backend.sock'),True)
            attachment=proof.attach(str(root/'backend.sock'),root)
            for off in range(0,len(payload),4096):
                self.assertEqual(os.pwrite(attachment.fd,payload[off:off+4096],off),4096)
            os.fsync(attachment.fd)
            proof.stop(server); server=None
            attachment.close(); attachment=None
            # Different attachment and restarted backend exclude the old
            # host block cache from satisfying the persisted-byte oracle.
            server=proof.nbd.start_server(binary,str(root/'state'),str(root/'reopened.sock'))
            attachment=proof.attach(str(root/'reopened.sock'),root)
            actual=bytearray()
            for off in range(0,len(payload),4096): actual.extend(os.pread(attachment.fd,4096,off))
            self.assertEqual(actual,payload)
        finally:
            if server: proof.stop(server)
            for owned in reversed(proof.nbd.owned): owned.close()
        for owned in proof.nbd.owned:
            self.assertFalse(os.path.exists('/sys/block/'+os.path.basename(owned.dev)+'/pid'))
        shutil.rmtree(root)

if __name__ == '__main__': unittest.main()
