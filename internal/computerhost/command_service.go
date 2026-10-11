package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// commandAuthority identifies the physical writer independently of any Session.
type commandAuthority struct {
	EnvironmentID, ComputerID, ComputerInstanceID, GuestChannelCredential string
	WriterGeneration                                                      int64
}

type commandService struct {
	PollEvery, ClaimErrorBackoff, CompleteErrorBackoff time.Duration
	LogLimits                                          *computerv0.CommandLogLimits
}

func (m commandService) Serve(ctx context.Context, instance vm.Machine, mount commandAuthority, client workerapi.ComputerCommandClient) error {
	commandCtx, stopCommands := context.WithCancel(ctx)
	var commands sync.WaitGroup
	defer func() { stopCommands(); commands.Wait() }()
	type commandResult struct {
		id    string
		err   error
		fatal bool
	}
	commandResults := make(chan commandResult)
	activeCommands := make(map[string]struct{})
	cancellations := make(map[string]bool)
	cancellationResults := make(chan commandResult)
	pollEvery := m.PollEvery
	if pollEvery <= 0 {
		pollEvery = 500 * time.Millisecond
	}
	claimErrorBackoff := m.ClaimErrorBackoff
	if claimErrorBackoff <= 0 {
		claimErrorBackoff = 2 * time.Second
	}
	poll := time.NewTimer(0)
	defer poll.Stop()
	for {
		select {
		case <-commandCtx.Done():
			return context.Cause(commandCtx)
		case result := <-cancellationResults:
			_, active := activeCommands[result.id]
			if result.err == nil && active {
				cancellations[result.id] = true
			} else {
				delete(cancellations, result.id)
			}
		case result := <-commandResults:
			if result.err != nil && commandCtx.Err() == nil {
				slog.WarnContext(commandCtx, "Computer command attempt failed", "command_id", result.id, "fatal", result.fatal, "error", result.err)
			}
			delete(activeCommands, result.id)
			if cancellations[result.id] {
				delete(cancellations, result.id)
			}
			if result.fatal {
				return result.err
			}
			if result.err != nil {
				poll.Reset(claimErrorBackoff)
			}
		case <-poll.C:
			activeIDs := make([]string, 0, len(activeCommands))
			for id := range activeCommands {
				activeIDs = append(activeIDs, id)
			}
			cancellationIDs := make([]string, 0, len(cancellations))
			for id := range cancellations {
				cancellationIDs = append(cancellationIDs, id)
			}
			claimed, err := client.ClaimComputerCommand(commandCtx, workerapi.ComputerCommandClaimRequest{
				EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration, ActiveCommandIDs: activeIDs, ActiveCancellationIDs: cancellationIDs,
			})
			if err != nil {
				if commandCtx.Err() == nil {
					slog.WarnContext(commandCtx, "Computer command claim failed", "computer_id", mount.ComputerID, "error", err)
				}
				poll.Reset(claimErrorBackoff)
				continue
			}
			if request := claimed.Cancellation; request != nil {
				id := request.CommandID
				if _, exists := cancellations[id]; !exists {
					_, attached := activeCommands[id]
					cancellations[id] = false
					if !attached {
						activeCommands[id] = struct{}{}
						cancellations[id] = true
					}
					commands.Add(1)
					go func() {
						defer commands.Done()
						result := commandResult{id: id}
						result.err = m.cancelComputerCommand(commandCtx, instance, mount, *request)
						results := cancellationResults
						if !attached {
							results = commandResults
							if result.err == nil {
								// Cancellation installed or found a replay record; this request cannot launch a process.
								replay := workerapi.ComputerCommand{TailOnly: true, CommandID: id, ComputerID: request.ComputerID, ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration, RequestFingerprint: request.RequestFingerprint, ExpiresAt: request.ExpiresAt, Request: json.RawMessage(`{}`)}
								completion, err := m.dispatchComputerBasicExec(commandCtx, instance, mount, replay, client)
								result.err = err
								var protocolError *computerBasicExecProtocolError
								result.fatal = errors.As(err, &protocolError)
								if err == nil {
									result.err = m.completeComputerBasicExec(commandCtx, client, completion)
									result.fatal = result.err != nil
								}
							}
						}
						select {
						case results <- result:
						case <-commandCtx.Done():
						}
					}()
				}
				poll.Reset(pollEvery)
				continue
			}
			var commandID string
			switch {
			case claimed.Release != nil:
				commandID = claimed.Release.Completion.CommandID
			case claimed.Command != nil:
				commandID = claimed.Command.CommandID
			default:
				poll.Reset(pollEvery)
				continue
			}
			if _, active := activeCommands[commandID]; !active {
				activeCommands[commandID] = struct{}{}
				commands.Add(1)
				go func() {
					defer commands.Done()
					result := commandResult{id: commandID}
					if claimed.Release != nil {
						result.err = m.releaseComputerCommand(commandCtx, instance, mount, *claimed.Release, client)
					} else {
						completion, err := m.dispatchComputerBasicExec(commandCtx, instance, mount, *claimed.Command, client)
						result.err = err
						var protocolError *computerBasicExecProtocolError
						result.fatal = errors.As(err, &protocolError)
						if err == nil {
							result.err = m.completeComputerBasicExec(commandCtx, client, completion)
							result.fatal = result.err != nil
						}
					}
					select {
					case commandResults <- result:
					case <-commandCtx.Done():
					}
				}()
			} else if claimed.Command != nil {
				clear(claimed.Command.Stdin)
				for _, secret := range claimed.Command.Secrets {
					clear(secret.Value)
				}
			}
			poll.Reset(pollEvery)
		}
	}
}

