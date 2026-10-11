package guestd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

type agentQueuedWrite struct {
	write func() error
	size  int64
}

const agentShutdownTimeout = 12 * time.Second // Allow the Runtime's ten-second cooperative stop to finish.

const maxAgentTransportFrameBytes = maxAgentFrameBytes + 64*1024

// A relay replaces only the upstream attachment. It retains local pipes,
// original mutation payloads and guest-generated evidence until acknowledged.
type agentRelay struct {
	mcp             *agentMCP
	mcpRequests     int
	session         *agentSession
	ctx             context.Context
	mu              sync.Mutex
	upstreamMu      sync.Mutex // Serializes upstream writes and their acknowledgments, never Session state admission.
	changed         chan struct{}
	connection      programConnection
	attachment      uint64
	cursor          uint64
	nextSequence    uint64
	sentSequence    uint64
	acknowledged    uint64
	outbox          []*agentv1.GuestSessionMessage
	expiryFrozen    bool
	captureFrozen   bool
	captureVerified bool
	shuttingDown    bool
	finishOnce      sync.Once
	logWG           sync.WaitGroup
	logChanged      chan struct{}
	logSent         [2]int64
	shutdownTimer   *time.Timer
	rootExited      bool
	exitDrainBytes  int
	exitDrainEvents int
	finished        chan struct{}
	writerOnce      sync.Once
	logsOnce        sync.Once
	writes          chan agentQueuedWrite
	writeBytes      atomic.Int64
	onClosed        func(error)
}

func newAgentRelay(ctx context.Context, session *agentSession, onClosed func(error)) *agentRelay {
	return &agentRelay{logChanged: make(chan struct{}, 1), session: session, ctx: ctx, changed: make(chan struct{}), nextSequence: 1, onClosed: onClosed, writes: make(chan agentQueuedWrite, 16), finished: make(chan struct{})}
}
func (relay *agentRelay) notifyLocked() { close(relay.changed); relay.changed = make(chan struct{}) }

func (relay *agentRelay) run() {
	go relay.sendLoop()
	go relay.expiryLoop()
	relay.startLogs()
	go relay.readLoop()
	go relay.watchProcess()
	go func() { <-relay.ctx.Done(); relay.finish(relay.ctx.Err()) }()
}

func (relay *agentRelay) startLogs() {
	relay.logsOnce.Do(func() {
		stdout, stderr := relay.session.process.logs()
		relay.logWG.Add(2)
		go func() { defer relay.logWG.Done(); relay.logLoop(stdout, agentv1.SessionLog_STREAM_STDOUT) }()
		go func() { defer relay.logWG.Done(); relay.logLoop(stderr, agentv1.SessionLog_STREAM_STDERR) }()
	})
}

func (relay *agentRelay) attach(connection programConnection, request *agentv1.SessionAttach) error {
	relay.upstreamMu.Lock()
	defer relay.upstreamMu.Unlock()
	relay.mu.Lock()
	locked := true
	defer func() {
		if locked {
			relay.mu.Unlock()
		}
	}()
	if request.GetAttachmentSequence() == 0 || request.GetAttachmentSequence() <= relay.attachment {
		return errors.New("session attachment sequence is stale")
	}
	if request.GetStart() != nil && !proto.Equal(request.GetStart(), relay.session.start) {
		return errors.New("reattachment changed the Session start")
	}
	if err := relay.session.renew(request.GetGrant()); err != nil {
		return err
	}
	if relay.connection != nil {
		_ = relay.connection.Close()
	}
	relay.connection = nil
	relay.attachment = request.GetAttachmentSequence()
	relay.cursor = 0
	relay.logSent = [2]int64{}
	relay.session.mu.Lock()
	ready, terminal, capturing := relay.session.ready, relay.session.terminal, relay.session.checkpointID != ""
	pendingTurn := relay.session.activeTurn
	var pendingSequence int64
	for id, turn := range relay.session.turns {
		if pendingSequence == 0 || turn.sequence < pendingSequence {
			pendingTurn, pendingSequence = id, turn.sequence
		}
	}
	identity := proto.Clone(relay.session.grant.GetIdentity()).(*agentv1.SessionIdentity)
	relay.session.mu.Unlock()
	message := &agentv1.GuestSessionMessage{Identity: identity, AttachmentSequence: relay.attachment, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: ready, Terminal: terminal, PendingTurnId: pendingTurn}}}
	// Make the in-flight connection visible to physical freeze/close, but keep
	// sendLoop behind upstreamMu until its attachment receipt is fully written.
	relay.connection = connection
	deadline := relay.writeDeadline()
	relay.mu.Unlock()
	locked = false
	err := connection.SetWriteDeadline(deadline)
	if err == nil {
		err = writeAgentTransportFrame(connection, message)
	}
	relay.mu.Lock()
	locked = true
	if relay.connection != connection || relay.attachment != request.GetAttachmentSequence() {
		_ = connection.Close()
		return errors.New("session attachment was superseded while writing its receipt")
	}
	if err != nil {
		_ = connection.Close()
		relay.connection = nil
		relay.notifyLocked()
		return err
	}
	// Install the current upstream transport before allowing resumed user code.
	if relay.expiryFrozen && !relay.captureFrozen && !capturing && !terminal {
		ctx, cancel := context.WithTimeout(relay.ctx, 10*time.Second)
		err := relay.session.process.thaw(ctx)
		cancel()
		if err != nil {
			_ = connection.Close()
			relay.connection = nil
			return err
		}
		relay.expiryFrozen = false
	}
	relay.notifyLocked()
	return nil
}

