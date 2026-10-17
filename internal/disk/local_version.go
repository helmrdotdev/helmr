//go:build linux || darwin

package disk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"golang.org/x/sys/unix"
)

// LocalVersionConfig binds one private host directory to its retained source.
// The caller keeps BaseSource and all supplied key references retained, supplies
// stable keys during construction, and reserves StagedBytes for local ciphertext.
// Parent directory durability and exclusive host ownership are prerequisites.
type LocalVersionConfig struct {
	Directory        string
	Base             VersionRoot
	BaseSource       blockformat.RangeSource
	Scope, ActiveKey string
	Keys             map[string][]byte
	DirtyBlocks      int
	StagedBytes      int64
	PackLimit        int
}

type localVersionHead struct {
	FormatVersion int         `json:"format_version"`
	Scope         string      `json:"scope"`
	Base          VersionRoot `json:"base"`
	Root          VersionRoot `json:"root"`
	Saved         VersionRoot `json:"saved"`
}

type localVersionSource struct {
	local *cas.File
	base  blockformat.RangeSource
	reads *versionRangeCache
}

func (s localVersionSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	r, err := (guestIORangeSource{s.local}).GetRange(ctx, digest, size, offset, length)
	if errors.Is(err, os.ErrNotExist) {
		return s.reads.GetRange(ctx, digest, size, offset, length)
	}
	return r, err
}

func (s localVersionSource) GetPackRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	r, err := (guestIORangeSource{s.local}).GetRange(ctx, digest, size, offset, length)
	if errors.Is(err, os.ErrNotExist) {
		return s.reads.GetPackRange(ctx, digest, size, offset, length)
	}
	return r, err
}

// LocalVersion owns an exclusive process lock and a crash-recoverable local
// root. Flush success means ciphertext and the selected root are fsynced locally;
// it is not external durability and does not tolerate loss of the host's disk.
// Do not unlink/replace the directory or owner.lock while any owner can use it.
type LocalVersion struct {
	guestRequest    atomic.Pointer[guestRequestProgress]
	guestActivity   chan struct{}
	life            sync.RWMutex
	request         sync.Mutex // Complete NBD requests versus the immutable cut only.
	commit          sync.Mutex
	collection      sync.Mutex
	storage         sync.RWMutex // Short deletion batches versus readers and encoders.
	revision        uint64       // Changes before each encoder, under commit.
	publication     sync.RWMutex // Joins local-file publishers before reachable eviction.
	store           *cas.File
	objectBatch     func() captureObjectBatch
	reads           *versionRangeCache
	evidence        *versionEvidenceCache
	disk            *WritableVersion
	directory       string
	head            localVersionHead
	installedRoot   VersionRoot
	lock            *os.File
	closed          bool
	stagedBytes     int64
	captures        map[*LocalCapture]VersionRoot
	headGeneration  uint64
	verifiedBacking map[string]bool
	phase           func(string) error // Deterministic test crash/fault injection, set before use.
}

func syncVersionDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func lockLocalVersion(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("local version requires private existing directory")
	}
	fd, err := unix.Open(filepath.Join(path, "owner.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "version owner")
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// CreateLocalVersion requires a new directory. Failed creation leaves its
// evidence for explicit owner cleanup; it never adopts an existing local root.
func CreateLocalVersion(ctx context.Context, cfg LocalVersionConfig) (_ *LocalVersion, retErr error) {
	if !filepath.IsAbs(cfg.Directory) || cfg.BaseSource == nil {
		return nil, errors.New("absolute private directory and retained source required")
	}
	if err := os.Mkdir(cfg.Directory, 0700); err != nil {
		return nil, err
	}
	if err := syncVersionDirectory(filepath.Dir(cfg.Directory)); err != nil {
		return nil, err
	}
	lock, err := lockLocalVersion(cfg.Directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	objects := filepath.Join(cfg.Directory, "objects")
	if err = os.Mkdir(objects, 0700); err != nil {
		return nil, err
	}
	if err = syncVersionDirectory(cfg.Directory); err != nil {
		return nil, err
	}
	p, err := newLocalVersion(ctx, cfg, lock, localVersionHead{FormatVersion: 1, Scope: cfg.Scope, Base: cfg.Base, Root: cfg.Base, Saved: cfg.Base})
	if err != nil {
		return nil, err
	}
	if err = p.persist(ctx, p.head); err != nil {
		_ = p.disk.Close()
		return nil, err
	}
	return p, nil
}

// OpenLocalVersion authenticates the recorded root and checks its admitted
// base/scope; missing or corrupt state is an error, never fresh initialization.
func OpenLocalVersion(ctx context.Context, cfg LocalVersionConfig) (_ *LocalVersion, retErr error) {
	if !filepath.IsAbs(cfg.Directory) || cfg.BaseSource == nil {
		return nil, errors.New("absolute private directory and retained source required")
	}
	lock, err := lockLocalVersion(cfg.Directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	file, err := os.Open(filepath.Join(cfg.Directory, "root"))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	var head localVersionHead
	err = decoder.Decode(&head)
	if err == nil {
		if e := decoder.Decode(new(any)); e != io.EOF {
			err = errors.New("local version root has trailing data")
		}
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	if head.FormatVersion != 1 || head.Scope != cfg.Scope || head.Base != cfg.Base || head.Root.LogicalBytes != cfg.Base.LogicalBytes || head.Saved.Validate(cfg.Base.LogicalBytes) != nil {
		return nil, errors.New("local version admission differs from recorded source")
	}
	return newLocalVersion(ctx, cfg, lock, head)
}

func newLocalVersion(ctx context.Context, cfg LocalVersionConfig, lock *os.File, head localVersionHead) (*LocalVersion, error) {
	objects := filepath.Join(cfg.Directory, "objects")
	info, err := os.Lstat(objects)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("local object directory is missing or invalid")
	}
	// Unlinked root/object stages cannot be referenced by a committed root.
	// The process lock excludes every writer before this private scratch cleanup.
	entries, err := os.ReadDir(cfg.Directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".root-") {
			if !entry.Type().IsRegular() {
				return nil, errors.New("unexpected root stage")
			}
			if err = os.Remove(filepath.Join(cfg.Directory, entry.Name())); err != nil {
				return nil, err
			}
			if err = syncVersionDirectory(cfg.Directory); err != nil {
				return nil, err
			}
		}
	}
	// Count retained objects after scratch cleanup; restart cannot reset the
	// staging reservation and accumulate an unbounded orphan history.
	remaining := cfg.StagedBytes
	err = filepath.WalkDir(objects, func(path string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if entry.IsDir() {
			return nil
		}
		stat, e := entry.Info()
		if e != nil {
			return e
		}
		if !stat.Mode().IsRegular() {
			return errors.New("unexpected local object entry")
		}
		if strings.HasPrefix(entry.Name(), ".object-") {
			if e = os.Remove(path); e != nil {
				return e
			}
			return syncVersionDirectory(filepath.Dir(path))
		}
		if stat.Size() > remaining {
			return ErrVersionStagingFull
		}
		remaining -= stat.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if remaining < 0 {
		return nil, ErrVersionStagingFull
	}
	store, err := cas.NewFile(objects)
	if err != nil {
		return nil, err
	}
	source := guestIORangeSource{cfg.BaseSource}
	reads := newVersionRangeCache(source)
	writer := blockformat.Writer{Source: localVersionSource{local: store, base: source, reads: reads}, Sink: store, Scope: cfg.Scope, ActiveKey: cfg.ActiveKey, Keys: cfg.Keys, PackLimit: cfg.PackLimit}
	disk, err := OpenWritableVersion(ctx, writer, head.Root, cfg.DirtyBlocks, remaining)
	if err != nil {
		return nil, err
	}
	return &LocalVersion{guestActivity: make(chan struct{}, 1), store: store, objectBatch: func() captureObjectBatch { return store.NewObjectBatch() }, reads: reads, evidence: newVersionEvidenceCache(), disk: disk, directory: cfg.Directory, head: head, installedRoot: head.Root, lock: lock, stagedBytes: cfg.StagedBytes, captures: make(map[*LocalCapture]VersionRoot), verifiedBacking: make(map[string]bool)}, nil
}

func (p *LocalVersion) step(phase string) error {
	if p.phase != nil {
		return p.phase(phase)
	}
	return nil
}
func (p *LocalVersion) persist(ctx context.Context, head localVersionHead) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(head)
	if err != nil {
		return err
	}
	wait := cas.BeginGuestIOWait(ctx)
	file, err := os.CreateTemp(p.directory, ".root-")
	wait.End()
	if err != nil {
		return err
	}
	defer func() { wait := cas.BeginGuestIOWait(ctx); _ = os.Remove(file.Name()); wait.End() }()
	wait = cas.BeginGuestIOWait(ctx)
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	wait.End()
	if err != nil {
		return err
	}
	if err = p.step("root-synced"); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	wait = cas.BeginGuestIOWait(ctx)
	err = os.Rename(file.Name(), filepath.Join(p.directory, "root"))
	wait.End()
	if err != nil {
		return err
	}
	p.installedRoot = head.Root
	if err = p.step("root-renamed"); err != nil {
		return err
	}
	wait = cas.BeginGuestIOWait(ctx)
	err = syncVersionDirectory(p.directory)
	wait.End()
	if err != nil {
		return err
	}
	return p.step("root-committed")
}

func (p *LocalVersion) ReadAt(ctx context.Context, b []byte, off int64) (int, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	readLockGuestIO(ctx, &p.storage)
	defer p.storage.RUnlock()
	n, err := p.disk.ReadAt(ctx, b, off)
	return n, deviceFailure(err)
}
func (p *LocalVersion) WriteAt(ctx context.Context, b []byte, off int64) (int, error) {
	return p.write(ctx, b, off, len(b), true)
}
func (p *LocalVersion) Trim(ctx context.Context, off int64, n int) error {
	_, err := p.write(ctx, nil, off, n, true)
	return err
}

// A tentative whole-request admission does not announce pressure: the smaller
// fallback can still make progress. Its first rejected block reports pressure.
func (p *LocalVersion) write(ctx context.Context, b []byte, off int64, length int, reportPressure bool) (int, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	readLockGuestIO(ctx, &p.storage)
	defer p.storage.RUnlock()
	n, err := p.disk.write(ctx, b, off, length, reportPressure)
	return n, deviceFailure(err)
}

func (p *LocalVersion) Flush(ctx context.Context) (VersionRoot, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return VersionRoot{}, os.ErrClosed
	}
	readLockGuestIO(ctx, &p.storage)
	defer p.storage.RUnlock()
	wait := cas.BeginGuestIOWait(ctx)
	p.commit.Lock()
	wait.End()
	defer p.commit.Unlock()
	root, err := p.flushLocked(ctx)
	return root, deviceFailure(err)
}

func (p *LocalVersion) flushLocked(ctx context.Context) (VersionRoot, error) {
	return p.flushBoundaryLocked(ctx, nil)
}

func (p *LocalVersion) flushBoundaryLocked(ctx context.Context, cut func()) (VersionRoot, error) {
	p.revision++
	root, err := p.disk.captureBoundary(ctx, cut, p.objectBatch())
	if err != nil {
		return VersionRoot{}, err
	}
	if err = p.step("objects-staged"); err != nil {
		return VersionRoot{}, err
	}
	head := p.head
	head.Root = root
	if err = p.persist(ctx, head); err != nil {
		return VersionRoot{}, err
	}
	p.head = head
	p.disk.mu.Lock()
	p.headGeneration = p.disk.rootGeneration
	p.disk.mu.Unlock()
	return root, nil
}

// Close does not flush unacknowledged writes or remove any recovery evidence.
func (p *LocalVersion) Close() error {
	p.life.Lock()
	defer p.life.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.reads.clear()
	return errors.Join(p.disk.Close(), p.lock.Close())
}

// Changes observes the local writer without filesystem or remote I/O.
func (p *LocalVersion) Changes() VersionChanges { return p.disk.Changes() }
