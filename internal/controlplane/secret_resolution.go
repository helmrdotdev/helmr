package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func computerSecretResolutions(
	bindings []db.LockComputerSecretsForAdmissionRow,
) []secret.Resolution {
	resolutions := make([]secret.Resolution, len(bindings))
	for index, binding := range bindings {
		resolutions[index] = secret.Resolution{
			PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
			SecretID: binding.SecretID, SecretVersionID: binding.CurrentVersionID,
			RevocationGeneration: binding.RevocationGeneration,
		}
	}
	return resolutions
}
