package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

const (
	defaultPreparedRuntimeInstanceTTL    = 5 * time.Minute
	defaultPreparedRuntimeControlTimeout = 15 * time.Second
)

var errPreparedRuntimeBackgroundBusy = errors.New("prepared runtime background work is busy")

type PreparedRuntimeInstanceClient interface {
	CreatePreparedRuntimeInstance(context.Context, api.WorkerPreparedRuntimeInstanceCreateRequest) (api.WorkerPreparedRuntimeInstanceCreateResponse, error)
	CreateRuntimePrepareInstance(context.Context, api.WorkerRuntimePrepareInstanceCreateRequest) (api.WorkerRuntimePrepareInstanceCreateResponse, error)
	RenewRuntimeInstance(context.Context, api.WorkerRuntimeInstanceRenewRequest) (api.WorkerRuntimeInstance, error)
	MarkRuntimeInstanceReady(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error)
	MarkRuntimeInstanceClosed(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error)
	MarkRuntimeInstanceFailed(context.Context, api.WorkerRuntimeInstanceStateRequest) (api.WorkerRuntimeInstance, error)
}

type RuntimePrepareCommandClient interface {
	PreparedRuntimeInstanceClient
	FollowWorkerCommands(context.Context, int64, func(api.WorkerCommand) error) error
	AcceptWorkerCommand(context.Context, int64) (api.WorkerCommandAcceptResponse, error)
	AcknowledgeWorkerCommand(context.Context, int64) (api.WorkerCommandAckResponse, error)
}

type PreparedRuntimePool struct {
	Connector             vm.Connector
	CAS                   cas.Store
	TempDir               string
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	Substrates            RuntimeSubstrateResolver
	RuntimeSubstrates     RuntimeSubstrateArtifactRegistrar
	CheckpointEncryptor   *checkpoint.Encryptor
	Network               compute.NetworkPolicy
	Size                  int
	ReservationTTL        time.Duration
	RuntimeInstances      PreparedRuntimeInstanceClient
	Log                   *slog.Logger
	BackgroundGate        *BackgroundWorkGate

	mu      sync.Mutex
	entries map[string][]preparedRuntimeEntry
	filling map[string]int
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
}

type preparedRuntimeEntry struct {
	session           vm.Session
	runtimeInstanceID string
	runtimeEpoch      int64
	instanceToken     string
	exit              *preparedRuntimeSessionExit
	ready             *preparedRuntimeReady
}

type preparedRuntimeReady struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newPreparedRuntimeReady() *preparedRuntimeReady {
	return &preparedRuntimeReady{done: make(chan struct{})}
}

func (r *preparedRuntimeReady) finish(err error) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		close(r.done)
	})
}

func (r *preparedRuntimeReady) wait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	return err
}

type preparedRuntimeSessionExit struct {
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newPreparedRuntimeSessionExit() *preparedRuntimeSessionExit {
	return &preparedRuntimeSessionExit{done: make(chan struct{})}
}

func (e *preparedRuntimeSessionExit) finish(err error) {
	if e == nil {
		return
	}
	e.once.Do(func() {
		e.mu.Lock()
		e.err = err
		e.mu.Unlock()
		close(e.done)
	})
}

func (e *preparedRuntimeSessionExit) exited() (error, bool) {
	if e == nil {
		return nil, false
	}
	select {
	case <-e.done:
		e.mu.Lock()
		err := e.err
		e.mu.Unlock()
		return err, true
	default:
		return nil, false
	}
}

func NewPreparedRuntimePool(connector vm.Connector, store cas.Store, size int, log *slog.Logger) *PreparedRuntimePool {
	ctx, cancel := context.WithCancel(context.Background())
	return &PreparedRuntimePool{
		Connector:      connector,
		CAS:            store,
		Size:           size,
		ReservationTTL: defaultPreparedRuntimeInstanceTTL,
		Log:            log,
		entries:        map[string][]preparedRuntimeEntry{},
		filling:        map[string]int{},
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (p *PreparedRuntimePool) Checkout(ctx context.Context, mount api.WorkerWorkspaceMount) (vm.Session, string, string, bool) {
	if p == nil || p.Size <= 0 {
		return nil, "", "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, p.Network)
	keyID := runtime.ID(key)
	runtimeInstanceID := strings.TrimSpace(mount.RuntimeInstanceID)
	if runtimeInstanceID == "" {
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "reason", "runtime_instance_missing")
		return nil, key, "", false
	}
	if mount.RuntimeEpoch <= 0 {
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "runtime_epoch_missing")
		return nil, key, "", false
	}
	p.mu.Lock()
	entries := p.entries[key]
	if len(entries) == 0 {
		p.mu.Unlock()
		return nil, key, "", false
	}
	index := -1
	for i := range entries {
		if entries[i].runtimeInstanceID == runtimeInstanceID && entries[i].runtimeEpoch == mount.RuntimeEpoch {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "runtime_epoch", mount.RuntimeEpoch, "reason", "reserved_session_missing")
		return nil, key, "", false
	}
	entry := entries[index]
	if err, exited := entry.exit.exited(); exited {
		p.mu.Unlock()
		p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), true)
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited")
		return nil, key, "", false
	}
	if strings.TrimSpace(entry.instanceToken) == "" {
		p.mu.Unlock()
		p.removeReadyEntry(key, entry, errors.New("prepared runtime entry missing runtime instance token"))
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "runtime_instance_token_missing")
		return nil, key, "", false
	}
	p.removeReadyEntryAtLocked(key, entries, index)
	available := p.readyCountLocked()
	p.mu.Unlock()
	if err := entry.ready.wait(ctx); err != nil {
		p.closeSession(context.Background(), entry.session)
		stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
		defer cancelState()
		if failErr := p.markRuntimeInstanceFailed(stateCtx, entry.runtimeInstanceID, entry.instanceToken, err); failErr != nil {
			p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", failErr.Error())
		}
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "runtime_ready_failed", "error", err.Error())
		return nil, key, "", false
	}
	if err, exited := entry.exit.exited(); exited {
		p.closeSession(context.Background(), entry.session)
		stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
		defer cancelState()
		if failErr := p.markRuntimeInstanceFailed(stateCtx, entry.runtimeInstanceID, entry.instanceToken, preparedRuntimeExitCause(err)); failErr != nil {
			p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", failErr.Error())
		}
		p.logInfo("prepared runtime pool miss", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "reason", "reserved_session_exited", "error", errorString(err))
		return nil, key, "", false
	}
	p.logInfo("prepared runtime pool hit", "runtime_key_id", keyID, "available", available)
	return entry.session, key, entry.instanceToken, true
}

