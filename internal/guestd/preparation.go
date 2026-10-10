package guestd

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

// The mounted image retains the sole authored execution and output. Attachment
// loss only loses an observation; it cannot start another prepare invocation.
type computerPreparation struct {
	mu      sync.Mutex
	expires time.Time
	changed chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	state   string
	code    string
	output  *preparationOutput
}

func handlePreparationControl(ctx context.Context, conn programConnection, bodyLen uint64, registry *computerOperationRegistry) error {
	if bodyLen != 0 {
		return errors.New("preparation control body must be empty")
	}
	if err := conn.SetReadDeadline(time.Now().Add(computerControlTimeout)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	var request computerv0.PreparationControlRequest
	if err := frameio.ReadProtoFrameBounded(conn, wire.PreparationControlFrameBytes, &request); err != nil {
		return err
	}
	defer clearPreparationRequest(&request)
	response, err := registry.controlPreparation(ctx, &request, (*computerMountEntry).executePreparation)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(computerControlTimeout)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(conn, response)
}

func clearPreparationRequest(request *computerv0.PreparationControlRequest) {
	clear(request.GetIdentity().GetChannelCredential())
	for _, secret := range request.GetStart().GetSecrets() {
		clear(secret.Value)
	}
}

func (r *computerOperationRegistry) controlPreparation(ctx context.Context, request *computerv0.PreparationControlRequest, run func(*computerMountEntry, context.Context, *computerv0.PreparationStart, *preparationOutput) (error, error)) (*computerv0.PreparationControlResponse, error) {
	identity := request.GetIdentity()
	if identity.GetPreparationId() == "" || identity.GetInstanceId() == "" || identity.GetEpoch() <= 0 || len(identity.GetChannelCredential()) != 32 || request.GetAcknowledgedThrough() < 0 || (request.GetLogStream() != "" && request.GetLogStream() != "stdout" && request.GetLogStream() != "stderr") || (request.GetLogStream() == "" && request.GetAcknowledgedThrough() != 0) || (request.GetLogStream() != "" && (request.GetStart() != nil || request.GetRenewOnly())) || (request.GetRenewOnly() && request.GetStart() != nil) {
		return nil, errors.New("invalid preparation control")
	}
	entry, release, ok := r.acquireExact(identity.GetInstanceId(), identity.GetPreparationId(), base64.RawURLEncoding.EncodeToString(identity.GetChannelCredential()), uint64(identity.GetEpoch()))
	if !ok {
		return nil, errors.New("preparation does not own mounted image")
	}
	defer release()
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if !r.currentMountLocked(entry, identity.GetInstanceId(), identity.GetPreparationId(), base64.RawURLEncoding.EncodeToString(identity.GetChannelCredential())) || entry.stopping || r.captureSealed() {
		return nil, errors.New("preparation image is stopping or capturing")
	}
	p := entry.preparation
	if p == nil && request.GetStart() == nil {
		return &computerv0.PreparationControlResponse{State: "absent"}, nil
	}
	if start := request.GetStart(); start != nil {
		if !definition.ValidDeclaredID(start.GetComputerDefinitionId()) || wire.ValidatePreparationControl(request) != nil {
			return nil, errors.New("invalid preparation start")
		}
		if p != nil {
			return nil, errors.New("preparation already started")
		}
		expires := time.Unix(0, request.GetExpiresAtUnixNano())
		if !expires.After(entry.authorityNow()) || ctx.Err() != nil {
			return nil, errors.New("preparation lease is expired")
		}
		entry.processesMu.Lock()
		unavailable := entry.recoveryRequired || entry.processAdmissions != 0
		entry.processesMu.Unlock()
		if unavailable {
			return nil, errors.New("preparation image has another process or requires recovery")
		}
		output, err := newPreparationOutput(start.GetLogLimits())
		if err != nil {
			return nil, err
		}
		lifetime, cancel := context.WithCancel(ctx)
		p = &computerPreparation{expires: expires, changed: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel, state: "running", output: output}
		entry.preparation = p
		entry.processesMu.Lock()
		entry.processAdmissions++
		entry.processesMu.Unlock()
		r.mu.Lock()
		entry.active++
		r.mu.Unlock()
		startCopy := proto.Clone(start).(*computerv0.PreparationStart)
		go p.watchLease(lifetime, entry.authorityNow)
		go func() {
			defer r.release(entry)
			defer cancel()
			err, cleanupErr := run(entry, lifetime, startCopy, output)
			// A failed launch or missing pipe proof is an unknown boundary. Actual
			// pipe completion closes each buffer first and remains authoritative.
			output.close(false)
			for _, s := range startCopy.GetSecrets() {
				clear(s.Value)
			}
			entry.processesMu.Lock()
			entry.processAdmissions--
			if cleanupErr != nil {
				entry.recoveryRequired = true
			}
			entry.processesMu.Unlock()
			p.mu.Lock()
			p.state = "succeeded"
			if err != nil || cleanupErr != nil || lifetime.Err() != nil {
				p.state = "failed"
				p.code = "preparation_execution_failed"
				if cleanupErr != nil {
					p.code = "preparation_scope_termination_failed"
				}
				if lifetime.Err() != nil {
					p.code = "preparation_cancelled"
				}
			}
			close(p.done)
			p.mu.Unlock()
		}()
	}
	p.mu.Lock()
	if p.state == "running" {
		now := entry.authorityNow()
		if !p.expires.After(now) {
			p.cancel()
		} else if next := time.Unix(0, request.GetExpiresAtUnixNano()); next.After(p.expires) {
			p.expires = next
			select {
			case p.changed <- struct{}{}:
			default:
			}
		}
	}
	response := &computerv0.PreparationControlResponse{State: p.state, ErrorCode: p.code}
	p.mu.Unlock()
	if stream := request.GetLogStream(); stream != "" {
		buffer := p.output.stream(stream)
		if through := request.GetAcknowledgedThrough(); through != 0 {
			if err := buffer.acknowledge(through); err != nil {
				return nil, err
			}
		}
		if record, ok := buffer.peek(); ok {
			response.Log = preparationLog(record)
		}
	}
	return response, nil
}

func (p *computerPreparation) watchLease(ctx context.Context, now func() time.Time) {
	for {
		p.mu.Lock()
		remaining := p.expires.Sub(now())
		p.mu.Unlock()
		if remaining <= 0 {
			p.cancel()
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.done:
			timer.Stop()
			return
		case <-p.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
