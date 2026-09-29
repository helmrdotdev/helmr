package controlplane

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerClaimComputerCommand(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerCommandClaimRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer exec claim JSON: %w", err))
		return
	}
	org, err := ids.Parse(request.OrgID)
	environment, e2 := ids.Parse(request.EnvironmentID)
	instance, e3 := ids.Parse(request.ComputerInstanceID)
	if err != nil || e2 != nil || e3 != nil || request.WriterGeneration <= 0 {
		writeError(w, badRequest(errors.New("canonical organization, environment, Instance and positive writer generation are required")))
		return
	}
	worker := workerFromContext(r.Context())
	var authority commandClaimAuthority
	var fingerprint []byte
	var release *workerapi.ComputerCommandRelease
	var cancellation *workerapi.ComputerCommandCancellation
	err = s.inTx(r.Context(), func(work *txWork) error {
		i, err := work.q.GetComputerInstance(r.Context(), db.GetComputerInstanceParams{ID: pgvalue.UUID(instance), EnvironmentID: pgvalue.UUID(environment)})
		if err != nil {
			return err
		}
		if i.OrgID != pgvalue.UUID(org) || i.WorkerHostID != pgvalue.UUID(worker.WorkerHostID) || i.WorkerGroupID != pgvalue.UUID(worker.WorkerGroupID) || i.WorkerEpoch != worker.WorkerEpoch || i.WriterGeneration != request.WriterGeneration {
			return pgx.ErrNoRows
		}
		commands, err := work.q.ListInstanceCommands(r.Context(), db.ListInstanceCommandsParams{ComputerInstanceID: i.ID, WriterGeneration: pgtype.Int8{Int64: request.WriterGeneration, Valid: true}})
		if err != nil {
			return err
		}
		for _, candidate := range commands {
			if candidate.Status == "stopping" && !slices.Contains(request.ActiveCancellationIDs, pgvalue.UUIDString(candidate.ID)) {
				receipt, err := work.q.GetIdempotencyClaim(r.Context(), db.GetIdempotencyClaimParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ClaimID})
				if err != nil {
					return err
				}
				grant, err := claimCommandCancellation(r.Context(), work.tx, worker, commandClaim{OrgID: pgvalue.UUID(org), CommandID: candidate.ID, ComputerInstanceID: i.ID, WriterGeneration: request.WriterGeneration})
				if err != nil {
					return err
				}
				cancellation = &workerapi.ComputerCommandCancellation{CommandID: pgvalue.UUIDString(candidate.ID), ComputerID: pgvalue.UUIDString(candidate.ComputerID), ComputerInstanceID: pgvalue.UUIDString(i.ID), WriterGeneration: grant.Instance.WriterGeneration, RequestFingerprint: hex.EncodeToString(receipt.RequestFingerprint), ExpiresAt: grant.Instance.WriterExpiresAt.Time}
				return nil
			}

			if slices.Contains(request.ActiveCommandIDs, pgvalue.UUIDString(candidate.ID)) {
				continue
			}
			if candidate.TerminalAt.Valid && !candidate.ProcessReconciledAt.Valid {
				receipt, err := work.q.GetIdempotencyClaim(r.Context(), db.GetIdempotencyClaimParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ClaimID})
				if err != nil {
					return err
				}
				release, err = commandRelease(candidate, request.OrgID, hex.EncodeToString(receipt.RequestFingerprint))
				if err != nil {
					return err
				}
				if release != nil {
					return completeCommand(r.Context(), work.tx, worker, release.Completion)
				}
			}
			if candidate.Status != "starting" && candidate.Status != "running" {
				continue
			}
			claim, err := work.q.GetIdempotencyClaim(r.Context(), db.GetIdempotencyClaimParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ClaimID})
			if err != nil {
				return err
			}
			authority, err = claimCommand(r.Context(), work.tx, worker, commandClaim{OrgID: pgvalue.UUID(org), CommandID: candidate.ID, ComputerInstanceID: i.ID, WriterGeneration: request.WriterGeneration})
			if err != nil {
				return err
			}

			fingerprint = claim.RequestFingerprint
			return nil
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("command claim is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("claim computer command"))
		return
	}
	if cancellation != nil {
		writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{Cancellation: cancellation})
		return
	}
	if release != nil {
		writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{Release: release})
		return
	}
	if !authority.Command.ID.Valid {
		writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{})
		return
	}
	stdin := bytes.Clone(authority.Command.Stdin)
	if len(stdin) > computerCommandStdinMaxBytes {
		clear(stdin)
		writeError(w, errors.New("command stdin exceeds its persisted limit"))
		return
	}
	defer clearComputerCommandBytes(stdin)
	environmentID, err := pgvalue.UUIDValue(authority.Command.EnvironmentID)
	if err != nil {
		writeError(w, errors.New("computer exec environment is invalid"))
		return
	}
	materials, err := s.secretDelivery.OpenDeliveries(environmentID, authority.Secrets)
	if err != nil {
		writeError(w, errors.New("open computer exec secrets"))
		return
	}
	defer clearComputerCommandMaterials(materials)
	deliveries, err := projectSecretDeliveries(materials)
	if err != nil {
		writeError(w, errors.New("project computer exec secrets"))
		return
	}
	defer clearComputerSecretDeliveries(deliveries)
	protected, err := computerProtectedEnv(r.Context(), s.db, authority.Command.EnvironmentID, authority.Command.ComputerID)
	if err != nil {
		writeError(w, conflict(secret.ErrDeliveryUnavailable))
		return
	}
	var execEnv map[string]string
	if err := json.Unmarshal(authority.Command.Env, &execEnv); err != nil {
		writeError(w, errors.New("decode computer exec environment"))
		return
	}
	launchRequest, err := json.Marshal(computerCommandSpec{
		Command:   authority.Command.Argv,
		Cwd:       authority.Command.Cwd.String,
		Env:       execEnv,
		TimeoutMS: authority.Command.TimeoutMs,
	})
	if err != nil {
		writeError(w, errors.New("encode computer exec launch"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{Command: &workerapi.ComputerCommand{
		CommandID: pgvalue.UUIDString(authority.Command.ID), ComputerID: pgvalue.UUIDString(authority.Command.ComputerID), ComputerInstanceID: pgvalue.UUIDString(authority.Instance.ID), RequestFingerprint: hex.EncodeToString(fingerprint), Request: launchRequest, Stdin: stdin, Secrets: deliveries, ProtectedEnv: protected, WriterGeneration: authority.Instance.WriterGeneration, ExpiresAt: authority.Instance.WriterExpiresAt.Time,
	}})
}

func clearComputerSecretDeliveries(deliveries []workerapi.SecretDelivery) {
	for index := range deliveries {
		clearComputerCommandBytes(deliveries[index].Value)
	}
}

func clearComputerCommandMaterials(materials []secret.DeliveryMaterial) {
	for index := range materials {
		clearComputerCommandBytes(materials[index].Value)
	}
}

func clearComputerCommandBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (s *Server) workerCompleteComputerCommand(w http.ResponseWriter, r *http.Request) {
	s.workerCommandCompletion(w, r, false)
}
func (s *Server) workerReconcileComputerCommand(w http.ResponseWriter, r *http.Request) {
	s.workerCommandCompletion(w, r, true)
}
func (s *Server) workerCommandCompletion(w http.ResponseWriter, r *http.Request, reconcile bool) {
	var request workerapi.ComputerCommandCompleteRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if _, err := parseCommandCompletion(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	err := s.inTx(r.Context(), func(work *txWork) error {
		return applyCommandCompletion(r.Context(), work.tx, workerFromContext(r.Context()), request, reconcile)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("command completion is stale or differs from its receipt")))
		return
	}
	if err != nil {
		writeError(w, errors.New("complete computer command"))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
