package executor

import (
	"context"
	"encoding/json"
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
			controlPlane: controlPlane,
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
			controlPlane: controlPlane,
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
		controlPlane: controlPlane,
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
