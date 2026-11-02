package controlplane

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// capacityIncompatibleError names why work cannot run on its placement's
// capacity, so callers can tell a configuration problem from an incompatible
// worker pool.
func capacityIncompatibleError(err error) error {
	var unassigned workergroup.PlacementUnassignedError
	var retired workergroup.GroupRetiredError
	code := "execution_capacity_incompatible"
	switch {
	case errors.As(err, &unassigned):
		code = "placement_unassigned"
	case errors.As(err, &retired):
		code = "worker_group_retired"
	case errors.Is(err, workergroup.ErrComputerPlacement):
		code = "computer_placement_mismatch"
	}
	return conflict(codedError{code: code, message: err.Error()})
}
