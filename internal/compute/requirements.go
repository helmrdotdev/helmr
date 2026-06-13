package compute

import (
	"encoding/json"
	"errors"
	"fmt"
)

type Placement struct {
	Region        string            `json:"region,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	DedicatedKey  string            `json:"dedicated_key,omitempty"`
	SnapshotKey   string            `json:"snapshot_key,omitempty"`
	PreferWarmRun bool              `json:"prefer_warm_run,omitempty"`
}

type RunRuntimeRequirements struct {
	Resources ResourceVector
	Runtime   RuntimeSelector
	Network   NetworkPolicy
	Placement Placement
}

type RunRuntimeRequirementFields struct {
	RequestedMilliCPU       int64
	RequestedMemoryMiB      int64
	RequestedDiskMiB        int64
	RequestedExecutionSlots int32
	RuntimeID               string
	RuntimeArch             string
	RuntimeABI              string
	KernelDigest            string
	InitramfsDigest         string
	RootfsDigest            string
	CNIProfile              string
	NetworkPolicyJSON       []byte
	NetworkPolicyLabel      string
	PlacementJSON           []byte
	PlacementLabel          string
}

func RunRuntimeRequirementsFromFields(fields RunRuntimeRequirementFields) (RunRuntimeRequirements, error) {
	networkLabel := fields.NetworkPolicyLabel
	if networkLabel == "" {
		networkLabel = "network policy"
	}
	placementLabel := fields.PlacementLabel
	if placementLabel == "" {
		placementLabel = "placement"
	}
	network := DefaultNetworkPolicy()
	if len(fields.NetworkPolicyJSON) > 0 {
		if err := json.Unmarshal(fields.NetworkPolicyJSON, &network); err != nil {
			return RunRuntimeRequirements{}, fmt.Errorf("%s: %w", networkLabel, err)
		}
	}
	var placement Placement
	if len(fields.PlacementJSON) > 0 {
		if err := json.Unmarshal(fields.PlacementJSON, &placement); err != nil {
			return RunRuntimeRequirements{}, fmt.Errorf("%s: %w", placementLabel, err)
		}
	}
	requirements := RunRuntimeRequirements{
		Resources: ResourceVector{
			MilliCPU:  fields.RequestedMilliCPU,
			MemoryMiB: fields.RequestedMemoryMiB,
			DiskMiB:   fields.RequestedDiskMiB,
			Slots:     fields.RequestedExecutionSlots,
		},
		Runtime: RuntimeSelector{
			ID:              fields.RuntimeID,
			Arch:            fields.RuntimeArch,
			ABI:             fields.RuntimeABI,
			KernelDigest:    fields.KernelDigest,
			InitramfsDigest: fields.InitramfsDigest,
			RootfsDigest:    fields.RootfsDigest,
			CNIProfile:      fields.CNIProfile,
		},
		Network:   network,
		Placement: placement,
	}
	return requirements, requirements.Validate()
}

func (r RunRuntimeRequirements) Validate() error {
	var problems []error
	if err := r.Resources.Validate(true); err != nil {
		problems = append(problems, err)
	}
	if r.Runtime.ID == "" {
		problems = append(problems, errors.New("runtime id is required"))
	}
	if r.Runtime.Arch == "" {
		problems = append(problems, errors.New("runtime arch is required"))
	}
	if r.Runtime.ABI == "" {
		problems = append(problems, errors.New("runtime abi is required"))
	}
	if r.Runtime.KernelDigest == "" {
		problems = append(problems, errors.New("runtime kernel digest is required"))
	}
	if r.Runtime.InitramfsDigest == "" {
		problems = append(problems, errors.New("runtime initramfs digest is required"))
	}
	if r.Runtime.RootfsDigest == "" {
		problems = append(problems, errors.New("runtime rootfs digest is required"))
	}
	if r.Runtime.CNIProfile == "" {
		problems = append(problems, errors.New("runtime cni profile is required"))
	}
	if err := r.Network.Validate(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}
