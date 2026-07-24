package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

type tokenCreateControl struct {
	*testRunLeaseControl
	request  api.WorkerCreateTokenRequest
	response api.TokenResponse
	err      error
}

func (control *tokenCreateControl) CreateRuntimeToken(
	_ context.Context,
	request api.WorkerCreateTokenRequest,
) (api.TokenResponse, error) {
	control.request = request
	return control.response, control.err
}

func TestHandleTokenCreateReturnsSemanticFailureToRuntime(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000112"
	control := &tokenCreateControl{
		testRunLeaseControl: &testRunLeaseControl{},
		err: &client.HTTPError{
			StatusCode: 400,
			Status:     "400 Bad Request",
			Message:    "Token timeout is invalid",
			Code:       "invalid_token_timeout",
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
		result <- task.handleTokenCreate(t.Context(), &runv0.TokenCreateRequested{
			CorrelationId: correlationID,
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
		decision.GetKind() != "failed" ||
		decision.GetDataJson() != `{"code":"invalid_token_timeout","message":"Token timeout is invalid","retryable":false}` {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestHandleTokenCreateWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "00000000-0000-0000-0000-000000000111"
	timeoutMS := uint64(600_000)
	timeoutAt := time.Now().Add(10 * time.Minute).UTC()
	control := &tokenCreateControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.TokenResponse{
			ID: "tok_aaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "pending",
			CallbackURL:       "https://api.example.test/api/token-callbacks/tok/callback",
			PublicAccessToken: "hlmr_pat_secret", TimeoutAt: &timeoutAt,
			Metadata: json.RawMessage(`{"approval":true}`), Tags: []string{"review"},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
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
		result <- task.handleTokenCreate(t.Context(), &runv0.TokenCreateRequested{
			CorrelationId: correlationID,
			TimeoutMs:     &timeoutMS,
			Tags:          []string{"review"},
			MetadataJson:  stringPointer(`{"approval":true}`),
			IdempotencyKey: stringPointer(
				"approval-1",
			),
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
		!json.Valid([]byte(decision.GetDataJson())) {
		t.Fatalf("decision = %+v", decision)
	}
	if control.request.Lease != lease ||
		control.request.TimeoutMS == nil ||
		*control.request.TimeoutMS != int64(timeoutMS) ||
		control.request.IdempotencyKey != "approval-1" ||
		string(control.request.Metadata) != `{"approval":true}` {
		t.Fatalf("request = %+v", control.request)
	}
}
