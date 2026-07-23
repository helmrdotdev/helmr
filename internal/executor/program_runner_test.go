package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

type staticRunLease struct {
	lease api.WorkerRunLease
}

func (s staticRunLease) CurrentWorkerRunLease() api.WorkerRunLease {
	return s.lease
}

func TestParseWaitRequest(t *testing.T) {
	timeout := uint64(15_000)
	metadata := `{"source":"program"}`
	request, err := parseWaitRequest(
		staticRunLease{lease: api.WorkerRunLease{ID: "lease-1"}},
		&runv0.RunWaitRequested{
			CorrelationId: "wait-1",
			Kind:          "sleep",
			ParamsJson:    `{"ms":100}`,
			MetadataJson:  &metadata,
			Tags:          []string{"timer"},
			TimeoutMs:     &timeout,
		},
	)
	if err != nil {
		t.Fatalf("parseWaitRequest: %v", err)
	}
	if request.Lease.ID != "lease-1" ||
		request.CorrelationID != "wait-1" ||
		request.Kind != api.WorkerRunWaitKind("sleep") ||
		string(request.Params) != `{"ms":100}` ||
		string(request.Metadata) != `{"source":"program"}` ||
		len(request.Tags) != 1 ||
		request.Tags[0] != "timer" ||
		request.TimeoutMS == nil ||
		*request.TimeoutMS != 15_000 {
		t.Fatalf("parsed wait request = %#v", request)
	}
}

func TestParseWaitRequestRejectsNonObjectMetadata(t *testing.T) {
	metadata := `[]`
	_, err := parseWaitRequest(
		staticRunLease{},
		&runv0.RunWaitRequested{
			CorrelationId: "wait-1",
			Kind:          "sleep",
			MetadataJson:  &metadata,
		},
	)
	if err == nil {
		t.Fatal("parseWaitRequest accepted non-object metadata")
	}
}