func (p *PreparedRuntimePool) Refill(ctx context.Context, mount api.WorkerWorkspaceMount) {
	if p == nil || p.Size <= 0 || p.Connector == nil || p.CAS == nil {
		return
	}
	if p.RuntimeInstances == nil {
		key := preparedRuntimeKeyFromWorkspaceMount(mount, p.Network)
		p.logInfo("prepared runtime pool refill skipped", "runtime_key_id", runtime.ID(key), "reason", "runtime_instance_client_missing")
		return
	}
	p.pruneUnrenewableReadyEntries(ctx, p.RuntimeInstances)
	key := preparedRuntimeKeyFromWorkspaceMount(mount, p.Network)
	keyID := runtime.ID(key)
	refillCtx, finish, ok := p.beginBackground(ctx)
	if !ok {
		p.logInfo("prepared runtime pool refill skipped", "runtime_key_id", keyID, "reason", "foreground_workspace_mount_active")
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		finish()
		return
	}
	if p.reservedCountLocked() >= p.Size {
		p.mu.Unlock()
		finish()
		return
	}
	p.filling[key]++
	p.wg.Add(1)
	p.mu.Unlock()
	refillCtx, cancelRefill := p.withPoolContext(refillCtx)
	go func() {
		defer p.wg.Done()
		defer cancelRefill()
		defer finish()
		p.refillOne(refillCtx, key, mount)
	}()
}

func (p *PreparedRuntimePool) FollowWarmCommands(ctx context.Context, client RuntimePrepareCommandClient, capabilities api.WorkerCapabilities) error {
	if p == nil || p.Size <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	if client == nil {
		return errors.New("prepared runtime warm command client is required")
	}
	var afterID int64
	reconnectDelay := 100 * time.Millisecond
	for {
		err := client.FollowWorkerCommands(ctx, afterID, func(command api.WorkerCommand) error {
			switch command.Kind {
			case string(api.WorkerCommandKindRuntimeSubstratePrepare):
				if _, err := client.AcceptWorkerCommand(ctx, command.ID); err != nil {
					return err
				}
				if err := p.PrepareRuntimeSubstrateFromCommand(ctx, command); err != nil {
					p.logInfo("runtime substrate prepare command failed", "worker_command_id", command.ID, "error", err.Error())
					return err
				}
			case string(api.WorkerCommandKindRuntimePrepare):
				if _, err := client.AcceptWorkerCommand(ctx, command.ID); err != nil {
					return err
				}
				if err := p.WarmFromCommand(ctx, client, capabilities, command); err != nil {
					p.logInfo("prepared runtime warm command failed", "worker_command_id", command.ID, "error", err.Error())
					return err
				}
			case string(api.WorkerCommandKindRuntimeStop):
				if _, err := client.AcceptWorkerCommand(ctx, command.ID); err != nil {
					return err
				}
				if err := p.StopRuntimeFromCommand(ctx, command); err != nil {
					p.logInfo("runtime stop command failed", "worker_command_id", command.ID, "runtime_instance_id", command.RuntimeInstanceID, "runtime_epoch", command.RuntimeEpoch, "error", err.Error())
					return err
				}
			default:
				if command.ID > afterID {
					afterID = command.ID
				}
				return nil
			}
			if _, err := client.AcknowledgeWorkerCommand(ctx, command.ID); err != nil {
				return err
			}
			if command.ID > afterID {
				afterID = command.ID
			}
			return nil
		})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			p.logInfo("prepared runtime warm command stream failed", "error", err.Error())
		}
		if err := sleepWithContext(ctx, reconnectDelay); err != nil {
			return err
		}
		if reconnectDelay < time.Second {
			reconnectDelay *= 2
		}
	}
}

