package computerhost

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

// AgentComputerClient supplies the authenticated control protocol for one
// physical continuation. The worker client implements it directly.
type AgentComputerClient interface {
	AgentControlClient
	AgentTurnClient
	AgentMessageClient
	AcquireAgentAttachment(context.Context, workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error)
	ValidateAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error
	CommitAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error
	CompleteAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error
	ValidateAgentComputerRestore(context.Context, workerapi.AgentComputerInstallationRequest) error
	CommitAgentComputerRestore(context.Context, workerapi.AgentComputerInstallationRequest) error
	CompleteAgentComputerRestore(context.Context, workerapi.AgentComputerInstallationRequest) error
	ReadAgentComputerControls(context.Context, workerapi.AgentComputerInstallationRequest) (workerapi.AgentComputerControlsResponse, error)
}

// AgentComputerOwner retains one exact physical installation through uncertain
// replies. Its lifetime outlasts individual Continue calls and owns every Session
// attachment and renewal. Close joins those jobs; it does not stop guest processes
// or release the machine. The surrounding physical owner retains those duties.
// The observer runs inside an owned event loop; it must not synchronously call
// Continue or Close on this owner. Done signals cancellation; Close joins cleanup.
type AgentComputerOwner struct {
	ctx          context.Context
	cancel       context.CancelCauseFunc
	machine      vm.GuestControlMachine
	client       AgentComputerClient
	environment  string
	installation *agentv1.ComputerSessionInstallation
	observe      func(context.Context, *agentv1.GuestSessionMessage) error
	operation    chan struct{}
	sessions     map[string]*ownedAgentSession
	jobs         sync.WaitGroup
	activated    chan struct{}
	activateOnce sync.Once
	failureMu    sync.Mutex
	failure      *SessionControlFailedError
}

type ownedAgentSession struct {
	grant              *agentv1.SessionGrant
	mu                 sync.Mutex
	connected, stopped bool
	changed            chan struct{}
}

func NewAgentComputerOwner(lifetime context.Context, machine vm.GuestControlMachine, client AgentComputerClient, environment string, installation *agentv1.ComputerSessionInstallation, observe func(context.Context, *agentv1.GuestSessionMessage) error) (*AgentComputerOwner, error) {
	if lifetime == nil || machine == nil || client == nil || environment == "" || installation.GetCapture() == nil || installation.GetEnvelope() == nil || len(installation.GetGrants()) == 0 || observe == nil {
		return nil, errors.New("computer owner requires its lifetime, installation, machine, client and observer")
	}
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(lifetime)
	return &AgentComputerOwner{ctx: ctx, cancel: cancel, machine: machine, client: client, environment: environment, installation: proto.Clone(installation).(*agentv1.ComputerSessionInstallation), observe: observe, sessions: make(map[string]*ownedAgentSession), activated: make(chan struct{}), operation: make(chan struct{}, 1)}, nil
}

// Continue serializes retries of this installation. Request cancellation leaves
// retained connections renewing under the physical owner's longer lifetime.
func (owner *AgentComputerOwner) Continue(ctx context.Context) (*agentv1.ComputerSessionReceipt, error) {
	select {
	case owner.operation <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-owner.ctx.Done():
		return nil, context.Cause(owner.ctx)
	}
	defer func() { <-owner.operation }()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(owner.ctx, func() { cancel(context.Cause(owner.ctx)) })
	defer func() { stop(); cancel(nil) }()
	if owner.ctx.Err() != nil {
		return nil, context.Cause(owner.ctx)
	}
	return ContinueAgentComputer(ctx, owner.machine, owner.installation, agentComputerContinuation{owner})
}
func (owner *AgentComputerOwner) Done() <-chan struct{} { return owner.ctx.Done() }
func (owner *AgentComputerOwner) Err() error {
	owner.failureMu.Lock()
	defer owner.failureMu.Unlock()
	if owner.failure != nil {
		return *owner.failure
	}
	return context.Cause(owner.ctx)
}
func (owner *AgentComputerOwner) Close() error {
	owner.cancel(context.Canceled)
	owner.operation <- struct{}{}
	defer func() { <-owner.operation }()
	owner.jobs.Wait()
	err := owner.Err()
	if err == context.Canceled {
		return nil
	}
	return err
}

type agentComputerContinuation struct{ owner *AgentComputerOwner }

