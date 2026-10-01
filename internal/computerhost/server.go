package computerhost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/localcache"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

const computerStartupTimeout = 20 * time.Minute

type Server struct {
	RestoreControl        ComputerRestoreControl
	ComputerSaves         ComputerSaveClient
	ComputerSaveEvery     time.Duration
	CAS                   cas.Store
	ComputerObjects       cas.ImmutableStore
	Mounts                *Mounts
	TempDir               string
	Heartbeat             time.Duration
	StartupTimeout        time.Duration
	FailureTimeout        time.Duration
	PollEvery             time.Duration
	ClaimErrorBackoff     time.Duration
	CompleteErrorBackoff  time.Duration
	Log                   *slog.Logger
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	Machines              *PreparedMachines
}

// NewServer returns m after verifying the collaborators every
// Computer mount needs. Timeouts, polling intervals, the logger and the
// artifact cache keep their defaults when unset.
func NewServer(m Server) (Server, error) {
	if err := m.validate(); err != nil {
		return Server{}, err
	}
	return m, nil
}

func (m Server) validate() error {
	if m.RestoreControl == nil {
		return errors.New("computer restore control plane is required")
	}
	if m.ComputerSaves == nil {
		return errors.New("computer save control plane is required")
	}
	if m.ComputerSaveEvery <= 0 {
		return errors.New("computer save interval must be positive")
	}
	if m.CAS == nil {
		return errors.New("computer server CAS is required")
	}
	if m.ComputerObjects == nil {
		return errors.New("computer object store is required")
	}
	if m.Mounts == nil {
		return errors.New("computer mount registry is required")
	}
	if m.Machines == nil {
		return errors.New("computer server prepared machines are required")
	}
	return nil
}

