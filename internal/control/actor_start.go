package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxActorMetadataBytes = 64 << 10
	maxRunMetadataBytes   = 256 << 10
	maxActorTags          = 10
	maxActorTagBytes      = 128
	maxQueuedRunTTLMS     = int64(31_536_000_000)
)

var (
	errActorStartInvalid            = errors.New("actor start request is invalid")
	errActorStartAuthority          = errors.New("actor start authority is unavailable")
	errActorStartWorkspaceConflict  = errors.New("actor start Workspace cannot accept execution")
	errActorStartSecretUnavailable  = errors.New("actor start Workspace Secret is unavailable")
	errActorStartIdempotencyReceipt = errors.New("actor start idempotency receipt is invalid")
)

type ActorKeyConflictError struct {
	Key string
}

func (e ActorKeyConflictError) Error() string {
	return fmt.Sprintf("Actor key %q already belongs to another Actor", e.Key)
}

type actorStartRequest struct {
	OrgID                 uuid.UUID
	ProjectID             uuid.UUID
	EnvironmentID         uuid.UUID
	ActorDeclaredID       string
	WorkspaceID           uuid.UUID
	WorkspaceAddress      json.RawMessage
	Key                   *string
	InputPresent          bool
	Input                 json.RawMessage
	IdempotencyKey        string
	Metadata              json.RawMessage
	Tags                  []string
	ExpiresAt             *time.Time
	ManagedQueueName      string
	ManagedConcurrencyKey *string
	ManagedPriority       int32
	ManagedQueuedTTLMS    *int64
	ManagedRetryPolicy    json.RawMessage
	ManagedRunMetadata    json.RawMessage
	ManagedRunTags        []string
	Authorize             func(context.Context, db.Querier) error
}

type actorStartResult struct {
	ActorID         uuid.UUID
	ActorPublicID   string
	InitialRecordID *uuid.UUID
	BootRunID       uuid.UUID
	BootRunPublicID string
	Replayed        bool
}

type actorStartReceipt struct {
	ActorID         string  `json:"actorId"`
	ActorPublicID   string  `json:"actorPublicId"`
	InitialRecordID *string `json:"initialRecordId,omitempty"`
	BootRunID       string  `json:"bootRunId"`
	BootRunPublicID string  `json:"bootRunPublicId"`
}

type normalizedActorStart struct {
	actorStartRequest
	fingerprint idempotency.ActorStartFingerprint
}