func (p *PreparedRuntimePool) StopRuntimeFromCommand(ctx context.Context, command api.WorkerCommand) error {
	if p == nil {
		return nil
	}
	runtimeInstanceID := strings.TrimSpace(command.RuntimeInstanceID)
	if runtimeInstanceID == "" {
		return errors.New("runtime stop command runtime_instance_id is required")
	}
	if command.RuntimeEpoch <= 0 {
		return errors.New("runtime stop command runtime_epoch is required")
	}
	stoppedKey, stoppedEntry, ok := p.claimReadyEntry(runtimeInstanceID, command.RuntimeEpoch)
	if !ok {
		p.logInfo("runtime stop command already closed or not held", "runtime_instance_id", runtimeInstanceID, "runtime_epoch", command.RuntimeEpoch)
		return nil
	}
	if stoppedEntry.session != nil {
		if err := stoppedEntry.session.Close(ctx); err != nil {
			stateCtx, cancelState := preparedRuntimeControlContext(ctx)
			defer cancelState()
			if markErr := p.markRuntimeInstanceFailed(stateCtx, stoppedEntry.runtimeInstanceID, stoppedEntry.instanceToken, err); markErr != nil {
				p.logInfo("runtime stop failed transition failed", "runtime_key_id", runtime.ID(stoppedKey), "runtime_instance_id", stoppedEntry.runtimeInstanceID, "error", markErr.Error())
				return errors.Join(err, markErr)
			}
			return nil
		}
	}
	if err := p.markRuntimeInstanceClosed(ctx, stoppedEntry.runtimeInstanceID, stoppedEntry.instanceToken); err != nil {
		return err
	}
	p.logInfo("runtime stop command closed prepared runtime", "runtime_key_id", runtime.ID(stoppedKey), "runtime_instance_id", stoppedEntry.runtimeInstanceID)
	return nil
}

func (p *PreparedRuntimePool) PrepareRuntimeSubstrateFromCommand(ctx context.Context, command api.WorkerCommand) error {
	if p == nil || p.CAS == nil || p.Substrates == nil {
		return nil
	}
	var directive api.WorkerRuntimeSubstratePrepareCommand
	if err := json.Unmarshal(command.Payload, &directive); err != nil {
		return fmt.Errorf("decode runtime substrate prepare command: %w", err)
	}
	mount := preparedRuntimeWorkspaceMountFromSource(directive.Source)
	if strings.TrimSpace(mount.DeploymentSandboxID) == "" {
		return errors.New("runtime substrate prepare command source is required")
	}
	if strings.TrimSpace(command.DeploymentSandboxID) == "" {
		return errors.New("runtime substrate prepare command deployment_sandbox_id is required")
	}
	if command.DeploymentSandboxID != mount.DeploymentSandboxID {
		return errors.New("runtime substrate prepare command target does not match source")
	}
	backgroundCtx, finish, ok := p.beginBackground(ctx)
	if !ok {
		p.logInfo("runtime substrate prepare skipped", "deployment_sandbox_id", mount.DeploymentSandboxID, "reason", "foreground_workspace_mount_active")
		return errPreparedRuntimeBackgroundBusy
	}
	defer finish()
	tempRoot := strings.TrimSpace(p.TempDir)
	if tempRoot == "" {
		tempRoot = os.TempDir()
	}
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(tempRoot, "runtime-substrate-prepare-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	materializer := WorkspaceMaterializer{
		CAS:                   p.CAS,
		TempDir:               p.TempDir,
		ArtifactCacheDir:      p.ArtifactCacheDir,
		ArtifactCacheMaxBytes: p.ArtifactCacheMaxBytes,
	}
	_, cleanupSandboxImage, topology, err := p.restoreSandboxImageAndRuntimeSubstrate(backgroundCtx, materializer, tempDir, mount,
		"runtime substrate prepare sandbox image restored",
		"runtime substrate prepared",
		"deployment_sandbox_id", mount.DeploymentSandboxID,
	)
	if err != nil {
		return err
	}
	defer cleanupSandboxImage()
	started := time.Now()
	registered, err := runtimeCheckpointer{
		cas:               p.CAS,
		encryptor:         p.CheckpointEncryptor,
		substrateSource:   runtimeSubstrateSourceFromPreparedSource(directive.Source),
		runtimeSubstrates: p.RuntimeSubstrates,
	}.ensureRuntimeSubstrateArtifact(backgroundCtx, topology.Substrate)
	p.logInfo("runtime substrate artifact registered", "deployment_sandbox_id", mount.DeploymentSandboxID, "duration_ms", time.Since(started).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "runtime_substrate_artifact_id", runtimeSubstrateArtifactID(registered), "error", errorString(err))
	return err
}

