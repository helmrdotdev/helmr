package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/sourceid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

const maxImageBuildTreeBytes = int64(11 << 30)

type workspaceImageAdmission struct {
	request               workerapi.WorkspaceImageAdmissionRequest
	leaseID               uuid.UUID
	environmentID         uuid.UUID
	deploymentID          uuid.UUID
	planDigest            string
	planDigestBytes       []byte
	admittedPathSetDigest string
	credentials           []imagebuild.RegistryCredential
	quotas                workerapi.WorkspaceImageQuotas
	output                workerapi.WorkspaceImageOutputContract
}

type workspaceImageOperationReceipt struct {
	BuildLeaseID         string                 `json:"buildLeaseId"`
	BuildLeaseGeneration int64                  `json:"buildLeaseGeneration"`
	DeclarationSlot      string                 `json:"declarationSlot"`
	OperationID          string                 `json:"operationId"`
	AttemptID            string                 `json:"attemptId"`
	RequestFingerprint   string                 `json:"requestFingerprint"`
	PlanDigest           string                 `json:"planDigest"`
	ResolutionSetDigest  string                 `json:"resolutionSetDigest"`
	RequestedCacheMode   imagebuild.CacheMode   `json:"requestedCacheMode"`
	Result               imagebuild.GuestResult `json:"result"`
}

func (s *Server) workerAdmitWorkspaceImage(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceImageAdmissionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace image admission JSON: %w", err)))
		return
	}
	normalized, err := normalizeWorkspaceImageAdmission(request, workerFromContext(r.Context()))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	assignment, err := s.admitWorkspaceImage(r.Context(), normalized, workerFromContext(r.Context()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = conflict(errors.New("workspace image build lease or secret authority is stale"))
		}
		var claimConflict idempotency.ConflictError
		if errors.As(err, &claimConflict) {
			err = conflict(errors.New("workspace image operation conflicts with its admitted request"))
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assignment)
}

func normalizeWorkspaceImageAdmission(
	request workerapi.WorkspaceImageAdmissionRequest,
	worker workerActor,
) (workspaceImageAdmission, error) {
	leaseID, environmentID, deploymentID, err := validateWorkspaceImageLease(request.Lease, worker)
	if err != nil {
		return workspaceImageAdmission{}, err
	}
	if !sourceid.Valid(request.DeclarationSlot) {
		return workspaceImageAdmission{}, errors.New("workspace image declaration_slot is invalid")
	}
	planDigest, err := imagebuild.Digest(request.Plan, request.Architecture)
	if err != nil {
		return workspaceImageAdmission{}, fmt.Errorf("validate workspace image plan: %w", err)
	}
	planDigestBytes, err := deployment.SHA256DigestBytes(planDigest)
	if err != nil {
		return workspaceImageAdmission{}, err
	}
	for label, digest := range map[string]string{
		"runtime identity": request.RuntimeIdentityID,
		"submitted source": request.SubmittedSourceDigest,
		"build tree":       request.BuildTreeDigest,
		"source archive":   request.SourceArchiveDigest,
	} {
		if _, err := deployment.SHA256DigestBytes(digest); err != nil {
			return workspaceImageAdmission{}, fmt.Errorf("workspace image %s digest is invalid", label)
		}
	}
	if request.BuildTreeSizeBytes < 1 || request.BuildTreeSizeBytes > maxImageBuildTreeBytes {
		return workspaceImageAdmission{}, errors.New("workspace image build tree size is outside the v0 contract")
	}
	if request.AdmittedPaths == nil ||
		request.SourceArchiveEntries != len(request.AdmittedPaths) {
		return workspaceImageAdmission{}, errors.New("workspace image admitted path set is incomplete")
	}
	credentials, err := imagebuild.RegistryCredentials(request.Plan, request.Architecture)
	if err != nil {
		return workspaceImageAdmission{}, err
	}
	resources := compute.ImageBuildGuestResources()
	quotas := workerapi.WorkspaceImageQuotas{
		CPUMillis: resources.MilliCPU, MemoryBytes: resources.MemoryMiB << 20,
		ScratchBytes: resources.DiskMiB << 20, PIDs: compute.ImageBuildGuestPIDsMax,
		MaxSourceArchiveBytes:   imagebuild.MaxSourceArchiveBytes,
		MaxSourceArchiveEntries: imagebuild.MaxSourceArchiveEntries,
		MaxOCIArchiveBytes:      imagebuild.MaxOCIArchiveBytes,
	}
	output := workerapi.WorkspaceImageOutputContract{
		Architecture: request.Architecture,
		MediaType:    deployment.WorkspaceImageArtifactMediaType,
		MaxSizeBytes: imagebuild.MaxOCIArchiveBytes,
	}
	admittedPathSetDigest := imagebuild.PathSetDigest(request.AdmittedPaths)
	if err := imagebuild.ValidateSourceAdmission(imagebuild.SourceAdmission{
		Architecture: request.Architecture, Plan: request.Plan, PlanDigest: planDigest,
		SubmittedSourceDigest: request.SubmittedSourceDigest, BuildTreeDigest: request.BuildTreeDigest,
		AdmittedPaths: request.AdmittedPaths, AdmittedPathSetDigest: admittedPathSetDigest,
		SourceArchiveDigest: request.SourceArchiveDigest, SourceArchiveSizeBytes: request.SourceArchiveSizeBytes,
		SourceArchiveEntries: request.SourceArchiveEntries,
	}); err != nil {
		return workspaceImageAdmission{}, err
	}
	return workspaceImageAdmission{
		request: request, leaseID: leaseID, environmentID: environmentID, deploymentID: deploymentID,
		planDigest: planDigest, planDigestBytes: planDigestBytes,
		admittedPathSetDigest: admittedPathSetDigest, credentials: credentials,
		quotas: quotas, output: output,
	}, nil
}

