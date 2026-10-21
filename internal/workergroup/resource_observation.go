package workergroup

import (
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func validateResourceObservation(r workerapi.ResourceObservation) error {
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
	seen := map[workerapi.ReadyLeafKey]bool{}
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
