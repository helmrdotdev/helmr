package controlplane

import (
	"encoding/json"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// requireWorkerContract admits a /worker/v1 request only from a worker that
// speaks this build's contract. It runs before body decoding, rate guards,
// authentication and any database access, so a mismatched worker changes no
// state and learns why in a stable error even after a wire change.
func requireWorkerContract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mismatch, ok := workerapi.ContractMismatch(r.Header.Values(workerapi.ContractHeader)); ok {
			writeError(w, conflict(workerContractMismatchError{mismatch}))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// workerContractMismatchError names both contracts so that the worker can
// report which build must change.
type workerContractMismatchError struct {
	workerapi.ContractMismatchError
}

func (e workerContractMismatchError) ErrorCode() string {
	return workerapi.ContractMismatchCode
}

func (e workerContractMismatchError) ErrorDetails() map[string]json.RawMessage {
	worker, _ := json.Marshal(e.Worker)
	controlPlane, _ := json.Marshal(e.ControlPlane)
	return map[string]json.RawMessage{
		workerapi.ContractMismatchWorkerDetail:       worker,
		workerapi.ContractMismatchControlPlaneDetail: controlPlane,
	}
}
