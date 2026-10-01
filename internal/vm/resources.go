package vm

import "errors"

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
