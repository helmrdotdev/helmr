//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type Connector struct {
	cfg             Config
	artifacts       runtimeArtifacts
	kernelArgs      string
	datapath        *datapath.Manager
	hostRuntime     *hostRuntimeEvidenceStore
	computerDevices *sync.Map // Shared by per-launch connector copies; keyed by exact vm.Owner.
}

// Launch purpose is connector-owned, independent of the SDK's VM lifetime
// context. Only the local qualification probe has no Control Plane reservation.
type launchMode uint8

const (
	workloadLaunch launchMode = iota
	startupProbeLaunch
)

// QualifiedRuntime is the only Firecracker value that implements workload VM
// interfaces. A raw Connector is a candidate host runtime until Qualify proves
// the exact jailer, device, network, VMM, and Guest health path.
type QualifiedRuntime struct {
	connector    *Connector
	evidence     HostRuntimeEvidence
	capabilities RuntimeCapabilities
}

func NewConnector(cfg Config) (*Connector, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	artifacts, err := loadRuntimeArtifacts(cfg)
	if err != nil {
		return nil, err
	}
	return &Connector{
		cfg:             cfg,
		artifacts:       artifacts,
		kernelArgs:      runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4),
		datapath:        datapath.NewManager(),
		hostRuntime:     newHostRuntimeEvidenceStore(),
		computerDevices: &sync.Map{},
	}, nil
}

func (c *Connector) Qualify(ctx context.Context) (*QualifiedRuntime, error) {
	if c == nil {
		return nil, errors.New("the Firecracker runtime candidate is nil")
	}
	if err := c.preflight(ctx); err != nil {
		return nil, fmt.Errorf("preflight Firecracker runtime candidate: %w", err)
	}
	evidence, err := c.hostRuntimeEvidence(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect Firecracker host runtime: %w", err)
	}
	if err := c.probeGuest(ctx); err != nil {
		return nil, fmt.Errorf("prove jailed Firecracker Guest readiness: %w", err)
	}
	capabilities, err := c.runtimeCapabilities()
	if err != nil {
		return nil, fmt.Errorf("inspect Firecracker runtime capabilities: %w", err)
	}
	return &QualifiedRuntime{
		connector: c, evidence: evidence, capabilities: capabilities,
	}, nil
}

func (runtime *QualifiedRuntime) DatapathHealth() error {
	if runtime == nil || runtime.connector == nil {
		return errors.New("the qualified Firecracker runtime is nil")
	}
	return runtime.connector.datapathHealth()
}

func (c *Connector) datapathHealth() error {
	if c == nil {
		return errors.New("the Firecracker connector is nil")
	}
	return c.datapath.Health()
}

func (runtime *QualifiedRuntime) RuntimeCapabilities() RuntimeCapabilities {
	return runtime.capabilities
}

func (runtime *QualifiedRuntime) HostRuntimeEvidence() HostRuntimeEvidence {
	return runtime.evidence
}

func (c *Connector) runtimeCapabilities() (RuntimeCapabilities, error) {
	capabilities, err := runtimeArtifactCapabilities(c.artifacts)
	if err != nil {
		return RuntimeCapabilities{}, err
	}
	capabilities.VCPUCount = c.cfg.VCPUCount
	capabilities.MemoryMiB = c.cfg.MemoryMiB
	return capabilities, nil
}

// ProbeGuest proves that the exact bound host runtime can cross the jailer,
// device, network, VMM, and Guest health boundaries before the Worker
// advertises capacity to the Control Plane.
func (c *Connector) probeGuest(ctx context.Context) error {
	if c == nil {
		return errors.New("the Firecracker connector is nil")
	}
	probeCtx, cancelProbe := context.WithTimeout(
		ctx,
		c.cfg.InitTimeout+c.cfg.HealthTimeout+stopTimeout,
	)
	defer cancelProbe()
	identity, err := c.hostRuntime.vmPlatform()
	if err != nil {
		return fmt.Errorf("resolve startup probe runtime identity: %w", err)
	}
	ownerID := uuid.NewV7().String()
	machine, err := c.connect(probeCtx, startupProbeLaunch, vm.ConnectRequest{
		ID:        ownerID,
		OwnerKind: vm.OwnerInstance,
		Binding: vm.WorkloadBinding{
			WorkerEpoch:        1,
			OwnerID:            ownerID,
			Generation:         1,
			ComputerInstanceID: ownerID,
			VMPlatformID:       identity.ID,
		},
	})
	if err != nil {
		return fmt.Errorf("start the Firecracker startup probe Guest: %w", err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := machine.Close(cleanupCtx); err != nil {
		return fmt.Errorf("clean the Firecracker startup probe Guest: %w", err)
	}
	return nil
}

func (c *Connector) connect(ctx context.Context, mode launchMode, request vm.ConnectRequest) (vm.Machine, error) {
	owner := vm.Owner{Kind: request.OwnerKind, ID: request.ID}
	if err := request.Binding.Validate(owner); err != nil {
		return nil, fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	child, err := c.connectorForRequest(request)
	if err != nil {
		return nil, err
	}
	return child.start(
		ctx,
		mode,
		request.ID,
		request.OwnerKind,
		request.Binding,
		"",
		"",
		"",
		nil,
		request.Topology,
		request.ReadOnlyDrives,
		nil,
		nil,
	)
}

func (c *Connector) connectorForRequest(
	request vm.ConnectRequest,
) (*Connector, error) {
	cfg := c.cfg
	if request.OwnerKind != vm.OwnerInstance {
		return nil, errors.New("the Firecracker owner kind is invalid")
	}
	if len(request.ReadOnlyDrives) != 0 {
		return nil, errors.New("runtime attachment cannot add read-only drives")
	}
	if request.Resources != (vm.Resources{}) {
		return nil, errors.New("runtime attachment cannot change resources")
	}
	child := *c
	child.cfg = cfg
	child.kernelArgs = runtimeKernelArgs(request.Topology, nil, cfg.NetworkResolverIPv4)
	return &child, nil
}

func (runtime *QualifiedRuntime) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.CheckpointableMachine, error) {
	return runtime.connector.materialize(ctx, request)
}

func (c *Connector) materialize(ctx context.Context, request vm.MaterializeRequest) (vm.CheckpointableMachine, error) {
	if err := c.validateMaterializeRequest(request); err != nil {
		return nil, err
	}
	cfg, err := c.configForMaterializeRequest(request)
	if err != nil {
		return nil, err
	}
	child := *c
	child.cfg = cfg
	child.kernelArgs = runtimeKernelArgs(request.Topology, request.ReadOnlyDrives, c.cfg.NetworkResolverIPv4)
	return child.start(
		ctx,
		workloadLaunch,
		request.ID,
		request.OwnerKind,
		request.Binding,
		"",
		"",
		"",
		nil,
		request.Topology,
		request.ReadOnlyDrives,
		request.RecordPhase,
		nil,
	)
}