func (s *Server) admitWorkspaceImage(
	ctx context.Context,
	normalized workspaceImageAdmission,
	worker workerActor,
) (workerapi.WorkspaceImageAssignment, error) {
	var assignment workerapi.WorkspaceImageAssignment
	err := s.inTx(ctx, func(work *txWork) error {
		authority, err := lockWorkspaceImageBuildLease(ctx, work.q, normalized.request.Lease, worker)
		if err != nil {
			return err
		}
		if authority.SubmittedSourceDigest != normalized.request.SubmittedSourceDigest {
			return conflict(errors.New("workspace image submitted source does not match the deployment"))
		}
		if !authority.RuntimeIdentityID.Valid ||
			authority.RuntimeIdentityID.String != normalized.request.RuntimeIdentityID {
			return conflict(errors.New("workspace image runtime identity does not match the current worker authority"))
		}
		cacheMode := imagebuild.CacheMode(authority.Deployment.ImageCacheMode)
		if cacheMode != imagebuild.CachePrefer && cacheMode != imagebuild.CacheBypass {
			return errors.New("deployment image cache mode is invalid")
		}
		cacheScope, err := imagebuild.CacheScope(
			normalized.environmentID,
			normalized.request.DeclarationSlot,
			normalized.request.Architecture,
		)
		if err != nil {
			return err
		}
		fingerprint := idempotency.WorkspaceImageBuildFingerprint{
			Architecture: normalized.request.Architecture, PlanDigest: normalized.planDigest,
			SubmittedSourceDigest: normalized.request.SubmittedSourceDigest,
			BuildTreeDigest:       normalized.request.BuildTreeDigest, BuildTreeSizeBytes: normalized.request.BuildTreeSizeBytes,
			AdmittedPathSetDigest:  normalized.admittedPathSetDigest,
			SourceArchiveDigest:    normalized.request.SourceArchiveDigest,
			SourceArchiveSizeBytes: normalized.request.SourceArchiveSizeBytes,
			SourceArchiveEntries:   normalized.request.SourceArchiveEntries,
			ImageCacheMode:         string(cacheMode), CacheScope: cacheScope,
			ImageBuildContract: imagebuild.Contract,
			Quotas: idempotency.WorkspaceImageBuildQuotas{
				CPUMillis: normalized.quotas.CPUMillis, MemoryBytes: normalized.quotas.MemoryBytes,
				ScratchBytes: normalized.quotas.ScratchBytes, PIDs: normalized.quotas.PIDs,
				MaxSourceArchiveBytes:   normalized.quotas.MaxSourceArchiveBytes,
				MaxSourceArchiveEntries: normalized.quotas.MaxSourceArchiveEntries,
				MaxOCIArchiveBytes:      normalized.quotas.MaxOCIArchiveBytes,
			},
			Output: idempotency.WorkspaceImageBuildOutputContract{
				Architecture: normalized.output.Architecture, MediaType: normalized.output.MediaType,
				MaxSizeBytes: normalized.output.MaxSizeBytes,
			},
		}
		claimRequest, err := idempotency.NewWorkspaceImageBuildRequest(
			normalized.environmentID, normalized.leaseID, normalized.request.Lease.LeaseSequence,
			normalized.request.DeclarationSlot, fingerprint,
		)
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, claimRequest)
		if err != nil {
			return err
		}
		operationID := pgvalue.MustUUIDValue(acquired.Claim.ID)
		pending := acquired.Claim.State == "pending"
		if pending {
			if _, err := work.q.LockRegistryCredentialImageOperation(ctx, db.LockRegistryCredentialImageOperationParams{
				EnvironmentID: pgvalue.UUID(normalized.environmentID), ImageOperationID: acquired.Claim.ID,
			}); err != nil {
				return err
			}
		} else if acquired.Claim.State != "completed" && acquired.Claim.State != "failed" {
			return conflict(errors.New("workspace image operation has an invalid state"))
		}
		bindings, err := resolveWorkspaceImageRegistryBindings(
			ctx, work.q, normalized, operationID, acquired.New, pending,
		)
		if err != nil {
			return err
		}
		resolutionSetDigest := imagebuild.ResolutionSetDigest(bindings)
		requestFingerprint, err := workspaceImageDigest(acquired.Claim.RequestFingerprint)
		if err != nil {
			return err
		}
		assignment = workerapi.WorkspaceImageAssignment{
			Lease: normalized.request.Lease, DeclarationSlot: normalized.request.DeclarationSlot,
			OperationID: operationID.String(), RequestFingerprint: requestFingerprint,
			RuntimeIdentityID: normalized.request.RuntimeIdentityID,
			Architecture:      normalized.request.Architecture, Plan: normalized.request.Plan,
			PlanDigest: normalized.planDigest, SubmittedSourceDigest: normalized.request.SubmittedSourceDigest,
			BuildTreeDigest: normalized.request.BuildTreeDigest, BuildTreeSizeBytes: normalized.request.BuildTreeSizeBytes,
			AdmittedPaths:          slices.Clone(normalized.request.AdmittedPaths),
			AdmittedPathSetDigest:  normalized.admittedPathSetDigest,
			SourceArchiveDigest:    normalized.request.SourceArchiveDigest,
			SourceArchiveSizeBytes: normalized.request.SourceArchiveSizeBytes,
			SourceArchiveEntries:   normalized.request.SourceArchiveEntries,
			RequestedCacheMode:     cacheMode, CacheScope: cacheScope,
			ImageBuildContract: imagebuild.Contract,
			Quotas:             normalized.quotas, Output: normalized.output,
			RegistryBindings: bindings, ResolutionSetDigest: resolutionSetDigest,
		}
		if !pending {
			terminal, err := workspaceImageTerminalResult(
				acquired.Claim, normalized.leaseID, normalized.request.Lease.LeaseSequence,
				normalized.request.DeclarationSlot, operationID, requestFingerprint,
				normalized.planDigest, resolutionSetDigest, cacheMode,
			)
			if err != nil {
				return err
			}
			assignment.TerminalResult = &terminal
		}
		return validateWorkspaceImageAssignment(assignment, worker)
	})
	if err != nil {
		return workerapi.WorkspaceImageAssignment{}, err
	}
	s.attachWorkspaceImageCache(ctx, normalized.environmentID, &assignment)
	return assignment, nil
}

