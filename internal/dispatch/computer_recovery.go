package dispatch

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Allocation and its Computer-owned charge commit together. The caller holds
// Computer and Instance authority; any rejection rolls back the allocation.
func admitComputerPreparation(ctx context.Context, tx pgx.Tx, computerID, instanceID pgtype.UUID) error {
	_, err := db.New(tx).ChargeComputerPreparation(ctx, db.ChargeComputerPreparationParams{ComputerID: computerID, InstanceID: instanceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}
