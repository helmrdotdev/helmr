package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

const (
	emailCodeDeliveryWorkers = 4
	emailCodeDeliveryQueue   = 64
)

type emailCodeDeliveryJob struct {
	deliver func(context.Context) error
}

// EmailCodeDelivery bounds provider calls and queued delivery memory for one
// Control Plane process.
type EmailCodeDelivery struct {
	log     *slog.Logger
	queue   chan emailCodeDeliveryJob
	workers int

	mu        sync.Mutex
	accepting bool
}

func NewEmailCodeDelivery(log *slog.Logger) *EmailCodeDelivery {
	return newEmailCodeDelivery(log, emailCodeDeliveryWorkers, emailCodeDeliveryQueue)
}

func newEmailCodeDelivery(log *slog.Logger, workers int, queueSize int) *EmailCodeDelivery {
	return &EmailCodeDelivery{
		log:       log,
		queue:     make(chan emailCodeDeliveryJob, queueSize),
		workers:   workers,
		accepting: true,
	}
}

func (d *EmailCodeDelivery) enqueue(job emailCodeDeliveryJob) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.accepting {
		return false
	}
	select {
	case d.queue <- job:
		return true
	default:
		d.log.Warn(
			"email code delivery queue saturated",
			"workers", d.workers,
			"queued", len(d.queue),
			"queue_capacity", cap(d.queue),
		)
		return false
	}
}

func (d *EmailCodeDelivery) Run(ctx context.Context) error {
	var workers sync.WaitGroup
	for range d.workers {
		workers.Go(func() {
			d.runWorker(ctx)
		})
	}
	<-ctx.Done()
	d.stopAccepting()
	workers.Wait()
	return ctx.Err()
}

func (d *EmailCodeDelivery) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.queue:
			if ctx.Err() != nil {
				return
			}
			err := job.deliver(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				d.log.Warn("send email code failed", "error", err)
			}
		}
	}
}

func (d *EmailCodeDelivery) stopAccepting() {
	d.mu.Lock()
	d.accepting = false
	d.mu.Unlock()
}
