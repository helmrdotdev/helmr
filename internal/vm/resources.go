package vm

import (
	"errors"
	"fmt"
	"math"
)

// Resources is a VM resource shape; memory and disk are in MiB.
type Resources struct {
	MilliCPU  int64 `json:"milli_cpu"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
	Slots     int32 `json:"execution_slots"`
}

func (r Resources) Validate() error {
	var problems []error
	if r.MilliCPU <= 0 {
		problems = append(problems, errors.New("milli_cpu must be positive"))
	}
	if r.MemoryMiB <= 0 {
		problems = append(problems, errors.New("memory_mib must be positive"))
	}
	if r.DiskMiB < 0 {
		problems = append(problems, errors.New("disk_mib must not be negative"))
	}
	if r.Slots <= 0 {
		problems = append(problems, errors.New("slots must be positive"))
	}
	return errors.Join(problems...)
}

// VCPUCountForMilliCPU is the resource conversion from a
// positive milliCPU request to the VM vCPU shape. The subtraction
// form avoids overflowing at MaxInt64.
func VCPUCountForMilliCPU(milliCPU int64) (int64, error) {
	if milliCPU <= 0 {
		return 0, fmt.Errorf("milliCPU must be positive, got %d", milliCPU)
	}
	return (milliCPU-1)/1000 + 1, nil
}

// ReservedCPUMillis accounts for the whole vCPUs the VM actually receives.
// Fractional authored requests do not configure a fractional host CPU quota.
func ReservedCPUMillis(milliCPU int64) (int64, error) {
	vcpus, err := VCPUCountForMilliCPU(milliCPU)
	if err != nil {
		return 0, err
	}
	if vcpus > math.MaxInt64/1000 {
		return 0, errors.New("physical CPU reservation exceeds supported integer range")
	}
	return vcpus * 1000, nil
}
