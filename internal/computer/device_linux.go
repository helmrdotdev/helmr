//go:build linux

package computer

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/helmrdotdev/helmr/internal/nbd"
	"golang.org/x/sys/unix"
)

// Device owns a local generation, its export and its exclusive kernel attachment.
// Source retention and capacity reservations remain with the Runtime owner.
// Close never releases those reservations or deletes recovery evidence.
type Device struct {
	mu         sync.Mutex
	disk       *LocalGeneration
	attachment *nbd.Attachment
	cancel     context.CancelFunc
	done       chan struct{}
	serveErr   error
	closed     bool
}

// AttachDevice transfers disk ownership even on failure: a non-nil result must
// be closed successfully before its arena or source reservations can be removed.
// An uncertain helper claim leaves the export alive for explicit reconciliation.
// cfg.Arena must be a private existing directory independent of the VMM jail.
func AttachDevice(ctx context.Context, disk *LocalGeneration, cfg nbd.Config) (*Device, error) {
	if disk == nil {
		return nil, errors.New("local generation required")
	}
	d := &Device{disk: disk}
	if err := ctx.Err(); err != nil {
		return d, err
	}
	disk.life.RLock()
	size, closed := disk.head.Root.LogicalBytes, disk.closed
	disk.life.RUnlock()
	arena, arenaErr := os.Lstat(cfg.Arena)
	if arenaErr != nil || !arena.IsDir() || arena.Mode().Perm() != 0700 || !filepath.IsAbs(cfg.Arena) {
		return d, errors.Join(errors.New("private absolute device arena required"), arenaErr)
	}
	if closed || cfg.Size != size || filepath.Dir(cfg.Socket) != cfg.Arena {
		return d, errors.New("device geometry or arena differs from generation")
	}
	// ListenUnix must not unlink an earlier owner's socket on failed setup.
	listener, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return d, err
	}
	if err := os.Chmod(cfg.Socket, 0600); err != nil {
		_ = listener.Close()
		return d, err
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	d.cancel, d.done = cancel, make(chan struct{})
	go func() {
		d.serveErr = disk.ServeNBD(serveCtx, listener)
		if serveCtx.Err() != nil {
			d.serveErr = errors.Join(serveCtx.Err(), d.serveErr)
		}
		close(d.done)
	}()
	d.attachment, err = nbd.Claim(ctx, cfg)
	return d, err
}

// BindConsumer must precede VMM launch; only its lifecycle owner can certify exit.
func (d *Device) BindConsumer(exited <-chan struct{}) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.attachment == nil {
		return errors.New("device attachment unavailable")
	}
	return d.attachment.BindConsumer(exited)
}

// LinkInto exposes an owned private device node without changing the host's
// global /dev node. The Runtime owns directory and must retain it until exit.
func (d *Device) LinkInto(ctx context.Context, directory string, uid, gid int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.attachment == nil {
		return "", errors.New("device attachment unavailable")
	}
	if !filepath.IsAbs(directory) {
		return "", errors.New("absolute VMM directory required")
	}
	source, err := d.attachment.ExposeInto(ctx, uid, gid)
	if err != nil {
		return "", err
	}
	node, err := os.OpenFile(source, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer node.Close()
	info, err := node.Stat()
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return "", errors.Join(errors.New("owned block device required"), err)
	}
	target := filepath.Join(directory, "computer.nbd")
	if err := unix.Linkat(unix.AT_FDCWD, source, unix.AT_FDCWD, target, 0); err != nil {
		return "", err
	}
	linked, err := os.Lstat(target)
	if err != nil || !os.SameFile(info, linked) {
		return "", errors.Join(errors.New("device node identity changed"), err)
	}
	return target, nil
}

// Flush requires the VMM dispatch hold. It drains the kernel device first, then
// returns the fsynced local generation. This is not remote publication.
func (d *Device) Flush(ctx context.Context) (GenerationRoot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.attachment == nil {
		return GenerationRoot{}, errors.New("device attachment unavailable")
	}
	if err := d.attachment.Flush(ctx); err != nil {
		return GenerationRoot{}, err
	}
	return d.disk.Flush(ctx)
}

// Wait reports export termination, not consumer exit or safe device release.
// A live VMM owner must stop the VMM if the export terminates unexpectedly.
func (d *Device) Wait(ctx context.Context) error {
	if d.done == nil {
		return errors.New("device export was not started")
	}
	select {
	case <-d.done:
		return d.serveErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is retryable after an unproven consumer/helper exit. It never cancels the
// export or closes the generation while the kernel attachment may still use it.
func (d *Device) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.attachment != nil {
		if err := d.attachment.Release(ctx); err != nil {
			return err
		}
	}
	if d.cancel != nil {
		d.cancel()
		select {
		case <-d.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := d.disk.Close(); err != nil {
		return err
	}
	d.closed = true
	return nil
}