func (p *PreparedRuntimePool) WarmFromCommand(ctx context.Context, client PreparedRuntimeInstanceClient, _ api.WorkerCapabilities, command api.WorkerCommand) error {
	if p == nil {
		return nil
	}
	if client == nil {
		return errors.New("prepared runtime instance client is required")
	}
	var directive api.WorkerRuntimePrepareCommand
	if err := json.Unmarshal(command.Payload, &directive); err != nil {
		return fmt.Errorf("decode prepared runtime warm command: %w", err)
	}
	if strings.TrimSpace(directive.DeploymentSandboxID) == "" {
		return errors.New("prepared runtime warm command deployment_sandbox_id is required")
	}
	mount := preparedRuntimeWorkspaceMountFromSource(directive.Source)
	if strings.TrimSpace(mount.DeploymentSandboxID) == "" {
		return errors.New("prepared runtime warm command source is required")
	}
	key := preparedRuntimeKeyFromWorkspaceMount(mount, p.Network)
	keyID := runtime.ID(key)
	runtimeInstanceID := strings.TrimSpace(directive.RuntimeInstance.ID)
	runtimeEpoch := directive.RuntimeInstance.RuntimeEpoch
	if runtimeEpoch <= 0 {
		runtimeEpoch = 1
	}
	instanceToken := strings.TrimSpace(directive.RuntimeInstance.InstanceToken)
	if runtimeInstanceID == "" || instanceToken == "" {
		return errors.New("prepared runtime warm command runtime_instance id and instance_token are required")
	}
	if p.Size <= 0 || p.Connector == nil || p.CAS == nil {
		reason := errors.New("prepared runtime pool is not configured")
		p.logInfo("prepared runtime warm skipped", "runtime_key_id", keyID, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return markPreparedRuntimeCommandFailed(stateCtx, client, runtimeInstanceID, instanceToken, reason)
	}
	backgroundCtx, finish, ok := p.beginBackground(ctx)
	if !ok {
		reason := errors.New("foreground_workspace_mount_active")
		p.logInfo("prepared runtime warm skipped", "runtime_key_id", keyID, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return markPreparedRuntimeCommandFailed(stateCtx, client, runtimeInstanceID, instanceToken, reason)
	}
	defer finish()
	refillCtx, cancelRefill := p.withPoolContext(backgroundCtx)
	defer cancelRefill()
	p.pruneUnrenewableReadyEntries(refillCtx, client)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		reason := errors.New("prepared runtime pool closed")
		p.logInfo("prepared runtime warm skipped", "runtime_key_id", keyID, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return markPreparedRuntimeCommandFailed(stateCtx, client, runtimeInstanceID, instanceToken, reason)
	}
	if p.reservedCountLocked() >= p.Size {
		p.mu.Unlock()
		reason := errors.New("target_satisfied")
		p.logInfo("prepared runtime warm skipped", "runtime_key_id", keyID, "reason", reason.Error())
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		return markPreparedRuntimeCommandFailed(stateCtx, client, runtimeInstanceID, instanceToken, reason)
	}
	p.filling[key]++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.filling[key]--
		p.mu.Unlock()
	}()
	return p.prepareAndStore(refillCtx, key, mount, runtimeInstanceID, runtimeEpoch, instanceToken)
}

func (p *PreparedRuntimePool) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		if p.cancel != nil {
			p.cancel()
		}
	}
	var closingEntries []preparedRuntimeEntry
	for key, keyEntries := range p.entries {
		closingEntries = append(closingEntries, keyEntries...)
		delete(p.entries, key)
	}
	p.mu.Unlock()
	var err error
	for _, entry := range closingEntries {
		if closeErr := entry.session.Close(ctx); closeErr != nil {
			p.markRuntimeInstanceFailed(ctx, entry.runtimeInstanceID, entry.instanceToken, closeErr)
			err = closeErr
			continue
		}
		if closeErr := p.markRuntimeInstanceClosed(ctx, entry.runtimeInstanceID, entry.instanceToken); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	p.wg.Wait()
	return err
}

func (p *PreparedRuntimePool) refillOne(ctx context.Context, key string, mount api.WorkerWorkspaceMount) {
	keyID := runtime.ID(key)
	defer func() {
		p.mu.Lock()
		p.filling[key]--
		p.mu.Unlock()
	}()
	runtimeInstanceID := uuid.Must(uuid.NewV7()).String()
	instanceToken, err := runtime.NewInstanceToken()
	if err != nil {
		p.logInfo("prepared runtime pool refill failed", "runtime_key_id", keyID, "error", err.Error())
		return
	}
	instance, err := p.RuntimeInstances.CreatePreparedRuntimeInstance(ctx, api.WorkerPreparedRuntimeInstanceCreateRequest{
		ID:                 runtimeInstanceID,
		WorkspaceMountID:   mount.ID,
		GuestdChannelToken: mount.GuestdChannelToken,
		RuntimeKeyHash:     runtime.Hash(key),
		RuntimeKey:         json.RawMessage(key),
		NetworkPolicy:      compute.NetworkPolicyJSON(p.Network),
		InstanceToken:      instanceToken,
		ExpiresAt:          time.Now().Add(p.reservationTTL()),
	})
	if err != nil {
		p.logInfo("prepared runtime pool instance skipped", "runtime_key_id", keyID, "error", err.Error())
		return
	}
	runtimeInstanceID = strings.TrimSpace(instance.Instance.ID)
	runtimeEpoch := instance.Instance.RuntimeEpoch
	if runtimeEpoch <= 0 {
		runtimeEpoch = 1
	}
	instanceToken = strings.TrimSpace(instance.Instance.InstanceToken)
	if runtimeInstanceID == "" || instanceToken == "" {
		p.logInfo("prepared runtime pool instance failed", "runtime_key_id", keyID, "error", "runtime instance response missing id or token")
		return
	}
	if err := p.prepareAndStore(ctx, key, mount, runtimeInstanceID, runtimeEpoch, instanceToken); err != nil {
		p.logInfo("prepared runtime pool refill failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", err.Error())
	}
}

