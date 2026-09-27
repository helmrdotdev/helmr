package secret

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxComputerSecretPlacements = 64

var ErrDeliveryUnavailable = errors.New("secret delivery authority is unavailable")

type DeliveryEnvelope struct {
	Mode            string
	PlacementKind   string
	PlacementTarget string
	Secret          db.Secret
	Version         db.SecretVersion
}

type DeliveryMaterial struct {
	PlacementKind   string
	PlacementTarget string
	Value           []byte
}

type deliveryStore interface {
	LockAttemptSecretDelivery(context.Context, db.LockAttemptSecretDeliveryParams) ([]db.LockAttemptSecretDeliveryRow, error)
	GetSecretVersion(context.Context, db.GetSecretVersionParams) (db.SecretVersion, error)
}

type processDeliveryStore interface {
	LockProcessSecretDelivery(context.Context, db.LockProcessSecretDeliveryParams) ([]db.LockProcessSecretDeliveryRow, error)
	GetSecretVersion(context.Context, db.GetSecretVersionParams) (db.SecretVersion, error)
}

func LockAttemptDelivery(
	ctx context.Context,
	store deliveryStore,
	runID pgtype.UUID,
	attemptNumber int32,
	computerID pgtype.UUID,
) ([]DeliveryEnvelope, error) {
	if !runID.Valid || !computerID.Valid || attemptNumber <= 0 {
		return nil, ErrDeliveryUnavailable
	}
	rows, err := store.LockAttemptSecretDelivery(ctx, db.LockAttemptSecretDeliveryParams{
		RunID:         runID,
		AttemptNumber: pgtype.Int4{Int32: attemptNumber, Valid: true},
		ComputerID:    computerID,
	})
	if err != nil {
		return nil, fmt.Errorf("lock attempt secret delivery: %w", err)
	}
	if len(rows) > maxComputerSecretPlacements {
		return nil, ErrDeliveryUnavailable
	}

	versions := make(map[uuid.UUID]db.SecretVersion)
	envelopes := make([]DeliveryEnvelope, 0, len(rows))
	for _, row := range rows {
		if err := validateDeliveryRow(row, runID, attemptNumber, computerID); err != nil {
			return nil, err
		}
		versionID, err := pgvalue.UUIDValue(row.ResolutionSecretVersionID)
		if err != nil {
			return nil, ErrDeliveryUnavailable
		}
		version, ok := versions[versionID]
		if !ok {
			version, err = store.GetSecretVersion(ctx, db.GetSecretVersionParams{
				EnvironmentID: row.Secret.EnvironmentID,
				SecretID:      row.Secret.ID,
				VersionID:     row.ResolutionSecretVersionID,
			})
			if err != nil {
				return nil, fmt.Errorf("get resolved secret version: %w", err)
			}
			if version.ID != row.ResolutionSecretVersionID || version.SecretID != row.Secret.ID {
				return nil, ErrDeliveryUnavailable
			}
			versions[versionID] = version
		}
		envelopes = append(envelopes, DeliveryEnvelope{
			Mode:            row.ComputerSecret.Mode,
			PlacementKind:   row.ComputerSecret.PlacementKind,
			PlacementTarget: row.ComputerSecret.PlacementTarget,
			Secret:          row.Secret,
			Version:         version,
		})
	}
	return envelopes, nil
}

