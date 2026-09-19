package idempotency

import (
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const operationTurnInterrupt operation = "turn.interrupt"
const operationTurnOutput operation = "turn.output.write"

type TurnProducer struct {
	MessageDeliveryID uuid.UUID `json:"message_delivery_id,omitempty"`
	TurnID            uuid.UUID `json:"turnId"`
	RunID             uuid.UUID `json:"runId"`
	AttemptNumber     int32     `json:"attemptNumber"`
	RunGeneration     int64     `json:"runGeneration"`
}

func NewTurnInterruptRequest(environmentID, sessionID, turnID uuid.UUID, key string) (Request, error) {
	if turnID == uuid.Nil() {
		return nil, errors.New("turn ID is required")
	}
	return newTurnRequest(environmentID, sessionID, key, operationTurnInterrupt, struct {
		TurnID uuid.UUID `json:"turnId"`
	}{turnID})
}

func NewTurnOutputRequest(environmentID, sessionID uuid.UUID, key string, producer TurnProducer, data json.RawMessage) (Request, error) {
	if producer.TurnID == uuid.Nil() || producer.RunID == uuid.Nil() || producer.AttemptNumber <= 0 || producer.RunGeneration <= 0 {
		return nil, errors.New("turn producer is required")
	}
	return newTurnRequest(environmentID, sessionID, key, operationTurnOutput, struct {
		Producer TurnProducer    `json:"producer"`
		Data     json.RawMessage `json:"data"`
	}{producer, data})
}

func newTurnRequest(environmentID, sessionID uuid.UUID, key string, op operation, value any) (Request, error) {
	if environmentID == uuid.Nil() || sessionID == uuid.Nil() {
		return nil, errors.New("Session scope is required")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, err
	}
	return sealedRequest{value: request{environmentID: environmentID, operation: op, scope: sessionID[:], key: key, fingerprint: func() ([32]byte, error) { return operationFingerprint(op, canonical), nil }}}, nil
}

// Session operations share Session-before-claim lock order. The route is part of
// the namespace, and the fingerprint fixes its payload and any exact target.
func NewSessionOperationRequest(environmentID, sessionID uuid.UUID, key, name string, value any) (Request, error) {
	switch name {
	case "session.send", "session.enqueue", "turn.message", "session.close", "session.resume", "session.recover", "session.output.write", "session.run.cancel":
		return newTurnRequest(environmentID, sessionID, key, operation(name), value)
	default:
		return nil, errors.New("unknown Session operation")
	}
}
