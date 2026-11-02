package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestCapacityIncompatibleErrorNamesPlacementReason(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{workergroup.ErrCapacityIncompatible, "execution_capacity_incompatible"},
		{workergroup.PlacementUnassignedError{Placement: "secure"}, "placement_unassigned"},
		{fmt.Errorf("admit: %w", workergroup.GroupRetiredError{Placement: "secure", Status: "disabled"}), "worker_group_retired"},
		{workergroup.ErrComputerPlacement, "computer_placement_mismatch"},
	} {
		t.Run(test.code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, computerError(test.err, computerCreateOperation))
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusConflict || body.Error.Code != test.code || body.Error.Message != test.err.Error() {
				t.Fatalf("response = %d %+v, want %s", recorder.Code, body.Error, test.code)
			}
		})
	}
}
