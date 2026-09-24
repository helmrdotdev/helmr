//go:build linux || darwin

package computer

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

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"golang.org/x/sys/unix"
)

// LocalGenerationConfig binds one private host directory to its retained source.
// The caller keeps BaseSource and all supplied key references retained, supplies
// stable keys during construction, and reserves StagedBytes for local ciphertext.
// Parent directory durability and exclusive host ownership are prerequisites.
type LocalGenerationConfig struct {
	Directory        string
	Base             GenerationRoot
	BaseSource       blockformat.RangeSource
	Scope, ActiveKey string
	Keys             map[string][]byte
	DirtyBlocks      int
	StagedBytes      int64
	PackLimit        int
}

type localGenerationHead struct {
	FormatVersion int            `json:"format_version"`
	Scope         string         `json:"scope"`
	Base          GenerationRoot `json:"base"`
	Root          GenerationRoot `json:"root"`
}

type localGenerationSource struct {
	local *cas.File
	base  blockformat.RangeSource
}

func (s localGenerationSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	r, err := s.local.GetRange(ctx, digest, size, offset, length)
	if errors.Is(err, os.ErrNotExist) {
		return s.base.GetRange(ctx, digest, size, offset, length)
	}
	return r, err
}

// LocalGeneration owns an exclusive process lock and a crash-recoverable local
// root. Flush success means ciphertext and the selected root are fsynced locally;
// it is not external durability and does not tolerate loss of the host's disk.
// Do not unlink/replace the directory or owner.lock while any owner can use it.
type LocalGeneration struct {
	life          sync.RWMutex
	commit        sync.Mutex
	store         *cas.File
	disk          *WritableGeneration
	directory     string
	head          localGenerationHead
	installedRoot GenerationRoot
	lock          *os.File
	closed        bool
	stagedBytes   int64
	captures      map[*LocalCapture]GenerationRoot
	phase         func(string) error // Deterministic test crash/fault injection, set before use.
}

func syncGenerationDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func lockLocalGeneration(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("local generation requires private existing directory")
	}
	fd, err := unix.Open(filepath.Join(path, "owner.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "generation owner")
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// CreateLocalGeneration requires a new directory. Failed creation leaves its
// evidence for explicit owner cleanup; it never adopts an existing local root.
func CreateLocalGeneration(ctx context.Context, cfg LocalGenerationConfig) (_ *LocalGeneration, retErr error) {
	if !filepath.IsAbs(cfg.Directory) || cfg.BaseSource == nil {
		return nil, errors.New("absolute private directory and retained source required")
	}
	if err := os.Mkdir(cfg.Directory, 0700); err != nil {
		return nil, err
	}
	if err := syncGenerationDirectory(filepath.Dir(cfg.Directory)); err != nil {
		return nil, err
	}
	lock, err := lockLocalGeneration(cfg.Directory)
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
	if err = syncGenerationDirectory(cfg.Directory); err != nil {
		return nil, err
	}
	p, err := newLocalGeneration(ctx, cfg, lock, localGenerationHead{FormatVersion: 1, Scope: cfg.Scope, Base: cfg.Base, Root: cfg.Base})
	if err != nil {
		return nil, err
	}
	if err = p.persist(ctx, p.head); err != nil {
		_ = p.disk.Close()
		return nil, err
	}
	return p, nil
}

// OpenLocalGeneration authenticates the recorded root and checks its admitted
// base/scope; missing or corrupt state is an error, never fresh initialization.
func OpenLocalGeneration(ctx context.Context, cfg LocalGenerationConfig) (_ *LocalGeneration, retErr error) {
	if !filepath.IsAbs(cfg.Directory) || cfg.BaseSource == nil {
		return nil, errors.New("absolute private directory and retained source required")
	}
	lock, err := lockLocalGeneration(cfg.Directory)
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
	var head localGenerationHead
	err = decoder.Decode(&head)
	if err == nil {
		if e := decoder.Decode(new(any)); e != io.EOF {
			err = errors.New("local generation root has trailing data")
		}
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return nil, err
	}
	if head.FormatVersion != 1 || head.Scope != cfg.Scope || head.Base != cfg.Base || head.Root.LogicalBytes != cfg.Base.LogicalBytes {
		return nil, errors.New("local generation admission differs from recorded source")
	}
	return newLocalGeneration(ctx, cfg, lock, head)
}

func newLocalGeneration(ctx context.Context, cfg LocalGenerationConfig, lock *os.File, head localGenerationHead) (*LocalGeneration, error) {
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
			if err = syncGenerationDirectory(cfg.Directory); err != nil {
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
			return syncGenerationDirectory(filepath.Dir(path))
		}
		if stat.Size() > remaining {
			return ErrGenerationStagingFull
		}
		remaining -= stat.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if remaining < 0 {
		return nil, ErrGenerationStagingFull
	}
	store, err := cas.NewFile(objects)
	if err != nil {
		return nil, err
	}
	writer := blockformat.Writer{Source: localGenerationSource{local: store, base: cfg.BaseSource}, Sink: store, Scope: cfg.Scope, ActiveKey: cfg.ActiveKey, Keys: cfg.Keys, PackLimit: cfg.PackLimit}
	disk, err := OpenWritableGeneration(ctx, writer, head.Root, cfg.DirtyBlocks, remaining)
	if err != nil {
		return nil, err
	}
	return &LocalGeneration{store: store, disk: disk, directory: cfg.Directory, head: head, installedRoot: head.Root, lock: lock, stagedBytes: cfg.StagedBytes, captures: make(map[*LocalCapture]GenerationRoot)}, nil
}

func (p *LocalGeneration) step(phase string) error {
	if p.phase != nil {
		return p.phase(phase)
	}
	return nil
}
func (p *LocalGeneration) persist(ctx context.Context, head localGenerationHead) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(head)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(p.directory, ".root-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = p.step("root-synced"); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(p.directory, "root")); err != nil {
		return err
	}
	p.installedRoot = head.Root
	if err = p.step("root-renamed"); err != nil {
		return err
	}
	if err = syncGenerationDirectory(p.directory); err != nil {
		return err
	}
	return p.step("root-committed")
}

func (p *LocalGeneration) ReadAt(ctx context.Context, b []byte, off int64) (int, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	return p.disk.ReadAt(ctx, b, off)
}
func (p *LocalGeneration) WriteAt(ctx context.Context, b []byte, off int64) (int, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	return p.disk.WriteAt(ctx, b, off)
}
func (p *LocalGeneration) Trim(ctx context.Context, off int64, n int) error {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	return p.disk.Trim(ctx, off, n)
}

func (p *LocalGeneration) Flush(ctx context.Context) (GenerationRoot, error) {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return GenerationRoot{}, os.ErrClosed
	}
	p.commit.Lock()
	defer p.commit.Unlock()
	return p.flushLocked(ctx)
}

func (p *LocalGeneration) flushLocked(ctx context.Context) (GenerationRoot, error) {
	root, err := p.disk.Capture(ctx)
	if err != nil {
		return GenerationRoot{}, err
	}
	if err = p.step("objects-staged"); err != nil {
		return GenerationRoot{}, err
	}
	head := p.head
	head.Root = root
	if err = p.persist(ctx, head); err != nil {
		return GenerationRoot{}, err
	}
	p.head = head
	return root, nil
}

// Close does not flush unacknowledged writes or remove any recovery evidence.
func (p *LocalGeneration) Close() error {
	p.life.Lock()
	defer p.life.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return errors.Join(p.disk.Close(), p.lock.Close())
}

// Publish exports the root returned by a prior successful Flush. Local bytes stay
// retained while newer writes and flushes proceed. Close joins publication before
// clearing keys. The caller must keep remote source/key ownership until the
// generation has been committed by the Control Plane; this does not commit it.
func (p *LocalGeneration) Publish(ctx context.Context, root GenerationRoot, publisher ContinuationPublication, maxObjects int) error {
	p.life.RLock()
	defer p.life.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	p.commit.Lock()
	capacity := p.head.Base.LogicalBytes
	pin := &LocalCapture{}
	p.captures[pin] = root
	p.commit.Unlock()
	defer func() { p.commit.Lock(); delete(p.captures, pin); p.commit.Unlock() }()
	if root.LogicalBytes != capacity {
		return errors.New("publication capacity differs from admitted Computer")
	}
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		return err
	}
	_, err = publishGeneration(ctx, p.store, p.disk.writer.Source, p.disk.writer.Scope, p.disk.writer.Keys, locator, root.LogicalBytes, maxObjects, publisher, publisher)
	return err
}