// startActor is the durable Actor creation primitive. Claim replay, promoted
// declaration and Workspace authority, Actor/input/boot-Run creation,
// Workspace ownership, Secret resolution, and run.admit intent commit as one
// primary-database transaction.
func (s *Server) startActor(ctx context.Context, request actorStartRequest) (actorStartResult, error) {
	normalized, err := normalizeActorStart(request)
	if err != nil {
		return actorStartResult{}, err
	}
	var claimRequest idempotency.Request
	if normalized.IdempotencyKey != "" {
		claimRequest, err = idempotency.NewActorStartRequest(
			normalized.EnvironmentID,
			normalized.ActorDeclaredID,
			normalized.IdempotencyKey,
			normalized.fingerprint,
		)
		if err != nil {
			return actorStartResult{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
		}
	}

	var result actorStartResult
	err = s.inTx(ctx, func(work *txWork) error {
		if normalized.Key != nil {
			if err := work.q.LockActorStartKey(ctx, db.LockActorStartKeyParams{
				EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
				ActorDeclaredID: normalized.ActorDeclaredID,
				Key:             *normalized.Key,
			}); err != nil {
				return fmt.Errorf("lock Actor start key: %w", err)
			}
		}
		var claim *db.IdempotencyClaim
		if claimRequest != nil {
			claims, err := s.claims.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				replayed, err := actorStartResultFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				result = replayed
				return nil
			}
			if acquired.Claim.State != "pending" {
				return errActorStartIdempotencyReceipt
			}
			claim = &acquired.Claim
		}
		if normalized.Key != nil {
			_, err := work.q.GetActorByKey(ctx, db.GetActorByKeyParams{
				EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
				ActorDeclaredID: normalized.ActorDeclaredID,
				Key:             pgvalue.Text(*normalized.Key),
			})
			if err == nil {
				return ActorKeyConflictError{Key: *normalized.Key}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check Actor start key: %w", err)
			}
		}
		if normalized.ExpiresAt != nil && !normalized.ExpiresAt.After(time.Now().UTC()) {
			return fmt.Errorf("%w: expiresAt must be in the future", errActorStartInvalid)
		}

		authority, err := work.q.LockActorStartAuthority(ctx, db.LockActorStartAuthorityParams{
			ActorDeclaredID: normalized.ActorDeclaredID,
			WorkspaceID:     pgvalue.UUID(normalized.WorkspaceID),
			OrgID:           pgvalue.UUID(normalized.OrgID),
			ProjectID:       pgvalue.UUID(normalized.ProjectID),
			EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorStartAuthority
		}
		if err != nil {
			return fmt.Errorf("lock Actor start authority: %w", err)
		}
		if normalized.Authorize != nil {
			if err := normalized.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		if authority.WorkspaceState != db.WorkspaceStateActive ||
			(authority.WorkspaceDesiredState != db.WorkspaceDesiredStateActive &&
				authority.WorkspaceDesiredState != db.WorkspaceDesiredStateStopped) ||
			authority.WorkspaceDirtyState != db.WorkspaceDirtyStateClean ||
			!authority.HeadVersionID.Valid ||
			authority.OwnerActorID.Valid || authority.OwnerRunID.Valid ||
			authority.HasActiveLease || authority.HasActiveProcess {
			return errActorStartWorkspaceConflict
		}
		if !authority.ProgramArchitecture.Valid ||
			!authority.WorkspaceArchitecture.Valid ||
			authority.ProgramArchitecture.String != authority.WorkspaceArchitecture.String {
			return errActorStartWorkspaceConflict
		}
		runAuthority, err := deployment.ResolveActorRunAdmission(
			authority.ActorManifestVersion,
			normalized.ActorDeclaredID,
			authority.ActorManifest,
			authority.ActorManifestDigest,
			authority.QueueConfig,
			normalized.ManagedQueueName,
		)
		if err != nil {
			return fmt.Errorf("%w: %v", errActorStartAuthority, err)
		}
		managedQueuedTTL := normalized.ManagedQueuedTTLMS
		if managedQueuedTTL == nil {
			managedQueuedTTL = runAuthority.QueuedTTLMS
		}
		managedRetryPolicy := normalized.ManagedRetryPolicy
		if len(managedRetryPolicy) == 0 {
			managedRetryPolicy = runAuthority.RetryPolicy
		}
		bindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, authority.WorkspaceID)
		if err != nil {
			return fmt.Errorf("lock Actor start Workspace Secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errActorStartSecretUnavailable
			}
		}

		actorID := uuid.Must(uuid.NewV7())
		runID := uuid.Must(uuid.NewV7())
		actorPublicID, err := publicid.New(publicid.Actor)
		if err != nil {
			return err
		}
		runPublicID, err := publicid.New(publicid.Run)
		if err != nil {
			return err
		}
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		claimID := pgtype.UUID{}
		if claim != nil {
			claimID = claim.ID
		}
		_, err = work.q.CreateActor(ctx, db.CreateActorParams{
			ID: pgvalue.UUID(actorID), PublicID: actorPublicID,
			OrgID: pgvalue.UUID(normalized.OrgID), ProjectID: pgvalue.UUID(normalized.ProjectID),
			Key: pgvalue.TextPtr(normalized.Key), ManagedQueueName: runAuthority.QueueName,
			ManagedConcurrencyKey:        pgvalue.TextPtr(normalized.ManagedConcurrencyKey),
			ManagedQueueConcurrencyLimit: int8Ptr(runAuthority.QueueConcurrencyLimit),
			ManagedPriority:              normalized.ManagedPriority, ManagedQueuedTtlMs: int8Ptr(managedQueuedTTL),
			ManagedMaxActiveDurationMs: runAuthority.MaxActiveDurationMS,
			ManagedRetryPolicy:         managedRetryPolicy,
			ManagedRunMetadata:         normalized.ManagedRunMetadata, ManagedRunTags: normalized.ManagedRunTags,
			ExpiresAt: pgvalue.TimestamptzUTCZeroInvalid(timePtrValue(normalized.ExpiresAt)),
			Metadata:  normalized.Metadata, Tags: normalized.Tags,
			WorkspaceID: authority.WorkspaceID, EnvironmentID: pgvalue.UUID(normalized.EnvironmentID),
			DeploymentDefinitionID: authority.ActorDefinitionID, ActorDeclaredID: normalized.ActorDeclaredID,
		})
		if err != nil {
			var postgresError *pgconn.PgError
			if errors.As(err, &postgresError) &&
				postgresError.ConstraintName == "actors_environment_declared_id_key_uidx" {
				return ActorKeyConflictError{Key: stringPtrValue(normalized.Key)}
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return errActorStartAuthority
			}
			return fmt.Errorf("create Actor: %w", err)
		}

		var initialRecordID *uuid.UUID
		inputHighWatermark := int64(0)
		if normalized.InputPresent {
			recordID := uuid.Must(uuid.NewV7())
			if _, err := work.q.CreateActorStartInputRecord(ctx, db.CreateActorStartInputRecordParams{
				ID: pgvalue.UUID(recordID), Data: normalized.Input, ClaimID: claimID,
				EnvironmentID: pgvalue.UUID(normalized.EnvironmentID), ActorID: pgvalue.UUID(actorID),
			}); err != nil {
				return fmt.Errorf("create initial Actor input: %w", err)
			}
			initialRecordID = &recordID
			inputHighWatermark = 1
		}
		run, err := work.q.CreateActorStartRun(ctx, db.CreateActorStartRunParams{
			EnvironmentID: pgvalue.UUID(normalized.EnvironmentID), ActorID: pgvalue.UUID(actorID),
			WorkspaceID: authority.WorkspaceID, ClaimID: claimID,
			ID: pgvalue.UUID(runID), PublicID: runPublicID,
			BaseWorkspaceVersionID: authority.HeadVersionID,
			InputHighWatermark:     pgtype.Int8{Int64: inputHighWatermark, Valid: true},
			RootSpanID:             rootSpanID,
		})
		if err != nil {
			return fmt.Errorf("create Actor boot Run: %w", err)
		}
		if _, err := work.q.SetActorCurrentRun(ctx, db.SetActorCurrentRunParams{
			RunID: run.ID, EnvironmentID: run.EnvironmentID,
			ID: pgvalue.UUID(actorID), WorkspaceID: authority.WorkspaceID,
		}); err != nil {
			return fmt.Errorf("install Actor boot Run: %w", err)
		}
		if _, err := work.q.ReserveWorkspaceForActor(ctx, db.ReserveWorkspaceForActorParams{
			ActorID: pgvalue.UUID(actorID), EnvironmentID: pgvalue.UUID(normalized.EnvironmentID),
			ID: authority.WorkspaceID, ExpectedStateVersion: authority.WorkspaceStateVersion,
			ExpectedHeadVersionID: authority.HeadVersionID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errActorStartWorkspaceConflict
			}
			return fmt.Errorf("reserve Workspace for Actor: %w", err)
		}
		for _, binding := range bindings {
			if _, err := work.q.CreateSecretResolution(ctx, db.CreateSecretResolutionParams{
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: authority.WorkspaceID,
				RunID: run.ID, AttemptNumber: pgtype.Int4{Int32: 1, Valid: true},
				PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
				SecretID: binding.SecretID, SecretVersionID: binding.CurrentVersionID,
				RevocationGeneration: binding.RevocationGeneration,
			}); err != nil {
				return fmt.Errorf("record Actor boot Run Secret resolution: %w", err)
			}
		}
		if _, err := work.q.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceID: authority.WorkspaceID,
			EnvironmentID: pgvalue.UUID(normalized.EnvironmentID), RunID: run.ID,
		}); err != nil {
			return fmt.Errorf("create Actor boot Run admission outbox: %w", err)
		}
		result = actorStartResult{
			ActorID: actorID, ActorPublicID: actorPublicID, InitialRecordID: initialRecordID,
			BootRunID: runID, BootRunPublicID: runPublicID,
		}
		if claim != nil {
			receipt, err := json.Marshal(actorStartReceiptFromResult(result))
			if err != nil {
				return err
			}
			claims, err := s.claims.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func normalizeActorStart(request actorStartRequest) (normalizedActorStart, error) {
	if request.OrgID == uuid.Nil || request.ProjectID == uuid.Nil ||
		request.EnvironmentID == uuid.Nil || request.WorkspaceID == uuid.Nil {
		return normalizedActorStart{}, errActorStartInvalid
	}
	if err := api.ValidateActorDeclaredID(request.ActorDeclaredID); err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
	}
	if request.Key != nil {
		if err := api.ValidateActorKey(*request.Key); err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
		}
		key := *request.Key
		request.Key = &key
	}
	workspace, err := canonicalJSON(request.WorkspaceAddress)
	if err != nil || !jsonObject(workspace) {
		return normalizedActorStart{}, fmt.Errorf("%w: Workspace address must be an unambiguous JSON object", errActorStartInvalid)
	}
	request.WorkspaceAddress = workspace
	if request.InputPresent {
		input, err := canonicalJSON(request.Input)
		if err != nil || len(input) > maxActorInputBytes {
			return normalizedActorStart{}, fmt.Errorf("%w: initial input must be unambiguous JSON no larger than 1 MiB", errActorStartInvalid)
		}
		request.Input = input
	} else {
		request.Input = nil
	}
	request.Metadata, err = normalizeActorStartMetadata(request.Metadata, maxActorMetadataBytes, "Actor")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
	}
	request.ManagedRunMetadata, err = normalizeActorStartMetadata(request.ManagedRunMetadata, maxRunMetadataBytes, "managed Run")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
	}
	request.Tags, err = normalizeActorStartTags(request.Tags, "Actor")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
	}
	request.ManagedRunTags, err = normalizeActorStartTags(request.ManagedRunTags, "managed Run")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
	}
	if request.ExpiresAt != nil {
		expiresAt := request.ExpiresAt.UTC()
		request.ExpiresAt = &expiresAt
	}
	if request.ManagedQueueName != "" {
		if err := api.ValidateQueueName(request.ManagedQueueName); err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
		}
	}
	if request.ManagedConcurrencyKey != nil {
		value := *request.ManagedConcurrencyKey
		if len(value) == 0 || len(value) > 512 || !utf8.ValidString(value) ||
			strings.IndexByte(value, 0) >= 0 || hasInvalidConcurrencyKeyEdge(value) {
			return normalizedActorStart{}, fmt.Errorf("%w: managed Run concurrency key is invalid", errActorStartInvalid)
		}
		request.ManagedConcurrencyKey = &value
	}
	if request.ManagedQueuedTTLMS != nil &&
		(*request.ManagedQueuedTTLMS < 1 || *request.ManagedQueuedTTLMS > maxQueuedRunTTLMS) {
		return normalizedActorStart{}, fmt.Errorf(
			"%w: managed Run queued TTL must be between 1 and %d ms",
			errActorStartInvalid,
			maxQueuedRunTTLMS,
		)
	}
	if len(request.ManagedRetryPolicy) > 0 {
		canonicalRetry, err := canonicalJSON(request.ManagedRetryPolicy)
		if err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: retry must be unambiguous JSON", errActorStartInvalid)
		}
		if _, err := deployment.ParseRetryManifest(canonicalRetry); err != nil {
			return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
		}
		request.ManagedRetryPolicy = canonicalRetry
	}
	fingerprint := idempotency.ActorStartFingerprint{
		Key: request.Key, InputPresent: request.InputPresent, Input: request.Input,
		WorkspaceAddress: request.WorkspaceAddress, Metadata: request.Metadata,
		Tags: request.Tags, ExpiresAt: request.ExpiresAt,
		ManagedQueueName: request.ManagedQueueName, ManagedConcurrencyKey: request.ManagedConcurrencyKey,
		ManagedPriority: request.ManagedPriority, ManagedQueuedTTLMS: request.ManagedQueuedTTLMS,
		ManagedRetryPolicy: request.ManagedRetryPolicy,
		ManagedRunMetadata: request.ManagedRunMetadata, ManagedRunTags: request.ManagedRunTags,
	}
	return normalizedActorStart{actorStartRequest: request, fingerprint: fingerprint}, nil
}

