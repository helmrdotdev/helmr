package blockproof

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func setup(t *testing.T, blocks, limit int) (*Store, *Disk) {
	t.Helper()
	s, e := NewStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	d, e := NewDisk(s, int64(blocks*BlockSize), limit)
	if e != nil {
		t.Fatal(e)
	}
	return s, d
}
func write(t *testing.T, d *Disk, b []byte, o int64) {
	t.Helper()
	if n, e := d.WriteAt(b, o); e != nil || n != len(b) {
		t.Fatalf("write %d: %v", n, e)
	}
}
func capture(t *testing.T, d *Disk) string {
	t.Helper()
	id, e := d.Capture()
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func restore(t *testing.T, s *Store, id string) *Disk {
	t.Helper()
	d, e := Restore(s, id, 100)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func equalDisk(t *testing.T, d *Disk, b []byte) {
	t.Helper()
	got := make([]byte, len(b))
	if _, e := d.ReadAt(got, 0); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, b) {
		t.Fatal("disk differs from byte oracle")
	}
}
func TestOracleSnapshotsBranchesAndLazyReads(t *testing.T) {
	s, d := setup(t, 12, 12)
	oracle := make([]byte, 12*BlockSize)
	rng := rand.New(rand.NewSource(42))
	var pins []string
	var snapshots [][]byte
	for i := 0; i < 120; i++ {
		n := rng.Intn(6000) + 1
		o := rng.Intn(len(oracle) - n)
		b := make([]byte, n)
		rng.Read(b)
		write(t, d, b, int64(o))
		copy(oracle[o:], b)
		if i%13 == 0 {
			pins = append(pins, capture(t, d))
			snapshots = append(snapshots, bytes.Clone(oracle))
		}
	}
	for i, id := range pins {
		before := s.count()
		r := restore(t, s, id)
		if s.count()-before != 1 {
			t.Fatal("restore fetched data blocks")
		}
		equalDisk(t, r, snapshots[i])
	}
	// Branch writes and explicit zero override an inherited block, preserving pin.
	branch := restore(t, s, pins[len(pins)-1])
	if e := branch.Trim(BlockSize, BlockSize); e != nil {
		t.Fatal(e)
	}
	branchID := capture(t, branch)
	want := bytes.Clone(snapshots[len(snapshots)-1])
	clear(want[BlockSize : 2*BlockSize])
	equalDisk(t, restore(t, s, branchID), want)
	equalDisk(t, restore(t, s, pins[len(pins)-1]), snapshots[len(snapshots)-1])
}
func TestFreezeAllowsLaterWritesAndBoundsAllOutstandingBlocks(t *testing.T) {
	s, d := setup(t, 4, 2)
	a := bytes.Repeat([]byte{1}, BlockSize)
	b := bytes.Repeat([]byte{2}, BlockSize)
	write(t, d, a, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	s.BeforePut = func() error {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan string, 1)
	errs := make(chan error, 1)
	go func() { id, e := d.Capture(); done <- id; errs <- e }()
	<-entered
	write(t, d, b, 0)
	if _, e := d.WriteAt(a, BlockSize); !errors.Is(e, ErrCapacity) {
		t.Fatal("frozen generation did not count toward capacity", e)
	}
	close(release)
	id := <-done
	if e := <-errs; e != nil {
		t.Fatal(e)
	}
	s.BeforePut = nil
	want := make([]byte, 4*BlockSize)
	copy(want, a)
	equalDisk(t, restore(t, s, id), want)
	copy(want, b)
	equalDisk(t, d, want)
}
func TestPublicationFailureFencingAndIntegrity(t *testing.T) {
	s, d := setup(t, 3, 3)
	write(t, d, bytes.Repeat([]byte{1}, BlockSize), 0)
	id := capture(t, d)
	h := new(Head)
	epoch := h.Claim()
	if e := h.Publish(s, epoch, "", id); e != nil {
		t.Fatal(e)
	}
	write(t, d, []byte{9}, 0)
	s.BeforePut = func() error { return errors.New("upload failed") }
	if _, e := d.Capture(); e == nil {
		t.Fatal("upload unexpectedly succeeded")
	}
	if h.ID() != id {
		t.Fatal("head moved")
	}
	s.BeforePut = nil
	next := capture(t, d)
	if e := h.Publish(s, epoch, "wrong", next); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	newEpoch := h.Claim()
	if e := h.Publish(s, epoch, id, next); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	m, e := s.load(next)
	if e != nil {
		t.Fatal(e)
	}
	segment := filepath.Join(s.dir, m.Blocks[0].Segment)
	original, e := os.ReadFile(segment)
	if e != nil {
		t.Fatal(e)
	}
	for _, corrupt := range []bool{false, true} {
		if corrupt {
			bad := bytes.Clone(original)
			bad[0] ^= 255
			if e = os.WriteFile(segment, bad, 0600); e != nil {
				t.Fatal(e)
			}
		} else {
			if e = os.Remove(segment); e != nil {
				t.Fatal(e)
			}
		}
		if e = h.Publish(s, newEpoch, id, next); e == nil {
			t.Fatal("published bad segment")
		}
		if h.ID() != id {
			t.Fatal("bad object moved head")
		}
		r := restore(t, s, next)
		if _, e = r.ReadAt(make([]byte, 1), 0); e == nil {
			t.Fatal("bad read succeeded")
		}
		if e = os.WriteFile(segment, original, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e = h.Publish(s, newEpoch, id, next); e != nil {
		t.Fatal(e)
	}
}
func TestCapacityAndBoundsAreAtomic(t *testing.T) {
	_, d := setup(t, 3, 1)
	if _, e := d.WriteAt(make([]byte, BlockSize+1), 0); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	equalDisk(t, d, make([]byte, 3*BlockSize))
	for _, off := range []int64{-1, 3 * BlockSize} {
		if _, e := d.WriteAt([]byte{1}, off); e == nil {
			t.Fatal("out of range accepted")
		}
	}
}

func TestReadModifyWriteFailureDoesNotPartiallyWrite(t *testing.T) {
	s, d := setup(t, 3, 3)
	write(t, d, bytes.Repeat([]byte{7}, 3*BlockSize), 0)
	id := capture(t, d)
	m, e := s.load(id)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(s.dir, m.Blocks[2].Segment)
	f, e := os.OpenFile(path, os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.WriteAt([]byte{99}, m.Blocks[2].Offset); e != nil {
		t.Fatal(e)
	}
	f.Close()
	r := restore(t, s, id)
	if _, e = r.WriteAt(make([]byte, 2*BlockSize), 1); e == nil {
		t.Fatal("corrupt partial block accepted")
	}
	got := make([]byte, 2*BlockSize)
	if _, e = r.ReadAt(got, 0); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{7}, 2*BlockSize)) {
		t.Fatal("failed write partially mutated earlier blocks")
	}
}
func TestManifestUploadFailureAndCorruption(t *testing.T) {
	s, d := setup(t, 2, 2)
	write(t, d, []byte{8}, 0)
	calls := 0
	s.BeforePut = func() error {
		calls++
		if calls == 2 {
			return errors.New("manifest upload failure")
		}
		return nil
	}
	if _, e := d.Capture(); e == nil {
		t.Fatal("manifest upload succeeded")
	}
	want := make([]byte, 2*BlockSize)
	want[0] = 8
	equalDisk(t, d, want)
	s.BeforePut = nil
	id := capture(t, d)
	equalDisk(t, restore(t, s, id), want)
	if e := os.WriteFile(filepath.Join(s.dir, id), []byte(`{}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Restore(s, id, 2); e == nil {
		t.Fatal("corrupt manifest accepted")
	}
	h := new(Head)
	if e := h.Publish(s, h.Claim(), "", id); e == nil {
		t.Fatal("corrupt manifest published")
	}
}
