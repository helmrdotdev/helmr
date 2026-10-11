package guestd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

var errTurnMessageClosed = errors.New("turn message admission is closed")

var errSessionReconstructionRequired = errors.New("session runtime failure requires process reconstruction")

type agentTurnProof struct {
	sequence   int64
	settled    bool
	closed     bool
	scopes     []nativeScopeEvidence
	evidence   string
	generation int64
}

type agentPendingOperation struct {
	mcpResult  chan *agentv1.OperationResult
	evidence   string
	operation  *agentv1.Operation
	generation int64
	leaseEpoch int64
}

// The guest owns these facts independently of authored JSON. Its local pipe and
// outstanding operation identities survive replacement of a host attachment.
type agentSession struct {
	logs            [2]*diagnosticBuffer
	checkpointID    string
	checkpointReady *agentv1.SessionCheckpointReady

	startSent           bool
	controlSequence     int64
	pendingResume       string
	terminalSequence    int64
	mu                  sync.Mutex
	entry               *computerMountEntry
	grant               *agentv1.SessionGrant
	start               *agentv1.SessionStart
	process             agentSessionProcess
	clock               func() time.Time
	ready               bool
	held                bool
	reconstructRequired bool
	terminal            bool
	physicalClosed      bool
	activeTurn          string
	turns               map[string]*agentTurnProof
	pending             map[string]*agentPendingOperation
	deliveries          map[string]*agentv1.GuestCommand
}

func newAgentSession(entry *computerMountEntry, grant *agentv1.SessionGrant, start *agentv1.SessionStart, process agentSessionProcess, clock func() time.Time) (*agentSession, error) {
	if clock == nil {
		clock = entry.authorityNow
	}
	if err := validateSessionGrant(entry, grant, clock()); err != nil {
		return nil, err
	}
	if start == nil || start.GetComputerId() != grant.GetComputerId() || start.GetAgentId() == "" || start.GetDeploymentId() == "" ||
		(start.GetRecoveryKind() != agentv1.SessionStart_RECOVERY_KIND_INITIAL && start.GetRecoveryKind() != agentv1.SessionStart_RECOVERY_KIND_RECONSTRUCTED) || process == nil {
		return nil, errors.New("session start does not match its Computer grant")
	}
	limits := start.GetLogLimits()
	if limits.GetChunkBytes() > maxAgentFrameBytes {
		return nil, errors.New("session log chunk exceeds transport bound")
	}
	var logs [2]*diagnosticBuffer
	for i := range logs {
		var err error
		logs[i], err = newDiagnosticBuffer(diagnosticLimits{ChunkBytes: int(limits.GetChunkBytes()), BufferBytes: limits.GetBufferBytes(), BufferRecords: int(limits.GetBufferRecords())})
		if err != nil {
			return nil, err
		}
	}
	return &agentSession{logs: logs, entry: entry, grant: proto.Clone(grant).(*agentv1.SessionGrant), start: proto.Clone(start).(*agentv1.SessionStart), process: process, clock: clock,
		terminalSequence: start.GetTerminalSequence(), turns: make(map[string]*agentTurnProof), pending: make(map[string]*agentPendingOperation), deliveries: make(map[string]*agentv1.GuestCommand)}, nil
}

func (session *agentSession) authorizeLocked() error {
	if session.terminal {
		return errors.New("session process is terminal")
	}
	return validateSessionGrant(session.entry, session.grant, session.clock())
}

func (session *agentSession) renew(grant *agentv1.SessionGrant) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if !sameSessionGrantOwner(session.grant, grant) || grant.GetAuthorityGeneration() < session.grant.GetAuthorityGeneration() || grant.GetExpiresAtUnixNano() < session.grant.GetExpiresAtUnixNano() {
		return errors.New("session renewal changed or regressed its owner")
	}
	if err := validateSessionGrant(session.entry, grant, session.clock()); err != nil {
		return err
	}
	session.grant = proto.Clone(grant).(*agentv1.SessionGrant)
	return nil
}

func (session *agentSession) send(command *agentv1.GuestCommand) error {
	write, err := session.prepareCommand(command)
	if err != nil || write == nil {
		return err
	}
	return write()
}

