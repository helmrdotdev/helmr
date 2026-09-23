"""Local fixture contract tests; these do not run Firecracker or establish KVM proof."""
import contextlib, importlib.util, io, pathlib, tempfile, unittest

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

if __name__ == '__main__': unittest.main()