func (p *PreparedRuntimePool) prepareAndStore(ctx context.Context, key string, mount api.WorkerWorkspaceMount, runtimeInstanceID string, runtimeEpoch int64, instanceToken string) error {
	keyID := runtime.ID(key)
	if runtimeEpoch <= 0 {
		runtimeEpoch = 1
	}
	failInstance := func(err error) error {
		if err == nil {
			return nil
		}
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		if markErr := p.markRuntimeInstanceFailed(stateCtx, runtimeInstanceID, instanceToken, err); markErr != nil {
			p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", markErr.Error())
			return errors.Join(err, markErr)
		}
		return nil
	}
	materializer := WorkspaceMaterializer{
		Connector:             p.Connector,
		CAS:                   p.CAS,
		TempDir:               p.TempDir,
		ArtifactCacheDir:      p.ArtifactCacheDir,
		ArtifactCacheMaxBytes: p.ArtifactCacheMaxBytes,
		Substrates:            p.Substrates,
		Network:               p.Network,
		Log:                   p.Log,
	}
	tempDir := strings.TrimSpace(p.TempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return failInstance(err)
	}
	sandboxImagePath, cleanupSandboxImage, topology, err := p.restoreSandboxImageAndRuntimeSubstrate(ctx, materializer, tempDir, mount,
		"prepared runtime pool sandbox image restored",
		"prepared runtime pool substrate resolved",
		"runtime_key_id", keyID,
	)
	if err != nil {
		return failInstance(err)
	}
	defer cleanupSandboxImage()
	var runtimeSubstrateArtifactIDValue string
	if topology.Substrate != nil {
		started := time.Now()
		registered, err := runtimeCheckpointer{
			cas:               p.CAS,
			encryptor:         p.CheckpointEncryptor,
			substrateSource:   runtimeSubstrateSourceFromWorkspaceMount(mount),
			runtimeSubstrates: p.RuntimeSubstrates,
		}.ensureRuntimeSubstrateArtifact(ctx, topology.Substrate)
		p.logInfo("prepared runtime pool substrate artifact resolved", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "substrate_digest", runtimeSubstrateDigest(topology), "runtime_substrate_artifact_id", runtimeSubstrateArtifactID(registered), "error", errorString(err))
		if err != nil {
			return failInstance(err)
		}
		runtimeSubstrateArtifactIDValue = runtimeSubstrateArtifactID(registered)
	}
	connector, ok := p.Connector.(vm.MaterializingConnector)
	if !ok {
		err := errors.New("connector does not support mount")
		return failInstance(err)
	}
	started := time.Now()
	session, err := connector.Materialize(ctx, vm.MaterializeRequest{
		ID:                 "prepared-runtime-" + keyID,
		RootfsDigest:       mount.RootfsDigest,
		ImageDigest:        mount.ImageDigest,
		ImageFormat:        mount.ImageFormat,
		WorkspaceMountPath: mount.WorkspaceMountPath,
		Resources: compute.ResourceVector{
			MilliCPU:  mount.RequestedMilliCPU,
			MemoryMiB: mount.RequestedMemoryMiB,
			DiskMiB:   mount.RequestedDiskMiB,
			Slots:     mount.RequestedExecutionSlots,
		},
		Network:  p.Network,
		Topology: topology,
	})
	p.logInfo("prepared runtime pool session materialized", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "error", errorString(err))
	if err != nil {
		return failInstance(err)
	}
	keepSession := false
	defer func() {
		if !keepSession {
			p.closeSession(ctx, session)
		}
	}()
	if err := p.prepareGuestRuntime(ctx, session, key, mount, sandboxImagePath); err != nil {
		p.logInfo("prepared runtime pool guest prepare failed", "runtime_key_id", keyID, "error", err.Error())
		return failInstance(err)
	}
	entry := preparedRuntimeEntry{
		session:           session,
		runtimeInstanceID: runtimeInstanceID,
		runtimeEpoch:      runtimeEpoch,
		instanceToken:     instanceToken,
		exit:              newPreparedRuntimeSessionExit(),
		ready:             newPreparedRuntimeReady(),
	}
	p.monitorReadyEntry(key, entry)
	p.mu.Lock()
	if p.closed || p.readyCountLocked() >= p.Size {
		p.mu.Unlock()
		stateCtx, cancelState := preparedRuntimeControlContext(ctx)
		defer cancelState()
		if err := p.markRuntimeInstanceClosed(stateCtx, runtimeInstanceID, instanceToken); err != nil {
			p.logInfo("prepared runtime pool instance close transition failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", err.Error())
			return err
		}
		return nil
	}
	p.entries[key] = append(p.entries[key], entry)
	p.mu.Unlock()
	keepSession = true
	if err, exited := entry.exit.exited(); exited {
		if failErr := p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), true); failErr != nil {
			return errors.Join(preparedRuntimeExitCause(err), failErr)
		}
		return nil
	}
	if _, err := p.RuntimeInstances.MarkRuntimeInstanceReady(ctx, api.WorkerRuntimeInstanceStateRequest{
		ID:                         runtimeInstanceID,
		InstanceToken:              instanceToken,
		ExpiresAt:                  time.Now().Add(p.reservationTTL()),
		RuntimeSubstrateArtifactID: runtimeSubstrateArtifactIDValue,
	}); err != nil {
		entry.ready.finish(err)
		p.logInfo("prepared runtime pool instance ready transition failed", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "error", err.Error())
		if failErr := p.removeReadyEntryAndFail(key, entry, err, true); failErr != nil {
			return errors.Join(err, failErr)
		}
		return nil
	}
	entry.ready.finish(nil)

	p.mu.Lock()
	available := p.readyCountLocked()
	stillReady := false
	for _, candidate := range p.entries[key] {
		if candidate.runtimeInstanceID == entry.runtimeInstanceID && candidate.runtimeEpoch == entry.runtimeEpoch && candidate.instanceToken == entry.instanceToken {
			stillReady = true
			break
		}
	}
	p.mu.Unlock()
	if !stillReady {
		return nil
	}
	p.logInfo("prepared runtime pool refilled", "runtime_key_id", keyID, "runtime_instance_id", runtimeInstanceID, "available", available)
	return nil
}

