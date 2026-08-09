package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxRunMetadataBytes = 256 << 10
	maxTags             = 10
	maxQueuedRunTTLMS   = int64(31_536_000_000)
)

var (
	errActorStartInvalid            = errors.New("actor start request is invalid")
	errActorStartNotDeployed        = errors.New("actor declaration is not deployed")
	errActorStartWorkspaceNotFound  = errors.New("actor start workspace was not found")
	errActorStartAuthority          = errors.New("actor start authority is unavailable")
	errActorStartWorkspaceConflict  = errors.New("actor start workspace cannot accept execution")
	errActorStartSecretUnavailable  = errors.New("actor start workspace secret is unavailable")
	errActorStartIdempotencyReceipt = errors.New("actor start idempotency receipt is invalid")
)

type ActorKeyConflictError struct {
	Key string
}

func (e ActorKeyConflictError) Error() string {
	return fmt.Sprintf("actor key %q already belongs to another actor", e.Key)
}

type actorStartRequest struct {
	OrgID                 uuid.UUID
	ProjectID             uuid.UUID
	EnvironmentID         uuid.UUID
	ActorDeclaredID       string
	WorkspaceID           uuid.UUID
	Key                   *string
	InputPresent          bool
	Input                 json.RawMessage
	IdempotencyKey        string
	ManagedQueueName      string
	ManagedConcurrencyKey *string
	ManagedPriority       int32
	ManagedQueuedTTLMS    *int64
	ManagedRetryPolicy    json.RawMessage
	ManagedRunMetadata    json.RawMessage
	ManagedRunTags        []string
	Authorize             func(context.Context, db.Querier) error
	DisallowedWorkspaceID uuid.UUID
}

type actorStartResult struct {
	SessionID       uuid.UUID
	InitialRecordID *uuid.UUID
	BootRunID       uuid.UUID
	Replayed        bool
}

type actorStartReceipt struct {
	SessionID       string  `json:"actorId"`
	InitialRecordID *string `json:"initialRecordId,omitempty"`
	BootRunID       string  `json:"bootRunId"`
}

type normalizedActorStart struct {
	actorStartRequest
	fingerprint idempotency.ActorStartFingerprint
}

