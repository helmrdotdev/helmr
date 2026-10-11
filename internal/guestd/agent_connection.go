package guestd

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

var errAgentSessionAbsent = errors.New("session process is absent; attachment cannot run setup")

func handleAgentSessionConnection(ctx context.Context, connection programConnection, bodyLength uint64, computers *computerOperationRegistry) error {
	if bodyLength != 0 {
		return errors.New("session connection has an unexpected stream body")
	}
	if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	request := new(agentv1.SessionAttach)
	if err := frameio.ReadProtoFrameBounded(connection, maxProgramSecretFrameBytes, request); err != nil {
		return err
	}
	defer clearAgentSecrets(request.GetSecrets())
	if len(request.GetSecrets()) > maxProgramSecretPlacements {
		return errors.New("too many Session secret placements")
	}
	plaintext := 0
	for _, secret := range request.GetSecrets() {
		plaintext += len(secret.GetValue())
	}
	if plaintext > maxProgramSecretPlaintextBytes {
		return errors.New("session secrets exceed their bound")
	}
	if request.GetStart() != nil {
		if err := connection.SetReadDeadline(time.Now().Add(10 * time.Minute)); err != nil {
			return err
		}
		if err := connection.SetWriteDeadline(time.Now().Add(10 * time.Minute)); err != nil {
			return err
		}
	}
	relay, err := computers.openAgentSession(ctx, request, agentProgramTransfer{connection: connection, request: request})
	if errors.Is(err, errAgentSessionAbsent) {
		return frameio.WriteProtoFrame(connection, &agentv1.GuestSessionMessage{Identity: request.GetGrant().GetIdentity(), AttachmentSequence: request.GetAttachmentSequence(), Message: &agentv1.GuestSessionMessage_Absent{Absent: &agentv1.SessionAbsent{}}})
	}
	if err != nil {
		return err
	}
	if err := relay.attach(connection, request); err != nil {
		return err
	}
	return relay.serve(connection, request.GetAttachmentSequence())
}

func clearAgentSecrets(secrets []*agentv1.SessionSecret) {
	for _, secret := range secrets {
		if secret != nil {
			clear(secret.Value)
			secret.Value = nil
		}
	}
}