func (m Server) Serve(ctx context.Context, mount workerapi.ComputerInstanceAssignment, client workerapi.ComputerServerControlPlaneClient) (runErr error) {
	if mount.WriterGeneration <= 0 {
		return errors.New("computer Instance writer generation is required")
	}
	// A Server built without NewServer fails the mount here
	// instead of when a later phase first needs the missing collaborator.
	if err := m.validate(); err != nil {
		_ = m.failComputerMount(client, mount, err)
		return fmt.Errorf("computer server is misconfigured: %w", err)
	}
	totalStarted := time.Now()
	m.logComputerMountPhase(mount, "computer mount started", "state", "starting")
	renewEvery := m.Heartbeat
	if renewEvery <= 0 {
		renewEvery = 15 * time.Second
	}
	renewal := m.startRenewalLoop(ctx, workerapi.ComputerInstanceRenewRequest{
		EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration,
	}, client, renewEvery)
	defer renewal.stopAndWait()
	startupCtx, cancelStartup := context.WithTimeout(renewal.ctx, m.startupTimeout())
	defer cancelStartup()
	phaseStarted := time.Now()
	checkout, computerInstanceID, err := m.materializeMachine(startupCtx, &mount)
	m.logComputerMountPhase(mount, "computer mount machine materialized", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failComputerMount(client, renewal.authority(mount), err)
		return fmt.Errorf("checkout computer mount instance: %w", err)
	}
	rawMachine := checkout.Machine()
	instance := checkout.mount
	defer func() {
		if !checkout.beginTeardown() {
			// The claim has passed to checkpoint capture or to physical
			// cleanup, which excludes the source and reports the Instance.
			return
		}
		if closeErr := m.closeMachine(instance); closeErr != nil {
			failure := computerMountFailure{
				code: "computer_mount_instance_close_failed",
				err:  errors.New("computer mount instance cleanup failed"),
			}
			m.logComputerMountPhase(mount, "computer mount machine close failed", "error", closeErr.Error())
			// The Server no longer owns this machine. Retain its resources
			// until reconciliation proves physical cleanup.
			checkout.Relinquish()
			var priorFailure computerMountFailure
			if !errors.As(runErr, &priorFailure) || !priorFailure.reported {
				runErr = errors.Join(runErr, m.failComputerMount(client, renewal.authority(mount), failure))
			}
			runErr = errors.Join(runErr, fmt.Errorf("close computer mount instance: %w", closeErr))
			return
		}
		if releaseErr := checkout.Release(); releaseErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("release computer mount instance checkout: %w", releaseErr))
		}
	}()
	writerGeneration := checkout.writerGeneration
	if writerGeneration != mount.WriterGeneration {
		return errors.New("computer Instance writer differs from prepared machine")
	}
	phaseStarted = time.Now()
	if err := m.registerComputerMountContext(startupCtx, instance, mount, computerInstanceID); err != nil {
		m.logComputerMountPhase(mount, "computer mount guest registered", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		if renewalErr := renewal.stopAndWait(); renewalErr != nil {
			err = renewalErr
		}
		_ = m.failComputerMount(client, renewal.authority(mount), err)
		return err
	}
	m.logComputerMountPhase(mount, "computer mount guest registered", "duration_ms", time.Since(phaseStarted).Milliseconds())
	if mount.RestoreCheckpointID != "" {
		if err := m.activateRestore(startupCtx, instance, mount); err != nil {
			_ = m.failComputerMount(client, renewal.authority(mount), err)
			return fmt.Errorf("activate restored Computer: %w", err)
		}
	}
	if err := instance.saves.bind(workerapi.ComputerSaveBeginRequest{EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: writerGeneration}, mount.ComputerID); err != nil {
		return err
	}
	saveFailure := make(chan error, 1)
	saveResults, err := instance.saves.run(renewal.ctx, m.ComputerSaveEvery, m.ComputerSaves, m.ComputerObjects, func(ctx context.Context) (computerSaveCapture, error) {
		return captureComputerSave(ctx, rawMachine, mount.ComputerID)
	}, func(err error) { saveFailure <- err; renewal.cancel() })
	if err != nil {
		return fmt.Errorf("start Computer preservation: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.failureTimeout())
		defer cancel()
		_ = instance.saves.Quiesce(cleanupCtx)
		select {
		case failure := <-saveFailure:
			cause := computerMountFailure{code: "computer_preservation_failed", err: fmt.Errorf("computer preservation failed: %w", failure)}
			if ctx.Err() == nil && checkout.beginTeardown() {
				reportErr := m.failComputerMount(client, renewal.authority(mount), cause)
				cause.reported = reportErr == nil
				runErr = errors.Join(runErr, reportErr)
			}
			runErr = errors.Join(runErr, cause)
		default:
		}
	}()
	unregisterMount := m.Mounts.register(mount, instance, m.channelCredential(mount))
	defer unregisterMount()

	m.logComputerMountPhase(mount, "computer mount ready", "duration_ms", time.Since(totalStarted).Milliseconds())
	return m.serveComputerMount(ctx, renewal, instance, checkout, mount, client, saveResults)
}

func (m Server) serveComputerMount(
	ctx context.Context,
	renewal *computerMountRenewal,
	instance *instanceMount,
	checkout *machineCheckout,
	mount workerapi.ComputerInstanceAssignment,
	client workerapi.ComputerServerControlPlaneClient,
	saveResults <-chan error,
) error {
	commandCtx, stopCommands := context.WithCancel(renewal.ctx)
	var commands sync.WaitGroup
	defer func() { stopCommands(); commands.Wait() }()
	type commandResult struct {
		id    string
		err   error
		fatal bool
	}
	runCleanupResult := make(chan error, 1)
	commands.Add(1)
	go func() {
		defer commands.Done()
		runCleanupResult <- m.reconcileComputerRuns(commandCtx, instance, mount, client)
	}()
	commandResults := make(chan commandResult)
	activeCommands := make(map[string]struct{})
	// false means a cancellation RPC is in flight; true means Guest accepted it.
	cancellations := make(map[string]bool)
	cancellationResults := make(chan commandResult)
	machineExited := make(chan error, 1)
	go func() {
		machineExited <- instance.Wait(renewal.ctx)
	}()
	// Reporting a failure commits the Server to its own teardown; once capture
	// or physical cleanup owns the claim, that owner reports the Instance.
	failAndReturn := func(cause error) error {
		if ctx.Err() == nil && checkout.beginTeardown() {
			_ = m.failComputerMount(client, renewal.authority(mount), cause)
		}
		return cause
	}
	stopAndReturn := func() error {
		err := renewal.stopAndWait()
		if ctx.Err() == nil && err != nil {
			return failAndReturn(err)
		}
		return errors.Join(ctx.Err(), err)
	}
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
	checkpointReleased := func() error {
		_, releaseErr := instance.CheckpointReleaseResult(context.Background())
		_ = renewal.stopAndWait()
		// Once the Server no longer owns its claim, the claim's owner handles a
		// failed release.
		if releaseErr != nil && checkout.beginTeardown() {
			failure := computerMountFailure{
				code: "computer_mount_checkpoint_release_failed",
				err:  fmt.Errorf("release checkpoint source: %w", releaseErr),
			}
			reportErr := m.failComputerMount(client, renewal.authority(mount), failure)
			failure.reported = reportErr == nil
			return errors.Join(failure, reportErr)
		}
		return nil
	}
	renewDone := renewal.done
	renewUpdates := renewal.updates
	for {
		select {
		case <-ctx.Done():
			return stopAndReturn()
		case update := <-renewUpdates:
			switch strings.TrimSpace(update.DesiredState) {
			case "closed":
				if err := m.stopControlledComputerMount(renewal.ctx, instance, checkout, renewal.authority(mount), client); err != nil {
					return err
				}
				_ = renewal.stopAndWait()
				return nil
			}
		case err := <-renewDone:
			renewDone = nil
			renewal.once.Do(func() { renewal.err = err })
			if err != nil {
				return failAndReturn(err)
			}
		case <-instance.releaseForCheckpointDone:
			// Failed stop may leave Wait blocked forever. Report through the mount
			// owner so instance reconciliation retains and reclaims its checkout.
			return checkpointReleased()
		case err := <-machineExited:
			machineExited = nil
			if released, _ := instance.CheckpointReleaseResult(context.Background()); released {
				return checkpointReleased()
			}
			if !checkout.beginTeardown() {
				// The claim passed to capture or physical cleanup, which
				// reports the Instance.
				_ = renewal.stopAndWait()
				return nil
			}
			if renewal.ctx.Err() != nil {
				continue
			}
			if ctx.Err() != nil {
				return stopAndReturn()
			}
			if err == nil {
				err = errors.New("computer mount machine exited")
			}
			return failAndReturn(computerMountFailure{
				code: "computer_mount_vm_exited",
				err:  fmt.Errorf("computer mount VM exited: %w", err),
			})
		case request := <-instance.failureRequests:
			if !checkout.beginTeardown() {
				request.result <- errors.New("computer source is no longer owned by this server")
				return nil
			}
			failure := computerMountFailure{
				code: "computer_mount_program_start_failed",
				err:  errors.New("program process failed before start proof"),
			}
			closeErr := m.closeMachine(instance)
			if closeErr != nil {
				m.logComputerMountPhase(
					mount,
					"computer mount Program start failure cleanup failed",
					"error", closeErr.Error(),
				)
				failure = computerMountFailure{
					code: "computer_mount_instance_close_failed",
					err:  errors.New("computer mount instance cleanup failed"),
				}
			}
			reportErr := m.failComputerMount(client, renewal.authority(mount), failure)
			failure.reported = reportErr == nil
			request.result <- errors.Join(closeErr, reportErr)
			return failure
		case err := <-saveResults:
			instance.saves.mu.Lock()
			stopped := instance.saves.stopped
			instance.saves.mu.Unlock()
			if stopped {
				saveResults = nil
				continue
			}
			if renewal.ctx.Err() != nil {
				return stopAndReturn()
			}
			if err == nil {
				err = errors.New("computer preservation stopped unexpectedly")
			}
			return fmt.Errorf("computer preservation failed: %w", err)

		case err := <-runCleanupResult:
			if commandCtx.Err() != nil {
				runCleanupResult = nil
				continue
			}
			return failAndReturn(err)
		case result := <-cancellationResults:
			_, active := activeCommands[result.id]
			if result.err == nil && active {
				cancellations[result.id] = true
			} else {
				delete(cancellations, result.id)
			}
		case result := <-commandResults:
			delete(activeCommands, result.id)
			if cancellations[result.id] {
				delete(cancellations, result.id)
			}
			if result.fatal {
				return failAndReturn(result.err)
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
			claimed, err := client.ClaimComputerCommand(renewal.ctx, workerapi.ComputerCommandClaimRequest{
				OrgID: mount.OrgID, EnvironmentID: mount.EnvironmentID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: mount.WriterGeneration, ActiveCommandIDs: activeIDs, ActiveCancellationIDs: cancellationIDs,
			})
			if err != nil {
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
								replay := workerapi.ComputerCommand{CommandID: id, ComputerID: request.ComputerID, ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration, RequestFingerprint: request.RequestFingerprint, ExpiresAt: request.ExpiresAt, Request: json.RawMessage(`{}`)}
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

func (m Server) dispatchComputerBasicExec(
	ctx context.Context,
	machine vm.Machine,
	mount workerapi.ComputerInstanceAssignment,
	exec workerapi.ComputerCommand,
	client workerapi.ComputerServerControlPlaneClient,
) (workerapi.ComputerCommandCompleteRequest, error) {
	defer func() {
		clear(exec.Stdin)
		for index := range exec.Secrets {
			clear(exec.Secrets[index].Value)
		}
	}()
	channelCredential := m.channelCredential(mount)
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
	result, err := m.readCommandOutput(ctx, stream, mount, exec, client)
	if err != nil {
		return workerapi.ComputerCommandCompleteRequest{}, err
	}
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
		OrgID:              mount.OrgID,
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

func (m Server) completeComputerBasicExec(
	ctx context.Context,
	client workerapi.ComputerServerControlPlaneClient,
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

func (m Server) logComputerMountPhase(mount workerapi.ComputerInstanceAssignment, message string, attrs ...any) {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	base := []any{
		"computer_id", strings.TrimSpace(mount.ComputerID),
		"computer_instance_id", strings.TrimSpace(mount.ComputerInstanceID),
		"org_id", strings.TrimSpace(mount.OrgID),
		"project_id", strings.TrimSpace(mount.ProjectID),
		"environment_id", strings.TrimSpace(mount.EnvironmentID),
	}
	base = append(base, attrs...)
	log.Info(message, base...)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type computerMountRenewal struct {
	mu      sync.Mutex
	latest  workerapi.ComputerInstanceRenewResponse
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan error
	updates chan workerapi.ComputerInstanceRenewResponse
	once    sync.Once
	err     error
}

// authority snapshots control versions without mutating the descriptor shared
// with Command goroutines. Startup and deferred cleanup use the same observation.
func (r *computerMountRenewal) authority(mount workerapi.ComputerInstanceAssignment) workerapi.ComputerInstanceAssignment {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest.DesiredVersion >= mount.DesiredVersion && r.latest.ObservedVersion >= mount.ObservedVersion && r.latest.DesiredVersion > 0 {
		mount.DesiredVersion = r.latest.DesiredVersion
		mount.ObservedVersion = r.latest.ObservedVersion
	}
	return mount
}

func (r *computerMountRenewal) stopAndWait() error {
	r.once.Do(func() {
		r.cancel()
		r.err = <-r.done
	})
	return r.err
}

func (m Server) startRenewalLoop(ctx context.Context, request workerapi.ComputerInstanceRenewRequest, client workerapi.ComputerServerControlPlaneClient, every time.Duration) *computerMountRenewal {
	renewCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	updates := make(chan workerapi.ComputerInstanceRenewResponse, 1)
	renewal := &computerMountRenewal{ctx: renewCtx, cancel: cancel, done: done, updates: updates}
	go func() {
		var err error
		defer func() { done <- err }()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				response, renewErr := client.RenewComputerInstance(renewCtx, request)
				if renewErr == nil {
					renewErr = validateInstanceRenewal(request, response, time.Now())
				}
				if renewErr != nil {
					err = fmt.Errorf("renew computer mount: %w", renewErr)
					cancel()
					return
				}
				renewal.mu.Lock()
				renewal.latest = response
				renewal.mu.Unlock()
				select {
				case updates <- response:
				default:
				}
			}
		}
	}()
	return renewal
}

type computerMountFailure struct {
	code     string
	err      error
	reported bool
}

func (e computerMountFailure) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func (e computerMountFailure) Unwrap() error {
	return e.err
}

func (m Server) materializeMachine(ctx context.Context, mount *workerapi.ComputerInstanceAssignment) (*machineCheckout, string, error) {
	if mount == nil {
		return nil, "", computerMountFailure{code: "computer_mount_missing", err: errors.New("computer mount is required")}
	}
	mount.ComputerInstanceID = strings.TrimSpace(mount.ComputerInstanceID)
	if mount.ComputerInstanceID == "" {
		return nil, "", computerMountFailure{code: "computer_instance_missing", err: errors.New("computer mount claim must include an instance id")}
	}
	if mount.WorkerEpoch <= 0 {
		return nil, "", computerMountFailure{code: "computer_instance_fence_missing", err: errors.New("computer mount claim must include the worker epoch")}
	}
	target := mount.Target
	if strings.TrimSpace(target.BaseComputerDiskVersionID) == "" {
		return nil, "", computerMountFailure{code: "computer_disk_version_missing", err: errors.New("computer mount target version is required")}
	}
	if strings.TrimSpace(mount.ComputerMountPath) == "" {
		return nil, "", computerMountFailure{code: "computer_mount_path_missing", err: errors.New("computer mount path is required")}
	}
	checkout, key, ok := m.Machines.checkout(ctx, *mount)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, key, err
		}
		return nil, key, computerMountFailure{
			code: "computer_instance_not_prepared",
			err:  fmt.Errorf("computer instance %q at worker epoch %d is not prepared", mount.ComputerInstanceID, mount.WorkerEpoch),
		}
	}
	machine := checkout.Machine()
	if machine == nil {
		err := errors.New("prepared computer instance machine is unavailable")
		if releaseErr := checkout.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release prepared computer instance checkout: %w", releaseErr))
		}
		return nil, key, computerMountFailure{code: "computer_instance_not_prepared", err: err}
	}
	releaseFailedCheckout := func(err error) error {
		if closeErr := m.closeMachine(checkout.mount); closeErr != nil {
			checkout.Relinquish()
			return errors.Join(err, computerMountFailure{
				code: "computer_mount_instance_close_failed",
				err:  fmt.Errorf("close prepared computer instance: %w", closeErr),
			})
		}
		if releaseErr := checkout.Release(); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release prepared computer instance checkout: %w", releaseErr))
		}
		return err
	}
	if strings.TrimSpace(mount.RestoreCheckpointID) != checkout.restoreCheckpointID {
		err := computerMountFailure{code: "computer_restore_checkpoint_mismatch", err: errors.New("computer mount restore checkpoint does not match prepared machine provenance")}
		return nil, key, releaseFailedCheckout(err)
	}
	m.logComputerMountPhase(*mount, "computer prepared machine checked out", "computer_instance_id", key)
	return checkout, key, nil
}

func (m Server) restoreCASObject(ctx context.Context, tempDir string, label string, artifact workerapi.CASObject) (string, func(), error) {
	cleanup := func() {}
	codeLabel := strings.ReplaceAll(label, "-", "_")
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return "", cleanup, computerMountFailure{code: codeLabel + "_artifact_missing", err: errors.New(label + " artifact digest is required")}
	}
	if artifact.SizeBytes <= 0 {
		return "", cleanup, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size_bytes must be positive", label)}
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType == "" {
		return "", cleanup, computerMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("%s artifact media_type is required", label)}
	}
	stat, err := m.CAS.Stat(ctx, digest)
	if err != nil {
		return "", cleanup, computerMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("stat %s artifact: %w", label, err)}
	}
	if stat.SizeBytes != artifact.SizeBytes || strings.TrimSpace(stat.MediaType) != mediaType {
		return "", cleanup, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact metadata mismatch", label)}
	}
	if cacheDir := strings.TrimSpace(m.ArtifactCacheDir); cacheDir != "" {
		return m.restoreCASObjectWithCache(ctx, tempDir, cacheDir, label, codeLabel, artifact)
	}
	return m.restoreCASObjectUncached(ctx, tempDir, label, codeLabel, artifact)
}

func (m Server) restoreCASObjectUncached(ctx context.Context, tempDir string, label string, codeLabel string, artifact workerapi.CASObject) (string, func(), error) {
	cleanup := func() {}
	digest := strings.TrimSpace(artifact.Digest)
	reader, err := m.CAS.Get(ctx, digest)
	if err != nil {
		return "", cleanup, computerMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("get %s artifact: %w", label, err)}
	}
	defer reader.Close()
	file, err := os.CreateTemp(tempDir, label+"-*")
	if err != nil {
		return "", cleanup, computerMountFailure{code: "computer_mount_temp_unavailable", err: fmt.Errorf("create %s artifact temp file: %w", label, err)}
	}
	path := file.Name()
	cleanup = func() { _ = os.Remove(path) }
	hash := sha256.New()
	written, copyErr := copyCASObject(ctx, io.MultiWriter(file, hash), reader, artifact.SizeBytes)
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("copy %s artifact: %w", label, copyErr)}
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("close %s artifact: %w", label, closeErr)}
	}
	if written != artifact.SizeBytes {
		cleanup()
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size mismatch", label)}
	}
	if sha256sum.DigestHash(hash) != digest {
		cleanup()
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact digest mismatch", label)}
	}
	return path, cleanup, nil
}

