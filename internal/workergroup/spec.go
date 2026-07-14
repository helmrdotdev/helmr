package workergroup

import (
	"errors"
	"fmt"
	"strings"
)

type Spec struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	AllowsRun   bool   `json:"allows_run"`
	AllowsBuild bool   `json:"allows_build"`
}

func Normalize(spec Spec) (Spec, error) {
	spec.ID = strings.TrimSpace(spec.ID)
	spec.Name = strings.TrimSpace(spec.Name)
	spec.Description = strings.TrimSpace(spec.Description)
	if spec.ID == "" {
		return Spec{}, errors.New("worker group id is required")
	}
	if spec.Name == "" {
		spec.Name = spec.ID
	}
	if !spec.AllowsRun && !spec.AllowsBuild {
		return Spec{}, fmt.Errorf("worker group %q must allow run, build, or both", spec.ID)
	}
	return spec, nil
}

type Desired struct {
	Spec                           Spec
	Capacity                       Capacity
	EnrollmentPolicyFingerprint    string
	AllowedAttestationFingerprints []string
	LaunchAttestationFingerprint   string
}

type Capacity struct {
	MilliCPU           int64 `json:"milli_cpu"`
	MemoryBytes        int64 `json:"memory_bytes"`
	WorkloadDiskBytes  int64 `json:"workload_disk_bytes"`
	ScratchBytes       int64 `json:"scratch_bytes"`
	BuildCacheBytes    int64 `json:"build_cache_bytes"`
	ArtifactCacheBytes int64 `json:"artifact_cache_bytes"`
	VMSlots            int32 `json:"vm_slots"`
	BuildExecutors     int32 `json:"build_executors"`
}

func (capacity Capacity) Validate(spec Spec) error {
	if capacity.MilliCPU <= 0 || capacity.MemoryBytes <= 0 || capacity.WorkloadDiskBytes <= 0 || capacity.ScratchBytes <= 0 {
		return errors.New("worker group cpu, memory, workload disk, and scratch capacity must be positive")
	}
	if capacity.BuildCacheBytes < 0 || capacity.ArtifactCacheBytes < 0 || capacity.VMSlots < 0 || capacity.BuildExecutors < 0 {
		return errors.New("worker group capacity must not be negative")
	}
	if spec.AllowsRun && capacity.VMSlots == 0 {
		return errors.New("run worker group vm slots must be positive")
	}
	if spec.AllowsBuild && capacity.BuildExecutors == 0 {
		return errors.New("build worker group executors must be positive")
	}
	return nil
}