// Admit and snapshot authority before blocking on the local pipe. Renewal,
// physical expiry and transport acknowledgments never wait on that write.
func (session *agentSession) prepareCommand(command *agentv1.GuestCommand) (func() error, error) {
	if proto.Size(command) > maxAgentFrameBytes {
		return nil, errors.New("session command exceeds local frame bound")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	send, err := session.admitCommandLocked(command)
	if err != nil || !send {
		return nil, err
	}
	return func() error {
		// Once admitted, the frame may remain in the local pipe through a
		// physical freeze. Closing the process interrupts a blocked writer.
		if err := session.process.write(context.Background(), command); err != nil {
			return &agentPipeWriteError{err}
		}
		return nil
	}, nil
}

func (session *agentSession) admitCommandLocked(command *agentv1.GuestCommand) (bool, error) {
	if session.checkpointID != "" {
		return false, errors.New("session capture has sealed command admission")
	}

	if err := validateSessionGrant(session.entry, session.grant, session.clock()); err != nil {
		return false, err
	}
	if !proto.Equal(command.GetIdentity(), session.grant.GetIdentity()) {
		return false, errors.New("session command identity changed")
	}
	if command.GetOperationResult() != nil {
		return session.respondLocked(command)
	}
	if session.terminal {
		return false, errors.New("session process is terminal")
	}
	if command.GetStart() != nil {
		if !proto.Equal(command.GetStart(), session.start) {
			return false, errors.New("session setup cannot be restarted")
		}
		if session.startSent {
			return false, nil
		}
		session.startSent = true
		return true, nil
	}
	if command.GetDeliveryId() == "" {
		return false, errors.New("session delivery identity is required")
	}
	if prior := session.deliveries[command.GetDeliveryId()]; prior != nil {
		if !proto.Equal(prior, command) {
			return false, errors.New("session delivery identity reused with different content")
		}
		// The original local write is still owned here. Its receipt is retained
		// in the relay outbox until host acknowledgment and replayed on attachment.
		// Do not create another local receipt for the same admitted delivery.
		return false, nil
	}
	control := command.GetSuspend() != nil || command.GetInterrupt() != nil || command.GetResume() != nil
	if control && command.GetControlSequence() <= 0 {
		return false, errors.New("control sequence is required")
	}
	staleControl := control && command.GetControlSequence() <= session.controlSequence
	if control && !staleControl {
		session.controlSequence = command.GetControlSequence()
	}
	switch payload := command.GetCommand().(type) {
	case *agentv1.GuestCommand_Dispatch:
		if !session.ready || session.held || (session.activeTurn != "" && session.activeTurn != payload.Dispatch.GetTurnId()) || payload.Dispatch.GetTurnId() == "" || payload.Dispatch.GetSequence() <= session.terminalSequence {
			return false, errors.New("session cannot dispatch this Turn")
		}
		session.activeTurn = payload.Dispatch.GetTurnId()
		if session.turns[session.activeTurn] == nil {
			session.turns[session.activeTurn] = &agentTurnProof{sequence: payload.Dispatch.GetSequence()}
		}
	case *agentv1.GuestCommand_Message:
		turn := session.turns[payload.Message.GetTurnId()]
		if turn == nil || turn.closed || session.held || payload.Message.GetTurnId() != session.activeTurn {
			session.deliveries[command.GetDeliveryId()] = proto.Clone(command).(*agentv1.GuestCommand)
			return false, errTurnMessageClosed
		}
	case *agentv1.GuestCommand_Acknowledged:
		if payload.Acknowledged.GetTurnId() == "" || payload.Acknowledged.GetSequence() <= 0 {
			return false, errors.New("terminal acknowledgment identity is required")
		}
		if payload.Acknowledged.GetSequence() <= session.terminalSequence {
			break
		}
		turn := session.turns[payload.Acknowledged.GetTurnId()]
		if turn == nil || !turn.settled || turn.sequence != payload.Acknowledged.GetSequence() || payload.Acknowledged.GetTurnId() == session.activeTurn {
			return false, errors.New("active Turn cannot be retired")
		}
	case *agentv1.GuestCommand_Suspend, *agentv1.GuestCommand_Interrupt:
		if !staleControl {
			session.holdLocked()
		}
	case *agentv1.GuestCommand_Resume:
		if session.reconstructRequired {
			return false, errSessionReconstructionRequired
		}
		if !staleControl {
			session.pendingResume = command.GetDeliveryId()
		}
		// Admission remains held until the local runtime confirms this exact
		// resume. A later suspend or runtime failure supersedes its receipt.
	case *agentv1.GuestCommand_Shutdown:
		session.holdLocked()
		session.reconstructRequired = true
	case *agentv1.GuestCommand_Reconcile:
		if session.turns[payload.Reconcile.GetTurnId()] == nil {
			return false, errors.New("unknown Turn finalization")
		}
	default:
		return false, errors.New("unsupported Session command")
	}
	session.deliveries[command.GetDeliveryId()] = proto.Clone(command).(*agentv1.GuestCommand)
	return true, nil
}

func (session *agentSession) respondLocked(command *agentv1.GuestCommand) (bool, error) {
	result := command.GetOperationResult()
	pending := session.pending[result.GetRequestId()]
	if pending == nil {
		return false, nil
	} // A repeated host receipt has already reached the same local pipe.
	if result.GetOutcome() == nil {
		return false, errors.New("operation result has no outcome")
	}
	if session.terminal || (pending.operation.GetTurnId() != "" && session.turns[pending.operation.GetTurnId()] == nil) {
		delete(session.pending, result.GetRequestId())
		return false, nil
	}
	if pending.operation.GetMethod() == agentv1.Operation_METHOD_CLOSE_PROCESSING && result.GetError() == nil {
		turn := session.turns[pending.operation.GetTurnId()]
		if turn == nil {
			return false, errors.New("processing receipt belongs to an unknown Turn")
		}
		turn.closed = true
	}
	if (pending.operation.GetMethod() == agentv1.Operation_METHOD_FINALIZE || pending.operation.GetMethod() == agentv1.Operation_METHOD_FAIL) && result.GetError() == nil {
		var outcome struct {
			Status   string          `json:"status"`
			Result   json.RawMessage `json:"result"`
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
		}
		if err := decodeAgentJSON(result.GetValueJson(), &outcome); err != nil {
			return false, err
		}
		if outcome.Status != "completed" && outcome.Status != "failed" && outcome.Status != "interrupted" && outcome.Status != "cancelled" {
			return false, errors.New("invalid terminal operation receipt")
		}
		session.turns[pending.operation.GetTurnId()].settled = true
	}
	delete(session.pending, result.GetRequestId())
	if pending.mcpResult != nil {
		pending.mcpResult <- proto.Clone(result).(*agentv1.OperationResult)
		return false, nil
	}
	return true, nil
}

func (session *agentSession) receive(event *agentv1.ProgramEvent) (*agentv1.GuestSessionMessage, *agentv1.GuestCommand, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.authorizeLocked(); err != nil {
		return nil, nil, err
	}
	if !proto.Equal(event.GetIdentity(), session.grant.GetIdentity()) {
		return nil, nil, errors.New("authored event belongs to another Session")
	}
	envelope := &agentv1.GuestSessionMessage{Identity: proto.Clone(session.grant.GetIdentity()).(*agentv1.SessionIdentity), Message: &agentv1.GuestSessionMessage_Event{Event: event}}
	switch value := event.GetEvent().(type) {
	case *agentv1.ProgramEvent_CheckpointReady:
		// A timed-out probe can finish after a coordinated source abort. It has
		// no authority to change the current process or another capture.
		if session.checkpointID == "" || value.CheckpointReady.GetCheckpointId() != session.checkpointID {
			return nil, nil, nil
		}
		if session.checkpointReady != nil && proto.Equal(session.checkpointReady, value.CheckpointReady) {
			return nil, nil, nil
		}
		if session.checkpointReady != nil || value.CheckpointReady.GetOutcome() == nil {
			return nil, nil, errors.New("unexpected Session checkpoint response")
		}
		session.checkpointReady = proto.Clone(value.CheckpointReady).(*agentv1.SessionCheckpointReady)
		return nil, nil, nil
	case *agentv1.ProgramEvent_Ready:
		if session.ready {
			return nil, nil, errors.New("session reported ready more than once")
		}
		session.ready = true
	case *agentv1.ProgramEvent_Failed:
		session.holdLocked()
		session.reconstructRequired = true
	case *agentv1.ProgramEvent_DeliveryResult:
		if value.DeliveryResult.GetOutcome() == nil {
			return nil, nil, errors.New("session delivery receipt has no outcome")
		}
		delivery := session.deliveries[value.DeliveryResult.GetDeliveryId()]
		if delivery == nil {
			return nil, nil, errors.New("unknown Session delivery receipt")
		}
		// The runtime returns null only when it applied Resume. Superseded
		// controls return a successful object without changing runtime state.
		if delivery.GetResume() != nil && bytes.Equal(bytes.TrimSpace(value.DeliveryResult.GetValueJson()), []byte("null")) && session.pendingResume == value.DeliveryResult.GetDeliveryId() {
			session.held = false
		}
		if session.pendingResume == value.DeliveryResult.GetDeliveryId() {
			session.pendingResume = ""
		}
		turnID := delivery.GetDispatch().GetTurnId()
		if delivery.GetReconcile() != nil {
			turnID = delivery.GetReconcile().GetTurnId()
		}
		if turnID != "" && value.DeliveryResult.GetError() == nil {
			turn := session.turns[turnID]
			if turn == nil || !turn.settled {
				return nil, nil, errors.New("dispatch completed without an acknowledged terminal mutation")
			}
			if session.activeTurn == turnID {
				session.activeTurn = ""
			}
		}
		if delivery.GetAcknowledged() != nil && value.DeliveryResult.GetError() == nil {
			session.terminalSequence = max(session.terminalSequence, delivery.GetAcknowledged().GetSequence())
			retired := delivery.GetAcknowledged().GetTurnId()
			delete(session.turns, retired)
			for id, pending := range session.pending {
				if pending.operation.GetTurnId() == retired {
					delete(session.pending, id)
				}
			}
			for id, command := range session.deliveries {
				if command.GetDispatch().GetTurnId() == retired || command.GetMessage().GetTurnId() == retired || command.GetReconcile().GetTurnId() == retired || command.GetAcknowledged().GetTurnId() == retired {
					delete(session.deliveries, id)
				}
			}
		}
		if delivery.GetSuspend() != nil || delivery.GetResume() != nil || delivery.GetInterrupt() != nil || delivery.GetShutdown() != nil {
			delete(session.deliveries, value.DeliveryResult.GetDeliveryId())
		}
	case *agentv1.ProgramEvent_CancelOperation:
		pending := session.pending[value.CancelOperation.GetRequestId()]
		if pending == nil {
			return nil, nil, nil
		}
		envelope.OperationAuthorityGeneration, envelope.OperationLeaseEpoch = pending.generation, pending.leaseEpoch
	case *agentv1.ProgramEvent_Operation:
		operation := value.Operation
		if operation.GetRequestId() == "" {
			return nil, nil, errors.New("operation request identity is required")
		}
		if pending := session.pending[operation.GetRequestId()]; pending != nil {
			if !proto.Equal(pending.operation, operation) {
				return nil, nil, errors.New("operation request identity reused with different content")
			}
			envelope.OperationAuthorityGeneration, envelope.OperationLeaseEpoch = pending.generation, pending.leaseEpoch
			envelope.DrainEvidence = pending.evidence
			return envelope, nil, nil
		}
		pendingBytes := proto.Size(operation)
		for _, pending := range session.pending {
			pendingBytes += proto.Size(pending.operation)
		}
		if len(session.pending) >= 256 || pendingBytes > 32*1024*1024 {
			return nil, session.operationError(operation.GetRequestId(), errors.New("session pending operation limit exceeded")), nil
		}
		reply, evidence, err := session.admitOperationLocked(operation)
		if err != nil {
			return nil, session.operationError(operation.GetRequestId(), err), nil
		}
		if reply != nil {
			return nil, reply, nil
		}
		session.pending[operation.GetRequestId()] = &agentPendingOperation{evidence: evidence, operation: proto.Clone(operation).(*agentv1.Operation), generation: session.grant.GetAuthorityGeneration(), leaseEpoch: session.grant.GetComputerLeaseEpoch()}
		envelope.DrainEvidence = evidence
		envelope.OperationAuthorityGeneration, envelope.OperationLeaseEpoch = session.grant.GetAuthorityGeneration(), session.grant.GetComputerLeaseEpoch()
	default:
		return nil, nil, errors.New("empty Session event")
	}
	return envelope, nil, nil
}

func (session *agentSession) operationError(requestID string, err error) *agentv1.GuestCommand {
	return &agentv1.GuestCommand{Identity: proto.Clone(session.grant.GetIdentity()).(*agentv1.SessionIdentity), Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: requestID, Outcome: &agentv1.OperationResult_Error{Error: &agentv1.OperationError{Code: "guest_rejected", Message: err.Error()}}}}}
}

