package control

import (
	"errors"
	"testing"
)

func TestWorkerWorkspaceExecFailureDoesNotClassifyUnknownInfrastructureError(t *testing.T) {
	failure, ok := workerWorkspaceExecFailure(errors.New("database connection lost"))
	if ok || failure.Code != "" {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}
