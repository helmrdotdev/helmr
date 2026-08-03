package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func workspaceSecretResolutions(
	bindings []db.LockWorkspaceSecretsForAdmissionRow,
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

func activeSecretResolutions(
	bindings []secret.DeliveryEnvelope,
) ([]secret.Resolution, error) {
	resolutions := make([]secret.Resolution, len(bindings))
	for index, binding := range bindings {
		if !binding.Secret.CurrentVersionID.Valid || binding.Secret.State != "active" {
			return nil, secret.ErrDeliveryUnavailable
		}
		resolutions[index] = secret.Resolution{
			PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget,
			SecretID: binding.Secret.ID, SecretVersionID: binding.Secret.CurrentVersionID,
			RevocationGeneration: binding.Secret.RevocationGeneration,
		}
	}
	return resolutions, nil
}