func normalizeActorStartMetadata(raw json.RawMessage, limit int, label string) ([]byte, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	canonical, err := canonicalJSON(raw)
	if err != nil || !jsonObject(canonical) {
		return nil, fmt.Errorf("%s metadata must be an unambiguous JSON object", label)
	}
	if len(canonical) > limit {
		return nil, fmt.Errorf("%s metadata exceeds %d bytes", label, limit)
	}
	return canonical, nil
}

func normalizeActorStartTags(raw []string, label string) ([]string, error) {
	if len(raw) > maxActorTags {
		return nil, fmt.Errorf("%s tags must contain at most %d values", label, maxActorTags)
	}
	seen := make(map[string]struct{}, len(raw))
	tags := make([]string, 0, len(raw))
	for _, tag := range raw {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" || len([]byte(trimmed)) > maxActorTagBytes || !utf8.ValidString(trimmed) {
			return nil, fmt.Errorf("%s tags must be nonempty UTF-8 strings no larger than %d bytes", label, maxActorTagBytes)
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		tags = append(tags, trimmed)
	}
	sort.Strings(tags)
	return tags, nil
}

func jsonObject(raw []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func hasInvalidConcurrencyKeyEdge(value string) bool {
	invalid := func(value byte) bool {
		return value == 0x20 || (value >= 0x09 && value <= 0x0d)
	}
	return invalid(value[0]) || invalid(value[len(value)-1])
}

func int8Ptr(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func timePtrValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func actorStartReceiptFromResult(result actorStartResult) actorStartReceipt {
	receipt := actorStartReceipt{
		ActorID: result.ActorID.String(), ActorPublicID: result.ActorPublicID,
		BootRunID: result.BootRunID.String(), BootRunPublicID: result.BootRunPublicID,
	}
	if result.InitialRecordID != nil {
		value := result.InitialRecordID.String()
		receipt.InitialRecordID = &value
	}
	return receipt
}

func actorStartResultFromReceipt(raw []byte) (actorStartResult, error) {
	var receipt actorStartReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return actorStartResult{}, errActorStartIdempotencyReceipt
	}
	actorID, err := uuid.Parse(receipt.ActorID)
	if err != nil || publicid.ValidateFor(publicid.Actor, receipt.ActorPublicID) != nil {
		return actorStartResult{}, errActorStartIdempotencyReceipt
	}
	runID, err := uuid.Parse(receipt.BootRunID)
	if err != nil || publicid.ValidateFor(publicid.Run, receipt.BootRunPublicID) != nil {
		return actorStartResult{}, errActorStartIdempotencyReceipt
	}
	var initialRecordID *uuid.UUID
	if receipt.InitialRecordID != nil {
		value, err := uuid.Parse(*receipt.InitialRecordID)
		if err != nil {
			return actorStartResult{}, errActorStartIdempotencyReceipt
		}
		initialRecordID = &value
	}
	return actorStartResult{
		ActorID: actorID, ActorPublicID: receipt.ActorPublicID,
		InitialRecordID: initialRecordID, BootRunID: runID, BootRunPublicID: receipt.BootRunPublicID,
	}, nil
}
