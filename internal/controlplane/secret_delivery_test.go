package controlplane

import (
	"bytes"
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
		!bytes.Equal(deliveries[0].Value, []byte("env-value")) ||
		deliveries[1].File == nil ||
		deliveries[1].File.Path != "/run/token" ||
		!bytes.Equal(deliveries[1].Value, []byte("file-value")) {
		t.Fatalf("unexpected Secret delivery order: %#v", deliveries)
	}
}
