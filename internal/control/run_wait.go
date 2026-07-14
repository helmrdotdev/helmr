package control

import (
	"context"
	"errors"
	"log/slog"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const runWaitRequeueLimit = int32(1000)

func (s *Server) requeueResolvedRunWaits(ctx context.Context, orgID pgtype.UUID) {
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	var rows []db.RequeueResolvedRunWaitsRow
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		rows, err = work.q.RequeueResolvedRunWaits(ctx, db.RequeueResolvedRunWaitsParams{
			OrgID:      orgID,
			LimitCount: runWaitRequeueLimit,
		})
		return err
	})
	if err != nil {
		log.Error("requeue resolved run waits failed", "org_id", pgvalue.UUIDString(orgID), "error", err)
		return
	}
	if s.runEnqueuer == nil {
		return
	}
	for _, row := range rows {
		if _, err := s.runEnqueuer.EnqueueRun(ctx, row.OrgID, row.RunID); err != nil && !errors.Is(err, dispatch.ErrNoEnqueueCandidate) {
			log.Error("enqueue resumed run failed", "run_id", pgvalue.UUIDString(row.RunID), "run_wait_id", pgvalue.UUIDString(row.ID), "error", err)
		}
	}
}
