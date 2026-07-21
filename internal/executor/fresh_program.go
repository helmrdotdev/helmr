package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const (
	maxFreshProgramSecrets     = 64
	maxFreshProgramSecretBytes = 128 << 20
)

type FreshProgramControl interface {
	AcknowledgeRunStart(
		context.Context,
		api.WorkerRunLeaseReceipt,
	) (api.WorkerRunStartResponse, error)
	AcknowledgeRunEntrypoint(
		context.Context,
		api.WorkerRunEntrypointRequest,
	) error
}

type freshProgramEventSink interface {
	AppendRunLog(
		context.Context,
		api.WorkerRunLeaseReceipt,
		api.WorkerLogStream,
		uint64,
		[]byte,
	) error
}

type freshProgram struct {
	session          vm.Session
	lease            api.WorkerRunLeaseReceipt
	entrypoint       *runv0.EntrypointIdentity
	observedEventSeq uint64
}

func (r GuestRunner) startFreshProgram(
	ctx context.Context,
	claim *api.WorkerRunLeaseClaimResponse,
	control FreshProgramControl,
	events freshProgramEventSink,
) (freshProgram, error) {
	if claim == nil {
		return freshProgram{}, errors.New("Run Lease claim is required")
	}
	defer clearFreshProgramDelivery(claim)
	if control == nil {
		return freshProgram{}, errors.New("fresh Program control is required")
	}
	if events == nil {
		return freshProgram{}, errors.New("fresh Program event sink is required")
	}
	if r.WorkspaceMounts == nil {
		return freshProgram{}, errors.New(
			"workspace mount session registry is required",
		)
	}
	fresh, err := validateFreshProgramClaim(claim)
	if err != nil {
		return freshProgram{}, err
	}
	admissionCtx, cancelAdmission := context.WithDeadline(
		ctx,
		claim.Lease.StartDeadlineAt,
	)
	defer cancelAdmission()
	opened, err := r.WorkspaceMounts.OpenWorkspaceMountSession(
		admissionCtx,
		claim.Lease.WorkspaceMountID,
	)
	if err != nil {
		return freshProgram{}, err
	}
	keepSession := false
	defer func() {
		if !keepSession {
			_ = opened.Session.Close(context.Background())
		}
	}()
	if opened.Session.Stream() == nil {
		return freshProgram{}, errors.New("Workspace mount stream is required")
	}
	if err := writeFreshProgramContext(
		admissionCtx,
		opened.Session,
		func(stream vm.Stream) error {
			return writeFreshProgramAdmission(
				stream,
				opened.ChannelToken,
				claim,
				fresh,
			)
		},
	); err != nil {
		return freshProgram{}, err
	}
	var event runv0.RunEvent
	if err := readProtoFrameContext(admissionCtx, opened.Session, &event); err != nil {
		return freshProgram{}, fmt.Errorf(
			"read Program process-started proof: %w",
			err,
		)
	}
	started := event.GetProgramProcessStarted()
	if started == nil ||
		started.GetRunId() != claim.Lease.RunID ||
		started.GetAttemptNumber() != uint32(claim.Lease.AttemptNumber) ||
		started.GetRunLeaseId() != claim.Lease.ID {
		return freshProgram{}, errors.New(
			"Program process-started proof does not match Run Lease",
		)
	}
	observedEventSeq := uint64(1)
	startResponse, err := control.AcknowledgeRunStart(
		admissionCtx,
		claim.Lease,
	)
	if err != nil {
		return freshProgram{}, fmt.Errorf("acknowledge fresh Run start: %w", err)
	}
	if !equalRunLeaseReceipt(startResponse.Lease, claim.Lease) {
		return freshProgram{}, errors.New(
			"Run start acknowledgement changed the Run Lease receipt",
		)
	}
	if err := writeFreshProgramContext(
		admissionCtx,
		opened.Session,
		func(stream vm.Stream) error {
			return frameio.WriteProtoFrame(
				stream,
				programStartRelease(startResponse.Lease),
			)
		},
	); err != nil {
		return freshProgram{}, fmt.Errorf(
			"write Program-start release: %w",
			err,
		)
	}
	ready, err := readFreshEntrypointReady(
		admissionCtx,
		opened.Session,
		startResponse.Lease,
		events,
		&observedEventSeq,
	)
	if err != nil {
		return freshProgram{}, err
	}
	kind, err := validateFreshEntrypoint(ready, startResponse.Lease)
	if err != nil {
		return freshProgram{}, err
	}
	if err := control.AcknowledgeRunEntrypoint(
		admissionCtx,
		api.WorkerRunEntrypointRequest{
			Lease:                startResponse.Lease,
			EntrypointKind:       kind,
			EntrypointDeclaredID: ready.GetEntrypoint().GetDeclaredId(),
		},
	); err != nil {
		return freshProgram{}, fmt.Errorf("acknowledge Run entrypoint: %w", err)
	}
	if err := writeFreshProgramContext(
		admissionCtx,
		opened.Session,
		func(stream vm.Stream) error {
			return frameio.WriteProtoFrame(
				stream,
				&runv0.ProgramSupervisorCommand{
					Command: &runv0.ProgramSupervisorCommand_EntrypointRelease{
						EntrypointRelease: &runv0.EntrypointRelease{
							RunId: startResponse.Lease.RunID,
							AttemptNumber: uint32(
								startResponse.Lease.AttemptNumber,
							),
							Entrypoint: ready.GetEntrypoint(),
						},
					},
				},
			)
		},
	); err != nil {
		return freshProgram{}, fmt.Errorf("write entrypoint release: %w", err)
	}
	keepSession = true
	return freshProgram{
		session:          opened.Session,
		lease:            startResponse.Lease,
		entrypoint:       ready.GetEntrypoint(),
		observedEventSeq: observedEventSeq,
	}, nil
}