func (session *agentSession) admitOperationLocked(operation *agentv1.Operation) (*agentv1.GuestCommand, string, error) {
	method := operation.GetMethod()
	if session.checkpointID != "" {
		return nil, "", errors.New("session capture has sealed operation admission")
	}
	if method == agentv1.Operation_METHOD_HOLD {
		session.holdLocked()
		session.reconstructRequired = true
		return nil, "", nil
	}
	turn := session.turns[operation.GetTurnId()]
	if turn == nil || turn.settled || session.activeTurn != operation.GetTurnId() {
		return nil, "", errors.New("operation has no current owned Turn")
	}
	switch method {
	case agentv1.Operation_METHOD_CONVERGE_NATIVE:
		var claim struct {
			Disposition string                `json:"disposition"`
			Scopes      []nativeScopeEvidence `json:"scopes"`
		}
		if err := decodeAgentJSON(operation.GetPayloadJson(), &claim); err != nil {
			return nil, "", err
		}
		if claim.Disposition != "returned" && claim.Disposition != "failed" {
			return nil, "", errors.New("invalid convergence disposition")
		}
		if err := session.process.converge(claim.Scopes); err != nil {
			return nil, "", err
		}
		turn.scopes = append([]nativeScopeEvidence(nil), claim.Scopes...)
		turn.evidence, turn.generation = rand.Text(), session.grant.GetAuthorityGeneration()
		return &agentv1.GuestCommand{Identity: proto.Clone(session.grant.GetIdentity()).(*agentv1.SessionIdentity), Command: &agentv1.GuestCommand_OperationResult{OperationResult: &agentv1.OperationResult{RequestId: operation.GetRequestId(), Outcome: &agentv1.OperationResult_ValueJson{ValueJson: []byte("null")}}}}, "", nil
	case agentv1.Operation_METHOD_FINALIZE, agentv1.Operation_METHOD_FAIL:
		if !turn.closed || turn.evidence == "" {
			return nil, "", errors.New("turn has no current closed processing and convergence evidence")
		}
		for _, pending := range session.pending {
			if pending.operation.GetTurnId() == operation.GetTurnId() && agentMutation(pending.operation.GetMethod()) {
				return nil, "", errors.New("turn has unacknowledged mutations")
			}
		}
		if err := session.process.converge(turn.scopes); err != nil {
			return nil, "", err
		}
		if turn.generation != session.grant.GetAuthorityGeneration() {
			// A retained idle claim can be attested again after renewal, but the
			// previous authority's evidence is never promoted without this check.
			turn.evidence, turn.generation = rand.Text(), session.grant.GetAuthorityGeneration()
		}
		return nil, turn.evidence, nil
	case agentv1.Operation_METHOD_RESPOND, agentv1.Operation_METHOD_REGISTER_MESSAGES, agentv1.Operation_METHOD_ASK:
		if turn.closed || session.held {
			return nil, "", errors.New("turn processing admission is closed")
		}
	case agentv1.Operation_METHOD_CLOSE_PROCESSING, agentv1.Operation_METHOD_WITHDRAW_ASK, agentv1.Operation_METHOD_OUTPUT:
		// Admitted callback/output and question cleanup can finish after closure.
	case agentv1.Operation_METHOD_WAIT_ASK:
		// Observations are independently cancellable and cannot mutate a peer Turn.
	default:
		return nil, "", fmt.Errorf("unsupported Agent operation %d", method)
	}
	if agentMutation(method) && method != agentv1.Operation_METHOD_CLOSE_PROCESSING {
		turn.evidence = ""
	}
	return nil, "", nil
}

