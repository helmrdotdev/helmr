package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type tokenCreateControl struct {
	*testRunLeaseControl
	request      workerapi.CreateTokenRequest
	requests     []workerapi.CreateTokenRequest
	response     api.TokenResponse
	err          error
	errors       []error
	firstAttempt chan struct{}
}

func (control *tokenCreateControl) CreateRuntimeToken(
	_ context.Context,
	request workerapi.CreateTokenRequest,
) (api.TokenResponse, error) {
	control.request = request
	control.requests = append(control.requests, request)
	if len(control.errors) != 0 {
		err := control.errors[0]
		control.errors = control.errors[1:]
		if control.firstAttempt != nil {
			close(control.firstAttempt)
			control.firstAttempt = nil
		}
		return api.TokenResponse{}, err
	}
	return control.response, control.err
}

func TestHandleTokenCreateReturnsSemanticFailureToRuntime(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000112"
	control := &tokenCreateControl{
		testRunLeaseControl: &testRunLeaseControl{},
		err: &httpclient.Error{
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

func TestHandleTokenCreateRetryUsesRenewedAssignment(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000115"
	firstAttempt := make(chan struct{})
	control := &tokenCreateControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.TokenResponse{
			ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc39", Status: "pending",
			CallbackURL:       "https://api.example.test/api/token-callbacks/tok/callback",
			PublicAccessToken: "hlmr_pat_secret",
			Metadata:          json.RawMessage(`{}`),
			CreatedAt:         time.Now().UTC(),
			UpdatedAt:         time.Now().UTC(),
		},
		errors: []error{&httpclient.Error{
			StatusCode: 503,
			Status:     "503 Service Unavailable",
			Message:    "temporary control failure",
		}},
		firstAttempt: firstAttempt,
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
	}
	go renewRunSourceReceiptAfterAttempt(task, firstAttempt)
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
	if _, err := wire.ReadResumeDecision(header, reader, bodyLen); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if len(control.requests) != 2 {
		t.Fatalf("requests = %+v", control.requests)
	}
	assertRetriedWithStableFence(t, control.requests[0].Lease, control.requests[1].Lease, len(control.requests))
}

func TestHandleTokenCreateWritesCorrelatedDecision(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	correlationID := "019c10d5-a6f7-7af1-8f5f-000000000111"
	timeoutMS := uint64(600_000)
	timeoutAt := time.Now().Add(10 * time.Minute).UTC()
	control := &tokenCreateControl{
		testRunLeaseControl: &testRunLeaseControl{},
		response: api.TokenResponse{
			ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", Status: "pending",
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
			MetadataJson:  new(`{"approval":true}`),
			IdempotencyKey: new(
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
	if control.request.Lease != lease.Fence() ||
		control.request.TimeoutMS == nil ||
		*control.request.TimeoutMS != int64(timeoutMS) ||
		control.request.IdempotencyKey != "approval-1" ||
		string(control.request.Metadata) != `{"approval":true}` {
		t.Fatalf("request = %+v", control.request)
	}
}