type computerBasicExecProtocolError struct {
	err error
}

func (e *computerBasicExecProtocolError) Error() string {
	return e.err.Error()
}

func (e *computerBasicExecProtocolError) Unwrap() error {
	return e.err
}

func computerBasicExecProtocol(err error) error {
	return &computerBasicExecProtocolError{err: err}
}

func (m commandService) dispatchComputerBasicExec(
	ctx context.Context,
	machine vm.Machine,
	mount commandAuthority,
	exec workerapi.ComputerCommand,
	client workerapi.ComputerCommandClient,
) (workerapi.ComputerCommandCompleteRequest, error) {
	defer func() {
		clear(exec.Stdin)
		for index := range exec.Secrets {
			clear(exec.Secrets[index].Value)
		}
	}()
	channelCredential := strings.TrimSpace(mount.GuestChannelCredential)
	if channelCredential == "" {
		return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(
			errors.New("computer mount guest channel credential is required"),
		)
	}
	if strings.TrimSpace(exec.CommandID) == "" || strings.TrimSpace(exec.RequestFingerprint) == "" {
		return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(errors.New("command claim is incomplete"))
	}
	if exec.ComputerInstanceID != mount.ComputerInstanceID || exec.ComputerID != mount.ComputerID || exec.WriterGeneration <= 0 || exec.WriterGeneration != mount.WriterGeneration || !exec.ExpiresAt.After(time.Now()) {
		return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(errors.New("command claim does not match the live Instance"))
	}
	request := &computerv0.ComputerBasicExecRequest{
		LogLimits:   m.LogLimits,
		TailOnly:    exec.TailOnly,
		Envelope:    &computerv0.ComputerCommandAuthority{OperationId: exec.CommandID, ComputerInstanceId: exec.ComputerInstanceID, ComputerId: exec.ComputerID, ChannelCredential: channelCredential, WriterGeneration: exec.WriterGeneration, OperationExpiresAtUnixNano: exec.ExpiresAt.UnixNano(), RequestFingerprint: exec.RequestFingerprint},
		RequestJson: string(exec.Request), ProtectedEnv: exec.ProtectedEnv.Values(), ProxyCa: exec.ProtectedEnv.PublicCA(), Stdin: exec.Stdin,
	}
	for _, delivery := range exec.Secrets {
		secret := &computerv0.ComputerSecretDelivery{Value: delivery.Value}
		switch {
		case delivery.Env != nil && delivery.File == nil:
			secret.PlacementKind = "env"
			secret.PlacementTarget = strings.TrimSpace(delivery.Env.Name)
		case delivery.Env == nil && delivery.File != nil:
			secret.PlacementKind = "file"
			secret.PlacementTarget = strings.TrimSpace(delivery.File.Path)
		default:
			return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(
				errors.New("computer exec secret placement is invalid"),
			)
		}
		if secret.PlacementTarget == "" {
			return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(
				errors.New("computer exec secret placement target is required"),
			)
		}
		request.Secrets = append(request.Secrets, secret)
	}
	stream, err := machine.OpenStream(ctx)
	if err != nil {
		return workerapi.ComputerCommandCompleteRequest{}, fmt.Errorf("open computer exec stream: %w", err)
	}
	defer stream.Close()
	closed := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() { defer close(closed); _ = stream.Close() })
	defer func() {
		if !stopClose() {
			<-closed
		}
	}()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeComputerBasicExec,
		ComputerID:  mount.ComputerID,
		OperationID: exec.CommandID,
	}, 0); err != nil {
		return workerapi.ComputerCommandCompleteRequest{}, fmt.Errorf("write computer exec header: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return workerapi.ComputerCommandCompleteRequest{}, fmt.Errorf("write computer exec request: %w", err)
	}
	result, err := m.readCommandOutput(ctx, stream, mount, exec, client, func(result *computerv0.ComputerBasicExecResult) error {
		completion, err := commandCompletion(mount, exec, result)
		if err != nil {
			return err
		}
		return m.completeComputerBasicExec(ctx, client, completion)
	})
	if err != nil {
		return workerapi.ComputerCommandCompleteRequest{}, err
	}
	return commandCompletion(mount, exec, result)
}