func (relay *agentRelay) serve(connection programConnection, sequence uint64) error {
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	relay.writerOnce.Do(func() { go relay.writeLoop() })
	defer func() {
		relay.mu.Lock()
		if relay.connection == connection {
			relay.connection = nil
			relay.notifyLocked()
		}
		relay.mu.Unlock()
		_ = connection.Close()
	}()
	for {
		message := new(agentv1.HostSessionMessage)
		if err := frameio.ReadProtoFrameBounded(connection, maxAgentTransportFrameBytes, message); err != nil {
			return err
		}
		if err := relay.handleHostMessage(connection, sequence, message); err != nil {
			return err
		}
	}
}

// One retained writer preserves local command order across host attachments.
// Admission and host receipts continue while an authored reader is blocked or
// physically frozen. Only lifetime cancellation/physical close discards its pipe.
func (relay *agentRelay) writeLoop() {
	for {
		select {
		case <-relay.ctx.Done():
			return
		case next := <-relay.writes:
			err := next.write()
			relay.writeBytes.Add(-next.size)
			relay.mu.Lock()
			relay.pruneLocked()
			relay.notifyLocked()
			relay.mu.Unlock()
			if err != nil {
				relay.finish(err)
				return
			}
		}
	}
}

func (relay *agentRelay) handleHostMessage(connection programConnection, sequence uint64, message *agentv1.HostSessionMessage) error {
	// A fast peer may acknowledge before the writer has reacquired mu. Serialize
	// that receipt with publication without blocking local MCP state on socket I/O.
	relay.upstreamMu.Lock()
	defer relay.upstreamMu.Unlock()
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.connection != connection || relay.attachment != sequence || message.GetAttachmentSequence() != sequence {
		return errors.New("superseded Session connection")
	}
	switch value := message.GetMessage().(type) {
	case *agentv1.HostSessionMessage_Acknowledged:
		through := value.Acknowledged.GetThroughSequence()
		if through > relay.sentSequence {
			return errors.New("session acknowledgment exceeds sent events")
		}
		relay.acknowledged = max(relay.acknowledged, through)
		relay.pruneLocked()
	case *agentv1.HostSessionMessage_LogAcknowledged:
		ack := value.LogAcknowledged
		index := int(ack.GetStream()) - 1
		if index < 0 || index >= len(relay.logSent) || ack.GetThroughSequence() <= 0 || ack.GetThroughSequence() > relay.logSent[index] {
			return errors.New("session log acknowledgment exceeds sent records")
		}
		if err := relay.session.logs[index].acknowledge(ack.GetThroughSequence()); err != nil {
			return err
		}
	case *agentv1.HostSessionMessage_Renew:
		if err := relay.session.renew(value.Renew); err != nil {
			return err
		}
		if relay.expiryFrozen && !relay.captureFrozen {
			relay.session.mu.Lock()
			terminal, capturing := relay.session.terminal, relay.session.checkpointID != ""
			relay.session.mu.Unlock()
			if !terminal && !capturing {
				ctx, cancel := context.WithTimeout(relay.ctx, 10*time.Second)
				err := relay.session.process.thaw(ctx)
				cancel()
				if err != nil {
					return err
				}
			}
			if !capturing {
				relay.expiryFrozen = false
			}
		}
	case *agentv1.HostSessionMessage_Command:
		if relay.captureFrozen || relay.expiryFrozen {
			return errors.New("session physical execution is frozen")
		}
		size := int64(proto.Size(value.Command))
		full := len(relay.writes) == cap(relay.writes) || relay.writeBytes.Load()+size > 32*1024*1024
		shutdown := value.Command.GetShutdown() != nil
		if full && !shutdown {
			return errors.New("session command queue is full")
		}
		write, err := relay.session.prepareCommand(value.Command)
		if errors.Is(err, errSessionReconstructionRequired) || errors.Is(err, errTurnMessageClosed) {
			code := "reconstruction_required"
			if errors.Is(err, errTurnMessageClosed) {
				code = "turn_closed"
			}
			identity := value.Command.GetIdentity()
			relay.enqueueLocked(&agentv1.GuestSessionMessage{Identity: identity, Message: &agentv1.GuestSessionMessage_Event{Event: &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_DeliveryResult{DeliveryResult: &agentv1.DeliveryResult{DeliveryId: value.Command.GetDeliveryId(), Outcome: &agentv1.DeliveryResult_Error{Error: &agentv1.OperationError{Code: code, Message: err.Error()}}}}}}})
			return nil
		}
		if err != nil {
			return err
		}
		if write != nil && !full {
			relay.writeBytes.Add(size)
			relay.writes <- agentQueuedWrite{write: write, size: size}
		}
		if value.Command.GetShutdown() != nil {
			relay.shuttingDown = true
			if relay.shutdownTimer == nil {
				relay.shutdownTimer = time.AfterFunc(agentShutdownTimeout, func() { relay.finish(errors.New("session shutdown deadline")) })
			}
		}
	default:
		return errors.New("empty host Session message")
	}
	relay.notifyLocked()
	return nil
}

