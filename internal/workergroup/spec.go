package workergroup

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$`)

func ValidateName(name string) error {
	if name == "" || len(name) > 128 || !namePattern.MatchString(name) {
		return errors.New("worker group name must be a lowercase identifier of 1 to 128 letters, digits, or internal hyphens")
	}
	return nil
}

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

type Capacity struct {
	MilliCPU                int64 `json:"milli_cpu"`
	MemoryBytes             int64 `json:"memory_bytes"`
	GuestEphemeralDiskBytes int64 `json:"guest_ephemeral_disk_bytes"`
	BuildCacheBytes         int64 `json:"build_cache_bytes"`
	ArtifactCacheBytes      int64 `json:"artifact_cache_bytes"`
	VMSlots                 int32 `json:"vm_slots"`
}

func (capacity Capacity) Validate(spec Spec) error {
	if capacity.MilliCPU <= 0 || capacity.MemoryBytes <= 0 || capacity.GuestEphemeralDiskBytes <= 0 {
		return errors.New("worker group cpu, memory, and guest ephemeral disk capacity must be positive")
	}
	if capacity.BuildCacheBytes < 0 || capacity.ArtifactCacheBytes < 0 || capacity.VMSlots < 0 {
		return errors.New("worker group capacity must not be negative")
	}
	if spec.AllowsRun && capacity.VMSlots == 0 {
		return errors.New("run worker group vm slots must be positive")
	}
	return nil
}
