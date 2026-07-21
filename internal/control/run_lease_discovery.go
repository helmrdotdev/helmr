package control

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
) (api.WorkerRunLeaseDiscoveryResponse, error) {
	rows, err := store.DiscoverWorkerRunLeaseWork(ctx, db.DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID:         workerGroupID,
		WorkerProtocolVersion: protocolVersion,
		RowLimit:              workerRunLeaseDiscoveryLimit,
		WorkerInstanceID:      workerInstanceID,
		WorkerEpoch:           workerEpoch,
	})
	if err != nil {
		return api.WorkerRunLeaseDiscoveryResponse{}, err
	}

	items := make([]api.WorkerRunLeaseWork, 0, len(rows))
	for _, row := range rows {
		items = append(items, api.WorkerRunLeaseWork{
			LeaseID:       pgvalue.MustUUIDValue(row.ID).String(),
			LeaseSequence: row.LeaseSequence,
		})
	}
	return api.WorkerRunLeaseDiscoveryResponse{Items: items}, nil
}