func (p *PreparedRuntimePool) restoreSandboxImageAndRuntimeSubstrate(ctx context.Context, materializer WorkspaceMaterializer, tempDir string, mount api.WorkerWorkspaceMount, sandboxLogMessage string, substrateLogMessage string, logAttrs ...any) (string, func(), vm.RuntimeTopology, error) {
	started := time.Now()
	sandboxImagePath, cleanupSandboxImage, err := materializer.restoreCASObject(ctx, tempDir, "sandbox-image", mount.SandboxImageArtifact)
	p.logInfo(sandboxLogMessage, append(append([]any{}, logAttrs...),
		"duration_ms", time.Since(started).Milliseconds(),
		"size_bytes", mount.SandboxImageArtifact.SizeBytes,
		"error", errorString(err),
	)...)
	if err != nil {
		cleanupSandboxImage()
		return "", func() {}, vm.RuntimeTopology{}, err
	}
	started = time.Now()
	topology, err := runtimeSubstrateTopology(ctx, p.Substrates, sandboxImagePath, mount)
	p.logInfo(substrateLogMessage, append(append([]any{}, logAttrs...),
		"duration_ms", time.Since(started).Milliseconds(),
		"substrate_digest", runtimeSubstrateDigest(topology),
		"error", errorString(err),
	)...)
	if err != nil {
		cleanupSandboxImage()
		return "", func() {}, vm.RuntimeTopology{}, err
	}
	return sandboxImagePath, cleanupSandboxImage, topology, nil
}

func (p *PreparedRuntimePool) monitorReadyEntry(key string, entry preparedRuntimeEntry) {
	if p == nil || entry.session == nil || entry.exit == nil {
		return
	}
	p.wg.Go(func() {
		err := entry.session.Wait(p.ctx)
		entry.exit.finish(err)
		if p.ctx != nil && p.ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		p.removeReadyEntryAndFail(key, entry, preparedRuntimeExitCause(err), false)
	})
}

func preparedRuntimeExitCause(err error) error {
	if err != nil {
		return err
	}
	return errors.New("prepared runtime session exited")
}

func (p *PreparedRuntimePool) readyCountLocked() int {
	total := 0
	for _, entries := range p.entries {
		total += len(entries)
	}
	return total
}

func (p *PreparedRuntimePool) fillingCountLocked() int {
	total := 0
	for _, count := range p.filling {
		total += count
	}
	return total
}

func (p *PreparedRuntimePool) reservedCountLocked() int {
	return p.readyCountLocked() + p.fillingCountLocked()
}

func (p *PreparedRuntimePool) pruneUnrenewableReadyEntries(ctx context.Context, client PreparedRuntimeInstanceClient) {
	if p == nil || client == nil {
		return
	}
	type candidate struct {
		key   string
		entry preparedRuntimeEntry
	}
	p.mu.Lock()
	var candidates []candidate
	for key, entries := range p.entries {
		for _, entry := range entries {
			candidates = append(candidates, candidate{key: key, entry: entry})
		}
	}
	p.mu.Unlock()
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.entry.runtimeInstanceID) == "" || strings.TrimSpace(candidate.entry.instanceToken) == "" {
			p.removeReadyEntry(candidate.key, candidate.entry, errors.New("prepared runtime entry missing runtime instance id or token"))
			continue
		}
		_, err := client.RenewRuntimeInstance(ctx, api.WorkerRuntimeInstanceRenewRequest{
			ID:            candidate.entry.runtimeInstanceID,
			InstanceToken: candidate.entry.instanceToken,
			ExpiresAt:     time.Now().Add(p.reservationTTL()),
		})
		if err == nil {
			continue
		}
		p.removeReadyEntry(candidate.key, candidate.entry, fmt.Errorf("renew prepared runtime instance: %w", err))
	}
}

