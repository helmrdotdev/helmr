package controlplane

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/secret"
)

func TestProjectSecretDeliveriesUsesCanonicalPlacementOrder(t *testing.T) {
	deliveries, err := projectSecretDeliveries([]secret.DeliveryMaterial{
		{PlacementKind: "file", PlacementTarget: "/run/token", Value: []byte("file-value")},
		{PlacementKind: "env", PlacementTarget: "TOKEN", Value: []byte("env-value")},
	})
	if err != nil {
		t.Fatalf("projectSecretDeliveries: %v", err)
	}
	if len(deliveries) != 2 ||
		deliveries[0].Env == nil ||
		deliveries[0].Env.Name != "TOKEN" ||
		deliveries[1].File == nil ||
		deliveries[1].File.Path != "/run/token" {
		t.Fatalf("unexpected Secret delivery order: %#v", deliveries)
	}
}
