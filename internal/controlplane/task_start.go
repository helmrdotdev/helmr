package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxTaskPayloadBytes = 16 << 20

var (
	errTaskStartInvalid           = errors.New("task start request is invalid")
	errTaskNotDeployed            = errors.New("task declaration is not deployed")
	errTaskWorkspaceNotFound      = errors.New("task start workspace was not found")
	errTaskWorkspaceUnavailable   = errors.New("task start workspace cannot accept execution")
	errTaskSecretUnavailable      = errors.New("task start workspace secret is unavailable")
	errTaskStartAuthority         = errors.New("task start authority is unavailable")
	errTaskStartReceiptInvalid    = errors.New("task start idempotency receipt is invalid")
	errTaskPayloadPresenceInvalid = errors.New("task payload presence does not match its declaration")
)

type taskStartRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	TaskDeclaredID string
	PayloadPresent bool
	Payload        json.RawMessage
	WorkspaceID    uuid.UUID
	IdempotencyKey string
	QueueName      string
	ConcurrencyKey *string
	Priority       int32
	QueuedTTLMS    *int64
	RetryPolicy    json.RawMessage
	Metadata       json.RawMessage
	Tags           []string
}

type taskStartResult struct {
	RunID    uuid.UUID
	Replayed bool
}

type taskStartReceipt struct {
	RunID string `json:"run_id"`
}

type normalizedTaskStart struct {
	taskStartRequest
	fingerprint idempotency.TaskStartFingerprint
}