func writeFreshProgramContext(
	ctx context.Context,
	session vm.Session,
	write func(vm.Stream) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = session.Close(context.Background())
		close(closed)
	})
	err := write(session.Stream())
	if !stop() {
		<-closed
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return err
}

func readFreshEntrypointReady(
	ctx context.Context,
	session vm.Session,
	lease api.WorkerRunLeaseReceipt,
	events freshProgramEventSink,
	observedEventSeq *uint64,
) (*runv0.EntrypointReady, error) {
	for {
		var event runv0.RunEvent
		if err := readProtoFrameContext(ctx, session, &event); err != nil {
			return nil, fmt.Errorf("read entrypoint-ready event: %w", err)
		}
		*observedEventSeq = *observedEventSeq + 1
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if err := events.AppendRunLog(
				ctx,
				lease,
				api.WorkerLogStreamStdout,
				*observedEventSeq,
				value.StdoutChunk,
			); err != nil {
				return nil, fmt.Errorf("append pre-entrypoint stdout: %w", err)
			}
		case *runv0.RunEvent_StderrChunk:
			if err := events.AppendRunLog(
				ctx,
				lease,
				api.WorkerLogStreamStderr,
				*observedEventSeq,
				value.StderrChunk,
			); err != nil {
				return nil, fmt.Errorf("append pre-entrypoint stderr: %w", err)
			}
		case *runv0.RunEvent_EntrypointReady:
			return value.EntrypointReady, nil
		default:
			return nil, errors.New(
				"Program emitted an unsupported event before entrypoint-ready",
			)
		}
	}
}