// copyCASObject never writes beyond the reserved descriptor size. Inspect one
// additional byte in memory to reject oversized bodies without filling host disk.
func copyCASObject(ctx context.Context, destination io.Writer, source io.Reader, size int64) (int64, error) {
	reader := &contextReader{ctx: ctx, reader: source}
	written, err := io.CopyN(destination, reader, size)
	if err != nil {
		return written, err
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if n != 0 {
		return written, errors.New("artifact body exceeds declared size")
	}
	if err != io.EOF {
		return written, err
	}
	return written, nil
}

func (m Server) restoreCASObjectWithCache(ctx context.Context, tempDir string, cacheDir string, label string, codeLabel string, artifact workerapi.CASObject) (string, func(), error) {
	cachePath, err := artifactCachePath(cacheDir, artifact.Digest)
	if err != nil {
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_missing", err: err}
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return "", func() {}, computerMountFailure{code: "computer_mount_cache_unavailable", err: fmt.Errorf("create %s artifact cache dir: %w", label, err)}
	}
	cacheRoot := filepath.Join(cacheDir, "sha256")
	var pinnedPath string
	var pinnedCleanup func()
	var pinnedSource *os.File
	err = localcache.WithRootLock(cacheRoot, func(lock localcache.RootLock) error {
		var pinErr error
		pinnedPath, pinnedSource, pinnedCleanup, pinErr = pinCachedArtifact(tempDir, label, cachePath)
		if pinErr != nil {
			return errArtifactCacheMiss
		}
		_ = localcache.Touch(cachePath)
		return nil
	})
	if err == nil {
		if finishCachedArtifact(pinnedPath, pinnedSource, artifact.SizeBytes) == nil && validateCachedArtifact(pinnedPath, artifact) == nil {
			return pinnedPath, pinnedCleanup, nil
		}
		pinnedCleanup()
	} else if !errors.Is(err, errArtifactCacheMiss) {
		return "", func() {}, computerMountFailure{code: "computer_mount_cache_unavailable", err: fmt.Errorf("open %s artifact cache: %w", label, err)}
	}
	reader, err := m.CAS.Get(ctx, strings.TrimSpace(artifact.Digest))
	if err != nil {
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_missing", err: fmt.Errorf("get %s artifact: %w", label, err)}
	}
	defer reader.Close()
	staged, err := os.CreateTemp(filepath.Dir(cachePath), ".staging-"+filepath.Base(cachePath)+"-*")
	if err != nil {
		return "", func() {}, computerMountFailure{code: "computer_mount_cache_unavailable", err: fmt.Errorf("stage %s artifact cache: %w", label, err)}
	}
	stagedPath := staged.Name()
	hash := sha256.New()
	written, copyErr := copyCASObject(ctx, io.MultiWriter(staged, hash), reader, artifact.SizeBytes)
	closeErr := staged.Close()
	if copyErr != nil {
		_ = os.Remove(stagedPath)
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("copy %s artifact: %w", label, copyErr)}
	}
	if closeErr != nil {
		_ = os.Remove(stagedPath)
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("close %s artifact cache: %w", label, closeErr)}
	}
	if written != artifact.SizeBytes {
		_ = os.Remove(stagedPath)
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact size mismatch", label)}
	}
	if sha256sum.DigestHash(hash) != strings.TrimSpace(artifact.Digest) {
		_ = os.Remove(stagedPath)
		return "", func() {}, computerMountFailure{code: codeLabel + "_artifact_corrupt", err: fmt.Errorf("%s artifact digest mismatch", label)}
	}
	if err := os.Chmod(stagedPath, 0o644); err != nil {
		_ = os.Remove(stagedPath)
		return "", func() {}, computerMountFailure{code: "computer_mount_cache_unavailable", err: fmt.Errorf("chmod %s artifact cache: %w", label, err)}
	}
	defer func() {
		if stagedPath != "" {
			_ = os.Remove(stagedPath)
		}
	}()
	err = localcache.WithRootLock(cacheRoot, func(lock localcache.RootLock) error {
		// Publish the verified inode even if another writer filled this digest key.
		if err := os.Rename(stagedPath, cachePath); err != nil {
			return fmt.Errorf("publish %s artifact cache: %w", label, err)
		}
		stagedPath = ""
		if _, err := lock.EnforceByteLimit(m.ArtifactCacheMaxBytes, cleanArtifactCachePreserveSet(map[string]bool{cachePath: true})); err != nil {
			return fmt.Errorf("evict %s artifact cache: %w", label, err)
		}
		var pinErr error
		pinnedPath, pinnedSource, pinnedCleanup, pinErr = pinCachedArtifact(tempDir, label, cachePath)
		return pinErr
	})
	if err != nil {
		return "", func() {}, computerMountFailure{code: "computer_mount_cache_unavailable", err: err}
	}
	if err := finishCachedArtifact(pinnedPath, pinnedSource, artifact.SizeBytes); err != nil {
		pinnedCleanup()
		return "", func() {}, computerMountFailure{code: "computer_mount_cache_unavailable", err: err}
	}
	return pinnedPath, pinnedCleanup, nil
}

