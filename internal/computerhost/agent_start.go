package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"io"
	"os"
	"time"
	"uuid"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

// AgentStartClient authenticates the physical owner for both transport and
// business startup. Ordinary control renewal is deliberately insufficient.
type AgentStartClient interface {
	ReleaseAgentStart(context.Context, workerapi.AgentStartReleaseRequest) (workerapi.AgentAuthorityResponse, error)
	AcquireAgentAttachment(context.Context, workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error)
	AuthorizeAgentStart(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error)
}

var ErrAgentSessionStopped = errors.New("session process is already stopped")

// StartAllocatedAgentSession starts one CP-allocated process on its owned
// Computer. The surrounding physical owner must maintain the Computer lease
// during artifact I/O and retain the returned connection for event/control work.
func (p *PreparedMachines) StartAllocatedAgentSession(lifetime context.Context, machine vm.GuestControlMachine, environment string, grant *agentv1.SessionGrant, client AgentStartClient) (connection *AgentSessionConnection, attached *agentv1.SessionAttached, resultErr error) {
	if client == nil || machine == nil || environment == "" || grant.GetIdentity().GetSessionId() == "" {
		return nil, nil, errors.New("session start requires its physical owner and authenticated client")
	}
	ctx, cancel := context.WithTimeout(lifetime, 10*time.Minute)
	defer cancel()
	grant = proto.Clone(grant).(*agentv1.SessionGrant)
	session := workerapi.RuntimeSession{EnvironmentID: environment, SessionID: grant.Identity.SessionId, ProcessEpoch: grant.Identity.ProcessEpoch, ComputerLeaseEpoch: grant.ComputerLeaseEpoch}
	attachment, err := client.AcquireAgentAttachment(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	if attachment.Stopped {
		return nil, nil, ErrAgentSessionStopped
	}
	if attachment.AttachmentSequence <= 0 {
		return nil, nil, errors.New("session attachment sequence is invalid")
	}
	grant.AuthorityGeneration, grant.ExpiresAtUnixNano = attachment.AuthorityGeneration, attachment.ExpiresAt.UnixNano()
	connection, attached, err = OpenAgentSession(lifetime, machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: uint64(attachment.AttachmentSequence)})
	if err == nil {
		return connection, attached, nil
	}
	if !errors.Is(err, ErrAgentSessionAbsent) || !attachment.Starting {
		return nil, nil, err
	}
	if p.SessionLogLimits.GetChunkBytes() <= 0 || p.SessionLogLimits.GetChunkBytes() > agentTransportFrameLimit-64*1024 || p.SessionLogLimits.GetBufferBytes() < int64(p.SessionLogLimits.GetChunkBytes()) || p.SessionLogLimits.GetBufferRecords() <= 0 {
		return nil, nil, errors.New("session startup requires configured diagnostic bounds")
	}
	request := workerapi.AgentControlRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence}
	startup, err := client.AuthorizeAgentStart(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		for _, value := range startup.Secrets {
			clear(value.Value)
		}
	}()
	if startup.ComputerID != grant.ComputerId {
		return nil, nil, errors.New("session startup selected another Computer")
	}
	if p.Reservations == nil {
		return nil, nil, errors.New("session Program requires host disk accounting")
	}
	if startup.Program.Runtime.SizeBytes < 1 || startup.Program.Runtime.SizeBytes > artifact.MaxRuntimePhysicalBytes || startup.Program.Artifact.SizeBytes < 1 || startup.Program.Artifact.SizeBytes > artifact.MaxProgramPhysicalBytes {
		return nil, nil, errors.New("invalid Session Program staging size")
	}
	invocation := uuid.NewV7().String()
	key := reservation.Key{Kind: "session-program", ID: invocation, Epoch: grant.Identity.ProcessEpoch}
	created, err := p.Reservations.Reserve(key, reservation.Vector{HostDiskBytes: startup.Program.Runtime.SizeBytes + startup.Program.Artifact.SizeBytes})
	if err != nil {
		return nil, nil, err
	}
	if !created {
		return nil, nil, errors.New("session Program staging is already owned")
	}
	var cleanupErr error
	defer func() {
		if cleanupErr == nil {
			resultErr = errors.Join(resultErr, p.Reservations.Release(key))
		}
	}()
	if p.TempDir != "" {
		if err = os.MkdirAll(p.TempDir, 0700); err != nil {
			return nil, nil, err
		}
	}
	directory, err := os.MkdirTemp(p.TempDir, "session-program-")
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if resultErr != nil && connection != nil {
			resultErr = errors.Join(resultErr, connection.Close())
			connection, attached = nil, nil
		}
	}()
	defer func() {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(directory))
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	images, err := p.prepareProgramArtifacts(ctx, directory, invocation, &startup.Program)
	if err != nil {
		return nil, nil, err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, images.close()) }()
	source := &agentProgramSource{client: client, request: request, startup: startup, grant: grant, images: images}
	// Host verification can itself outlive the initial attachment grant. A fresh
	// business release is needed before the guest will accept transfer admission.
	grant, err = source.ReleaseStart(ctx)
	if err != nil {
		return nil, nil, err
	}
	start := &agentv1.SessionStart{LogLimits: p.SessionLogLimits, AgentId: startup.AgentKey, ComputerId: startup.ComputerID, DeploymentId: startup.Program.DeploymentID, ConversationKey: startup.SessionKey, ParentSessionId: startup.ParentSessionID, TerminalSequence: startup.TerminalSequence, RecoveryKind: agentv1.SessionStart_RECOVERY_KIND_INITIAL, Program: &agentv1.SessionProgram{
		Runtime: &agentv1.SessionProgramArtifact{Digest: startup.Program.Runtime.Digest, SizeBytes: startup.Program.Runtime.SizeBytes}, Artifact: &agentv1.SessionProgramArtifact{Digest: startup.Program.Artifact.Digest, SizeBytes: startup.Program.Artifact.SizeBytes},
	}}
	if grant.Identity.ProcessEpoch > 1 {
		start.RecoveryKind = agentv1.SessionStart_RECOVERY_KIND_RECONSTRUCTED
		start.RecoveryReason = "process_replaced"
	}
	requestAttach := &agentv1.SessionAttach{Grant: grant, Start: start, AttachmentSequence: uint64(attachment.AttachmentSequence)}
	for _, value := range startup.Secrets {
		item := &agentv1.SessionSecret{Value: value.Value}
		switch {
		case value.Env != nil && value.File == nil:
			item.Placement = &agentv1.SessionSecret_Env{Env: value.Env.Name}
		case value.File != nil && value.Env == nil:
			item.Placement = &agentv1.SessionSecret_File{File: value.File.Path}
		default:
			return nil, nil, errors.New("invalid Session Secret placement")
		}
		requestAttach.Secrets = append(requestAttach.Secrets, item)
	}
	if startup.ProtectedEnv != nil {
		requestAttach.ProtectedEnv = startup.ProtectedEnv.Env
		requestAttach.ProxyCa = startup.ProtectedEnv.CA
	}
	return StartAgentSession(lifetime, machine, requestAttach, source)
}

