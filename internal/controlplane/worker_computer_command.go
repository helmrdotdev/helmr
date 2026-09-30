package controlplane

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
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
	claimed, err := command.Claim(r.Context(), s.tx, workerFromContext(r.Context()), command.ClaimRequest{
		OrgID: org, EnvironmentID: environment, InstanceID: instance, WriterGeneration: request.WriterGeneration,
		ActiveCommandIDs: activeCommandIDs(request.ActiveCommandIDs), ActiveCancellationIDs: activeCommandIDs(request.ActiveCancellationIDs),
	})
	if err != nil {
		writeError(w, commandError(err, commandClaimOperation))
		return
	}
	if cancellation := claimed.Cancellation; cancellation != nil {
		writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{Cancellation: &workerapi.ComputerCommandCancellation{
			CommandID: cancellation.CommandID.String(), ComputerID: cancellation.ComputerID.String(), ComputerInstanceID: cancellation.InstanceID.String(),
			WriterGeneration: cancellation.WriterGeneration, RequestFingerprint: hex.EncodeToString(cancellation.RequestFingerprint), ExpiresAt: cancellation.ExpiresAt,
		}})
		return
	}
	if release := claimed.Release; release != nil {
		writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{Release: &workerapi.ComputerCommandRelease{
			ComputerID: release.ComputerID.String(), RequestFingerprint: hex.EncodeToString(release.RequestFingerprint), Completion: completionRequest(release.Completion),
		}})
		return
	}
	if claimed.Start == nil {
		writeJSON(w, http.StatusOK, workerapi.ComputerCommandClaimResponse{})
		return
	}
	authority := claimed.Start
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
	protected, err := workerProtectedEnv(r.Context(), s.db, authority.Command.EnvironmentID, authority.Command.ComputerID)
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
		CommandID: pgvalue.UUIDString(authority.Command.ID), ComputerID: pgvalue.UUIDString(authority.Command.ComputerID), ComputerInstanceID: pgvalue.UUIDString(authority.Instance.ID), RequestFingerprint: hex.EncodeToString(authority.RequestFingerprint), Request: launchRequest, Stdin: stdin, Secrets: deliveries, ProtectedEnv: protected, WriterGeneration: authority.Instance.WriterGeneration, ExpiresAt: authority.Instance.WriterExpiresAt.Time,
	}})
}

// activeCommandIDs are the canonical Command IDs a worker host reports it
// already runs or cancels; any other value names no Command and is skipped.
func activeCommandIDs(values []string) []uuid.UUID {
	result := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		if id, err := uuid.Parse(value); err == nil && id.String() == value {
			result = append(result, id)
		}
	}
	return result
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
	s.workerCommandCompletion(w, r, command.Complete)
}
func (s *Server) workerReconcileComputerCommand(w http.ResponseWriter, r *http.Request) {
	s.workerCommandCompletion(w, r, command.Reconcile)
}
func (s *Server) workerCommandCompletion(w http.ResponseWriter, r *http.Request, settle func(context.Context, db.TxBeginner, workergroup.HostPrincipal, command.CompletionReport) error) {
	var request workerapi.ComputerCommandCompleteRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	report, err := completionReport(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = settle(r.Context(), s.tx, workerFromContext(r.Context()), report); err != nil {
		writeError(w, commandError(err, commandCompletionOperation))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// completionReport reads the canonical identities of a completion request;
// the command owner validates the reported result.
func completionReport(request workerapi.ComputerCommandCompleteRequest) (command.CompletionReport, error) {
	org, e1 := ids.Parse(request.OrgID)
	commandID, e2 := ids.Parse(request.CommandID)
	instance, e3 := ids.Parse(request.ComputerInstanceID)
	if e1 != nil || e2 != nil || e3 != nil || request.WriterGeneration <= 0 {
		return command.CompletionReport{}, errors.New("canonical command, organization and instance IDs and a positive writer generation are required")
	}
	return command.CompletionReport{
		OrgID: org, CommandID: commandID, InstanceID: instance, WriterGeneration: request.WriterGeneration,
		Outcome: request.Outcome, ExitCode: request.ExitCode, Error: request.Error,
	}, nil
}

// completionRequest is the wire form of a recorded completion.
func completionRequest(report command.CompletionReport) workerapi.ComputerCommandCompleteRequest {
	return workerapi.ComputerCommandCompleteRequest{
		OrgID: report.OrgID.String(), CommandID: report.CommandID.String(), ComputerInstanceID: report.InstanceID.String(),
		WriterGeneration: report.WriterGeneration, Outcome: report.Outcome, ExitCode: report.ExitCode, Error: report.Error,
	}
}