// pinCachedArtifact runs under the cache root lock. A hardlink pins the inode
// directly at its final private path. If linking fails (for example across
// filesystems), an open FD pins the source until it can be copied outside lock.
func pinCachedArtifact(tempDir, label, cachePath string) (string, *os.File, func(), error) {
	info, err := os.Stat(cachePath)
	if err != nil {
		return "", nil, func() {}, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, func() {}, errors.New("cached artifact is not a regular file")
	}
	file, err := os.CreateTemp(tempDir, label+"-*")
	if err != nil {
		return "", nil, func() {}, err
	}
	path := file.Name()
	var source *os.File
	cleanup := func() {
		if source != nil {
			_ = source.Close()
		}
		_ = os.Remove(path)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	if err := os.Remove(path); err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	if err := os.Link(cachePath, path); err == nil {
		return path, nil, cleanup, nil
	}
	source, err = os.Open(cachePath)
	if err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	return path, source, cleanup, nil
}

func finishCachedArtifact(path string, source *os.File, expectedSize int64) error {
	if source == nil {
		return nil
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("cached artifact is not a regular file")
	}
	if info.Size() != expectedSize {
		return errors.New("cached artifact size mismatch")
	}
	target, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	return errors.Join(copyErr, closeErr)
}

var errArtifactCacheMiss = errors.New("artifact cache miss")

func artifactCachePath(cacheDir string, digest string) (string, error) {
	hash, ok := strings.CutPrefix(strings.TrimSpace(digest), "sha256:")
	if !ok || len(hash) != 64 {
		return "", fmt.Errorf("unsupported artifact digest %q", digest)
	}
	return filepath.Join(cacheDir, "sha256", hash), nil
}

func cleanArtifactCachePreserveSet(paths map[string]bool) map[string]bool {
	if len(paths) == 0 {
		return nil
	}
	cleaned := make(map[string]bool, len(paths))
	for path, keep := range paths {
		if keep {
			cleaned[filepath.Clean(path)] = true
		}
	}
	return cleaned
}

func validateCachedArtifact(path string, artifact workerapi.CASObject) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cached artifact is not a regular file")
	}
	if info.Size() != artifact.SizeBytes {
		return fmt.Errorf("cached artifact size mismatch")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if sha256sum.DigestHash(hash) != strings.TrimSpace(artifact.Digest) {
		return fmt.Errorf("cached artifact digest mismatch")
	}
	return nil
}

