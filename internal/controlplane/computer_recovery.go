package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/jackc/pgx/v5/pgtype"
)

// Fresh preparation succeeds at the ready commit. Frozen restores retain their
// budget until the whole-Instance activation acknowledgement opens admission.
func (s *Server) markComputerInstanceReady(ctx context.Context, group pgtype.UUID, params db.MarkComputerInstanceReadyParams) (db.ComputerInstance, error) {
	var row db.ComputerInstance
	err := s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		var err error
		row, err = dispatch.RecordComputerInstanceReady(ctx, tx, group, params)
		return err
	})
	return row, err
}