func validateFreshProgramClaim(
	claim *api.WorkerRunLeaseClaimResponse,
) (*api.WorkerRunLeaseFresh, error) {
	lease := claim.Lease
	if strings.TrimSpace(lease.ID) == "" ||
		strings.TrimSpace(lease.RunID) == "" ||
		lease.AttemptNumber <= 0 ||
		strings.TrimSpace(lease.WorkspaceID) == "" ||
		strings.TrimSpace(lease.WorkspaceMountID) == "" ||
		strings.TrimSpace(lease.WorkspaceLeaseID) == "" ||
		lease.MountFencingGeneration <= 0 ||
		lease.StartDeadlineAt.IsZero() ||
		!lease.StartDeadlineAt.After(time.Now()) {
		return nil, errors.New("fresh Program Run Lease receipt is incomplete")
	}
	if strings.TrimSpace(claim.Workspace.WriteCapability) == "" {
		return nil, errors.New(
			"fresh Program Workspace write capability is required",
		)
	}
	execution := claim.Execution
	if execution.Fresh == nil ||
		execution.Restore != nil ||
		execution.Attach != nil ||
		len(execution.Fresh.ProgramStart) == 0 {
		return nil, errors.New(
			"Run Lease execution must contain exactly one fresh Program",
		)
	}
	if len(claim.Secrets) > maxFreshProgramSecrets {
		return nil, fmt.Errorf(
			"fresh Program has %d Secrets, exceeds max %d",
			len(claim.Secrets),
			maxFreshProgramSecrets,
		)
	}
	totalSecretBytes := 0
	for _, secret := range claim.Secrets {
		if len(secret.Value) > maxFreshProgramSecretBytes-totalSecretBytes {
			return nil, fmt.Errorf(
				"fresh Program Secret plaintext exceeds max %d bytes",
				maxFreshProgramSecretBytes,
			)
		}
		totalSecretBytes += len(secret.Value)
	}
	return execution.Fresh, nil
}

func writeFreshProgramAdmission(
	stream vm.Stream,
	channelToken string,
	claim *api.WorkerRunLeaseClaimResponse,
	fresh *api.WorkerRunLeaseFresh,
) error {
	lease := claim.Lease
	channelToken = strings.TrimSpace(channelToken)
	if channelToken == "" {
		return errors.New("Workspace mount guest channel token is required")
	}
	if err := wire.WriteStreamFrameHeader(
		stream,
		wire.StreamHeader{
			Type:             wire.StreamTypeProgramRun,
			RunID:            lease.RunID,
			WorkspaceID:      lease.WorkspaceID,
			WorkspaceMountID: lease.WorkspaceMountID,
		},
		0,
	); err != nil {
		return fmt.Errorf("write Program run header: %w", err)
	}
	if err := frameio.WriteProtoFrame(
		stream,
		&workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  lease.WorkspaceMountID,
			WorkspaceId:       lease.WorkspaceID,
			ChannelToken:      channelToken,
			FencingGeneration: uint64(lease.MountFencingGeneration),
			WriteLeaseId:      lease.WorkspaceLeaseID,
			FencingToken:      claim.Workspace.WriteCapability,
		},
	); err != nil {
		return fmt.Errorf("write Program Workspace envelope: %w", err)
	}
	if err := frameio.WriteProtoFrame(
		stream,
		&runv0.ProgramRunRequest{
			RunId:               lease.RunID,
			AttemptNumber:       uint32(lease.AttemptNumber),
			RunLeaseId:          lease.ID,
			ProgramStartFrame:   fresh.ProgramStart,
			SecretCount:         uint32(len(claim.Secrets)),
			StartDeadlineUnixMs: lease.StartDeadlineAt.UnixMilli(),
		},
	); err != nil {
		return fmt.Errorf("write Program run request: %w", err)
	}
	for index := range claim.Secrets {
		secret, err := freshProgramSecret(claim.Secrets[index])
		if err != nil {
			return err
		}
		writeErr := frameio.WriteProtoFrame(
			stream,
			&runv0.ProgramSupervisorCommand{
				Command: &runv0.ProgramSupervisorCommand_SecretDelivery{
					SecretDelivery: secret,
				},
			},
		)
		clearBytes(secret.Value)
		secret.Value = nil
		if writeErr != nil {
			return fmt.Errorf("write Program Secret delivery: %w", writeErr)
		}
		clearBytes(claim.Secrets[index].Value)
		claim.Secrets[index].Value = nil
	}
	if err := frameio.WriteProtoFrame(
		stream,
		&runv0.ProgramSupervisorCommand{
			Command: &runv0.ProgramSupervisorCommand_SecretsComplete{
				SecretsComplete: &runv0.ProgramSecretsComplete{
					RunId:         lease.RunID,
					AttemptNumber: uint32(lease.AttemptNumber),
					RunLeaseId:    lease.ID,
					SecretCount:   uint32(len(claim.Secrets)),
				},
			},
		},
	); err != nil {
		return fmt.Errorf("write Program Secret completion: %w", err)
	}
	return nil
}

