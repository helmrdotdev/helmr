package computer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// reclaimCasBlobs retires abandoned CAS blobs, then sweeps retired blobs'
// remote storage. Retirement always commits before any remote I/O.
func (r *Retention) reclaimCasBlobs(ctx context.Context) error {
	candidates, err := r.queries.ListAbandonedCasBlobs(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, digest := range candidates {
		if _, err := r.queries.RetireAbandonedCasBlob(ctx, digest); err != nil {
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
		digests, err := r.queries.ClaimRetiredCasBlobs(ctx, 1)
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
		if recordErr := r.queries.RecordCasBlobReclamation(ctx, db.RecordCasBlobReclamationParams{
			Digest: digest, LastError: recorded,
		}); recordErr != nil {
			failures = append(failures, recordErr)
		}
	}
	return errors.Join(failures...)
}

func (r *Retention) sweep(ctx context.Context, digest string) error {
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
