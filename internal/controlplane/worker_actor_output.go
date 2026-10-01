package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type parsedWorkerActorOutputAppend struct {
	turnID            uuid.UUID
	generation        int64
	messageDeliveryID uuid.UUID
	lease             parsedRunLeaseFence
	correlationID     uuid.UUID
	data              json.RawMessage
	idempotencyKey    string
}

func (s *Server) workerWriteTurnOutput(w http.ResponseWriter, r *http.Request) {
	var request workerapi.WriteTurnOutputRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid actor output append JSON: %w", err))
		return
	}
	parsed, err := parseWorkerActorOutputAppend(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	output, err := session.AppendTurnOutputFromRun(r.Context(), s.tx, s.db, workerExecutionFence(worker, parsed.lease, request.Lease), session.TurnOutput{
		TurnID: parsed.turnID, RunGeneration: parsed.generation, MessageDeliveryID: parsed.messageDeliveryID,
		CorrelationID: parsed.correlationID, IdempotencyKey: parsed.idempotencyKey, Data: parsed.data,
	})
	if err != nil {
		if failure, ok := sessionWorkerFailure(err); ok {
			writeJSON(w, http.StatusOK, workerapi.WriteOutputResponse{
				CorrelationID: request.CorrelationID,
				Failed:        &failure,
			})
			return
		}
		mapped := sessionError(err, sessionWorkerOutputOperation)
		if errorStatus(mapped) == http.StatusInternalServerError {
			s.log.Error("append Actor output", "run_lease_id", request.Lease.ID, "error", err)
		}
		writeError(w, mapped)
		return
	}
	record := projectWorkerSessionEvent(output.Event(), output.DeploymentID())
	writeJSON(w, http.StatusOK, workerapi.WriteOutputResponse{
		CorrelationID: request.CorrelationID,
		Completed:     &record,
	})
}

func parseWorkerActorOutputAppend(
	request workerapi.WriteTurnOutputRequest,
) (parsedWorkerActorOutputAppend, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	if request.RunGeneration <= 0 {
		return parsedWorkerActorOutputAppend{}, errors.New("run_generation must be positive")
	}
	var delivery uuid.UUID
	if request.MessageDeliveryID != nil {
		delivery, err = parseCanonicalUUID("message_delivery_id", *request.MessageDeliveryID)
		if err != nil {
			return parsedWorkerActorOutputAppend{}, err
		}
	}
	canonical, err := canonicalJSON(request.Data)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, errors.New("data must be valid JSON")
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return parsedWorkerActorOutputAppend{}, err
	}
	return parsedWorkerActorOutputAppend{
		lease:  lease,
		turnID: turnID, generation: request.RunGeneration,
		messageDeliveryID: delivery,
		correlationID:     correlationID,
		data:              canonical,
		idempotencyKey:    idempotencyKey,
	}, nil
}
