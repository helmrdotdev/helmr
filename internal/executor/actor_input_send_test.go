package executor

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type actorInputSendControl struct {
	*testRunLeaseControl
	request  api.WorkerSendActorInputRequest
	response api.WorkerSendActorInputResponse
}

func (control *actorInputSendControl) SendRunActorInput(
	_ context.Context,
	request api.WorkerSendActorInputRequest,
) (api.WorkerSendActorInputResponse, error) {
	control.request = request
	return control.response, nil
}

func (control *actorInputSendControl) AppendActorOutput(
	context.Context,
	api.WorkerAppendActorOutputRequest,
) (api.WorkerAppendActorOutputResponse, error) {
	return api.WorkerAppendActorOutputResponse{}, errors.New("unexpected Actor output append")
}

func TestHandleActorInputSendWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000111"
	control := &actorInputSendControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.WorkerSendActorInputResponse{
			CorrelationID: correlationID,
			Completed:     &api.SendActorInputResponse{Sequence: 7},
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
		result <- task.handleActorInputSend(t.Context(), &runv0.ActorInputSendRequested{
			CorrelationId: correlationID,
			DeclaredId:    "mailbox",
			Address: &runv0.ActorInputSendRequested_ActorKey{
				ActorKey: "primary",
			},
			DataJson:       `{"hello":"world"}`,
			IdempotencyKey: stringPointer("send-1"),
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
		decision.GetKind() != "completed" ||
		decision.GetDataJson() != `{"sequence":7}` {
		t.Fatalf("decision = %+v", decision)
	}
	if control.request.Lease != lease ||
		control.request.ActorDeclaredID != "mailbox" ||
		control.request.ActorKey != "primary" ||
		control.request.IdempotencyKey != "send-1" {
		t.Fatalf("request = %+v", control.request)
	}
}

func stringPointer(value string) *string {
	return &value
}
