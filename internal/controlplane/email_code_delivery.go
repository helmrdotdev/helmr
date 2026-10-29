package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	emailCodeDeliveryWorkers = 4
	emailCodeDeliveryQueue   = 64
)

type emailCodeDeliveryJob struct {
	id      string
	purpose string
	deliver func(context.Context) error
	fail    func(context.Context) error
	done    chan error
}

func (job emailCodeDeliveryJob) complete(err error) {
	if job.done == nil {
		return
	}
	select {
	case job.done <- err:
	default:
	}
}

// EmailCodeDelivery bounds provider calls and queued delivery memory for one
// Control Plane process.
type EmailCodeDelivery struct {
	log             *slog.Logger
	queue           chan emailCodeDeliveryJob
	workers         int
	shutdownTimeout time.Duration

	mu        sync.Mutex
	accepting bool
	pending   map[string]emailCodeDeliveryJob
}

func NewEmailCodeDelivery(log *slog.Logger, shutdownTimeout time.Duration) *EmailCodeDelivery {
	return newEmailCodeDelivery(log, emailCodeDeliveryWorkers, emailCodeDeliveryQueue, shutdownTimeout)
}

func newEmailCodeDelivery(log *slog.Logger, workers int, queueSize int, shutdownTimeout time.Duration) *EmailCodeDelivery {
	return &EmailCodeDelivery{
		log:             log,
		queue:           make(chan emailCodeDeliveryJob, queueSize),
		workers:         workers,
		shutdownTimeout: shutdownTimeout,
		accepting:       true,
		pending:         make(map[string]emailCodeDeliveryJob, workers+queueSize),
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
		d.pending[job.id] = job
		return true
	default:
		d.log.Warn(
			"magic link delivery queue saturated",
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
	defer cancel()
	workers.Wait()
	if err := d.failPending(shutdownCtx); err != nil {
		return err
	}
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
			job.complete(err)
			if err != nil && !errors.Is(err, context.Canceled) {
				d.log.Warn("send magic link failed", "purpose", job.purpose, "error", err)
			}
			d.finish(job.id, ctx.Err() == nil)
		}
	}
}

func (d *EmailCodeDelivery) stopAccepting() {
	d.mu.Lock()
	d.accepting = false
	d.mu.Unlock()
}

func (d *EmailCodeDelivery) finish(id string, finalized bool) {
	if !finalized {
		return
	}
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

func (d *EmailCodeDelivery) failPending(ctx context.Context) error {
	d.mu.Lock()
	pending := make([]emailCodeDeliveryJob, 0, len(d.pending))
	for _, job := range d.pending {
		pending = append(pending, job)
	}
	d.mu.Unlock()

	var result error
	for _, job := range pending {
		if err := job.fail(ctx); err != nil {
			result = errors.Join(result, fmt.Errorf("mark queued magic link %s failed: %w", job.id, err))
			job.complete(err)
		} else {
			job.complete(context.Canceled)
		}
	}
	d.mu.Lock()
	clear(d.pending)
	d.mu.Unlock()
	return result
}
