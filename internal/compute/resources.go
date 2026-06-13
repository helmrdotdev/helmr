package compute

import "errors"

var ErrNoCapacity = errors.New("no compute capacity available")

type ResourceVector struct {
	MilliCPU  int64 `json:"milli_cpu"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
	Slots     int32 `json:"execution_slots"`
}

func DefaultRunResources() ResourceVector {
	return ResourceVector{
		MilliCPU:  2000,
		MemoryMiB: 2048,
		Slots:     1,
	}
}

func (r ResourceVector) Validate(requirePositive bool) error {
	var problems []error
	if requirePositive {
		if r.MilliCPU <= 0 {
			problems = append(problems, errors.New("milli_cpu must be positive"))
		}
		if r.MemoryMiB <= 0 {
			problems = append(problems, errors.New("memory_mib must be positive"))
		}
		if r.Slots <= 0 {
			problems = append(problems, errors.New("slots must be positive"))
		}
	}
	if r.MilliCPU < 0 {
		problems = append(problems, errors.New("milli_cpu must not be negative"))
	}
	if r.MemoryMiB < 0 {
		problems = append(problems, errors.New("memory_mib must not be negative"))
	}
	if r.DiskMiB < 0 {
		problems = append(problems, errors.New("disk_mib must not be negative"))
	}
	if r.Slots < 0 {
		problems = append(problems, errors.New("slots must not be negative"))
	}
	return errors.Join(problems...)
}

func (r ResourceVector) Fits(request ResourceVector) bool {
	return r.MilliCPU >= request.MilliCPU &&
		r.MemoryMiB >= request.MemoryMiB &&
		r.DiskMiB >= request.DiskMiB &&
		r.Slots >= request.Slots
}
