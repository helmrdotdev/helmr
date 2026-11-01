package sessiontest

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
)

// RecordUpload stands in for the storage grant a Host receives before a
// create-only upload: it records a write receipt of the object for every live
// allocation, so certification admits a fixture's directly stored bytes.
func RecordUpload(ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, digest string, size int64, media string) error {
	if _, err := executor.Exec(ctx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, digest, size); err != nil {
		return err
	}
	_, err := executor.Exec(ctx, `INSERT INTO allocation_storage_writes(worker_host_id,worker_epoch,allocation_id,org_id,digest,size_bytes,media_type)
 SELECT live.worker_host_id,live.worker_epoch,live.allocation_id,e.org_id,$1,$2,$3 FROM (`+db.AllocationUsageSQL+`) live
 JOIN environments e ON e.id=live.environment_id ON CONFLICT DO NOTHING`, digest, size, media)
	return err
}