func (s *Server) startTask(ctx context.Context, request taskStartRequest) (taskStartResult, error) {
	normalized, err := normalizeTaskStart(request)
	if err != nil {
		return taskStartResult{}, err
	}
	var claimRequest idempotency.Request
	if normalized.IdempotencyKey != "" {
		claimRequest, err = idempotency.NewTaskStartRequest(
			normalized.EnvironmentID,
			normalized.TaskDeclaredID,
			normalized.IdempotencyKey,
			normalized.fingerprint,
		)
		if err != nil {
			return taskStartResult{}, fmt.Errorf("%w: %v", errTaskStartInvalid, err)
		}
	}

	var result taskStartResult
	err = s.inTx(ctx, func(work *txWork) error {
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
				replayed, err := taskStartResultFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				result = replayed
				return nil
			}
			if acquired.Claim.State != "pending" {
				return errTaskStartReceiptInvalid
			}
			claim = &acquired.Claim
		}

		program, err := work.q.LockTaskStartDeploymentAuthority(
			ctx,
			db.LockTaskStartDeploymentAuthorityParams{
				TaskDeclaredID: normalized.TaskDeclaredID,
				OrgID:          pgvalue.UUID(normalized.OrgID),
				ProjectID:      pgvalue.UUID(normalized.ProjectID),
				EnvironmentID:  pgvalue.UUID(normalized.EnvironmentID),
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskNotDeployed
		}
		if err != nil {
			return fmt.Errorf("lock task start deployment authority: %w", err)
		}
		admission, err := deployment.ResolveTaskRunAdmission(
			program.TaskManifestVersion,
			normalized.TaskDeclaredID,
			program.TaskManifest,
			program.TaskManifestDigest,
			program.QueueConfig,
			normalized.QueueName,
			normalized.QueuedTTLMS,
			normalized.RetryPolicy,
		)
		if err != nil {
			return fmt.Errorf("%w: %v", errTaskStartAuthority, err)
		}
		if admission.HasPayload != normalized.PayloadPresent {
			return errTaskPayloadPresenceInvalid
		}

		workspaceID := pgvalue.UUID(normalized.WorkspaceID)
		workspace, err := work.q.LockWorkspaceAdmissionAuthority(
			ctx,
			db.LockWorkspaceAdmissionAuthorityParams{
				EnvironmentID: pgvalue.UUID(normalized.EnvironmentID),
				ID:            workspaceID,
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskWorkspaceUnavailable
		}
		bindings, err := work.q.LockWorkspaceSecretsForAdmission(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("lock task start workspace secrets: %w", err)
		}
		for _, binding := range bindings {
			if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
				return errTaskSecretUnavailable
			}
		}
		if err != nil {
			return fmt.Errorf("lock task start workspace authority: %w", err)
		}
		if workspace.OrgID != pgvalue.UUID(normalized.OrgID) ||
			workspace.ProjectID != pgvalue.UUID(normalized.ProjectID) ||
			workspace.State != db.WorkspaceStateActive ||
			(workspace.DesiredState != db.WorkspaceDesiredStateActive &&
				workspace.DesiredState != db.WorkspaceDesiredStateStopped) ||
			workspace.DirtyState != db.WorkspaceDirtyStateClean ||
			!workspace.HeadVersionID.Valid ||
			workspace.OwnerSessionID.Valid || workspace.OwnerRunID.Valid ||
			workspace.HasActiveLease || workspace.HasActiveProcess {
			return errTaskWorkspaceUnavailable
		}
		runID := uuid.NewV7()
		rootSpanID, err := tracing.NewSpanID()
		if err != nil {
			return err
		}
		claimID := pgtype.UUID{}
		if claim != nil {
			claimID = claim.ID
		}
		admissionTime, err := work.q.GetRunAdmissionTime(ctx)
		if err != nil {
			return fmt.Errorf("get task run admission time: %w", err)
		}
		now := admissionTime.Time.UTC()
		queuedExpiresAt := pgtype.Timestamptz{}
		if admission.QueuedTTLMS != nil {
			queuedExpiresAt = pgvalue.Timestamptz(now.Add(
				time.Duration(*admission.QueuedTTLMS) * time.Millisecond,
			))
		}
		run, err := work.q.CreateRootRunFromCurrentDeployment(
			ctx,
			db.CreateRootRunFromCurrentDeploymentParams{
				EntrypointDeclaredID: normalized.TaskDeclaredID,
				WorkspaceID:          workspace.ID,
				OrgID:                pgvalue.UUID(normalized.OrgID), ProjectID: pgvalue.UUID(normalized.ProjectID),
				BaseWorkspaceVersionID: workspace.HeadVersionID,
				EnvironmentID:          pgvalue.UUID(normalized.EnvironmentID), ClaimID: claimID,
				ID: pgvalue.UUID(runID), CauseKind: "api",
				Payload: normalized.Payload, Metadata: normalized.Metadata, Tags: normalized.Tags,
				QueueName: admission.QueueName, ConcurrencyKey: pgvalue.TextPtr(normalized.ConcurrencyKey),
				QueueConcurrencyLimit: int8Ptr(admission.QueueConcurrencyLimit),
				Priority:              normalized.Priority,
				QueueOriginAt:         pgvalue.Timestamptz(now),
				QueueScoreAt:          pgvalue.Timestamptz(now.Add(-time.Duration(normalized.Priority) * time.Second)),
				QueuedExpiresAt:       queuedExpiresAt,
				MaxActiveDurationMs:   admission.MaxActiveDurationMS,
				RetryPolicy:           admission.RetryPolicy, RootSpanID: rootSpanID,
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTaskStartAuthority
		}
		if err != nil {
			return fmt.Errorf("create task run: %w", err)
		}
		if _, err := work.q.ReserveWorkspaceForRun(ctx, db.ReserveWorkspaceForRunParams{
			RunID: run.ID, EnvironmentID: run.EnvironmentID, ID: workspace.ID,
			ExpectedStateVersion:  workspace.StateVersion,
			ExpectedHeadVersionID: workspace.HeadVersionID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskWorkspaceUnavailable
			}
			return fmt.Errorf("reserve task workspace: %w", err)
		}
		if err := secret.CreateAttemptResolutions(
			ctx, work.q, workspace.ID, run.ID, 1, workspaceSecretResolutions(bindings),
		); err != nil {
			return fmt.Errorf("record task run secret resolutions: %w", err)
		}
		result = taskStartResult{RunID: runID}
		if claim != nil {
			receipt, err := json.Marshal(taskStartReceipt{
				RunID: runID.String(),
			})
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

func normalizeTaskStart(request taskStartRequest) (normalizedTaskStart, error) {
	if request.OrgID == uuid.Nil() || request.ProjectID == uuid.Nil() ||
		request.EnvironmentID == uuid.Nil() {
		return normalizedTaskStart{}, errTaskStartInvalid
	}
	if err := api.ValidateDefinitionID(request.TaskDeclaredID); err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: %v", errTaskStartInvalid, err)
	}
	if request.WorkspaceID == uuid.Nil() {
		return normalizedTaskStart{}, errTaskStartInvalid
	}
	workspaceRaw, err := json.Marshal(api.WorkspaceIDTarget{ID: request.WorkspaceID.String()})
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: encode workspace", errTaskStartInvalid)
	}
	workspace, err := canonicalJSON(workspaceRaw)
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: canonicalize workspace", errTaskStartInvalid)
	}
	if request.PayloadPresent {
		payload, err := canonicalJSON(request.Payload)
		if err != nil || len(payload) > maxTaskPayloadBytes {
			return normalizedTaskStart{}, fmt.Errorf(
				"%w: payload must be unambiguous JSON no larger than %d bytes",
				errTaskStartInvalid,
				maxTaskPayloadBytes,
			)
		}
		request.Payload = payload
	} else {
		request.Payload = nil
	}
	request.Metadata, err = normalizeMetadata(request.Metadata, maxRunMetadataBytes, "run")
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: %v", errTaskStartInvalid, err)
	}
	request.Tags, err = normalizeTags(request.Tags, maxTags, "run")
	if err != nil {
		return normalizedTaskStart{}, fmt.Errorf("%w: %v", errTaskStartInvalid, err)
	}
	if request.QueueName != "" {
		if err := api.ValidateQueueName(request.QueueName); err != nil {
			return normalizedTaskStart{}, fmt.Errorf("%w: %v", errTaskStartInvalid, err)
		}
	}
	if request.ConcurrencyKey != nil {
		value := *request.ConcurrencyKey
		if len(value) == 0 || len(value) > 512 || !utf8.ValidString(value) ||
			strings.IndexByte(value, 0) >= 0 || hasInvalidConcurrencyKeyEdge(value) {
			return normalizedTaskStart{}, fmt.Errorf("%w: concurrency key is invalid", errTaskStartInvalid)
		}
		request.ConcurrencyKey = &value
	}
	if request.QueuedTTLMS != nil &&
		(*request.QueuedTTLMS < 1 || *request.QueuedTTLMS > maxQueuedRunTTLMS) {
		return normalizedTaskStart{}, fmt.Errorf("%w: queued TTL is invalid", errTaskStartInvalid)
	}
	if len(request.RetryPolicy) > 0 {
		retry, err := canonicalJSON(request.RetryPolicy)
		if err != nil {
			return normalizedTaskStart{}, fmt.Errorf("%w: retry is invalid", errTaskStartInvalid)
		}
		if _, err := deployment.ParseRetryManifest(retry); err != nil {
			return normalizedTaskStart{}, fmt.Errorf("%w: retry is invalid: %v", errTaskStartInvalid, err)
		}
		request.RetryPolicy = retry
	}
	return normalizedTaskStart{
		taskStartRequest: request,
		fingerprint: idempotency.TaskStartFingerprint{
			PayloadPresent: request.PayloadPresent, Payload: request.Payload,
			Workspace: workspace, QueueName: request.QueueName,
			ConcurrencyKey: request.ConcurrencyKey, Priority: request.Priority,
			QueuedTTLMS: request.QueuedTTLMS, RetryPolicy: request.RetryPolicy,
			Metadata: request.Metadata, Tags: request.Tags,
		},
	}, nil
}

func taskStartResultFromReceipt(raw []byte) (taskStartResult, error) {
	var receipt taskStartReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return taskStartResult{}, errTaskStartReceiptInvalid
	}
	runID, err := ids.Parse(receipt.RunID)
	if err != nil {
		return taskStartResult{}, errTaskStartReceiptInvalid
	}
	return taskStartResult{RunID: runID}, nil
}