func resolveWorkspaceImageRegistryBindings(
	ctx context.Context,
	q db.Querier,
	normalized workspaceImageAdmission,
	operationID uuid.UUID,
	isNew bool,
	requireDelivery bool,
) ([]imagebuild.RegistryBinding, error) {
	if !isNew {
		rows, err := q.ListRegistryCredentialResolutions(ctx, db.ListRegistryCredentialResolutionsParams{
			EnvironmentID: pgvalue.UUID(normalized.environmentID), DeploymentID: pgvalue.UUID(normalized.deploymentID),
			BuildLeaseID: pgvalue.UUID(normalized.leaseID), ImageOperationID: pgvalue.UUID(operationID),
		})
		if err != nil {
			return nil, err
		}
		return replayWorkspaceImageRegistryBindings(ctx, q, normalized, rows, requireDelivery)
	}
	if len(normalized.credentials) == 0 {
		return []imagebuild.RegistryBinding{}, nil
	}
	names := make([]string, 0, len(normalized.credentials))
	seenNames := make(map[string]struct{}, len(normalized.credentials))
	for _, credential := range normalized.credentials {
		if _, exists := seenNames[credential.PasswordSecret]; !exists {
			seenNames[credential.PasswordSecret] = struct{}{}
			names = append(names, credential.PasswordSecret)
		}
	}
	slices.Sort(names)
	secrets, err := q.LockRegistryCredentialSecretsByName(ctx, db.LockRegistryCredentialSecretsByNameParams{
		EnvironmentID: pgvalue.UUID(normalized.environmentID), SecretNames: names,
	})
	if err != nil {
		return nil, err
	}
	if len(secrets) != len(names) {
		return nil, conflict(errors.New("workspace image registry secret is unavailable"))
	}
	byName := make(map[string]db.Secret, len(secrets))
	for _, secretValue := range secrets {
		if secretValue.State != "active" || !secretValue.CurrentVersionID.Valid {
			return nil, conflict(errors.New("workspace image registry secret is unavailable"))
		}
		byName[secretValue.Name] = secretValue
	}
	bindings := make([]imagebuild.RegistryBinding, 0, len(normalized.credentials))
	for _, credential := range normalized.credentials {
		secretValue, ok := byName[credential.PasswordSecret]
		if !ok {
			return nil, conflict(errors.New("workspace image registry secret is unavailable"))
		}
		resolution, err := q.CreateRegistryCredentialResolution(ctx, db.CreateRegistryCredentialResolutionParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), EnvironmentID: pgvalue.UUID(normalized.environmentID),
			DeploymentID: pgvalue.UUID(normalized.deploymentID), BuildLeaseID: pgvalue.UUID(normalized.leaseID),
			ImageOperationID: pgvalue.UUID(operationID), PlanDigest: bytes.Clone(normalized.planDigestBytes),
			RegistryAuthority: credential.Authority, Username: credential.Username,
			SecretID: secretValue.ID, SecretVersionID: secretValue.CurrentVersionID,
			RevocationGeneration: secretValue.RevocationGeneration,
		})
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, registryBindingFromResolution(resolution))
	}
	return bindings, nil
}

