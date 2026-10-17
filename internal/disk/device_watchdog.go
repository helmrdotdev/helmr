//go:build linux || darwin

package disk

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"sync/atomic"
	"time"
)

// ErrDeviceHung distinguishes an unresponsive operation from a deliberately
// parked staging-pressure request. Kernel NBD timeouts cannot make this distinction.
var ErrDeviceHung = errors.New("computer device request made no progress")

const guestRequestHangLimit = 10 * time.Second

type guestRequestProgress struct {
	state atomic.Pointer[guestRequestState]
	wake  chan struct{}
}
type guestRequestState struct {
	at    time.Time
	waits int
}

func (p *LocalVersion) beginGuestRequest() {
	request := &guestRequestProgress{wake: p.guestActivity}
	request.state.Store(&guestRequestState{at: time.Now()})
	p.guestRequest.Store(request)
	request.notify()
}
func (p *LocalVersion) guestRequestContext(ctx context.Context) context.Context {
	return cas.WithGuestIOWait(ctx, p.guestRequest.Load().storageWait)
}
func (r *guestRequestProgress) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *guestRequestProgress) storageWait(waiting bool) {
	for {
		prior := r.state.Load()
		next := *prior
		if waiting {
			next.waits++
		} else {
			next.waits--
			if next.waits == 0 {
				next.at = time.Now()
			}
		}
		if r.state.CompareAndSwap(prior, &next) {
			r.notify()
			return
		}
	}
}
func (p *LocalVersion) advanceGuestRequest() {
	request := p.guestRequest.Load()
	if request == nil {
		return
	}
	for {
		prior := request.state.Load()
		next := *prior
		next.at = time.Now()
		if request.state.CompareAndSwap(prior, &next) {
			request.notify()
			return
		}
	}
}
func (p *LocalVersion) endGuestRequest() {
	p.guestRequest.Store(nil)
	select {
	case p.guestActivity <- struct{}{}:
	default:
	}
}

// watchGuestRequests owns no device resource. AttachDevice joins it with the
// export before releasing local storage. Its pressure check never takes the
// version mutex: remote reads may own that mutex while unresponsive.
func (p *LocalVersion) watchGuestRequests(ctx context.Context) error {
	for ctx.Err() == nil {
		request := p.guestRequest.Load()
		var state *guestRequestState
		if request != nil {
			state = request.state.Load()
		}
		var timer *time.Timer
		var expired <-chan time.Time
		if request != nil {
			delay := max(time.Millisecond, time.Until(state.at.Add(guestRequestHangLimit)))
			if state.waits > 0 || p.disk.pressureActive.Load() {
				delay = guestRequestHangLimit
			}
			timer = time.NewTimer(delay)
			expired = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-p.guestActivity:
			if timer != nil {
				timer.Stop()
			}
		case <-expired:
			if current := p.guestRequest.Load(); current == request && current != nil {
				now := current.state.Load()
				if now.waits == 0 && now.at.Equal(state.at) && !p.disk.pressureActive.Load() && time.Since(now.at) >= guestRequestHangLimit {
					return ErrDeviceHung
				}
			}
		}
	}
	return ctx.Err()
}