func (c agentComputerContinuation) request(p *agentv1.ComputerSessionInstallation, r *agentv1.ComputerSessionReceipt) (workerapi.AgentComputerInstallationRequest, error) {
	installation, err := proto.Marshal(p)
	if err != nil {
		return workerapi.AgentComputerInstallationRequest{}, err
	}
	receipt, err := proto.Marshal(r)
	return workerapi.AgentComputerInstallationRequest{EnvironmentID: c.owner.environment, Installation: installation, Receipt: receipt}, err
}
func (c agentComputerContinuation) ValidateTarget(ctx context.Context, p *agentv1.ComputerSessionInstallation, r *agentv1.ComputerSessionReceipt) error {
	request, err := c.request(p, r)
	if err != nil {
		return err
	}
	if p.GetSourceAbort() {
		return c.owner.client.ValidateAgentComputerSourceAbort(ctx, request)
	}
	return c.owner.client.ValidateAgentComputerRestore(ctx, request)
}
func (c agentComputerContinuation) PrepareActivation(ctx context.Context, p *agentv1.ComputerSessionInstallation, r *agentv1.ComputerSessionReceipt) error {
	request, err := c.request(p, r)
	if err != nil {
		return err
	}
	if p.GetSourceAbort() {
		return c.owner.client.CommitAgentComputerSourceAbort(ctx, request)
	}
	return c.owner.client.CommitAgentComputerRestore(ctx, request)
}
func (c agentComputerContinuation) CurrentControls(ctx context.Context, p *agentv1.ComputerSessionInstallation, r *agentv1.ComputerSessionReceipt) (*agentv1.ComputerSessionControls, error) {
	request, err := c.request(p, r)
	if err != nil {
		return nil, err
	}
	result, err := c.owner.client.ReadAgentComputerControls(ctx, request)
	if err != nil {
		return nil, err
	}
	controls := new(agentv1.ComputerSessionControls)
	if err := proto.Unmarshal(result.Controls, controls); err != nil {
		return nil, err
	}
	return controls, nil
}
func (c agentComputerContinuation) AttachSessions(ctx context.Context, p *agentv1.ComputerSessionInstallation) error {
	for _, grant := range p.GetGrants() {
		id := grant.GetIdentity().GetSessionId()
		if c.owner.sessions[id] != nil {
			continue
		}
		member := &ownedAgentSession{grant: proto.Clone(grant).(*agentv1.SessionGrant), changed: make(chan struct{})}
		c.owner.sessions[id] = member
		c.owner.jobs.Go(func() { c.owner.runSession(member) })
	}
	for _, member := range c.owner.sessions {
		for {
			member.mu.Lock()
			ready, changed := member.connected || member.stopped, member.changed
			member.mu.Unlock()
			if ready {
				break
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
	}
	return nil
}
func (c agentComputerContinuation) ReconcileControls(ctx context.Context, p *agentv1.ComputerSessionInstallation, r *agentv1.ComputerSessionReceipt) error {
	if !r.GetActivated() || !r.GetInstalled() || r.GetFrozen() {
		return errors.New("computer reconciliation requires physical activation")
	}
	request, err := c.request(p, r)
	if err != nil {
		return err
	}
	c.owner.activateOnce.Do(func() { close(c.owner.activated) })
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if p.GetSourceAbort() {
			err = c.owner.client.CompleteAgentComputerSourceAbort(requestCtx, request)
		} else {
			err = c.owner.client.CompleteAgentComputerRestore(requestCtx, request)
		}
		cancel()
		if err == nil {
			return nil
		}
		var rejected *httpclient.Error
		if errors.As(err, &rejected) && rejected.StatusCode >= 400 && rejected.StatusCode < 500 && rejected.Code != workerapi.AgentComputerNotReady {
			return err
		}
		if err := sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return context.Cause(ctx)
		}
	}
}

func (member *ownedAgentSession) setState(connected, stopped bool) {
	member.mu.Lock()
	defer member.mu.Unlock()
	member.connected, member.stopped = connected, stopped
	close(member.changed)
	member.changed = make(chan struct{})
}
func (owner *AgentComputerOwner) runSession(member *ownedAgentSession) {
	for owner.ctx.Err() == nil {
		session := workerapi.RuntimeSession{EnvironmentID: owner.environment, SessionID: member.grant.GetIdentity().GetSessionId(), ProcessEpoch: member.grant.GetIdentity().GetProcessEpoch(), ComputerLeaseEpoch: member.grant.GetComputerLeaseEpoch()}
		requestCtx, cancel := context.WithTimeout(owner.ctx, 10*time.Second)
		attachment, err := owner.client.AcquireAgentAttachment(requestCtx, session)
		cancel()
		if err == nil && attachment.Stopped {
			member.setState(false, true)
			return
		}
		if err != nil {
			var rejected interface{ SessionAuthorityRejected() bool }
			var hostRejected interface{ WorkerAuthorityRejected() bool }
			if (errors.As(err, &rejected) && rejected.SessionAuthorityRejected()) || (errors.As(err, &hostRejected) && hostRejected.WorkerAuthorityRejected()) {
				owner.cancel(err)
				return
			}
		} else {
			if attachment.AttachmentSequence <= 0 || attachment.AuthorityGeneration <= 0 || !attachment.ExpiresAt.After(time.Now()) {
				owner.cancel(errors.New("invalid retained Session attachment authority"))
				return
			}
			grant := proto.Clone(member.grant).(*agentv1.SessionGrant)
			grant.AuthorityGeneration = attachment.AuthorityGeneration
			grant.ExpiresAtUnixNano = attachment.ExpiresAt.UnixNano()
			var connection *AgentSessionConnection
			var attached *agentv1.SessionAttached
			connection, attached, err = OpenAgentSession(owner.ctx, owner.machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: uint64(attachment.AttachmentSequence)})
			if err == nil {
				member.setState(true, false)
				err = serveExecutingAgentSession(owner.ctx, connection, attached, owner.environment, owner.client, owner.observe, owner.activated)
				member.setState(false, err == nil)
				if err == nil {
					return
				}
				var failed SessionControlFailedError
				if errors.As(err, &failed) {
					// Close may already have cancelled the lifetime while a failed guest
					// control's receipt was in flight. Joining must still report that failure.
					owner.failureMu.Lock()
					if owner.failure == nil {
						owner.failure = &failed
					}
					owner.failureMu.Unlock()
					owner.cancel(err)
					return
				}
				// Reacquisition distinguishes a durably stopped process from a transient
				// transport failure, including uncertainty after a physical-stop receipt.
			}
		}
		if sleepWithContext(owner.ctx, 250*time.Millisecond) != nil {
			return
		}
	}
}