func (p *PreparedRuntimePool) removeReadyEntry(key string, entry preparedRuntimeEntry, cause error) {
	keyID := runtime.ID(key)
	if !p.forgetReadyEntry(key, entry) {
		return
	}
	if entry.session != nil {
		p.closeSession(context.Background(), entry.session)
	}
	p.logInfo("prepared runtime pool entry pruned", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", errorString(cause))
}

func (p *PreparedRuntimePool) removeReadyEntryAndFail(key string, entry preparedRuntimeEntry, cause error, closeSession bool) error {
	keyID := runtime.ID(key)
	if !p.forgetReadyEntry(key, entry) {
		return nil
	}
	if closeSession && entry.session != nil {
		p.closeSession(context.Background(), entry.session)
	}
	stateCtx, cancelState := preparedRuntimeControlContext(context.Background())
	defer cancelState()
	if err := p.markRuntimeInstanceFailed(stateCtx, entry.runtimeInstanceID, entry.instanceToken, cause); err != nil {
		p.logInfo("prepared runtime pool instance fail transition failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", err.Error())
		return err
	}
	p.logInfo("prepared runtime pool entry failed", "runtime_key_id", keyID, "runtime_instance_id", entry.runtimeInstanceID, "error", errorString(cause))
	return nil
}

func (p *PreparedRuntimePool) forgetReadyEntry(key string, entry preparedRuntimeEntry) bool {
	removed := false
	p.mu.Lock()
	entries := p.entries[key]
	for i := range entries {
		if entries[i].runtimeInstanceID == entry.runtimeInstanceID && entries[i].runtimeEpoch == entry.runtimeEpoch && entries[i].instanceToken == entry.instanceToken {
			p.removeReadyEntryAtLocked(key, entries, i)
			removed = true
			break
		}
	}
	p.mu.Unlock()
	return removed
}

func (p *PreparedRuntimePool) claimReadyEntry(runtimeInstanceID string, runtimeEpoch int64) (string, preparedRuntimeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entries := range p.entries {
		for i, entry := range entries {
			if entry.runtimeInstanceID != runtimeInstanceID || entry.runtimeEpoch != runtimeEpoch {
				continue
			}
			p.removeReadyEntryAtLocked(key, entries, i)
			return key, entry, true
		}
	}
	return "", preparedRuntimeEntry{}, false
}

func (p *PreparedRuntimePool) removeReadyEntryAtLocked(key string, entries []preparedRuntimeEntry, index int) {
	entries = append(entries[:index], entries[index+1:]...)
	if len(entries) == 0 {
		delete(p.entries, key)
		return
	}
	p.entries[key] = entries
}

func preparedRuntimeWorkspaceMountFromSource(source api.WorkerPreparedRuntimeSource) api.WorkerWorkspaceMount {
	return api.WorkerWorkspaceMount{
		ID:                         uuid.Must(uuid.NewV7()).String(),
		WorkspaceID:                uuid.Must(uuid.NewV7()).String(),
		DeploymentSandboxID:        strings.TrimSpace(source.DeploymentSandboxID),
		RuntimeID:                  strings.TrimSpace(source.RuntimeID),
		SandboxImageArtifact:       source.SandboxImageArtifact,
		SandboxImageArtifactFormat: strings.TrimSpace(source.SandboxImageArtifactFormat),
		RootfsDigest:               strings.TrimSpace(source.RootfsDigest),
		ImageDigest:                strings.TrimSpace(source.ImageDigest),
		ImageFormat:                strings.TrimSpace(source.ImageFormat),
		WorkspaceMountPath:         strings.TrimSpace(source.WorkspaceMountPath),
		RequestedMilliCPU:          int64(source.ReservedCpuMillis),
		RequestedMemoryMiB:         int64(source.ReservedMemoryMiB),
		RequestedDiskMiB:           source.ReservedDiskMiB,
		RequestedExecutionSlots:    source.ReservedExecutionSlots,
		RuntimeABI:                 strings.TrimSpace(source.RuntimeABI),
		GuestdABI:                  strings.TrimSpace(source.GuestdABI),
		AdapterABI:                 strings.TrimSpace(source.AdapterABI),
	}
}

func (p *PreparedRuntimePool) prepareGuestRuntime(ctx context.Context, session vm.Session, key string, mount api.WorkerWorkspaceMount, sandboxImagePath string) error {
	keyID := runtime.ID(key)
	stream, err := session.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("open prepared runtime stream: %w", err)
	}
	defer stream.Close()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceRuntimePrepare,
		WorkspaceID: mount.WorkspaceID,
	}, 0); err != nil {
		return fmt.Errorf("write prepared runtime header: %w", err)
	}
	request := &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeKey: key,
		MountPath:  strings.TrimSpace(mount.WorkspaceMountPath),
		SandboxArtifact: &workspacev0.WorkspaceArtifact{
			Digest:    strings.TrimSpace(mount.SandboxImageArtifact.Digest),
			MediaType: strings.TrimSpace(mount.SandboxImageArtifact.MediaType),
			Encoding:  strings.TrimSpace(mount.SandboxImageArtifactFormat),
			SizeBytes: uint64(mount.SandboxImageArtifact.SizeBytes),
		},
	}
	if err := frameio.WriteProtoFrame(stream, request); err != nil {
		return fmt.Errorf("write prepared runtime request: %w", err)
	}
	started := time.Now()
	if err := writeFileFrameWithMetadataContext(ctx, session, stream, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: mount.WorkspaceID,
	}, sandboxImagePath, strings.TrimSpace(mount.SandboxImageArtifact.Digest), mount.SandboxImageArtifact.SizeBytes); err != nil {
		return fmt.Errorf("write prepared runtime sandbox image: %w", err)
	}
	p.logInfo("prepared runtime pool sandbox image sent", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "size_bytes", mount.SandboxImageArtifact.SizeBytes)
	var response workspacev0.PrepareWorkspaceRuntimeResponse
	started = time.Now()
	if err := readProtoFrameFromReaderContext(ctx, session, stream, &response); err != nil {
		return fmt.Errorf("read prepared runtime response: %w", err)
	}
	p.logInfo("prepared runtime pool response read", "runtime_key_id", keyID, "duration_ms", time.Since(started).Milliseconds(), "state", strings.TrimSpace(response.State))
	for _, guestPhase := range response.GetPhases() {
		if guestPhase == nil {
			continue
		}
		p.logInfo("prepared runtime pool guest phase",
			"runtime_key_id", keyID,
			"guest_phase", strings.TrimSpace(guestPhase.GetName()),
			"duration_ms", guestPhase.GetDurationMs(),
			"size_bytes", guestPhase.GetSizeBytes(),
			"entry_count", guestPhase.GetEntryCount(),
			"error", strings.TrimSpace(guestPhase.GetError()),
		)
	}
	if response.State != "prepared" {
		if phaseError := workspaceMountPhaseError(response.GetPhases()); phaseError != "" {
			return fmt.Errorf("prepared runtime returned state %q: %s", response.State, phaseError)
		}
		return fmt.Errorf("prepared runtime returned state %q", response.State)
	}
	if strings.TrimSpace(response.RuntimeKey) != key {
		return errors.New("prepared runtime key mismatch")
	}
	return nil
}