func agentMutation(method agentv1.Operation_Method) bool {
	return method != agentv1.Operation_METHOD_WAIT_TURN && method != agentv1.Operation_METHOD_WAIT_ASK && method != agentv1.Operation_METHOD_INSPECT_SESSION && method != agentv1.Operation_METHOD_LIST_SESSIONS && method != agentv1.Operation_METHOD_CONVERGE_NATIVE
}
func decodeAgentJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("agent JSON has trailing data")
	}
	return nil
}

// Caller holds session.mu. Every new hold invalidates an outstanding Resume,
// including continuation snapshots that do not change the command sequence.
func (session *agentSession) holdLocked() {
	session.held = true
	session.pendingResume = ""
}

func (session *agentSession) close(ctx context.Context) error {
	session.mu.Lock()
	session.terminal = true
	session.holdLocked()
	session.mu.Unlock()
	if err := session.process.close(ctx); err != nil {
		session.entry.processesMu.Lock()
		session.entry.recoveryRequired = true
		session.entry.processesMu.Unlock()
		return err
	}
	session.mu.Lock()
	session.physicalClosed = true
	session.mu.Unlock()
	return nil
}

// A failed pipe write can leave a partial frame. The retained protocol cannot be
// resumed safely, so callers fence this process instead of replaying the frame.
type agentPipeWriteError struct{ err error }

func (err *agentPipeWriteError) Error() string { return err.err.Error() }
func (err *agentPipeWriteError) Unwrap() error { return err.err }

// This is the response to an already admitted local operation. It can remain in
// the retained pipe across a physical freeze; never hold authority locks while
// waiting for its reader. Lifetime cancellation or physical close interrupts it.
func (session *agentSession) writeReply(ctx context.Context, command *agentv1.GuestCommand) error {
	return session.process.write(ctx, command)
}