func (relay *agentRelay) enqueue(message *agentv1.GuestSessionMessage) uint64 {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.enqueueLocked(message)
}
func (relay *agentRelay) enqueueLocked(message *agentv1.GuestSessionMessage) uint64 {
	saved := message // Ownership transfers to the relay until host acknowledgment.
	saved.EventSequence = relay.nextSequence
	relay.nextSequence++
	relay.outbox = append(relay.outbox, saved)
	relay.notifyLocked()
	return saved.EventSequence
}
func (relay *agentRelay) pruneLocked() {
	relay.session.mu.Lock()
	defer relay.session.mu.Unlock()
	retained := relay.outbox[:0]
	for _, event := range relay.outbox {
		if event.GetEventSequence() > relay.acknowledged {
			retained = append(retained, event)
			continue
		}
		operation := event.GetEvent().GetOperation()
		if operation != nil && relay.session.pending[operation.GetRequestId()] != nil {
			retained = append(retained, event)
		}
	}
	clear(relay.outbox[len(retained):])
	relay.outbox = retained
}

func (relay *agentRelay) sendLoop() {
	for {
		relay.mu.Lock()
		connection, attachment := relay.connection, relay.attachment
		var selected *agentv1.GuestSessionMessage
		relay.session.mu.Lock()
		authorityErr := validateSessionGrant(relay.session.entry, relay.session.grant, relay.session.clock())
		terminal := relay.session.terminal
		identity := proto.Clone(relay.session.grant.GetIdentity()).(*agentv1.SessionIdentity)
		relay.session.mu.Unlock()
		if connection != nil && authorityErr == nil && (terminal || (!relay.captureFrozen && !relay.expiryFrozen)) {
			for _, event := range relay.outbox {
				if event.GetEventSequence() > relay.cursor {
					selected = proto.Clone(event).(*agentv1.GuestSessionMessage)
					break
				}
			}
			// Lifecycle and operation envelopes have their own cursor and always get
			// first use of transport capacity. Each diagnostic stream has one retry.
			if selected == nil && !relay.captureFrozen {
				for i, buffer := range relay.session.logs {
					record, ok := buffer.peek()
					if !ok || record.Through <= relay.logSent[i] {
						continue
					}
					selected = &agentv1.GuestSessionMessage{Identity: identity, Message: &agentv1.GuestSessionMessage_Log{Log: diagnosticSessionLog(record, agentv1.SessionLog_Stream(i+1))}}
					break
				}
			}
		}
		changed := relay.changed
		relay.mu.Unlock()
		if selected == nil {
			select {
			case <-relay.ctx.Done():
				return
			case <-changed:
				continue
			case <-relay.logChanged:
				continue
			}
		}
		relay.upstreamMu.Lock()
		relay.mu.Lock()
		if relay.connection != connection || relay.attachment != attachment || (selected.GetLog() != nil && relay.captureFrozen) {
			relay.mu.Unlock()
			relay.upstreamMu.Unlock()
			continue
		}
		// The outbox remains owned by state reconciliation. Marshal a separate
		// immutable frame while a new attachment or freeze can close this socket.
		frame := proto.Clone(selected).(*agentv1.GuestSessionMessage)
		frame.AttachmentSequence = attachment
		deadline := relay.writeDeadline()
		relay.mu.Unlock()
		err := connection.SetWriteDeadline(deadline)
		if err == nil {
			err = writeAgentTransportFrame(connection, frame)
		}
		relay.mu.Lock()
		if relay.connection == connection && relay.attachment == attachment {
			if err != nil {
				_ = connection.Close()
				relay.connection = nil
			} else if log := frame.GetLog(); log != nil {
				relay.logSent[int(log.GetStream())-1] = log.GetThroughSequence()
			} else {
				relay.cursor = frame.GetEventSequence()
				relay.sentSequence = max(relay.sentSequence, relay.cursor)
			}
			relay.notifyLocked()
		}
		relay.mu.Unlock()
		relay.upstreamMu.Unlock()
	}
}