func replayWorkspaceImageRegistryBindings(
	ctx context.Context,
	q db.Querier,
	normalized workspaceImageAdmission,
	rows []db.RegistryCredentialResolution,
	requireDelivery bool,
) ([]imagebuild.RegistryBinding, error) {
	if len(rows) != len(normalized.credentials) {
		return nil, conflict(errors.New("workspace image credential audit is incomplete"))
	}
	bindings := make([]imagebuild.RegistryBinding, 0, len(rows))
	for index, row := range rows {
		expected := normalized.credentials[index]
		if row.RegistryAuthority != expected.Authority || row.Username != expected.Username ||
			!bytes.Equal(row.PlanDigest, normalized.planDigestBytes) {
			return nil, conflict(errors.New("workspace image credential audit conflicts with the plan"))
		}
		if requireDelivery {
			locked, err := q.LockRegistryCredentialResolutionForDelivery(
				ctx,
				db.LockRegistryCredentialResolutionForDeliveryParams{
					EnvironmentID: row.EnvironmentID, DeploymentID: row.DeploymentID,
					BuildLeaseID: row.BuildLeaseID, ImageOperationID: row.ImageOperationID,
					ResolutionID: row.ID, RegistryAuthority: row.RegistryAuthority,
					PlanDigest: bytes.Clone(normalized.planDigestBytes),
				},
			)
			if err != nil {
				return nil, err
			}
			if locked.Secret.Name != expected.PasswordSecret {
				return nil, conflict(errors.New("workspace image credential audit conflicts with the secret"))
			}
		}
		bindings = append(bindings, registryBindingFromResolution(row))
	}
	return bindings, nil
}

func registryBindingFromResolution(row db.RegistryCredentialResolution) imagebuild.RegistryBinding {
	return imagebuild.RegistryBinding{
		Authority: row.RegistryAuthority, Username: row.Username,
		ResolutionID: pgvalue.UUIDString(row.ID), SecretID: pgvalue.UUIDString(row.SecretID),
		SecretVersionID:      pgvalue.UUIDString(row.SecretVersionID),
		RevocationGeneration: row.RevocationGeneration,
	}
}

func workspaceImageTerminalResult(
	claim db.IdempotencyClaim,
	buildLeaseID uuid.UUID,
	buildLeaseGeneration int64,
	declarationSlot string,
	operationID uuid.UUID,
	requestFingerprint string,
	planDigest string,
	resolutionSetDigest string,
	requestedCacheMode imagebuild.CacheMode,
) (workerapi.WorkspaceImageTerminalResult, error) {
	var receipt workspaceImageOperationReceipt
	decoder := json.NewDecoder(bytes.NewReader(claim.Receipt))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return workerapi.WorkspaceImageTerminalResult{}, errors.New("workspace image terminal receipt is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workerapi.WorkspaceImageTerminalResult{}, errors.New("workspace image terminal receipt has trailing data")
	}
	if receipt.BuildLeaseID != buildLeaseID.String() ||
		receipt.BuildLeaseGeneration != buildLeaseGeneration ||
		receipt.DeclarationSlot != declarationSlot ||
		receipt.OperationID != operationID.String() ||
		receipt.RequestFingerprint != requestFingerprint ||
		receipt.PlanDigest != planDigest ||
		receipt.ResolutionSetDigest != resolutionSetDigest ||
		receipt.RequestedCacheMode != requestedCacheMode {
		return workerapi.WorkspaceImageTerminalResult{}, conflict(errors.New("workspace image terminal receipt conflicts with admission"))
	}
	if err := ids.Validate(receipt.AttemptID); err != nil {
		return workerapi.WorkspaceImageTerminalResult{}, errors.New("workspace image terminal attempt ID is invalid")
	}
	if err := imagebuild.ValidateGuestResult(receipt.Result); err != nil {
		return workerapi.WorkspaceImageTerminalResult{}, errors.New("workspace image terminal result is invalid")
	}
	if claim.State == "completed" && receipt.Result.Outcome != imagebuild.GuestSucceeded ||
		claim.State == "failed" && receipt.Result.Outcome != imagebuild.GuestFailed {
		return workerapi.WorkspaceImageTerminalResult{}, errors.New("workspace image terminal state does not match its result")
	}
	return workerapi.WorkspaceImageTerminalResult{
		AttemptID: receipt.AttemptID, Result: receipt.Result,
	}, nil
}

