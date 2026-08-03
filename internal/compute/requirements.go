package compute

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/runtimeid"
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
	Runtime   runtimeid.Selector
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
	NetworkABI              string
	PlacementJSON           []byte
	PlacementLabel          string
}

func RunRuntimeRequirementsFromFields(fields RunRuntimeRequirementFields) (RunRuntimeRequirements, error) {
	placementLabel := fields.PlacementLabel
	if placementLabel == "" {
		placementLabel = "placement"
	}
	var placement Placement
	if len(fields.PlacementJSON) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(fields.PlacementJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&placement); err != nil {
			return RunRuntimeRequirements{}, fmt.Errorf("%s: %w", placementLabel, err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return RunRuntimeRequirements{}, fmt.Errorf("%s: multiple JSON values are not allowed", placementLabel)
		}
	}
	requirements := RunRuntimeRequirements{
		Resources: ResourceVector{
			MilliCPU:  fields.RequestedMilliCPU,
			MemoryMiB: fields.RequestedMemoryMiB,
			DiskMiB:   fields.RequestedDiskMiB,
			Slots:     fields.RequestedExecutionSlots,
		},
		Runtime: runtimeid.Selector{
			ID:              fields.RuntimeID,
			Arch:            fields.RuntimeArch,
			ABI:             fields.RuntimeABI,
			KernelDigest:    fields.KernelDigest,
			InitramfsDigest: fields.InitramfsDigest,
			RootfsDigest:    fields.RootfsDigest,
			NetworkABI:      fields.NetworkABI,
		},
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
	if r.Runtime.NetworkABI == "" {
		problems = append(problems, errors.New("runtime network abi is required"))
	}
	if strings.TrimSpace(r.Placement.Region) != "" {
		problems = append(problems, errors.New("placement region is not supported; use the environment region route"))
	}
	return errors.Join(problems...)
}