func (relay *agentRelay) readLoop() {
	for {
		if !relay.awaitEventCapacity() {
			return
		}
		event, err := relay.session.process.read()
		if err != nil {
			relay.finish(err)
			return
		}
		relay.mu.Lock()
		if relay.rootExited {
			relay.exitDrainBytes += proto.Size(event)
			relay.exitDrainEvents++
		}
		overflow := relay.exitDrainBytes > 2*1024*1024 || relay.exitDrainEvents > 1024
		relay.mu.Unlock()
		if overflow {
			relay.finish(errors.New("session exit event tail exceeds its bound"))
			return
		}
		for {
			envelope, reply, err := relay.receiveProgramEvent(event)
			if errors.Is(err, errSessionGrantExpired) {
				relay.mu.Lock()
				changed := relay.changed
				relay.session.mu.Lock()
				stillExpired := errors.Is(validateSessionGrant(relay.session.entry, relay.session.grant, relay.session.clock()), errSessionGrantExpired)
				relay.session.mu.Unlock()
				relay.mu.Unlock()
				if !stillExpired {
					continue
				}
				select {
				case <-relay.ctx.Done():
					return
				case <-changed:
					continue
				}
			}
			if err != nil {
				relay.finish(err)
				return
			}
			if reply != nil {
				if err := relay.session.writeReply(relay.ctx, reply); err != nil {
					relay.finish(err)
					return
				}
			}
			if envelope != nil {
				relay.enqueue(envelope)
			}
			if event.GetCheckpointReady() != nil {
				relay.mu.Lock()
				relay.notifyLocked()
				relay.mu.Unlock()
			}
			if event.GetFailed() != nil {
				relay.finish(errors.New("session initialization failed"))
				return
			}
			break
		}
	}
}

func diagnosticSessionLog(record diagnosticRecord, stream agentv1.SessionLog_Stream) *agentv1.SessionLog {
	return &agentv1.SessionLog{Stream: stream, Kind: agentv1.SessionLog_Kind(record.Kind), Sequence: record.Sequence, ThroughSequence: record.Through, ObservedAtUnixNano: record.ObservedAt.UnixNano(), Data: record.Data, DroppedBytes: record.DroppedBytes, Complete: record.Complete}
}

