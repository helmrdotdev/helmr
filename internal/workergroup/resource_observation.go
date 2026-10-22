package workergroup

import (
	"github.com/helmrdotdev/helmr/internal/ids"
)

func validateResourceObservation(r ResourceObservation) error {
	if r.Sequence <= 0 || r.Sequence > maxJSONInteger || r.PolicyRevision <= 0 || r.PolicyRevision > maxJSONInteger {
		return invalidInput("resource sequence and policy revision must be positive safe integers")
	}
	for _, n := range []int64{r.LocalOptional.CPUMillis, r.LocalOptional.MemoryBytes, int64(r.LocalOptional.VMSlots), int64(r.LocalOptional.Tasks), r.OptionalDiskBytes, int64(r.DiskSlotsForRealWork)} {
		if n < 0 || n > maxJSONInteger {
			return invalidInput("resource observations must be nonnegative safe integers")
		}
	}
	if len(r.ReadyLeafKeys) > 32 {
		return invalidInput("too many ready leaf hints")
	}
	seen := map[ReadyLeafKey]bool{}
	for _, key := range r.ReadyLeafKeys {
		if _, err := ids.Parse(key.EnvironmentID); err != nil {
			return invalidInput("ready leaf environment must be a canonical UUIDv7")
		}
		if _, err := ids.Parse(key.BuildID); err != nil {
			return invalidInput("ready leaf build must be a canonical UUIDv7")
		}
		if seen[key] {
			return invalidInput("duplicate ready leaf hint")
		}
		seen[key] = true
	}
	return nil
}

type LocalOptionalResources struct {
	CPUMillis   int64 `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
	VMSlots     int32 `json:"vm_slots"`
	Tasks       int32 `json:"tasks"`
}

type ReadyLeafKey struct {
	EnvironmentID string `json:"environment_id"`
	BuildID       string `json:"build_id"`
}

type ResourceObservation struct {
	Sequence             int64                  `json:"sequence"`
	PolicyRevision       int64                  `json:"policy_revision"`
	LocalOptional        LocalOptionalResources `json:"local_optional"`
	OptionalDiskBytes    int64                  `json:"optional_disk_bytes"`
	DiskSlotsForRealWork int32                  `json:"disk_slots_for_real_work"`
	ReclaimPending       bool                   `json:"reclaim_pending"`
	ReadyLeafKeys        []ReadyLeafKey         `json:"ready_leaf_keys"`
}