func (m Server) registerComputerMount(ctx context.Context, machine vm.Machine, mount workerapi.ComputerInstanceAssignment, computerInstanceID string) error {
	channelCredential := m.channelCredential(mount)
	if channelCredential == "" {
		return errors.New("computer mount guest channel credential is required")
	}
	if strings.TrimSpace(mount.GuestChannelCredentialHash) == "" {
		return errors.New("computer mount guest channel credential hash is required")
	}
	stream, err := machine.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open prepared machine materialize stream: %w", err)
	}
	defer stream.Close()
	phaseStarted := time.Now()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:       wire.StreamTypeComputerMaterialize,
		ComputerID: mount.ComputerID,
	}, 0); err != nil {
		m.logComputerMountPhase(mount, "computer mount header written", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return fmt.Errorf("write computer materialize header: %w", err)
	}
	m.logComputerMountPhase(mount, "computer mount header written", "duration_ms", time.Since(phaseStarted).Milliseconds())
	request := &computerv0.MaterializeComputerRequest{
		Envelope: &computerv0.ComputerOperationEnvelope{
			ComputerInstanceId: strings.TrimSpace(computerInstanceID), ComputerId: mount.ComputerID,
			ChannelCredential: channelCredential,
			WriterGeneration:  uint64(mount.WriterGeneration),
		},
		MountPath: strings.TrimSpace(mount.ComputerMountPath),
		Target:    computerMountTargetProto(mount.Target),
		ComputerImage: &computerv0.ComputerArtifact{
			Digest:    strings.TrimSpace(mount.ComputerImage.Digest),
			MediaType: strings.TrimSpace(mount.ComputerImage.MediaType),
			Encoding:  "oci-tar",
			SizeBytes: uint64(mount.ComputerImage.SizeBytes),
		},
		UsePreparedRuntime: true,

		RestoredCheckpointId: strings.TrimSpace(mount.RestoreCheckpointID),
	}
	phaseStarted = time.Now()
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		m.logComputerMountPhase(mount, "computer mount request written", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return fmt.Errorf("write computer materialize request: %w", err)
	}
	m.logComputerMountPhase(mount, "computer mount request written", "duration_ms", time.Since(phaseStarted).Milliseconds())
	m.logComputerMountPhase(mount, "computer image transfer skipped", "prepared_machine_hit", true, "computer_instance_id", computerInstanceID, "size_bytes", mount.ComputerImage.SizeBytes)
	var response computerv0.MaterializeComputerResponse
	phaseStarted = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, machine, stream, &response); err != nil {
		m.logComputerMountPhase(mount, "computer mount response read", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return fmt.Errorf("read computer materialize response: %w", err)
	}
	m.logComputerMountPhase(mount, "computer mount response read", "duration_ms", time.Since(phaseStarted).Milliseconds(), "state", strings.TrimSpace(response.Status))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		m.logComputerMountPhase(mount, "computer mount guest phase",
			"guest_phase", strings.TrimSpace(guestPhase.GetName()),
			"duration_ms", guestPhase.GetDurationMs(),
			"size_bytes", guestPhase.GetSizeBytes(),
			"entry_count", guestPhase.GetEntryCount(),
			"error", strings.TrimSpace(guestPhase.GetError()),
		)
	}
	if response.Status != "running" {
		if phaseError := computerMountPhaseError(response.GetPhases()); phaseError != "" {
			return fmt.Errorf("computer materialize returned state %q: %s", response.Status, phaseError)
		}
		return fmt.Errorf("computer materialize returned state %q", response.Status)
	}
	if !proto.Equal(response.GetTarget(), request.GetTarget()) {
		return errors.New("computer materialize response target does not match the requested exact target")
	}
	expectedHash := strings.TrimSpace(mount.GuestChannelCredentialHash)
	if strings.TrimSpace(response.GuestChannelCredentialHash) != expectedHash {
		return errors.New("computer materialize guest channel credential hash mismatch")
	}
	return nil
}

