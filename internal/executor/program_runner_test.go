package executor

import (
	"bufio"
	"context"
	"net"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
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
			CorrelationId:  "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
			RunWaitId:      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
			ResumeAttachId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
			Kind:           "sleep",
			ParamsJson:     `{"ms":100}`,
			MetadataJson:   &metadata,
			Tags:           []string{"timer"},
			TimeoutMs:      &timeout,
		},
	)
	if err != nil {
		t.Fatalf("parseWaitRequest: %v", err)
	}
	if request.Lease.ID != "lease-1" ||
		request.CorrelationID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" ||
		request.RunWaitID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" ||
		request.ResumeAttachID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" ||
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
			CorrelationId:  "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
			RunWaitId:      "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
			ResumeAttachId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
			Kind:           "sleep",
			MetadataJson:   &metadata,
		},
	)
	if err == nil {
		t.Fatal("parseWaitRequest accepted non-object metadata")
	}
}

func TestHandleWaitReturnsExactWaitIdentity(t *testing.T) {
	const (
		correlationID  = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
		runWaitID      = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
		resumeAttachID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
	)
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		lease:   api.WorkerRunLeaseAssignment{ID: "lease-1", RunID: "run-1"},
		waits: &ControlRunWaits{Client: &fakeRunWaitClient{
			created: api.WorkerCreateRunWaitResponse{
				RunID: "run-1", RunWaitID: runWaitID, ResumeAttachID: resumeAttachID,
				ResolutionKind: "completed",
			},
		}},
	}
	result := make(chan error, 1)
	go func() {
		result <- task.handleWait(context.Background(), &runv0.RunWaitRequested{
			CorrelationId: correlationID, RunWaitId: runWaitID,
			ResumeAttachId: resumeAttachID, Kind: "sleep",
		})
	}()

	reader := bufio.NewReader(host)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if decision.GetCorrelationId() != correlationID ||
		decision.GetRunWaitId() != runWaitID ||
		decision.GetResumeAttachId() != resumeAttachID ||
		decision.GetKind() != "completed" {
		t.Fatalf("resume decision = %+v", decision)
	}
}
