// Package artifactgc owns retirement and repeated physical reclamation of failed
// uploads. Storage adapters never decide whether a referenced key may be deleted.
package artifactgc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store interface {
	RetiredUploads(context.Context, string) ([]string, error)
	ReclaimUpload(context.Context, string, string) error
	ReclaimVersions(context.Context, string) error
}

type Reclaimer struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	store   Store
	log     *slog.Logger
}

func New(pool *pgxpool.Pool, store Store, log *slog.Logger) (*Reclaimer, error) {
	if pool == nil || store == nil || log == nil {
		return nil, errors.New("artifact reclamation requires database, storage and logger")
	}
	return &Reclaimer{pool: pool, queries: db.New(pool), store: store, log: log}, nil
}

func (r *Reclaimer) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log.ErrorContext(ctx, "artifact reclamation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Reconcile commits retirement before remote I/O. Database connection loss or
// storage uncertainty leaves the permanent tombstone discoverable for retry.
func (r *Reclaimer) Reconcile(ctx context.Context) error {
	if _, err := r.queries.AbandonReclaimedComputerSaves(ctx, 100); err != nil {
		return err
	}
	if _, err := r.queries.ReleaseReclaimedComputerObjects(ctx, 1000); err != nil {
		return err
	}
	if err := r.collectComputerVersions(ctx); err != nil {
		return err
	}
	if err := r.collectComputerObjects(ctx); err != nil {
		return err
	}
	if err := r.collectComputerKeys(ctx); err != nil {
		return err
	}
	candidates, err := r.queries.ListAbandonedCasObjects(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, digest := range candidates {
		if _, err := r.queries.RetireAbandonedCasObject(ctx, digest); err != nil {
			// An adopter or registered owner won the race. The FK, not the
			// candidate list's earlier snapshot, is the final deletion barrier.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				continue
			}
			failures = append(failures, err)
		}
	}
	// Claim just before work, not a large batch that can expire while queued
	// locally. Storage work is bounded below the five-minute recovery interval.
	for range 100 {
		digests, err := r.queries.ClaimRetiredCasObjects(ctx, 1)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if len(digests) == 0 {
			break
		}
		digest := digests[0]
		sweepCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		err = r.sweep(sweepCtx, digest)
		cancel()
		recorded := pgtype.Text{}
		if err != nil {
			recorded = pgtype.Text{String: err.Error(), Valid: true}
			failures = append(failures, fmt.Errorf("reclaim %s: %w", digest, err))
		}
		if recordErr := r.queries.RecordCasReclamation(ctx, db.RecordCasReclamationParams{
			Digest: digest, LastError: recorded,
		}); recordErr != nil {
			failures = append(failures, recordErr)
		}
	}
	return errors.Join(failures...)
}

func (r *Reclaimer) sweep(ctx context.Context, digest string) error {
	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, 15*time.Second)
	discovered, discoveryErr := r.store.RetiredUploads(discoveryCtx, digest)
	cancelDiscovery()
	var failures []error
	if discoveryErr != nil {
		failures = append(failures, discoveryErr)
	}
	for _, id := range discovered {
		if err := r.queries.RegisterRetiredCasUpload(ctx, db.RegisterRetiredCasUploadParams{Digest: digest, UploadID: id}); err != nil {
			// Never abort an upload whose ID could be forgotten after a crash.
			return errors.Join(append(failures, err)...)
		}
	}
	retained, err := r.queries.ClaimRetiredCasUploads(ctx, db.ClaimRetiredCasUploadsParams{Digest: digest, RowLimit: 10})
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, id := range retained {
		uploadCtx, cancelUpload := context.WithTimeout(ctx, 10*time.Second)
		err := r.store.ReclaimUpload(uploadCtx, digest, id)
		cancelUpload()
		if err != nil {
			failures = append(failures, err)
		}
	}
	// A multipart completion can race abort and appear here or on a later pass.
	versionCtx, cancelVersions := context.WithTimeout(ctx, 30*time.Second)
	defer cancelVersions()
	if err := r.store.ReclaimVersions(versionCtx, digest); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
