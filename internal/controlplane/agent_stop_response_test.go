package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/session"
	"testing"
	"time"
)

func TestAgentStopResponseRetainsDeadline(t *testing.T) {
	deadline := time.Now().In(time.FixedZone("test", 3600))
	response := agentStopResponse(&session.ProcessStopView{ProcessEpoch: 1, State: "requested", Deadline: deadline})
	if !response.Deadline.Equal(deadline) || response.Deadline.Location() != time.UTC || response.State != "requested" {
		t.Fatalf("stop deadline changed: %+v", response)
	}
}
