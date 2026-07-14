package control

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRequeueResolvedRunWaitsScopesQueryAndEnqueuesResult(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &resolvedRunWaitStore{rows: []db.RequeueResolvedRunWaitsRow{
		{ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: orgID, RunID: runID},
	}}
	enqueuer := &recordingRunEnqueuer{}
	server := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  testTransactionalStore{Querier: store}, tx: panicTxBeginner{}, runEnqueuer: enqueuer,
	}

	server.requeueResolvedRunWaits(context.Background(), orgID)

	if store.params.OrgID != orgID || store.params.LimitCount != runWaitRequeueLimit {
		t.Fatalf("requeue params = %+v, want org %s limit %d", store.params, pgvalue.UUIDString(orgID), runWaitRequeueLimit)
	}
	if len(enqueuer.runIDs) != 1 || enqueuer.runIDs[0] != runID {
		t.Fatalf("enqueued run IDs = %v, want [%s]", enqueuer.runIDs, pgvalue.UUIDString(runID))
	}
}

func TestRequeueResolvedRunWaitsIgnoresNoEnqueueCandidate(t *testing.T) {
	orgID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &resolvedRunWaitStore{rows: []db.RequeueResolvedRunWaitsRow{{ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: orgID, RunID: pgvalue.UUID(uuid.Must(uuid.NewV7()))}}}
	server := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  testTransactionalStore{Querier: store}, tx: panicTxBeginner{},
		runEnqueuer: &recordingRunEnqueuer{err: dispatch.ErrNoEnqueueCandidate},
	}

	server.requeueResolvedRunWaits(context.Background(), orgID)
}

type resolvedRunWaitStore struct {
	db.Querier
	rows   []db.RequeueResolvedRunWaitsRow
	params db.RequeueResolvedRunWaitsParams
}

func (s *resolvedRunWaitStore) RequeueResolvedRunWaits(_ context.Context, params db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error) {
	s.params = params
	return s.rows, nil
}

type recordingRunEnqueuer struct {
	runIDs []pgtype.UUID
	err    error
}

func (e *recordingRunEnqueuer) EnqueueRun(_ context.Context, _ pgtype.UUID, runID pgtype.UUID) (dispatch.EnqueueResult, error) {
	e.runIDs = append(e.runIDs, runID)
	return dispatch.EnqueueResult{}, e.err
}