func (p *PreparedRuntimePool) beginBackground(ctx context.Context) (context.Context, func(), bool) {
	if p == nil || p.BackgroundGate == nil {
		return ctx, func() {}, true
	}
	return p.BackgroundGate.BeginBackground(ctx)
}

func (p *PreparedRuntimePool) withPoolContext(parent context.Context) (context.Context, context.CancelFunc) {
	if p == nil || p.ctx == nil {
		return context.WithCancel(parent)
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-p.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func preparedRuntimeControlContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	} else {
		parent = context.WithoutCancel(parent)
	}
	return context.WithTimeout(parent, defaultPreparedRuntimeControlTimeout)
}

func (p *PreparedRuntimePool) closeSession(parent context.Context, session vm.Session) {
	if session == nil {
		return
	}
	ctx, cancel := preparedRuntimeControlContext(parent)
	defer cancel()
	_ = session.Close(ctx)
}

func (p *PreparedRuntimePool) reservationTTL() time.Duration {
	if p == nil || p.ReservationTTL <= 0 {
		return defaultPreparedRuntimeInstanceTTL
	}
	return p.ReservationTTL
}

func (p *PreparedRuntimePool) markRuntimeInstanceClosed(ctx context.Context, id string, token string) error {
	if p == nil || p.RuntimeInstances == nil || strings.TrimSpace(id) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	_, err := p.RuntimeInstances.MarkRuntimeInstanceClosed(ctx, api.WorkerRuntimeInstanceStateRequest{
		ID:            strings.TrimSpace(id),
		InstanceToken: strings.TrimSpace(token),
	})
	return err
}

func (p *PreparedRuntimePool) markRuntimeInstanceFailed(ctx context.Context, id string, token string, failure error) error {
	if p == nil {
		return nil
	}
	return markRuntimeInstanceFailed(ctx, p.RuntimeInstances, id, token, failure)
}

func markRuntimeInstanceFailed(ctx context.Context, client PreparedRuntimeInstanceClient, id string, token string, failure error) error {
	if client == nil || strings.TrimSpace(id) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"message": errorString(failure)})
	_, err := client.MarkRuntimeInstanceFailed(ctx, api.WorkerRuntimeInstanceStateRequest{
		ID:            strings.TrimSpace(id),
		InstanceToken: strings.TrimSpace(token),
		Error:         body,
	})
	return err
}

func markPreparedRuntimeCommandFailed(ctx context.Context, client PreparedRuntimeInstanceClient, id string, token string, failure error) error {
	if err := markRuntimeInstanceFailed(ctx, client, id, token, failure); err != nil {
		return errors.Join(failure, err)
	}
	return nil
}

func writeFileFrameWithMetadataContext(ctx context.Context, session vm.Session, w io.Writer, header wire.StreamHeader, path string, digest string, size int64) error {
	header.BodyDigest = &digest
	if err := wire.WriteStreamFrameHeader(w, header, uint64(size)); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	result := make(chan error, 1)
	go func() {
		_, err := io.Copy(w, file)
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		closeCtx, cancel := preparedRuntimeControlContext(ctx)
		defer cancel()
		_ = session.Close(closeCtx)
		return ctx.Err()
	}
}

func (p *PreparedRuntimePool) logInfo(message string, attrs ...any) {
	if p == nil || p.Log == nil {
		return
	}
	p.Log.Info(message, attrs...)
}