func (relay *agentRelay) logLoop(reader io.Reader, stream agentv1.SessionLog_Stream) {
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	queue := relay.session.logs[int(stream)-1]
	buffer := make([]byte, queue.limits.ChunkBytes)
	notify := func() {
		select {
		case relay.logChanged <- struct{}{}:
		default:
		}
	}
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			_, _ = queue.append(buffer[:count], time.Now().UTC())
			notify()
		}
		if err != nil {
			queue.close(errors.Is(err, io.EOF), time.Now().UTC())
			notify()
			return
		}
	}
}

func (relay *agentRelay) expiryLoop() {
	for {
		relay.mu.Lock()
		relay.session.mu.Lock()
		expires := time.Unix(0, relay.session.grant.GetExpiresAtUnixNano())
		now := relay.session.clock()
		terminal := relay.session.terminal
		relay.session.mu.Unlock()
		if !terminal && !relay.expiryFrozen && !relay.captureFrozen && !now.Before(expires) {
			ctx, cancel := context.WithTimeout(relay.ctx, 10*time.Second)
			err := relay.session.process.freeze(ctx)
			cancel()
			if err != nil {
				relay.mu.Unlock()
				relay.finish(err)
				return
			}
			relay.expiryFrozen = true
			relay.notifyLocked()
		}
		changed := relay.changed
		frozen := relay.expiryFrozen || relay.captureFrozen || terminal
		relay.mu.Unlock()
		if frozen {
			select {
			case <-relay.ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		timer := time.NewTimer(max(time.Millisecond, expires.Sub(now)))
		select {
		case <-relay.ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (relay *agentRelay) finish(cause error) {
	relay.finishOnce.Do(func() {
		defer close(relay.finished)
		relay.mu.Lock()
		// Seal descriptor and HTTP admission before releasing the bridge lock.
		// Unexpected process loss can race a local factory request.
		relay.session.mu.Lock()
		relay.session.terminal = true
		relay.session.holdLocked()
		relay.session.mu.Unlock()
		bridge := relay.mcp
		relay.mu.Unlock()
		if bridge != nil {
			_ = bridge.server.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := relay.session.close(ctx)
		cancel()
		relay.mu.Lock()
		expected := relay.shuttingDown
		if relay.shutdownTimer != nil {
			relay.shutdownTimer.Stop()
		}
		if err == nil {
			relay.captureFrozen = false
			relay.expiryFrozen = false
		}
		relay.notifyLocked()
		relay.mu.Unlock()
		if err == nil {
			relay.logWG.Wait()
		}
		relay.session.mu.Lock()
		identity := proto.Clone(relay.session.grant.GetIdentity()).(*agentv1.SessionIdentity)
		relay.session.mu.Unlock()
		if err == nil && expected {
			relay.enqueue(&agentv1.GuestSessionMessage{Identity: identity, Message: &agentv1.GuestSessionMessage_Stopped{Stopped: &agentv1.SessionStopped{}}})
		} else {
			failure := errors.Join(cause, err)
			if failure == nil {
				failure = errors.New("session process ended")
			}
			event := &agentv1.ProgramEvent{Identity: identity, Event: &agentv1.ProgramEvent_Failed{Failed: &agentv1.SessionFailed{Code: "session_process_lost", Message: failure.Error()}}}
			relay.enqueue(&agentv1.GuestSessionMessage{Identity: identity, Message: &agentv1.GuestSessionMessage_Event{Event: event}})
		}
		if relay.onClosed != nil {
			relay.onClosed(err)
		}
	})
}

// Caller holds both relay and Session locks. Complete-set capture validates all
// members before invoking sealCaptureLocked for any of them.
func (relay *agentRelay) canCaptureLocked(checkpointID string) error {
	session := relay.session
	if checkpointID == "" || len(checkpointID) > 128 || session.checkpointID != "" || !session.ready || session.terminal || session.activeTurn != "" || len(session.pending) != 0 || relay.mcpRequests != 0 || relay.shuttingDown || relay.expiryFrozen || relay.captureFrozen || relay.writeBytes.Load() != 0 {
		return errors.New("session is not idle for coherent capture")
	}
	return session.authorizeLocked()
}
func (relay *agentRelay) sealCaptureLocked(checkpointID string) {
	session := relay.session
	session.checkpointID = checkpointID
	session.checkpointReady = nil
	relay.captureVerified = false
	command := &agentv1.GuestCommand{Identity: proto.Clone(session.grant.GetIdentity()).(*agentv1.SessionIdentity), Command: &agentv1.GuestCommand_Checkpoint{Checkpoint: &agentv1.SessionCheckpoint{CheckpointId: checkpointID}}}
	size := int64(proto.Size(command))
	relay.writeBytes.Add(size)
	relay.writes <- agentQueuedWrite{size: size, write: func() error { return session.process.write(context.Background(), command) }}
	relay.writerOnce.Do(func() { go relay.writeLoop() })
	relay.notifyLocked()
}
func (relay *agentRelay) awaitCheckpoint(ctx context.Context) error {
	for {
		relay.mu.Lock()
		relay.session.mu.Lock()
		ready, terminal := relay.session.checkpointReady, relay.session.terminal
		written := relay.writeBytes.Load() == 0
		relay.session.mu.Unlock()
		changed := relay.changed
		relay.mu.Unlock()
		if terminal {
			return errors.New("session ended during capture")
		}
		if ready != nil && written {
			if ready.GetError() != nil {
				return errors.New(ready.GetError().GetMessage())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-relay.ctx.Done():
			return relay.ctx.Err()
		case <-changed:
		}
	}
}

func (relay *agentRelay) freezeForCapture(ctx context.Context) error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	relay.session.mu.Lock()
	defer relay.session.mu.Unlock()
	session := relay.session
	if session.checkpointID == "" || session.checkpointReady == nil || session.checkpointReady.GetError() != nil || session.terminal || session.activeTurn != "" || len(session.pending) != 0 || relay.mcpRequests != 0 || relay.shuttingDown || relay.writeBytes.Load() != 0 {
		return errors.New("session has no idle checkpoint proof")
	}
	if err := session.authorizeLocked(); err != nil {
		return err
	}
	relay.captureVerified = false
	var scopes []nativeScopeEvidence
	if err := decodeAgentJSON(session.checkpointReady.GetScopesJson(), &scopes); err != nil {
		return err
	}
	if err := session.process.converge(scopes); err != nil {
		return err
	}
	if err := session.process.freeze(ctx); err != nil {
		return err
	}
	relay.captureFrozen = true
	if relay.connection != nil {
		_ = relay.connection.Close()
		relay.connection = nil
	}
	relay.notifyLocked()
	// Recheck membership after the parent scope is physically stopped.
	if err := session.process.converge(scopes); err != nil {
		return err
	}
	relay.captureVerified = true
	return nil
}

func writeAgentTransportFrame(writer io.Writer, message proto.Message) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > maxAgentTransportFrameBytes {
		return errors.New("agent transport frame exceeds its bound")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	_, err = io.Copy(writer, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(body)))
	return err
}

func (relay *agentRelay) writeDeadline() time.Time {
	relay.session.mu.Lock()
	defer relay.session.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	expires := time.Now().Add(time.Unix(0, relay.session.grant.GetExpiresAtUnixNano()).Sub(relay.session.clock()))
	if expires.Before(deadline) {
		return expires
	}
	return deadline
}

// Reserve one maximum event plus bounded pipe tails for a terminal notification.
// A detached host backpressures fd 3 without accumulating unbounded envelopes.
func (relay *agentRelay) awaitEventCapacity() bool {
	for {
		relay.mu.Lock()
		size := 0
		for _, event := range relay.outbox {
			size += proto.Size(event)
		}
		available := relay.rootExited || (len(relay.outbox) < 1024 && size <= 64*1024*1024-maxAgentTransportFrameBytes-2*1024*1024)
		changed := relay.changed
		relay.mu.Unlock()
		if available {
			return true
		}
		select {
		case <-relay.ctx.Done():
			return false
		case <-changed:
		}
	}
}

// Observe exit independently of fd 3 capacity, but first allow its bounded tail
// to deliver authored diagnostics. A descendant holding fd 3 cannot prevent the
// physical owner from joining the scope after this short drainage window.
func (relay *agentRelay) watchProcess() {
	err := relay.session.process.wait(relay.ctx)
	if relay.ctx.Err() != nil {
		return
	}
	relay.mu.Lock()
	relay.rootExited = true
	relay.notifyLocked()
	relay.mu.Unlock()
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-relay.finished:
		return
	case <-relay.ctx.Done():
		return
	case <-timer.C:
		relay.finish(err)
	}
}