func commandCompletion(mount commandAuthority, exec workerapi.ComputerCommand, result *computerv0.ComputerBasicExecResult) (workerapi.ComputerCommandCompleteRequest, error) {
	if strings.TrimSpace(result.GetRequestFingerprint()) !=
		strings.TrimSpace(exec.RequestFingerprint) {
		return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(
			errors.New("computer exec result fingerprint does not match its claim"),
		)
	}
	outcome := strings.TrimSpace(result.GetOutcome())
	if err := validateComputerBasicCommandOutcome(outcome); err != nil {
		return workerapi.ComputerCommandCompleteRequest{}, err
	}
	completion := workerapi.ComputerCommandCompleteRequest{
		Stdout:             workerapi.CommandOutputBoundary{ThroughSequence: int64(result.GetStdout().GetThroughSequence()), Complete: result.GetStdout().GetComplete(), Gapped: result.GetStdout().GetGapped()},
		Stderr:             workerapi.CommandOutputBoundary{ThroughSequence: int64(result.GetStderr().GetThroughSequence()), Complete: result.GetStderr().GetComplete(), Gapped: result.GetStderr().GetGapped()},
		EnvironmentID:      mount.EnvironmentID,
		CommandID:          exec.CommandID,
		ComputerInstanceID: mount.ComputerInstanceID,
		WriterGeneration:   exec.WriterGeneration,
		Outcome:            outcome,
	}
	if outcome == "exited" {
		exitCode := result.GetExitCode()
		completion.ExitCode = &exitCode
		if strings.TrimSpace(result.GetErrorJson()) != "" {
			return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(
				errors.New("exited computer exec returned an error"),
			)
		}
		return completion, nil
	}
	if strings.TrimSpace(result.GetErrorJson()) == "" ||
		!json.Valid([]byte(result.GetErrorJson())) {
		return workerapi.ComputerCommandCompleteRequest{}, computerBasicExecProtocol(
			errors.New("failed computer exec returned invalid error JSON"),
		)
	}
	completion.Error = json.RawMessage(result.GetErrorJson())
	return completion, nil
}

func validateComputerBasicCommandOutcome(outcome string) error {
	switch outcome {
	case "exited",
		"computer_command_cancelled",
		"computer_command_failed",
		"computer_command_timed_out",
		"computer_command_signaled",
		"computer_command_launch_failed",
		"computer_command_scope_termination_failed",
		"computer_command_secret_delivery_failed",
		"computer_command_output_capture_failed",
		"computer_command_result_uncertain":
		return nil
	case "computer_command_fenced",
		"computer_command_expired",
		"computer_command_invalid",
		"computer_command_fingerprint_conflict",
		"computer_command_unavailable":
		return computerBasicExecProtocol(
			fmt.Errorf("computer exec guest rejected authority: %s", outcome),
		)
	default:
		return computerBasicExecProtocol(
			fmt.Errorf("computer exec result outcome %q is unsupported", outcome),
		)
	}
}

func (m commandService) completeComputerBasicExec(
	ctx context.Context,
	client workerapi.ComputerCommandClient,
	request workerapi.ComputerCommandCompleteRequest,
) error {
	backoff := m.CompleteErrorBackoff
	if backoff <= 0 {
		backoff = 250 * time.Millisecond
	}
	for {
		err := client.CompleteComputerCommand(ctx, request)
		if err == nil {
			return nil
		}
		if !computerCommandCompletionRetryable(err) {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func computerCommandCompletionRetryable(err error) bool {
	var statusError interface{ HTTPStatusCode() int }
	if !errors.As(err, &statusError) {
		return true
	}
	status := statusError.HTTPStatusCode()
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}