type agentProgramSource struct {
	client  AgentStartClient
	request workerapi.AgentControlRequest
	startup workerapi.AgentStartResponse
	grant   *agentv1.SessionGrant
	images  *programSnapshots
}

func (s *agentProgramSource) WriteArtifact(ctx context.Context, d *agentv1.SessionProgramArtifact, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var file *os.File
	var err error
	switch {
	case d.Digest == s.startup.Program.Runtime.Digest && d.SizeBytes == s.startup.Program.Runtime.SizeBytes:
		file, _, err = s.images.runtime.VerifierFile()
	case d.Digest == s.startup.Program.Artifact.Digest && d.SizeBytes == s.startup.Program.Artifact.SizeBytes:
		file, err = s.images.artifact.VerifierFile()
	default:
		return errors.New("session requested an unverified Program artifact")
	}
	if err != nil {
		return err
	}
	// ReadAt leaves the verifier's offset independent from transport retries.
	_, err = io.Copy(w, io.NewSectionReader(file, 0, d.SizeBytes))
	return err
}
func (s *agentProgramSource) ReleaseStart(ctx context.Context) (*agentv1.SessionGrant, error) {
	authority, err := s.client.ReleaseAgentStart(ctx, workerapi.AgentStartReleaseRequest{Session: s.request.Session, AttachmentSequence: s.request.AttachmentSequence, BundleDigest: s.startup.BundleDigest})
	if err != nil {
		return nil, err
	}
	next := proto.Clone(s.grant).(*agentv1.SessionGrant)
	next.AuthorityGeneration, next.ExpiresAtUnixNano = authority.AuthorityGeneration, authority.ExpiresAt.UnixNano()
	return next, nil
}
