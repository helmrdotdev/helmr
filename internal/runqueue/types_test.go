package runqueue

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestQueueMessageValidate(t *testing.T) {
	message := Message{
		RunID:         "run-1",
		OrgID:         "org-1",
		WorkerGroupID: "group-1",
		QueueName:     "org/group",
		Requirements: compute.RunRequirements{
			Resources: compute.ResourceVector{MilliCPU: 1000, MemoryMiB: 1024, Slots: 1},
		},
	}

	if err := message.Validate(); err != nil {
		t.Fatalf("expected valid message: %v", err)
	}

	message.QueueName = ""
	if err := message.Validate(); err == nil {
		t.Fatal("expected missing queue name to fail validation")
	}
}
