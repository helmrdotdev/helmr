package cas

import (
	"context"
	"io"
)

// GuestIOWait associates a native storage dependency with the guest request
// that reached it. Background work carries no hook. Calls must be balanced and
// must bracket only blocking storage calls, excluding hashing and encoding.
type GuestIOWait func(waiting bool)

type guestIOWaitKey struct{}

func WithGuestIOWait(ctx context.Context, wait GuestIOWait) context.Context {
	return context.WithValue(ctx, guestIOWaitKey{}, wait)
}

func BeginGuestIOWait(ctx context.Context) GuestIOWait {
	wait, _ := ctx.Value(guestIOWaitKey{}).(GuestIOWait)
	if wait != nil {
		wait(true)
	}
	return wait
}

func (wait GuestIOWait) End() {
	if wait != nil {
		wait(false)
	}
}

// guestIOReader keeps hashing/validation between reads outside the wait.
type guestIOReader struct {
	ctx context.Context
	io.Reader
}

func (r guestIOReader) Read(p []byte) (int, error) {
	wait := BeginGuestIOWait(r.ctx)
	n, err := r.Reader.Read(p)
	wait.End()
	return n, err
}
