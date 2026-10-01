package executor

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/wire"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runObservabilityRetryControlPlane struct {
	*testRunLeaseControlPlane
	metadataRequests  []workerapi.UpdateRunMetadataRequest
	metadataErrors    []error
	metadataAttempted chan struct{}
	logRequests       []workerapi.StructuredLogRequest
	logErrors         []error
	logAttempted      chan struct{}
}

func (controlPlane *runObservabilityRetryControlPlane) UpdateRunMetadata(
	_ context.Context,
	request workerapi.UpdateRunMetadataRequest,
) error {
	controlPlane.metadataRequests = append(controlPlane.metadataRequests, request)
	if len(controlPlane.metadataErrors) == 0 {
		return nil
	}
	err := controlPlane.metadataErrors[0]
	controlPlane.metadataErrors = controlPlane.metadataErrors[1:]
	if controlPlane.metadataAttempted != nil {
		close(controlPlane.metadataAttempted)
		controlPlane.metadataAttempted = nil
	}
	return err
}

func (controlPlane *runObservabilityRetryControlPlane) AppendStructuredRunLog(
	_ context.Context,
	request workerapi.StructuredLogRequest,
) error {
	controlPlane.logRequests = append(controlPlane.logRequests, request)
	if len(controlPlane.logErrors) == 0 {
		return nil
	}
	err := controlPlane.logErrors[0]
	controlPlane.logErrors = controlPlane.logErrors[1:]
	if controlPlane.logAttempted != nil {
		close(controlPlane.logAttempted)
		controlPlane.logAttempted = nil
	}
	return err
}

