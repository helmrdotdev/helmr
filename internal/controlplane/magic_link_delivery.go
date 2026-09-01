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
	magicLinkDeliveryWorkers = 4
	magicLinkDeliveryQueue   = 64
)

type magicLinkDeliveryJob struct {
	id      string
	purpose string
	deliver func(context.Context) error
	fail    func(context.Context) error
	done    chan error
}

func (job magicLinkDeliveryJob) complete(err error) {
	if job.done == nil {
		return
	}
	select {
	case job.done <- err:
	default:
	}
}

// MagicLinkDelivery bounds provider calls and queued delivery memory for one
// Control Plane process.
type MagicLinkDelivery struct {
	log             *slog.Logger
	queue           chan magicLinkDeliveryJob
	workers         int
	shutdownTimeout time.Duration

	mu        sync.Mutex
	accepting bool
	pending   map[string]magicLinkDeliveryJob
}

func NewMagicLinkDelivery(log *slog.Logger, shutdownTimeout time.Duration) *MagicLinkDelivery {
	return newMagicLinkDelivery(log, magicLinkDeliveryWorkers, magicLinkDeliveryQueue, shutdownTimeout)
}

func newMagicLinkDelivery(log *slog.Logger, workers int, queueSize int, shutdownTimeout time.Duration) *MagicLinkDelivery {
	return &MagicLinkDelivery{
		log:             log,
		queue:           make(chan magicLinkDeliveryJob, queueSize),
		workers:         workers,
		shutdownTimeout: shutdownTimeout,
		accepting:       true,
		pending:         make(map[string]magicLinkDeliveryJob, workers+queueSize),
	}
}

func (d *MagicLinkDelivery) enqueue(job magicLinkDeliveryJob) bool {
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

func (d *MagicLinkDelivery) Run(ctx context.Context) error {
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

func (d *MagicLinkDelivery) runWorker(ctx context.Context) {
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

func (d *MagicLinkDelivery) stopAccepting() {
	d.mu.Lock()
	d.accepting = false
	d.mu.Unlock()
}

func (d *MagicLinkDelivery) finish(id string, finalized bool) {
	if !finalized {
		return
	}
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

func (d *MagicLinkDelivery) failPending(ctx context.Context) error {
	d.mu.Lock()
	pending := make([]magicLinkDeliveryJob, 0, len(d.pending))
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