func (r *computerOperationRegistry) openAgentSession(ctx context.Context, request *agentv1.SessionAttach, input agentProgramInput) (_ *agentRelay, resultErr error) {
	grant := request.GetGrant()
	id := grant.GetIdentity().GetSessionId()
	if id == "" || request.GetAttachmentSequence() == 0 {
		return nil, errors.New("session identity and attachment sequence are required")
	}
	r.mu.RLock()
	existing := r.agentSessions[id]
	r.mu.RUnlock()
	if existing != nil {
		existing.session.mu.Lock()
		currentEpoch, physicalClosed := existing.session.grant.GetIdentity().GetProcessEpoch(), existing.session.physicalClosed
		existing.session.mu.Unlock()
		if grant.GetIdentity().GetProcessEpoch() == currentEpoch {
			return existing, nil
		}
		if grant.GetIdentity().GetProcessEpoch() < currentEpoch || !physicalClosed || (request.GetStart() != nil && request.GetStart().GetRecoveryKind() != agentv1.SessionStart_RECOVERY_KIND_RECONSTRUCTED) {
			return nil, errors.New("session reconstruction requires a closed earlier process epoch")
		}
	}
	entry, release, ok := r.acquireExact(grant.GetComputerInstanceId(), grant.GetComputerId(), grant.GetChannelCredential(), uint64(grant.GetWriterGeneration()))
	if !ok {
		return nil, errors.New("session grant has no owned Computer mount")
	}
	claimed := false
	defer func() {
		if !claimed {
			release()
		}
	}()
	entry.lifecycleMu.Lock()
	entry.finalizationMu.Lock()
	r.mu.Lock()
	if r.agentSessions == nil {
		r.agentSessions = make(map[string]*agentRelay)
	}
	if r.agentStarting == nil {
		r.agentStarting = make(map[string]bool)
	}
	entry.processesMu.Lock()
	unavailable := entry.recoveryRequired || entry.stopping
	entry.processesMu.Unlock()
	err := validateSessionGrant(entry, grant, entry.authorityNow())
	if err == nil && (entry.retired || r.entries[entry.computerInstanceID] != entry || r.captureSealedLocked() || r.agentStarting[id] || r.agentSessions[id] != existing || unavailable) {
		err = errors.New("computer has sealed or superseded Session admission")
	}
	if err == nil && request.GetStart() == nil {
		err = errAgentSessionAbsent
	}
	if err == nil {
		r.agentStarting[id] = true
	}
	r.mu.Unlock()
	entry.finalizationMu.Unlock()
	entry.lifecycleMu.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() { r.mu.Lock(); delete(r.agentStarting, id); r.mu.Unlock() }()
	admissionCtx, cancelAdmission := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelAdmission()
	program, err := r.agentPrograms.materialize(admissionCtx, request.GetStart().GetProgram(), input)
	if err != nil {
		var cleanup programCleanupError
		if errors.As(err, &cleanup) {
			entry.processesMu.Lock()
			entry.recoveryRequired = true
			entry.processesMu.Unlock()
		}
		return nil, err
	}
	programOwned := true
	defer func() {
		if programOwned {
			if err := program.close(); err != nil {
				resultErr = errors.Join(resultErr, err)
				entry.processesMu.Lock()
				entry.recoveryRequired = true
				entry.processesMu.Unlock()
			}
		}
	}()
	released, err := input.releaseStart(admissionCtx)
	if err != nil {
		return nil, err
	}
	if !sameSessionGrantOwner(grant, released) || released.GetAuthorityGeneration() < grant.GetAuthorityGeneration() {
		return nil, errors.New("session start release changed its owner")
	}
	// Slow transfer cannot carry admission across retirement or a peer's
	// physical cleanup fault. Keep lifecycle ownership through registration.
	entry.lifecycleMu.Lock()
	entry.finalizationMu.Lock()
	admissionLocked := true
	unlockAdmission := func() {
		if admissionLocked {
			admissionLocked = false
			entry.finalizationMu.Unlock()
			entry.lifecycleMu.Unlock()
		}
	}
	defer unlockAdmission()
	r.mu.Lock()
	entry.processesMu.Lock()
	unavailable = entry.recoveryRequired || entry.stopping
	entry.processesMu.Unlock()
	err = validateSessionGrant(entry, released, entry.authorityNow())
	if err == nil && (entry.retired || r.entries[entry.computerInstanceID] != entry || r.captureSealedLocked() || !r.agentStarting[id] || r.agentSessions[id] != existing || unavailable) {
		err = errors.New("computer has sealed or superseded Session admission")
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	request.Grant = released
	grant = released
	process, err := createAgentProcess(ctx, entry, agentProcessOptions{Identity: grant.GetIdentity(), Secrets: request.GetSecrets(), ProtectedEnv: request.GetProtectedEnv(), ProxyCA: request.GetProxyCa(), Program: program.mounts(), ProgramLease: program})
	if err != nil {
		return nil, err
	}
	programOwned = false
	cleanup := func() error {
		stdout, stderr := process.logs()
		if reader, ok := stdout.(io.Closer); ok {
			defer reader.Close()
		}
		if reader, ok := stderr.(io.Closer); ok {
			defer reader.Close()
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return process.close(stopCtx)
	}
	session, err := newAgentSession(entry, grant, request.GetStart(), process, entry.authorityNow)
	if err != nil {
		unlockAdmission()
		if cleanupErr := cleanup(); cleanupErr != nil {
			entry.processesMu.Lock()
			entry.recoveryRequired = true
			entry.processesMu.Unlock()
			claimed = true
		}
		return nil, err
	}
	relay := newAgentRelay(ctx, session, func(closeErr error) {
		if closeErr == nil {
			release()
		}
	})
	// Register before the first process start so every uncertain launch retains
	// its owner. A failed epoch is never implicitly restarted by an attach retry.
	r.mu.Lock()
	r.agentSessions[id] = relay
	r.mu.Unlock()
	claimed = true
	// Registered ownership and agentStarting now protect lifetime and capture.
	// Peer authority renewals and commands must not wait for process startup.
	unlockAdmission()
	// Reader ownership starts before exec so failed startup diagnostics are
	// drained and the constructor's pipe ends never become orphaned.
	relay.startLogs()
	startCtx, stopStart := context.WithTimeout(ctx, time.Unix(0, grant.GetExpiresAtUnixNano()).Sub(entry.authorityNow()))
	startErr := process.start(startCtx)
	stopStart()
	if err := startErr; err != nil {
		relay.finish(err)
		go relay.sendLoop()
		return relay, nil
	}
	relay.run()
	if err := session.send(&agentv1.GuestCommand{Identity: grant.GetIdentity(), Command: &agentv1.GuestCommand_Start{Start: request.GetStart()}}); err != nil {
		relay.finish(err)
	}
	return relay, nil
}
