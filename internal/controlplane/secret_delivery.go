package controlplane

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type SecretDeliveryOpener interface {
	OpenDeliveries(uuid.UUID, []secret.DeliveryEnvelope) ([]secret.DeliveryMaterial, error)
}

func projectSecretDeliveries(materials []secret.DeliveryMaterial) ([]workerapi.SecretDelivery, error) {
	ordered := append([]secret.DeliveryMaterial(nil), materials...)
	slices.SortFunc(ordered, func(left, right secret.DeliveryMaterial) int {
		if compared := strings.Compare(left.PlacementKind, right.PlacementKind); compared != 0 {
			return compared
		}
		return strings.Compare(left.PlacementTarget, right.PlacementTarget)
	})
	deliveries := make([]workerapi.SecretDelivery, 0, len(ordered))
	for index, material := range ordered {
		if strings.TrimSpace(material.PlacementTarget) == "" {
			return nil, errors.New("secret placement target is required")
		}
		if index > 0 &&
			ordered[index-1].PlacementKind == material.PlacementKind &&
			ordered[index-1].PlacementTarget == material.PlacementTarget {
			return nil, errors.New("secret placement is duplicated")
		}
		delivery := workerapi.SecretDelivery{Value: append([]byte(nil), material.Value...)}
		switch material.PlacementKind {
		case "env":
			delivery.Env = &workerapi.SecretEnv{Name: material.PlacementTarget}
		case "file":
			delivery.File = &workerapi.SecretFile{Path: material.PlacementTarget}
		default:
			return nil, fmt.Errorf("secret placement kind %q is unsupported", material.PlacementKind)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}