// startActor is the durable Actor creation primitive. Claim replay, promoted
// declaration and Workspace authority, Actor/input/boot-Run creation,
// Workspace ownership, Secret resolution, and the queued Run commit as one
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
		if normalized.Authorize != nil {
			if err := normalized.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		var claim *db.IdempotencyClaim
		if claimRequest != nil {
			claims, err := idempotency.TransactionForQueries(work.q)
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

		deploymentAuthority, err := work.q.LockActorStartDeploymentAuthority(
			ctx,
			db.LockActorStartDeploymentAuthorityParams{
				ActorDeclaredID: normalized.ActorDeclaredID,
				OrgID:           pgvalue.UUID(normalized.OrgID),
				ProjectID:       pgvalue.UUID(normalized.ProjectID),
				EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorStartNotDeployed
		}
		if err != nil {
			return fmt.Errorf("lock actor start deployment authority: %w", err)
		}
		workspaceID := pgvalue.UUID(normalized.WorkspaceID)
		if normalized.DisallowedWorkspaceID != uuid.Nil &&
			workspaceID == pgvalue.UUID(normalized.DisallowedWorkspaceID) {
			return errActorStartWorkspaceConflict
		}

		if normalized.Key != nil {
			if err := work.q.LockActorStartKey(ctx, db.LockActorStartKeyParams{
				EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
				ActorDeclaredID: normalized.ActorDeclaredID,
				Key:             *normalized.Key,
			}); err != nil {
				return fmt.Errorf("lock actor start key: %w", err)
			}
			_, err := work.q.GetActorByKey(ctx, db.GetActorByKeyParams{
				EnvironmentID:   pgvalue.UUID(normalized.EnvironmentID),
				ActorDeclaredID: normalized.ActorDeclaredID,
				Key:             pgvalue.Text(*normalized.Key),
			})
			if err == nil {
				return ActorKeyConflictError{Key: *normalized.Key}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check actor start key: %w", err)
			}
		}

		authority, err := work.q.LockWorkspaceAdmissionAuthority(
			ctx,
			db.LockWorkspaceAdmissionAuthorityParams{
				EnvironmentID: pgvalue.UUID(normalized.EnvironmentID),
				ID:            workspaceID,
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errActorStartWorkspaceConflict
		}
		if err != nil {
			return fmt.Errorf("lock actor start workspace authority: %w", err)
		}
		if authority.OrgID != pgvalue.UUID(normalized.OrgID) ||
			authority.ProjectID != pgvalue.UUID(normalized.ProjectID) ||
			authority.State != db.WorkspaceStateActive ||
			(authority.DesiredState != db.WorkspaceDesiredStateActive &&
				authority.DesiredState != db.WorkspaceDesiredStateStopped) ||
			authority.DirtyState != db.WorkspaceDirtyStateClean ||
			!authority.HeadVersionID.Valid ||
			authority.OwnerSessionID.Valid || authority.OwnerRunID.Valid ||
			authority.HasActiveLease || authority.HasActiveProcess {
			return errActorStartWorkspaceConflict
		}
		bindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("lock actor start workspace secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errActorStartSecretUnavailable
			}
		}
		runAuthority, err := deployment.ResolveActorRunAdmission(
			deploymentAuthority.ActorManifestVersion,
			normalized.ActorDeclaredID,
			deploymentAuthority.ActorManifest,
			deploymentAuthority.ActorManifestDigest,
			deploymentAuthority.QueueConfig,
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
		actorID := uuid.Must(uuid.NewV7())
		runID := uuid.Must(uuid.NewV7())
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		claimID := pgtype.UUID{}
		if claim != nil {
			claimID = claim.ID
		}
		_, err = work.q.CreateActor(ctx, db.CreateActorParams{
			ID:    pgvalue.UUID(actorID),
			OrgID: pgvalue.UUID(normalized.OrgID), ProjectID: pgvalue.UUID(normalized.ProjectID),
			Key: pgvalue.TextPtr(normalized.Key), RunQueueName: runAuthority.QueueName,
			RunConcurrencyKey:        pgvalue.TextPtr(normalized.ManagedConcurrencyKey),
			RunQueueConcurrencyLimit: int8Ptr(runAuthority.QueueConcurrencyLimit),
			RunPriority:              normalized.ManagedPriority, RunQueueTtlMs: int8Ptr(managedQueuedTTL),
			RunMaxActiveDurationMs: runAuthority.MaxActiveDurationMS,
			RunRetryPolicy:         managedRetryPolicy,
			RunMetadata:            normalized.ManagedRunMetadata, RunTags: normalized.ManagedRunTags,
			WorkspaceID: authority.ID, EnvironmentID: pgvalue.UUID(normalized.EnvironmentID),
			DeploymentDefinitionID: deploymentAuthority.ActorDefinitionID, ActorDeclaredID: normalized.ActorDeclaredID,
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
			return fmt.Errorf("create actor: %w", err)
		}

		var initialRecordID *uuid.UUID
		inputHighWatermark := int64(0)
		if normalized.InputPresent {
			recordID := uuid.Must(uuid.NewV7())
			if _, err := work.q.CreateActorStartInputRecord(ctx, db.CreateActorStartInputRecordParams{
				ID: pgvalue.UUID(recordID), Data: normalized.Input, ClaimID: claimID,
				EnvironmentID: pgvalue.UUID(normalized.EnvironmentID), SessionID: pgvalue.UUID(actorID),
			}); err != nil {
				return fmt.Errorf("create initial actor input: %w", err)
			}
			initialRecordID = &recordID
			inputHighWatermark = 1
		}
		run, err := work.q.CreateActorStartRun(ctx, db.CreateActorStartRunParams{
			EnvironmentID: pgvalue.UUID(normalized.EnvironmentID), SessionID: pgvalue.UUID(actorID),
			WorkspaceID: authority.ID, ClaimID: claimID,
			ID:                     pgvalue.UUID(runID),
			BaseWorkspaceVersionID: authority.HeadVersionID,
			InputHighWatermark:     pgtype.Int8{Int64: inputHighWatermark, Valid: true},
			RootSpanID:             rootSpanID,
		})
		if err != nil {
			return fmt.Errorf("create actor boot run: %w", err)
		}
		if _, err := work.q.SetActorCurrentRun(ctx, db.SetActorCurrentRunParams{
			RunID: run.ID, EnvironmentID: run.EnvironmentID,
			ID: pgvalue.UUID(actorID), WorkspaceID: authority.ID,
		}); err != nil {
			return fmt.Errorf("install actor boot run: %w", err)
		}
		if _, err := work.q.ReserveWorkspaceForActor(ctx, db.ReserveWorkspaceForActorParams{
			SessionID: pgvalue.UUID(actorID), EnvironmentID: pgvalue.UUID(normalized.EnvironmentID),
			ID: authority.ID, ExpectedStateVersion: authority.StateVersion,
			ExpectedHeadVersionID: authority.HeadVersionID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errActorStartWorkspaceConflict
			}
			return fmt.Errorf("reserve workspace for actor: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, work.q, authority.ID, run.ID, 1, workspaceSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record actor boot run secret resolutions: %w", err)
		}
		result = actorStartResult{
			SessionID: actorID, InitialRecordID: initialRecordID,
			BootRunID: runID,
		}
		if claim != nil {
			receipt, err := json.Marshal(actorStartReceiptFromResult(result))
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionForQueries(work.q)
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
		request.EnvironmentID == uuid.Nil {
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
	if request.WorkspaceID == uuid.Nil {
		return normalizedActorStart{}, errActorStartInvalid
	}
	workspaceRaw, err := json.Marshal(api.WorkspaceIDTarget{ID: request.WorkspaceID.String()})
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: encode workspace address", errActorStartInvalid)
	}
	workspace, err := canonicalJSON(workspaceRaw)
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: canonicalize workspace address", errActorStartInvalid)
	}
	if request.InputPresent {
		input, err := canonicalJSON(request.Input)
		if err != nil || len(input) > maxActorInputBytes {
			return normalizedActorStart{}, fmt.Errorf("%w: initial input must be unambiguous JSON no larger than 1 MiB", errActorStartInvalid)
		}
		request.Input = input
	} else {
		request.Input = nil
	}
	request.ManagedRunMetadata, err = normalizeMetadata(request.ManagedRunMetadata, maxRunMetadataBytes, "managed run")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
	}
	request.ManagedRunTags, err = normalizeTags(request.ManagedRunTags, maxTags, "managed run")
	if err != nil {
		return normalizedActorStart{}, fmt.Errorf("%w: %v", errActorStartInvalid, err)
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
			return normalizedActorStart{}, fmt.Errorf("%w: managed run concurrency key is invalid", errActorStartInvalid)
		}
		request.ManagedConcurrencyKey = &value
	}
	if request.ManagedQueuedTTLMS != nil &&
		(*request.ManagedQueuedTTLMS < 1 || *request.ManagedQueuedTTLMS > maxQueuedRunTTLMS) {
		return normalizedActorStart{}, fmt.Errorf(
			"%w: managed run queued TTL must be between 1 and %d ms",
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
		WorkspaceAddress: workspace,
		ManagedQueueName: request.ManagedQueueName, ManagedConcurrencyKey: request.ManagedConcurrencyKey,
		ManagedPriority: request.ManagedPriority, ManagedQueuedTTLMS: request.ManagedQueuedTTLMS,
		ManagedRetryPolicy: request.ManagedRetryPolicy,
		ManagedRunMetadata: request.ManagedRunMetadata, ManagedRunTags: request.ManagedRunTags,
	}
	return normalizedActorStart{actorStartRequest: request, fingerprint: fingerprint}, nil
}

func normalizeMetadata(raw json.RawMessage, limit int, label string) ([]byte, error) {
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

func normalizeTags(raw []string, limit int, label string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	tags := make([]string, 0, len(raw))
	for _, tag := range raw {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" || len([]byte(trimmed)) > maxTagBytes || !utf8.ValidString(trimmed) {
			return nil, fmt.Errorf("%s tags must be nonempty UTF-8 strings no larger than %d bytes", label, maxTagBytes)
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		tags = append(tags, trimmed)
	}
	if len(tags) > limit {
		return nil, fmt.Errorf("%s tags must contain at most %d values", label, limit)
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

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func actorStartReceiptFromResult(result actorStartResult) actorStartReceipt {
	receipt := actorStartReceipt{
		SessionID: result.SessionID.String(),
		BootRunID: result.BootRunID.String(),
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
	actorID, err := ids.Parse(receipt.SessionID)
	if err != nil {
		return actorStartResult{}, errActorStartIdempotencyReceipt
	}
	runID, err := ids.Parse(receipt.BootRunID)
	if err != nil {
		return actorStartResult{}, errActorStartIdempotencyReceipt
	}
	var initialRecordID *uuid.UUID
	if receipt.InitialRecordID != nil {
		value, err := ids.Parse(*receipt.InitialRecordID)
		if err != nil {
			return actorStartResult{}, errActorStartIdempotencyReceipt
		}
		initialRecordID = &value
	}
	return actorStartResult{
		SessionID:       actorID,
		InitialRecordID: initialRecordID, BootRunID: runID,
	}, nil
}
