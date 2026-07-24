package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errActorUpdateInvalid   = errors.New("actor update request is invalid")
	errActorUpdateNotFound  = errors.New("actor not found")
	errActorUpdateConflict  = errors.New("actor update conflicts with current state")
	errActorUpdateAuthority = errors.New("actor update authority is unavailable")
)

type actorUpdateRequest struct {
	EnvironmentID    uuid.UUID
	ActorDeclaredID  string
	Address          actorReadAddress
	MetadataPresent  bool
	Metadata         json.RawMessage
	TagsPresent      bool
	Tags             []string
	ExpiresAtPresent bool
	ExpiresAt        *time.Time
}

type normalizedActorUpdate struct {
	actorUpdateRequest
}

func (s *Server) updateActor(
	ctx context.Context,
	request actorUpdateRequest,
) (api.ActorStatus, error) {
	normalized, err := normalizeActorUpdate(request)
	if err != nil {
		return api.ActorStatus{}, err
	}

	var status api.ActorStatus
	err = s.inTx(ctx, func(work *txWork) error {
		_, err := work.q.UpdateActorAnnotations(ctx, db.UpdateActorAnnotationsParams{
			SetMetadata:     normalized.MetadataPresent,
			Metadata:        normalized.Metadata,
			SetTags:         normalized.TagsPresent,
			Tags:            normalized.Tags,
			SetExpiresAt:    normalized.ExpiresAtPresent,
			ExpiresAt:       pgvalue.TimestamptzUTCZeroInvalid(timePtrValue(normalized.ExpiresAt)),
			EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
			ActorDeclaredID: normalized.ActorDeclaredID,
			AddressPublicID: pgvalue.Text(normalized.Address.publicID),
			AddressKey:      pgvalue.Text(normalized.Address.key),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			_, lockErr := work.q.LockActorUpdateAddress(ctx, db.LockActorUpdateAddressParams{
				EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
				ActorDeclaredID: normalized.ActorDeclaredID,
				AddressPublicID: pgvalue.Text(normalized.Address.publicID),
				AddressKey:      pgvalue.Text(normalized.Address.key),
			})
			if errors.Is(lockErr, pgx.ErrNoRows) {
				return errActorUpdateNotFound
			}
			if lockErr != nil {
				return fmt.Errorf("%w: lock Actor update address: %v", errActorUpdateAuthority, lockErr)
			}
			return errActorUpdateConflict
		}
		if err != nil {
			return fmt.Errorf("%w: update Actor annotations: %v", errActorUpdateAuthority, err)
		}

		row, err := work.q.GetActorRead(ctx, db.GetActorReadParams{
			EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
			ActorDeclaredID: normalized.ActorDeclaredID,
			AddressPublicID: pgvalue.Text(normalized.Address.publicID),
			AddressKey:      pgvalue.Text(normalized.Address.key),
		})
		if err != nil {
			return fmt.Errorf("%w: read updated Actor: %v", errActorUpdateAuthority, err)
		}
		status, err = projectActorStatus(actorReadRecordFromGet(row))
		if err != nil {
			return fmt.Errorf("%w: project updated Actor: %v", errActorUpdateAuthority, err)
		}
		return nil
	})
	if err != nil {
		return api.ActorStatus{}, err
	}
	return status, nil
}

func normalizeActorUpdate(request actorUpdateRequest) (normalizedActorUpdate, error) {
	if request.EnvironmentID == uuid.Nil {
		return normalizedActorUpdate{}, errActorUpdateInvalid
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return normalizedActorUpdate{}, fmt.Errorf("%w: %v", errActorUpdateInvalid, err)
	}
	if err := api.ValidateActorReference(api.ActorReference{
		ActorID:  request.Address.publicID,
		ActorKey: request.Address.key,
	}); err != nil {
		return normalizedActorUpdate{}, fmt.Errorf("%w: %v", errActorUpdateInvalid, err)
	}
	if !request.MetadataPresent && !request.TagsPresent && !request.ExpiresAtPresent {
		return normalizedActorUpdate{}, fmt.Errorf(
			"%w: at least one of metadata, tags, or expires_at is required",
			errActorUpdateInvalid,
		)
	}
	var err error
	if request.MetadataPresent {
		request.Metadata, err = normalizeMetadata(request.Metadata, maxActorMetadataBytes, "Actor")
		if err != nil {
			return normalizedActorUpdate{}, fmt.Errorf("%w: %v", errActorUpdateInvalid, err)
		}
	} else {
		request.Metadata = json.RawMessage(`{}`)
	}
	if request.TagsPresent {
		request.Tags, err = normalizeTags(request.Tags, maxTags, "Actor")
		if err != nil {
			return normalizedActorUpdate{}, fmt.Errorf("%w: %v", errActorUpdateInvalid, err)
		}
	} else {
		request.Tags = []string{}
	}
	if request.ExpiresAtPresent {
		if request.ExpiresAt == nil || request.ExpiresAt.IsZero() {
			return normalizedActorUpdate{}, fmt.Errorf("%w: expires_at must be a valid instant", errActorUpdateInvalid)
		}
		expiresAt := request.ExpiresAt.UTC()
		request.ExpiresAt = &expiresAt
	} else {
		request.ExpiresAt = nil
	}
	return normalizedActorUpdate{actorUpdateRequest: request}, nil
}

func actorUpdateRequestFromAPI(
	environmentID pgtype.UUID,
	actorDeclaredID string,
	request api.UpdateActorRequest,
) (actorUpdateRequest, error) {
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		return actorUpdateRequest{}, fmt.Errorf("%w: invalid Environment authority", errActorUpdateAuthority)
	}
	result := actorUpdateRequest{
		EnvironmentID:    environmentUUID,
		ActorDeclaredID:  actorDeclaredID,
		Address:          actorReadAddress{publicID: request.ActorID, key: request.ActorKey},
		MetadataPresent:  request.Metadata != nil,
		TagsPresent:      request.Tags != nil,
		ExpiresAtPresent: request.ExpiresAt != nil,
		ExpiresAt:        request.ExpiresAt,
	}
	if request.Metadata != nil {
		result.Metadata = *request.Metadata
	}
	if request.Tags != nil {
		result.Tags = *request.Tags
	}
	return result, nil
}