func TestWorkerRunMetadataRequestPreservesClosedMutation(t *testing.T) {
	amount := 2.5
	request, err := workerRunMetadataRequest(&programv0.MetadataUpdated{
		CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000001",
		Operation:     "increment",
		Key:           new("steps"),
		Amount:        &amount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.OperationID != "019c10d5-a6f7-7af1-8f5f-000000000001" ||
		request.Operation != "increment" ||
		request.Key != "steps" ||
		request.Amount == nil ||
		*request.Amount != amount {
		t.Fatalf("request = %+v", request)
	}
}

func TestWorkerStructuredLogRequestUsesObservedSequence(t *testing.T) {
	request, err := workerStructuredLogRequest(
		&programv0.StructuredLogRequested{
			CorrelationId:  "019c10d5-a6f7-7af1-8f5f-000000000001",
			Level:          "warn",
			Message:        "retrying",
			AttributesJson: `{"attempt":2}`,
		},
		9,
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.ObservedSeq != 9 ||
		request.Level != "warn" ||
		request.Message != "retrying" ||
		string(request.Attributes) != `{"attempt":2}` {
		t.Fatalf("request = %+v", request)
	}
}

func TestRuntimeOperationFailureKeepsSemanticControlError(t *testing.T) {
	failure, ok := runtimeOperationFailure(
		&httpclient.Error{
			StatusCode: http.StatusUnprocessableEntity,
			Code:       "run_metadata_rejected",
			Message:    "metadata is too large",
		},
		"fallback",
		"fallback",
	)
	if !ok {
		t.Fatal("semantic HTTP error was not recognized")
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"code":"run_metadata_rejected","message":"metadata is too large","retryable":false}` {
		t.Fatalf("failure = %s", raw)
	}
}

func TestRuntimeOperationFailureDoesNotClassifyGenericConflict(t *testing.T) {
	failure, ok := runtimeOperationFailure(
		&httpclient.Error{
			StatusCode: http.StatusConflict,
			Code:       "conflict",
			Message:    "worker run lease fence is stale",
		},
		"fallback",
		"fallback",
	)
	if ok || failure != (workerapi.RuntimeOperationFailure{}) {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}

func TestRuntimeOperationFailureUsesOwnerCodeForGenericValidationError(t *testing.T) {
	failure, ok := runtimeOperationFailure(
		&httpclient.Error{
			StatusCode: http.StatusUnprocessableEntity,
			Code:       "unprocessable_entity",
			Message:    "metadata is invalid",
		},
		"run_metadata_rejected",
		"metadata request was rejected",
	)
	if !ok || failure.Code != "run_metadata_rejected" || failure.Message != "metadata is invalid" {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}

func TestTaskControlObservabilityRetryKeepsStableFenceAcrossRenewal(t *testing.T) {
	transient := func() error {
		return &httpclient.Error{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Message:    "temporary Control Plane failure",
		}
	}
	t.Run("metadata", func(t *testing.T) {
		attempted := make(chan struct{})
		controlPlane := &runObservabilityRetryControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			metadataErrors:           []error{transient()},
			metadataAttempted:        attempted,
		}
		task := &guestRunLeaseTask{
			controlPlane: testControlPlane(t, controlPlane),
			lease:        testRunLeaseAssignment(time.Now().Add(time.Minute)),
		}
		go renewRunSourceReceiptAfterAttempt(task, attempted)
		err := (taskControlEvents{task: task}).ApplyRunMetadata(
			t.Context(),
			workerapi.RunLeaseAssignment{},
			&programv0.MetadataUpdated{
				CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000131",
				Operation:     "set",
				Key:           new("state"),
				ValueJson:     new(`"ready"`),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(controlPlane.metadataRequests) != 2 {
			t.Fatalf("requests = %+v", controlPlane.metadataRequests)
		}
		assertRetriedWithStableFence(
			t,
			controlPlane.metadataRequests[0].Lease,
			controlPlane.metadataRequests[1].Lease,
			len(controlPlane.metadataRequests),
		)
	})
	t.Run("structured log", func(t *testing.T) {
		attempted := make(chan struct{})
		controlPlane := &runObservabilityRetryControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			logErrors:                []error{transient()},
			logAttempted:             attempted,
		}
		task := &guestRunLeaseTask{
			controlPlane: testControlPlane(t, controlPlane),
			lease:        testRunLeaseAssignment(time.Now().Add(time.Minute)),
		}
		go renewRunSourceReceiptAfterAttempt(task, attempted)
		err := (taskControlEvents{task: task}).RecordStructuredRunLog(
			t.Context(),
			workerapi.RunLeaseAssignment{},
			17,
			&programv0.StructuredLogRequested{
				CorrelationId:  "019c10d5-a6f7-7af1-8f5f-000000000132",
				Level:          "info",
				Message:        "ready",
				AttributesJson: `{}`,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(controlPlane.logRequests) != 2 {
			t.Fatalf("requests = %+v", controlPlane.logRequests)
		}
		assertRetriedWithStableFence(
			t,
			controlPlane.logRequests[0].Lease,
			controlPlane.logRequests[1].Lease,
			len(controlPlane.logRequests),
		)
	})
}

func TestTaskControlObservabilityRejectsInvalidRequestBeforeControlPlane(t *testing.T) {
	controlPlane := &runObservabilityRetryControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
	}
	task := &guestRunLeaseTask{
		controlPlane: testControlPlane(t, controlPlane),
		lease:        testRunLeaseAssignment(time.Now().Add(time.Minute)),
	}
	events := taskControlEvents{task: task}
	if err := events.ApplyRunMetadata(
		t.Context(),
		workerapi.RunLeaseAssignment{},
		&programv0.MetadataUpdated{
			CorrelationId: "not-a-correlation-id",
			Operation:     "set",
		},
	); err == nil {
		t.Fatal("invalid metadata request was accepted")
	}
	if len(controlPlane.metadataRequests) != 0 {
		t.Fatalf("metadata requests = %+v", controlPlane.metadataRequests)
	}
	if err := events.RecordStructuredRunLog(
		t.Context(),
		workerapi.RunLeaseAssignment{},
		1,
		&programv0.StructuredLogRequested{
			CorrelationId: "not-a-correlation-id",
			Level:         "info",
			Message:       "invalid",
		},
	); err == nil {
		t.Fatal("invalid structured log request was accepted")
	}
	if len(controlPlane.logRequests) != 0 {
		t.Fatalf("structured log requests = %+v", controlPlane.logRequests)
	}
}

func TestFreshAdmissionObservabilityRetriesTransientControlFailure(t *testing.T) {
	controlPlane := &runObservabilityRetryControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		metadataErrors: []error{&httpclient.Error{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Message:    "temporary Control Plane failure",
		}},
	}
	lease := testRunLeaseAssignment(time.Now().Add(time.Minute))
	state := &freshAdmissionState{
		controlPlane: controlPlane,
		events:       runLeaseProgramEventSink{controlPlane: testControlPlane(t, controlPlane)},
		lease:        lease,
	}
	err := state.ApplyRunMetadata(
		t.Context(),
		workerapi.RunLeaseAssignment{},
		&programv0.MetadataUpdated{
			CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000133",
			Operation:     "set",
			Key:           new("state"),
			ValueJson:     new(`"admitted"`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(controlPlane.metadataRequests) != 2 ||
		controlPlane.metadataRequests[0].Lease != lease.Fence() ||
		controlPlane.metadataRequests[1].Lease != lease.Fence() {
		t.Fatalf("metadata requests = %+v", controlPlane.metadataRequests)
	}
}

func TestHotWaitMetadataRejectionRetainsCaptureRegistration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	host, guest := net.Pipe()
	defer guest.Close()
	if err := guest.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	lease := testFreshProgramClaim(t).Lease
	cp := &runObservabilityRetryControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{}, metadataErrors: []error{&httpclient.Error{StatusCode: http.StatusUnprocessableEntity, Code: "run_metadata_rejected", Message: "run metadata cannot be updated while a managed wait is pending"}}}
	protocol := newProgramProtocol(host)
	defer protocol.Close()
	captures := &computerhost.CaptureRuns{}
	task := &guestRunLeaseTask{program: freshProgram{channel: fakeGuestMachine{stream: host}, protocol: protocol}, lease: lease, controlPlane: testControlPlane(t, cp), captures: captures}
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- task.runHotWait(ctx, WaitRequest{RunWaitID: "wait"}, func(ctx context.Context, _ WaitRequest) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	correlation := "019c10d5-a6f7-7af1-8f5f-000000000133"
	if err := frameio.WriteProtoFrame(guest, &programv0.RunEvent{Event: &programv0.RunEvent_MetadataUpdated{MetadataUpdated: &programv0.MetadataUpdated{CorrelationId: correlation, Operation: "set", Key: new("phase"), ValueJson: new(`"waiting"`)}}}); err != nil {
		t.Fatal(err)
	}
	header, size, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := wire.ReadResumeDecision(header, guest, size)
	if err != nil {
		t.Fatal(err)
	}
	if decision.GetKind() != "failed" || decision.GetCorrelationId() != correlation {
		t.Fatalf("metadata decision=%v", decision)
	}
	if extra, err := captures.Register(lease, "another-wait", nil); err == nil {
		_ = extra.Detach()
		t.Fatal("metadata rejection detached the original capture wait")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	registration, err := captures.Register(lease, "next-wait", nil)
	if err != nil {
		t.Fatalf("resolved wait did not detach: %v", err)
	}
	if err := registration.Detach(); err != nil {
		t.Fatal(err)
	}
}