func validateCompletedWorkspaceImageOperations(
	ctx context.Context,
	q db.Querier,
	environmentID uuid.UUID,
	buildLeaseID uuid.UUID,
	buildLeaseGeneration int64,
	requestedCacheMode imagebuild.CacheMode,
	images []deployment.WorkspaceImage,
) error {
	if requestedCacheMode != imagebuild.CachePrefer && requestedCacheMode != imagebuild.CacheBypass {
		return errors.New("deployment image cache mode is invalid")
	}
	for _, image := range images {
		if image.Operation.BuildLeaseID != buildLeaseID.String() ||
			image.Operation.BuildLeaseGeneration != buildLeaseGeneration ||
			image.Operation.RequestedCacheMode != requestedCacheMode {
			return invalidDeploymentBuildOutput{err: errors.New("workspace image operation does not belong to the current build lease")}
		}
		operationID, err := ids.Parse(image.Operation.OperationID)
		if err != nil {
			return invalidDeploymentBuildOutput{err: errors.New("workspace image operation ID is invalid")}
		}
		claim, err := q.LockCompletedWorkspaceImageOperation(ctx, db.LockCompletedWorkspaceImageOperationParams{
			EnvironmentID: pgvalue.UUID(environmentID), ImageOperationID: pgvalue.UUID(operationID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return invalidDeploymentBuildOutput{err: errors.New("workspace image operation is not completed")}
		}
		if err != nil {
			return fmt.Errorf("lock completed workspace image operation: %w", err)
		}
		expectedSlot, err := idempotency.WorkspaceImageBuildSlotHash(
			environmentID,
			buildLeaseID,
			buildLeaseGeneration,
			image.Operation.DeclarationSlot,
		)
		if err != nil {
			return invalidDeploymentBuildOutput{err: fmt.Errorf("workspace image operation authority: %w", err)}
		}
		requestFingerprint, err := deployment.SHA256DigestBytes(image.Operation.RequestFingerprint)
		if err != nil || !bytes.Equal(claim.SlotHash, expectedSlot[:]) ||
			!bytes.Equal(claim.RequestFingerprint, requestFingerprint) {
			return invalidDeploymentBuildOutput{err: errors.New("workspace image operation claim does not exact-match the current build lease authority")}
		}
		terminal, err := workspaceImageTerminalResult(
			claim,
			buildLeaseID,
			buildLeaseGeneration,
			image.Operation.DeclarationSlot,
			operationID,
			image.Operation.RequestFingerprint,
			image.Operation.PlanDigest,
			image.Operation.ResolutionSetDigest,
			image.Operation.RequestedCacheMode,
		)
		if err != nil {
			return invalidDeploymentBuildOutput{err: fmt.Errorf("workspace image operation receipt: %w", err)}
		}
		if terminal.AttemptID != image.Operation.AttemptID ||
			terminal.Result.Outcome != imagebuild.GuestSucceeded ||
			terminal.Result.OCIDigest != image.Artifact.Digest ||
			terminal.Result.OCISizeBytes != image.Artifact.SizeBytes {
			return invalidDeploymentBuildOutput{err: errors.New("workspace image artifact does not exact-match its completed operation")}
		}
	}
	return nil
}

func validateWorkspaceImageAssignment(
	assignment workerapi.WorkspaceImageAssignment,
	worker workerActor,
) error {
	return imagebuild.ValidateGuestRequest(imagebuild.GuestRequest{
		Contract:    assignment.ImageBuildContract,
		OperationID: assignment.OperationID, AttemptID: uuid.Must(uuid.NewV7()).String(),
		BuildLeaseID: assignment.Lease.ID, BuildLeaseGeneration: assignment.Lease.LeaseSequence,
		WorkerEpoch: worker.WorkerEpoch, RuntimeIdentityID: assignment.RuntimeIdentityID,
		Architecture: assignment.Architecture, Plan: assignment.Plan, PlanDigest: assignment.PlanDigest,
		SubmittedSourceDigest: assignment.SubmittedSourceDigest, BuildTreeDigest: assignment.BuildTreeDigest,
		AdmittedPaths: assignment.AdmittedPaths, AdmittedPathSetDigest: assignment.AdmittedPathSetDigest,
		SourceArchiveDigest:    assignment.SourceArchiveDigest,
		SourceArchiveSizeBytes: assignment.SourceArchiveSizeBytes,
		SourceArchiveEntries:   assignment.SourceArchiveEntries,
		ResolutionSetDigest:    assignment.ResolutionSetDigest,
		RegistryBindings:       assignment.RegistryBindings, RequestedCacheMode: assignment.RequestedCacheMode,
	})
}

func (s *Server) attachWorkspaceImageCache(
	ctx context.Context,
	environmentID uuid.UUID,
	assignment *workerapi.WorkspaceImageAssignment,
) {
	if assignment.TerminalResult != nil {
		return
	}
	if assignment.RequestedCacheMode != imagebuild.CachePrefer {
		return
	}
	if s.cacheRepositories == nil {
		assignment.EffectiveCacheColdReason = workerapi.WorkspaceImageCacheUnavailable
		return
	}
	target, err := s.cacheRepositories.Target(environmentID, assignment.CacheScope)
	if err != nil || validateWorkspaceImageCacheTarget(target) != nil {
		assignment.EffectiveCacheColdReason = workerapi.WorkspaceImageCacheUnavailable
		return
	}
	for _, binding := range assignment.RegistryBindings {
		if binding.Authority == target.Authority {
			assignment.EffectiveCacheColdReason = workerapi.WorkspaceImageCacheRegistryAuthorityCollision
			return
		}
	}
	if err := s.cacheRepositories.Ensure(ctx, target); err != nil {
		assignment.EffectiveCacheColdReason = workerapi.WorkspaceImageCacheUnavailable
		return
	}
	assignment.CacheTarget = &workerapi.WorkspaceImageCacheTarget{
		Binding: imagebuild.CacheBinding{
			Authority: target.Authority, Username: target.Username, Ref: target.Ref,
		},
	}
}

func validateWorkspaceImageCacheTarget(target imagecache.Target) error {
	authority, err := imagebuild.CanonicalRegistryAuthority(target.Authority)
	if err != nil || authority != target.Authority || strings.TrimSpace(target.Username) == "" ||
		strings.TrimSpace(target.Username) != target.Username {
		return errors.New("image cache target is invalid")
	}
	if err := imagebuild.ValidateCacheReference(target.Ref); err != nil {
		return err
	}
	refAuthority, err := imagebuild.RegistryAuthority(target.Ref)
	if err != nil || refAuthority != target.Authority {
		return errors.New("image cache target ref does not match its authority")
	}
	return nil
}

func (s *Server) workerFetchWorkspaceImageCredentials(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceImageCredentialRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace image credential JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	response, err := s.fetchWorkspaceImageCredentials(r.Context(), request, worker)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = conflict(errors.New("workspace image credential authority is stale"))
		}
		writeError(w, err)
		return
	}
	defer clearWorkspaceImageCredentials(response.Envelope.RegistryCredentials)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) fetchWorkspaceImageCredentials(
	ctx context.Context,
	request workerapi.WorkspaceImageCredentialRequest,
	worker workerActor,
) (workerapi.WorkspaceImageCredentialResponse, error) {
	leaseID, environmentID, deploymentID, err := validateWorkspaceImageLease(request.Lease, worker)
	if err != nil {
		return workerapi.WorkspaceImageCredentialResponse{}, badRequest(err)
	}
	operationID, err := ids.Parse(request.OperationID)
	if err != nil {
		return workerapi.WorkspaceImageCredentialResponse{}, badRequest(errors.New("workspace image operation_id is invalid"))
	}
	if err := ids.Validate(request.AttemptID); err != nil {
		return workerapi.WorkspaceImageCredentialResponse{}, badRequest(errors.New("workspace image attempt_id is invalid"))
	}
	planDigest, err := deployment.SHA256DigestBytes(request.PlanDigest)
	if err != nil {
		return workerapi.WorkspaceImageCredentialResponse{}, badRequest(errors.New("workspace image plan_digest is invalid"))
	}
	if _, err := deployment.SHA256DigestBytes(request.ResolutionSetDigest); err != nil {
		return workerapi.WorkspaceImageCredentialResponse{}, badRequest(errors.New("workspace image resolution_set_digest is invalid"))
	}
	if s.registryCredentials == nil {
		return workerapi.WorkspaceImageCredentialResponse{}, unavailable(errors.New("registry credential opener is not configured"))
	}
	response := workerapi.WorkspaceImageCredentialResponse{Envelope: imagebuild.CredentialEnvelope{
		OperationID: request.OperationID, AttemptID: request.AttemptID,
		ResolutionSetDigest: request.ResolutionSetDigest,
		RegistryCredentials: []imagebuild.RegistryCredentialValue{},
	}}
	err = s.inTx(ctx, func(work *txWork) error {
		if _, err := lockWorkspaceImageBuildLease(ctx, work.q, request.Lease, worker); err != nil {
			return err
		}
		if _, err := work.q.LockRegistryCredentialImageOperation(ctx, db.LockRegistryCredentialImageOperationParams{
			EnvironmentID: pgvalue.UUID(environmentID), ImageOperationID: pgvalue.UUID(operationID),
		}); err != nil {
			return err
		}
		rows, err := work.q.ListRegistryCredentialResolutions(ctx, db.ListRegistryCredentialResolutionsParams{
			EnvironmentID: pgvalue.UUID(environmentID), DeploymentID: pgvalue.UUID(deploymentID),
			BuildLeaseID: pgvalue.UUID(leaseID), ImageOperationID: pgvalue.UUID(operationID),
		})
		if err != nil {
			return err
		}
		bindings := make([]imagebuild.RegistryBinding, 0, len(rows))
		for _, row := range rows {
			if !bytes.Equal(row.PlanDigest, planDigest) {
				return conflict(errors.New("workspace image credential plan does not match admission"))
			}
			bindings = append(bindings, registryBindingFromResolution(row))
		}
		if imagebuild.ResolutionSetDigest(bindings) != request.ResolutionSetDigest {
			return conflict(errors.New("workspace image credential resolution set does not match admission"))
		}
		for _, row := range rows {
			locked, err := work.q.LockRegistryCredentialResolutionForDelivery(
				ctx,
				db.LockRegistryCredentialResolutionForDeliveryParams{
					EnvironmentID: pgvalue.UUID(environmentID), DeploymentID: pgvalue.UUID(deploymentID),
					BuildLeaseID: pgvalue.UUID(leaseID), ImageOperationID: pgvalue.UUID(operationID),
					ResolutionID: row.ID, RegistryAuthority: row.RegistryAuthority,
					PlanDigest: bytes.Clone(planDigest),
				},
			)
			if err != nil {
				return err
			}
			password, err := s.registryCredentials.OpenRegistryCredential(
				environmentID, locked.Secret, locked.SecretVersion,
			)
			if err != nil {
				return conflict(errors.New("workspace image registry credential is unavailable"))
			}
			if len(password) < 1 || len(password) > imagebuild.MaxRegistryPasswordBytes {
				clear(password)
				return conflict(errors.New("workspace image registry credential size is invalid"))
			}
			response.Envelope.RegistryCredentials = append(response.Envelope.RegistryCredentials,
				imagebuild.RegistryCredentialValue{
					Authority: row.RegistryAuthority, Username: row.Username, Password: password,
				},
			)
		}
		if _, err := lockWorkspaceImageBuildLease(ctx, work.q, request.Lease, worker); err != nil {
			return err
		}
		if _, err := work.q.LockRegistryCredentialImageOperation(ctx, db.LockRegistryCredentialImageOperationParams{
			EnvironmentID: pgvalue.UUID(environmentID), ImageOperationID: pgvalue.UUID(operationID),
		}); err != nil {
			return err
		}
		return imagebuild.ValidateCredentialEnvelope(response.Envelope)
	})
	if err != nil {
		clearWorkspaceImageCredentials(response.Envelope.RegistryCredentials)
		return workerapi.WorkspaceImageCredentialResponse{}, err
	}
	return response, nil
}

