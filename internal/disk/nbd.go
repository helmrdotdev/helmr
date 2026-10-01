//go:build linux || darwin

package disk

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/nbd"
)

// ServeNBDTransmission serves one negotiated private connection. The caller
// exclusively owns the connection and disk, closes the connection on cancellation,
// and waits for this call before releasing the disk. Flush is host-local only.
func (p *LocalVersion) ServeNBDTransmission(ctx context.Context, stream io.ReadWriter) error {
	p.life.RLock()
	size := p.head.Root.LogicalBytes
	p.life.RUnlock()
	return nbd.ServeTransmission(ctx, stream, versionDevice{p}, size)
}

type versionDevice struct{ *LocalVersion }

func versionDeviceError(err error) error {
	if errors.Is(err, ErrVersionBufferFull) || errors.Is(err, ErrVersionStagingFull) {
		return errors.Join(syscall.ENOSPC, err)
	}
	return err
}

// Split at block boundaries so any admitted dirty budget can serve the export.
// Buffer pressure commits locally and retries the unapplied block once; staged
// storage exhaustion remains an error. No remote publication is triggered.
func (d versionDevice) WriteAt(ctx context.Context, b []byte, off int64) (int, error) {
	total := 0
	for len(b) > 0 {
		n := min(len(b), 4096-int(off%4096))
		_, err := d.LocalVersion.WriteAt(ctx, b[:n], off)
		if errors.Is(err, ErrVersionBufferFull) {
			if err = d.Flush(ctx); err == nil {
				_, err = d.LocalVersion.WriteAt(ctx, b[:n], off)
			}
		}
		if err != nil {
			return total, versionDeviceError(err)
		}
		total += n
		off += int64(n)
		b = b[n:]
	}
	return total, nil
}
func (d versionDevice) Trim(ctx context.Context, off int64, n int) error {
	for n > 0 {
		length := min(n, 4096-int(off%4096))
		err := d.LocalVersion.Trim(ctx, off, length)
		if errors.Is(err, ErrVersionBufferFull) {
			if err = d.Flush(ctx); err == nil {
				err = d.LocalVersion.Trim(ctx, off, length)
			}
		}
		if err != nil {
			return versionDeviceError(err)
		}
		n -= length
		off += int64(length)
	}
	return nil
}
func (d versionDevice) Flush(ctx context.Context) error {
	_, err := d.LocalVersion.Flush(ctx)
	return versionDeviceError(err)
}

// ServeNBD owns a private listener for one exclusive device connection, including
// negotiation and cancellation. It returns after all connection I/O has stopped;
// the caller remains responsible for device/consumer exclusion before disk Close.
func (p *LocalVersion) ServeNBD(ctx context.Context, listener net.Listener) error {
	p.life.RLock()
	size := p.head.Root.LogicalBytes
	p.life.RUnlock()
	return nbd.ServeOnce(ctx, listener, versionDevice{p}, size)
}