func computerMountTargetProto(target workerapi.ComputerMountTarget) *computerv0.ComputerMountTarget {
	return &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: target.BaseComputerDiskVersionID}
}

func computerMountPhaseError(phases []*computerv0.ComputerMountPhase) string {
	for i := len(phases) - 1; i >= 0; i-- {
		phase := phases[i]
		if phase == nil {
			continue
		}
		message := strings.TrimSpace(phase.GetError())
		if message == "" {
			continue
		}
		name := strings.TrimSpace(phase.GetName())
		if name == "" {
			return message
		}
		return name + ": " + message
	}
	return ""
}

func (m Server) registerComputerMountContext(ctx context.Context, machine vm.Machine, mount workerapi.ComputerInstanceAssignment, computerInstanceID string) error {
	result := make(chan error, 1)
	go func() {
		result <- m.registerComputerMount(ctx, machine, mount, computerInstanceID)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = m.closeMachine(machine)
		return ctx.Err()
	}
}

func (m Server) stopControlledComputerMount(ctx context.Context, machine vm.Machine, checkout *machineCheckout, mount workerapi.ComputerInstanceAssignment, client workerapi.ComputerServerControlPlaneClient) error {
	// Persistence is completed by Instance save/checkpoint publication before
	// its owner requests physical closure. Member completion cannot publish here.
	if !checkout.beginTeardown() {
		// The claim's current owner excludes the source and reports closure.
		return nil
	}
	if err := m.closeMachine(machine); err != nil {
		_ = m.failComputerMount(client, mount, computerMountFailure{
			code: "computer_mount_instance_close_failed",
			err:  fmt.Errorf("close computer instance: %w", err),
		})
		return fmt.Errorf("close computer instance: %w", err)
	}
	if err := checkout.Release(); err != nil {
		return fmt.Errorf("release computer mount resources: %w", err)
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.failureTimeout())
	defer cancel()
	if mount.ComputerInstanceID == "" || mount.WorkerEpoch <= 0 || mount.DesiredVersion <= 0 || mount.ObservedVersion < 0 {
		return errors.New("computer Instance close requires its observed authority")
	}
	request := workerapi.ComputerInstanceStateRequest{
		ID: mount.ComputerInstanceID, WorkerEpoch: mount.WorkerEpoch,
		DesiredVersion: mount.DesiredVersion, ExpectedObservedVersion: mount.ObservedVersion,
		CleanupProof: &workerapi.InstanceCleanupProof{Method: workerapi.InstanceCleanupMachineClosed, CompletedAt: time.Now().UTC()},
	}
	if err := retryControlRequest(stopCtx, func(ctx context.Context) error { _, err := client.MarkComputerInstanceClosed(ctx, request); return err }); err != nil {
		return fmt.Errorf("stop computer mount: %w", err)
	}
	return nil
}