func clearWorkspaceImageCredentials(credentials []imagebuild.RegistryCredentialValue) {
	for index := range credentials {
		clear(credentials[index].Password)
	}
}

func (s *Server) workerCompleteWorkspaceImage(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WorkspaceImageOperationResultRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace image result JSON: %w", err)))
		return
	}
	response, err := s.completeWorkspaceImage(r.Context(), request, workerFromContext(r.Context()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = conflict(errors.New("workspace image result authority is stale"))
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) completeWorkspaceImage(
	ctx context.Context,
	request workerapi.WorkspaceImageOperationResultRequest,
	worker workerActor,
) (workerapi.WorkspaceImageOperationResultResponse, error) {
	leaseID, environmentID, deploymentID, err := validateWorkspaceImageLease(request.Lease, worker)
	if err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(err)
	}
	operationID, err := ids.Parse(request.OperationID)
	if err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image operation_id is invalid"))
	}
	if err := ids.Validate(request.AttemptID); err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image attempt_id is invalid"))
	}
	if !sourceid.Valid(request.DeclarationSlot) {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image declaration_slot is invalid"))
	}
	if request.RequestedCacheMode != imagebuild.CachePrefer && request.RequestedCacheMode != imagebuild.CacheBypass {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image requested_cache_mode is invalid"))
	}
	requestFingerprint, err := deployment.SHA256DigestBytes(request.RequestFingerprint)
	if err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image request_fingerprint is invalid"))
	}
	planDigest, err := deployment.SHA256DigestBytes(request.PlanDigest)
	if err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image plan_digest is invalid"))
	}
	if _, err := deployment.SHA256DigestBytes(request.ResolutionSetDigest); err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(errors.New("workspace image resolution_set_digest is invalid"))
	}
	if err := imagebuild.ValidateGuestResult(request.Result); err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, badRequest(err)
	}
	response := workerapi.WorkspaceImageOperationResultResponse{
		OperationID: request.OperationID, AttemptID: request.AttemptID, Result: request.Result,
	}
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err := lockWorkspaceImageBuildLease(ctx, work.q, request.Lease, worker)
		if err != nil {
			return err
		}
		requestedCacheMode := imagebuild.CacheMode(authority.Deployment.ImageCacheMode)
		if requestedCacheMode != imagebuild.CachePrefer && requestedCacheMode != imagebuild.CacheBypass {
			return errors.New("deployment image cache mode is invalid")
		}
		if requestedCacheMode != request.RequestedCacheMode {
			return conflict(errors.New("workspace image result cache mode does not match admission"))
		}
		receiptRaw, err := json.Marshal(workspaceImageOperationReceipt{
			BuildLeaseID: request.Lease.ID, BuildLeaseGeneration: request.Lease.LeaseSequence,
			DeclarationSlot: request.DeclarationSlot,
			OperationID:     request.OperationID, AttemptID: request.AttemptID,
			RequestFingerprint: request.RequestFingerprint,
			PlanDigest:         request.PlanDigest, ResolutionSetDigest: request.ResolutionSetDigest,
			RequestedCacheMode: requestedCacheMode, Result: request.Result,
		})
		if err != nil {
			return err
		}
		claim, err := work.q.LockWorkspaceImageOperationForResult(ctx, db.LockWorkspaceImageOperationForResultParams{
			EnvironmentID: pgvalue.UUID(environmentID), ImageOperationID: pgvalue.UUID(operationID),
		})
		if err != nil {
			return err
		}
		expectedSlot, err := idempotency.WorkspaceImageBuildSlotHash(
			environmentID,
			leaseID,
			request.Lease.LeaseSequence,
			request.DeclarationSlot,
		)
		if err != nil {
			return badRequest(err)
		}
		if !bytes.Equal(claim.SlotHash, expectedSlot[:]) ||
			!bytes.Equal(claim.RequestFingerprint, requestFingerprint) {
			return conflict(errors.New("workspace image result request fingerprint does not match admission"))
		}
		rows, err := work.q.ListRegistryCredentialResolutions(ctx, db.ListRegistryCredentialResolutionsParams{
			EnvironmentID: pgvalue.UUID(environmentID), DeploymentID: pgvalue.UUID(deploymentID),
			BuildLeaseID: pgvalue.UUID(leaseID), ImageOperationID: pgvalue.UUID(operationID),
		})
		if err != nil {
			return err
		}
		bindings := make([]imagebuild.RegistryBinding, 0, len(rows))
		for _, row := range rows {
			if !bytes.Equal(row.PlanDigest, planDigest) {
				return conflict(errors.New("workspace image result plan does not match admission"))
			}
			bindings = append(bindings, registryBindingFromResolution(row))
		}
		if imagebuild.ResolutionSetDigest(bindings) != request.ResolutionSetDigest {
			return conflict(errors.New("workspace image result resolution set does not match admission"))
		}
		if claim.State != "pending" {
			same, err := sameCanonicalJSON(claim.Receipt, receiptRaw)
			if err != nil || !same {
				return conflict(errors.New("workspace image terminal result conflicts with replay"))
			}
			response.State = claim.State
			return nil
		}
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		if request.Result.Outcome == imagebuild.GuestSucceeded {
			claim, err = claims.Complete(ctx, claim, receiptRaw)
		} else {
			claim, err = claims.Fail(ctx, claim, receiptRaw)
		}
		if err != nil {
			return err
		}
		response.State = claim.State
		return nil
	})
	if err != nil {
		return workerapi.WorkspaceImageOperationResultResponse{}, err
	}
	return response, nil
}

