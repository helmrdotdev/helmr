package computer

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReclamationStore deletes remote bytes only after their availability has been
// permanently retired in PostgreSQL.
type ReclamationStore interface {
	RetiredUploads(context.Context, string) ([]string, error)
	ReclaimUpload(context.Context, string, string) error
	ReclaimVersions(context.Context, string) error
}

// CASReclaimer owns physical deletion and durable retry for unreferenced CAS
// blobs. Each blob lifetime belongs to one organization's keyspace and is swept
// in that keyspace's store. Domain collectors release eligible attachments
// before this sweep; availability FKs prevent retirement while any owner still
// pins the bytes.
type CASReclaimer struct {
	queries *db.Queries
	store   func(org uuid.UUID) ReclamationStore
	log     *slog.Logger
}

func NewCASReclaimer(pool *pgxpool.Pool, store func(org uuid.UUID) ReclamationStore, log *slog.Logger) (*CASReclaimer, error) {
	if pool == nil || store == nil || log == nil {
		return nil, errors.New("CAS reclamation requires database, storage and logger")
	}
	return &CASReclaimer{queries: db.New(pool), store: store, log: log}, nil
}

func (r *CASReclaimer) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log.ErrorContext(ctx, "CAS reclamation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
