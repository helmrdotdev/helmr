package computer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetentionStore is the remote CAS storage Retention reclaims after retirement commits.
// Storage adapters never decide whether a referenced key may be deleted.
type RetentionStore interface {
	RetiredUploads(context.Context, string) ([]string, error)
	ReclaimUpload(context.Context, string, string) error
	ReclaimVersions(context.Context, string) error
}

// Retention retires unreferenced Computer disk versions, objects and data keys,
// and repeatedly reclaims the physical storage of retired CAS blobs and failed
// uploads.
//
// cas_blobs is a shared physical-lifetime table, not a Computer table. Every
// retired blob is Computer-owned today only because RetireAbandonedCasBlob and
// RetireCollectedComputerObject are the sole writers of retired_at, and the
// availability FK from cas_objects rejects retiring a blob that still has an
// organization membership. Adding a non-Computer retirement trigger later needs
// a separate CAS blob owner rather than another caller of this sweep.
type Retention struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	store   RetentionStore
	log     *slog.Logger
}

func NewRetention(pool *pgxpool.Pool, store RetentionStore, log *slog.Logger) (*Retention, error) {
	if pool == nil || store == nil || log == nil {
		return nil, errors.New("artifact reclamation requires database, storage and logger")
	}
	return &Retention{pool: pool, queries: db.New(pool), store: store, log: log}, nil
}

func (r *Retention) Run(ctx context.Context) error {
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
func (r *Retention) Reconcile(ctx context.Context) error {
	if _, err := r.queries.AbandonReclaimedComputerSaves(ctx, 100); err != nil {
		return err
	}
	if _, err := r.queries.ReleaseReclaimedComputerObjects(ctx, 1000); err != nil {
		return err
	}
	if err := r.collectComputerDiskVersions(ctx); err != nil {
		return err
	}
	if err := r.collectComputerObjects(ctx); err != nil {
		return err
	}
	if err := r.collectComputerKeys(ctx); err != nil {
		return err
	}
	return r.reclaimCasBlobs(ctx)
}