func LockProcessDelivery(
	ctx context.Context,
	store processDeliveryStore,
	commandID pgtype.UUID,
	computerID pgtype.UUID,
) ([]DeliveryEnvelope, error) {
	if !commandID.Valid || !computerID.Valid {
		return nil, ErrDeliveryUnavailable
	}
	rows, err := store.LockProcessSecretDelivery(ctx, db.LockProcessSecretDeliveryParams{
		CommandID:  commandID,
		ComputerID: computerID,
	})
	if err != nil {
		return nil, fmt.Errorf("lock process secret delivery: %w", err)
	}
	if len(rows) > maxComputerSecretPlacements {
		return nil, ErrDeliveryUnavailable
	}
	versions := make(map[uuid.UUID]db.SecretVersion)
	envelopes := make([]DeliveryEnvelope, 0, len(rows))
	for _, row := range rows {
		if row.ComputerSecret.ComputerID != computerID ||
			row.ComputerSecret.EnvironmentID != row.Secret.EnvironmentID ||
			row.ComputerSecret.SecretID != row.Secret.ID ||
			(row.ComputerSecret.PlacementKind != "env" && row.ComputerSecret.PlacementKind != "file") ||
			row.ComputerSecret.PlacementTarget == "" ||
			row.Secret.Status != "active" ||
			!row.ResolutionID.Valid ||
			row.ResolutionCommandID != commandID ||
			!row.ResolutionSecretVersionID.Valid ||
			!row.ResolutionRevocationGeneration.Valid ||
			row.ResolutionRevocationGeneration.Int64 != row.Secret.RevocationGeneration {
			return nil, ErrDeliveryUnavailable
		}
		versionID, err := pgvalue.UUIDValue(row.ResolutionSecretVersionID)
		if err != nil {
			return nil, ErrDeliveryUnavailable
		}
		version, ok := versions[versionID]
		if !ok {
			version, err = store.GetSecretVersion(ctx, db.GetSecretVersionParams{
				EnvironmentID: row.Secret.EnvironmentID,
				SecretID:      row.Secret.ID,
				VersionID:     row.ResolutionSecretVersionID,
			})
			if err != nil {
				return nil, fmt.Errorf("get resolved process secret version: %w", err)
			}
			if version.ID != row.ResolutionSecretVersionID || version.SecretID != row.Secret.ID {
				return nil, ErrDeliveryUnavailable
			}
			versions[versionID] = version
		}
		envelopes = append(envelopes, DeliveryEnvelope{
			Mode:            row.ComputerSecret.Mode,
			PlacementKind:   row.ComputerSecret.PlacementKind,
			PlacementTarget: row.ComputerSecret.PlacementTarget,
			Secret:          row.Secret,
			Version:         version,
		})
	}
	return envelopes, nil
}

func validateDeliveryRow(
	row db.LockAttemptSecretDeliveryRow,
	runID pgtype.UUID,
	attemptNumber int32,
	computerID pgtype.UUID,
) error {
	if row.ComputerSecret.ComputerID != computerID ||
		row.ComputerSecret.EnvironmentID != row.Secret.EnvironmentID ||
		row.ComputerSecret.SecretID != row.Secret.ID ||
		(row.ComputerSecret.PlacementKind != "env" && row.ComputerSecret.PlacementKind != "file") ||
		row.ComputerSecret.PlacementTarget == "" ||
		row.Secret.Status != "active" ||
		!row.ResolutionID.Valid ||
		row.ResolutionRunID != runID ||
		!row.ResolutionAttemptNumber.Valid ||
		row.ResolutionAttemptNumber.Int32 != attemptNumber ||
		!row.ResolutionSecretVersionID.Valid ||
		!row.ResolutionRevocationGeneration.Valid ||
		row.ResolutionRevocationGeneration.Int64 != row.Secret.RevocationGeneration {
		return ErrDeliveryUnavailable
	}
	return nil
}

func (s *Store) OpenDeliveries(environmentID uuid.UUID, envelopes []DeliveryEnvelope) ([]DeliveryMaterial, error) {
	if len(envelopes) > maxComputerSecretPlacements {
		return nil, ErrDeliveryUnavailable
	}
	materials := make([]DeliveryMaterial, 0, len(envelopes))
	for _, envelope := range envelopes {
		if envelope.Mode == "protected" {
			continue
		}
		if envelope.Mode != "raw" {
			return nil, ErrDeliveryUnavailable
		}
		if envelope.Secret.EnvironmentID != pgvalue.UUID(environmentID) ||
			envelope.Secret.Status != "active" ||
			envelope.Version.SecretID != envelope.Secret.ID ||
			!envelope.Version.ID.Valid ||
			(envelope.PlacementKind != "env" && envelope.PlacementKind != "file") ||
			envelope.PlacementTarget == "" {
			return nil, ErrDeliveryUnavailable
		}
		value, err := s.decrypt(environmentID, envelope.Secret, envelope.Version)
		if err != nil {
			return nil, UnavailableError{Err: fmt.Errorf("open resolved secret version: %w", err)}
		}
		materials = append(materials, DeliveryMaterial{
			PlacementKind:   envelope.PlacementKind,
			PlacementTarget: envelope.PlacementTarget,
			Value:           value,
		})
	}
	return materials, nil
}