func freshProgramSecret(
	delivery api.WorkerSecretDelivery,
) (*runv0.ProgramSecret, error) {
	secret := &runv0.ProgramSecret{
		Value: append([]byte(nil), delivery.Value...),
	}
	switch {
	case delivery.Env != nil && delivery.File == nil:
		secret.Placement = &runv0.ProgramSecret_Env{
			Env: delivery.Env.Name,
		}
	case delivery.Env == nil && delivery.File != nil:
		secret.Placement = &runv0.ProgramSecret_File{
			File: delivery.File.Path,
		}
	default:
		clearBytes(secret.Value)
		return nil, errors.New(
			"Program Secret must contain exactly one placement",
		)
	}
	return secret, nil
}

func programStartRelease(
	lease api.WorkerRunLeaseReceipt,
) *runv0.ProgramSupervisorCommand {
	return &runv0.ProgramSupervisorCommand{
		Command: &runv0.ProgramSupervisorCommand_StartRelease{
			StartRelease: &runv0.ProgramStartRelease{
				RunId:         lease.RunID,
				AttemptNumber: uint32(lease.AttemptNumber),
				RunLeaseId:    lease.ID,
			},
		},
	}
}

func validateFreshEntrypoint(
	ready *runv0.EntrypointReady,
	lease api.WorkerRunLeaseReceipt,
) (string, error) {
	if ready == nil ||
		ready.GetRunId() != lease.RunID ||
		ready.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		ready.GetEntrypoint() == nil ||
		strings.TrimSpace(ready.GetEntrypoint().GetDeclaredId()) == "" {
		return "", errors.New(
			"entrypoint-ready event does not match Run Lease",
		)
	}
	switch ready.GetEntrypoint().GetKind().(type) {
	case *runv0.EntrypointIdentity_Task:
		return "task", nil
	case *runv0.EntrypointIdentity_Actor:
		return "actor", nil
	default:
		return "", errors.New("entrypoint-ready kind is unsupported")
	}
}

func equalRunLeaseReceipt(
	left api.WorkerRunLeaseReceipt,
	right api.WorkerRunLeaseReceipt,
) bool {
	if !left.StartDeadlineAt.Equal(right.StartDeadlineAt) ||
		!left.ExpiresAt.Equal(right.ExpiresAt) {
		return false
	}
	left.StartDeadlineAt = time.Time{}
	left.ExpiresAt = time.Time{}
	right.StartDeadlineAt = time.Time{}
	right.ExpiresAt = time.Time{}
	return left == right
}

func clearFreshProgramDelivery(claim *api.WorkerRunLeaseClaimResponse) {
	if claim == nil {
		return
	}
	if claim.Execution.Fresh != nil {
		clearBytes(claim.Execution.Fresh.ProgramStart)
		claim.Execution.Fresh.ProgramStart = nil
	}
	for index := range claim.Secrets {
		clearBytes(claim.Secrets[index].Value)
		claim.Secrets[index].Value = nil
	}
	claim.Workspace.WriteCapability = ""
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
