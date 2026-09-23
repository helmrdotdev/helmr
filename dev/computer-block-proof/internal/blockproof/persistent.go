package blockproof

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Persistent owns a local root and its advisory process lock. Flush commits a
// pinned generation independently of remote Head. The private directory must be
// on an intact local filesystem; this proof does not establish power-loss safety.
type Persistent struct {
	lifecycle sync.RWMutex
	commit    sync.Mutex
	disk      *Disk
	dir       string
	lock      *os.File
	closed    bool
	// phase is test-only deterministic fault/crash injection, set before use.
	phase func(string) error
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func acquire(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("local disk already owned: %w", err)
	}
	return f, nil
}
func release(f *os.File) error { return f.Close() } // Closing the descriptor releases flock, including after process death.

// CreatePersistent requires a missing root; OpenPersistent never initializes a
// missing/corrupt root. Failed initial creation may leave unreferenced objects.
func CreatePersistent(dir string, size int64, limit int) (*Persistent, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := acquire(dir)
	if err != nil {
		return nil, err
	}
	p := &Persistent{dir: dir, lock: lock}
	fail := func(err error) (*Persistent, error) { release(lock); return nil, err }
	if _, err = os.Lstat(filepath.Join(dir, "root")); !os.IsNotExist(err) {
		if err == nil {
			err = errors.New("local root already exists")
		}
		return fail(err)
	}
	store, err := NewStore(filepath.Join(dir, "objects"))
	if err != nil {
		return fail(err)
	}
	p.disk, err = NewDisk(store, size, limit)
	if err != nil {
		return fail(err)
	}
	if err = p.Flush(); err != nil {
		return fail(err)
	}
	return p, nil
}
func OpenPersistent(dir string, limit int) (*Persistent, error) {
	lock, err := acquire(dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Persistent, error) { release(lock); return nil, err }
	root, err := os.ReadFile(filepath.Join(dir, "root"))
	if err != nil {
		return fail(err)
	}
	// Do not repair missing object directories while opening.
	objects := filepath.Join(dir, "objects")
	info, err := os.Stat(objects)
	if err != nil {
		return fail(err)
	}
	if !info.IsDir() {
		return fail(errors.New("objects is not a directory"))
	}
	store := &Store{dir: objects}
	disk, err := Restore(store, string(root), limit)
	if err != nil {
		return fail(err)
	}
	for k, r := range disk.base {
		if _, err = store.block(r); err != nil {
			return fail(fmt.Errorf("root block %d: %w", k, err))
		}
	}
	return &Persistent{dir: dir, lock: lock, disk: disk}, nil
}
func (p *Persistent) Size() int64 { return p.disk.Size() }
func (p *Persistent) ReadAt(b []byte, off int64) (int, error) {
	p.lifecycle.RLock()
	defer p.lifecycle.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	return p.disk.ReadAt(b, off)
}
func (p *Persistent) WriteAt(b []byte, off int64) (int, error) {
	p.lifecycle.RLock()
	defer p.lifecycle.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	return p.disk.WriteAt(b, off)
}
func (p *Persistent) Trim(off int64, n int) error {
	p.lifecycle.RLock()
	defer p.lifecycle.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	return p.disk.Trim(off, n)
}
func (p *Persistent) step(phase string) error {
	if p.phase != nil {
		return p.phase(phase)
	}
	return nil
}
func (p *Persistent) Flush() error {
	p.lifecycle.RLock()
	defer p.lifecycle.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	// Capture and root publication share one order: a delayed old flush can never
	// publish after a newer flush. Foreground writes can still proceed during IO.
	p.commit.Lock()
	defer p.commit.Unlock()
	id, err := p.disk.Capture()
	if err != nil {
		return err
	}
	if err = p.step("objects-staged"); err != nil {
		return err
	}
	// Validate inherited references too; a successful flush never certifies a
	// root already known to depend on missing or corrupt data.
	m, err := p.disk.store.load(id)
	if err != nil {
		return err
	}
	for _, r := range m.Blocks {
		if _, err = p.disk.store.block(r); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(p.dir, "root-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.WriteString(id); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = p.step("root-synced"); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(p.dir, "root")); err != nil {
		return err
	}
	if err = p.step("root-renamed"); err != nil {
		return err
	}
	if err = syncDir(p.dir); err != nil {
		return err
	}
	return p.step("root-committed")
}

// Close releases ownership. It deliberately does not flush unacknowledged writes.
func (p *Persistent) Close() error {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return release(p.lock)
}
