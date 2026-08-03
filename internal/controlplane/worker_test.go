package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestValidateWorkerStartupRecoveryRequiresCanonicalUUIDv7(t *testing.T) {
	now := time.Now().UTC()
	valid := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	base := workerapi.StartupRecoveryRequest{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        now,
		Inventory:         []string{valid},
		Reclaimed:         []string{valid},
	}
	for _, test := range []struct {
		name string
		id   string
	}{
		{name: "uuidv4", id: "8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d"},
		{name: "uppercase", id: "019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC31"},
		{name: "whitespace", id: " " + valid},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Inventory = []string{test.id}
			request.Reclaimed = []string{test.id}
			err := validateWorkerStartupRecovery(request, now.Add(-time.Minute), now)
			if err == nil || !strings.Contains(err.Error(), "canonical UUIDv7") {
				t.Fatalf("error = %v, want canonical UUIDv7 rejection", err)
			}
		})
	}
}