func sameCanonicalJSON(left, right []byte) (bool, error) {
	canonicalLeft, err := jsoncanon.Transform(left)
	if err != nil {
		return false, err
	}
	canonicalRight, err := jsoncanon.Transform(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(canonicalLeft, canonicalRight), nil
}

func validateWorkspaceImageLease(
	lease workerapi.DeploymentBuildLease,
	worker workerActor,
) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	_, _, environmentID, deploymentID, err := parseDeploymentBuildLeaseIDs(lease)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, err
	}
	leaseID, err := ids.Parse(lease.ID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, errors.New("deployment build lease id must be a canonical UUIDv7")
	}
	workerInstanceID, err := ids.Parse(lease.WorkerInstanceID)
	if err != nil || workerInstanceID != worker.WorkerInstanceID || lease.WorkerEpoch != worker.WorkerEpoch ||
		lease.WorkerGroupID != worker.WorkerGroupID || lease.LeaseSequence < 1 {
		return uuid.Nil, uuid.Nil, uuid.Nil, errors.New("deployment build lease does not match the authenticated worker")
	}
	return leaseID, pgvalue.MustUUIDValue(environmentID), pgvalue.MustUUIDValue(deploymentID), nil
}

func lockWorkspaceImageBuildLease(
	ctx context.Context,
	q db.Querier,
	lease workerapi.DeploymentBuildLease,
	worker workerActor,
) (db.LockRegistryCredentialBuildLeaseRow, error) {
	leaseID, environmentID, deploymentID, err := validateWorkspaceImageLease(lease, worker)
	if err != nil {
		return db.LockRegistryCredentialBuildLeaseRow{}, err
	}
	row, err := q.LockRegistryCredentialBuildLease(ctx, db.LockRegistryCredentialBuildLeaseParams{
		EnvironmentID: pgvalue.UUID(environmentID), DeploymentID: pgvalue.UUID(deploymentID),
		BuildLeaseID: pgvalue.UUID(leaseID), BuildLeaseGeneration: lease.LeaseSequence,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
	})
	if err != nil {
		return db.LockRegistryCredentialBuildLeaseRow{}, err
	}
	orgID, _ := ids.Parse(lease.OrgID)
	projectID, _ := ids.Parse(lease.ProjectID)
	if row.Deployment.OrgID != pgvalue.UUID(orgID) || row.Deployment.ProjectID != pgvalue.UUID(projectID) ||
		row.DeploymentBuildLease.WorkerGroupID != lease.WorkerGroupID ||
		row.DeploymentBuildLease.RequestedCPUMillis != lease.RequestedCPUMillis ||
		row.DeploymentBuildLease.RequestedMemoryBytes != lease.RequestedMemoryBytes ||
		row.DeploymentBuildLease.RequestedGuestEphemeralDiskBytes != lease.RequestedGuestEphemeralDiskBytes ||
		row.DeploymentBuildLease.RequestedBuildExecutors != lease.RequestedBuildExecutors {
		return db.LockRegistryCredentialBuildLeaseRow{}, conflict(errors.New("deployment build lease authority does not match assignment"))
	}
	return row, nil
}

func workspaceImageDigest(value []byte) (string, error) {
	if len(value) != sha256.Size {
		return "", errors.New("workspace image fingerprint digest is invalid")
	}
	return sha256sum.Prefix + hex.EncodeToString(value), nil
}
