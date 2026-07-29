package compute

import "errors"

// WorkerDiskCapacity keeps a single-VM shape separate from the aggregate host
// pools consumed by placement. This prevents a worker with N VM slots from
// advertising only one VM's disk as its total capacity.
type WorkerDiskCapacity struct {
	VMGuestEphemeralDiskBytes   int64
	HostGuestEphemeralDiskBytes int64
}

func PartitionWorkerDiskCapacity(hostMiB, vmMiB, cacheBytes int64) (WorkerDiskCapacity, error) {
	const mib = int64(1024 * 1024)
	if hostMiB <= 0 || vmMiB <= 0 || cacheBytes < 0 || cacheBytes > hostMiB*mib {
		return WorkerDiskCapacity{}, errors.New("worker physical disk budget is invalid")
	}
	capacity := WorkerDiskCapacity{
		VMGuestEphemeralDiskBytes:   vmMiB * mib,
		HostGuestEphemeralDiskBytes: hostMiB*mib - cacheBytes,
	}
	return capacity, capacity.Validate()
}

func (c WorkerDiskCapacity) Validate() error {
	if c.VMGuestEphemeralDiskBytes <= 0 || c.HostGuestEphemeralDiskBytes <= 0 {
		return errors.New("worker disk capacity fields must be positive")
	}
	if c.VMGuestEphemeralDiskBytes > c.HostGuestEphemeralDiskBytes {
		return errors.New("single-VM disk shape exceeds aggregate host capacity")
	}
	return nil
}

func (c WorkerDiskCapacity) FitsVMs(count int64) bool {
	if count <= 0 || c.Validate() != nil {
		return false
	}
	return c.VMGuestEphemeralDiskBytes <= c.HostGuestEphemeralDiskBytes/count
}