func (m Server) startupTimeout() time.Duration {
	if m.StartupTimeout > 0 {
		return m.StartupTimeout
	}
	return computerStartupTimeout
}

func (m Server) failureTimeout() time.Duration {
	if m.FailureTimeout > 0 {
		return m.FailureTimeout
	}
	return 30 * time.Second
}

func (m Server) closeMachine(machine vm.Machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.failureTimeout())
	defer cancel()
	return machine.Close(ctx)
}

func (m Server) channelCredential(mount workerapi.ComputerInstanceAssignment) string {
	token := strings.TrimSpace(mount.GuestChannelCredential)
	if token == "" {
		return ""
	}
	return token
}

func (m Server) failComputerMount(client workerapi.ComputerServerControlPlaneClient, mount workerapi.ComputerInstanceAssignment, cause error) error {
	if mount.ComputerInstanceID == "" || mount.WorkerEpoch <= 0 || mount.DesiredVersion <= 0 || mount.ObservedVersion < 0 {
		return errors.New("computer Instance failure requires its observed authority")
	}
	body := computerMountError(cause)
	ctx, cancel := context.WithTimeout(context.Background(), m.failureTimeout())
	defer cancel()
	_, err := client.MarkComputerInstanceFailed(ctx, workerapi.ComputerInstanceStateRequest{
		ID: mount.ComputerInstanceID, WorkerEpoch: mount.WorkerEpoch,
		DesiredVersion: mount.DesiredVersion, ExpectedObservedVersion: mount.ObservedVersion,
		ReasonCode: workerapi.InstanceFailureReconcile, Error: body,
	})
	return err
}

func computerMountError(err error) json.RawMessage {
	code := "computer_mount_failed"
	var failure computerMountFailure
	if errors.As(err, &failure) && strings.TrimSpace(failure.code) != "" {
		code = strings.TrimSpace(failure.code)
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "computer_mount_startup_timeout"
	}
	body, marshalErr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Code:    code,
		Message: err.Error(),
	})
	if marshalErr != nil {
		return json.RawMessage(`{"code":"computer_mount_failed"}`)
	}
	return body
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(target []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(target)
}

func validateInstanceRenewal(request workerapi.ComputerInstanceRenewRequest, response workerapi.ComputerInstanceRenewResponse, now time.Time) error {
	if request.WriterGeneration <= 0 || response.ComputerInstanceID != request.ComputerInstanceID || response.WriterGeneration != request.WriterGeneration || response.DesiredVersion <= 0 || response.ObservedVersion < 0 {
		return errors.New("computer Instance renewal authority differs from request")
	}
	if response.DesiredState == "closed" {
		return nil
	}
	if response.DesiredState != "ready" || !response.WriterExpiresAt.After(now) {
		return errors.New("computer Instance renewal is expired or invalid")
	}
	return nil
}
