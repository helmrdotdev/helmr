package control

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const workerRunLeaseDiscoveryLimit int32 = 64

func discoverWorkerRunLeases(
	ctx context.Context,
	store db.Querier,
	workerGroupID string,
	workerInstanceID pgtype.UUID,
	workerEpoch int64,
	protocolVersion string,
) (workerapi.RunLeaseDiscoveryResponse, error) {
	rows, err := store.DiscoverWorkerRunLeaseWork(ctx, db.DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID:         workerGroupID,
		WorkerProtocolVersion: protocolVersion,
		RowLimit:              workerRunLeaseDiscoveryLimit,
		WorkerInstanceID:      workerInstanceID,
		WorkerEpoch:           workerEpoch,
	})
	if err != nil {
		return workerapi.RunLeaseDiscoveryResponse{}, err
	}

	items := make([]workerapi.RunLeaseWork, 0, len(rows))
	for _, row := range rows {
		items = append(items, workerapi.RunLeaseWork{
			LeaseID:       pgvalue.MustUUIDValue(row.ID).String(),
			LeaseSequence: row.LeaseSequence,
		})
	}
	return workerapi.RunLeaseDiscoveryResponse{Items: items}, nil
}
