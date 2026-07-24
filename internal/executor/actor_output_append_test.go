package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type actorOutputAppendControl struct {
	*testRunLeaseControl
	request  api.WorkerAppendActorOutputRequest
	response api.WorkerAppendActorOutputResponse
}

func (control *actorOutputAppendControl) AppendActorOutput(
	_ context.Context,
	request api.WorkerAppendActorOutputRequest,
) (api.WorkerAppendActorOutputResponse, error) {
	control.request = request
	return control.response, nil
}

func TestHandleActorOutputAppendWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000112"
	control := &actorOutputAppendControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerAppendActorOutputResponse{
			CorrelationID: correlationID,
			Completed: &api.ActorOutputRecord{
				ID: "arc_aaaaaaaaaaaaaaaaaaaaaaaaaa", Sequence: 8,
				Data: json.RawMessage(`{"status":"working"}`), ContentType: "application/json",
			},
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
	}
	result := make(chan error, 1)
	go func() {
		result <- task.handleActorOutputAppend(t.Context(), &runv0.ActorOutputAppendRequested{
			CorrelationId:  correlationID,
			DataJson:       `{"status":"working"}`,
			ContentType:    "application/json",
			IdempotencyKey: stringPointer("output-1"),
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
	if decision.GetCorrelationId() != correlationID || decision.GetKind() != "completed" {
		t.Fatalf("decision = %+v", decision)
	}
	if control.request.Lease != lease ||
		string(control.request.Data) != `{"status":"working"}` ||
		control.request.ContentType != "application/json" ||
		control.request.IdempotencyKey != "output-1" {
		t.Fatalf("request = %+v", control.request)
	}
}
