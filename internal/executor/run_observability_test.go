package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

type runObservabilityRetryControl struct {
	*testRunLeaseControl
	metadataRequests  []api.WorkerUpdateRunMetadataRequest
	metadataErrors    []error
	metadataAttempted chan struct{}
	logRequests       []api.WorkerStructuredLogRequest
	logErrors         []error
	logAttempted      chan struct{}
}

func (control *runObservabilityRetryControl) UpdateRunMetadata(
	_ context.Context,
	request api.WorkerUpdateRunMetadataRequest,
) error {
	control.metadataRequests = append(control.metadataRequests, request)
	if len(control.metadataErrors) == 0 {
		return nil
	}
	err := control.metadataErrors[0]
	control.metadataErrors = control.metadataErrors[1:]
	if control.metadataAttempted != nil {
		close(control.metadataAttempted)
		control.metadataAttempted = nil
	}
	return err
}

func (control *runObservabilityRetryControl) AppendStructuredRunLog(
	_ context.Context,
	request api.WorkerStructuredLogRequest,
) error {
	control.logRequests = append(control.logRequests, request)
	if len(control.logErrors) == 0 {
		return nil
	}
	err := control.logErrors[0]
	control.logErrors = control.logErrors[1:]
	if control.logAttempted != nil {
		close(control.logAttempted)
		control.logAttempted = nil
	}
	return err
}

func TestWorkerRunMetadataRequestPreservesClosedMutation(t *testing.T) {
	amount := 2.5
	request, err := workerRunMetadataRequest(&runv0.MetadataUpdated{
		CorrelationId: "00000000-0000-0000-0000-000000000001",
		Operation:     "increment",
		Key:           stringPointer("steps"),
		Amount:        &amount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.OperationID != "00000000-0000-0000-0000-000000000001" ||
		request.Operation != "increment" ||
		request.Key != "steps" ||
		request.Amount == nil ||
		*request.Amount != amount {
		t.Fatalf("request = %+v", request)
	}
}

func TestWorkerStructuredLogRequestUsesObservedSequence(t *testing.T) {
	request, err := workerStructuredLogRequest(
		&runv0.StructuredLogRequested{
			CorrelationId:  "00000000-0000-0000-0000-000000000001",
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
		&client.HTTPError{
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

func TestTaskControlObservabilityRetryUsesRenewedReceipt(t *testing.T) {
	transient := func() error {
		return &client.HTTPError{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Message:    "temporary control failure",
		}
	}
	t.Run("metadata", func(t *testing.T) {
		attempted := make(chan struct{})
		control := &runObservabilityRetryControl{
			testRunLeaseControl: &testRunLeaseControl{},
			metadataErrors:      []error{transient()},
			metadataAttempted:   attempted,
		}
		task := &guestRunLeaseTask{
			control: control,
			lease:   testRunLeaseReceipt(time.Now().Add(time.Minute)),
		}
		go renewRunSourceReceiptAfterAttempt(task, attempted)
		err := (taskControlEvents{task: task}).ApplyRunMetadata(
			t.Context(),
			api.WorkerRunLeaseReceipt{},
			&runv0.MetadataUpdated{
				CorrelationId: "00000000-0000-0000-0000-000000000131",
				Operation:     "set",
				Key:           stringPointer("state"),
				ValueJson:     stringPointer(`"ready"`),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(control.metadataRequests) != 2 {
			t.Fatalf("requests = %+v", control.metadataRequests)
		}
		assertRetriedWithRenewedReceipt(
			t,
			control.metadataRequests[0].Lease,
			control.metadataRequests[1].Lease,
			len(control.metadataRequests),
		)
	})
	t.Run("structured log", func(t *testing.T) {
		attempted := make(chan struct{})
		control := &runObservabilityRetryControl{
			testRunLeaseControl: &testRunLeaseControl{},
			logErrors:           []error{transient()},
			logAttempted:        attempted,
		}
		task := &guestRunLeaseTask{
			control: control,
			lease:   testRunLeaseReceipt(time.Now().Add(time.Minute)),
		}
		go renewRunSourceReceiptAfterAttempt(task, attempted)
		err := (taskControlEvents{task: task}).RecordStructuredRunLog(
			t.Context(),
			api.WorkerRunLeaseReceipt{},
			17,
			&runv0.StructuredLogRequested{
				CorrelationId:  "00000000-0000-0000-0000-000000000132",
				Level:          "info",
				Message:        "ready",
				AttributesJson: `{}`,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(control.logRequests) != 2 {
			t.Fatalf("requests = %+v", control.logRequests)
		}
		assertRetriedWithRenewedReceipt(
			t,
			control.logRequests[0].Lease,
			control.logRequests[1].Lease,
			len(control.logRequests),
		)
	})
}

func TestTaskControlObservabilityRejectsInvalidRequestBeforeControl(t *testing.T) {
	control := &runObservabilityRetryControl{
		testRunLeaseControl: &testRunLeaseControl{},
	}
	task := &guestRunLeaseTask{
		control: control,
		lease:   testRunLeaseReceipt(time.Now().Add(time.Minute)),
	}
	events := taskControlEvents{task: task}
	if err := events.ApplyRunMetadata(
		t.Context(),
		api.WorkerRunLeaseReceipt{},
		&runv0.MetadataUpdated{
			CorrelationId: "not-a-correlation-id",
			Operation:     "set",
		},
	); err == nil {
		t.Fatal("invalid metadata request was accepted")
	}
	if len(control.metadataRequests) != 0 {
		t.Fatalf("metadata requests = %+v", control.metadataRequests)
	}
	if err := events.RecordStructuredRunLog(
		t.Context(),
		api.WorkerRunLeaseReceipt{},
		1,
		&runv0.StructuredLogRequested{
			CorrelationId: "not-a-correlation-id",
			Level:         "info",
			Message:       "invalid",
		},
	); err == nil {
		t.Fatal("invalid structured log request was accepted")
	}
	if len(control.logRequests) != 0 {
		t.Fatalf("structured log requests = %+v", control.logRequests)
	}
}

func TestFreshAdmissionObservabilityRetriesTransientControlFailure(t *testing.T) {
	control := &runObservabilityRetryControl{
		testRunLeaseControl: &testRunLeaseControl{},
		metadataErrors: []error{&client.HTTPError{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Message:    "temporary control failure",
		}},
	}
	lease := testRunLeaseReceipt(time.Now().Add(time.Minute))
	state := &freshAdmissionState{
		control: control,
		lease:   lease,
	}
	err := state.ApplyRunMetadata(
		t.Context(),
		api.WorkerRunLeaseReceipt{},
		&runv0.MetadataUpdated{
			CorrelationId: "00000000-0000-0000-0000-000000000133",
			Operation:     "set",
			Key:           stringPointer("state"),
			ValueJson:     stringPointer(`"admitted"`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(control.metadataRequests) != 2 ||
		control.metadataRequests[0].Lease != lease ||
		control.metadataRequests[1].Lease != lease {
		t.Fatalf("metadata requests = %+v", control.metadataRequests)
	}
}
