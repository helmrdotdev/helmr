package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

type deploymentFinalizeStreamRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushed chan struct{}
}

func newDeploymentFinalizeStreamRecorder() *deploymentFinalizeStreamRecorder {
	return &deploymentFinalizeStreamRecorder{header: make(http.Header), flushed: make(chan struct{}, 16)}
}

func (r *deploymentFinalizeStreamRecorder) Header() http.Header { return r.header }

func (r *deploymentFinalizeStreamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *deploymentFinalizeStreamRecorder) Write(value []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(value)
}

func (r *deploymentFinalizeStreamRecorder) Flush() {
	select {
	case r.flushed <- struct{}{}:
	default:
	}
}

func (r *deploymentFinalizeStreamRecorder) snapshot() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.body.String()
}

func TestWriteDeploymentFinalizeEventUsesOneTypedSSEFrame(t *testing.T) {
	var output bytes.Buffer
	err := writeDeploymentFinalizeEvent(&output, api.DeploymentBundleFinalizeEventObjectVerified,
		api.DeploymentBundleFinalizeObject{Digest: "sha256:verified"})
	if err != nil {
		t.Fatal(err)
	}
	want := "event: object_verified\ndata: {\"digest\":\"sha256:verified\"}\n\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestDeploymentFinalizationStreamFlushesStartedPingsAndOrdersProgressBeforeCompletion(t *testing.T) {
	const bundleDigest = "sha256:bundle"
	recorder := newDeploymentFinalizeStreamRecorder()
	request := httptest.NewRequest(http.MethodPost, "/finalize", nil)
	release := make(chan struct{})
	finishStarted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamDeploymentFinalization(recorder, request, discardTestLogger(), time.Millisecond, bundleDigest, func(
			ctx context.Context,
			progress func(string) error,
		) (api.DeploymentResponse, error) {
			close(finishStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return api.DeploymentResponse{}, ctx.Err()
			}
			if err := progress("sha256:object"); err != nil {
				return api.DeploymentResponse{}, err
			}
			return api.DeploymentResponse{ID: "deployment-1", BundleDigest: bundleDigest}, nil
		})
	}()

	select {
	case <-finishStarted:
	case <-time.After(time.Second):
		t.Fatal("finalizer did not start")
	}
	select {
	case <-recorder.flushed:
	case <-time.After(time.Second):
		t.Fatal("started event was not flushed")
	}
	status, stream := recorder.snapshot()
	if status != http.StatusOK || !strings.HasPrefix(stream, "event: started\n") {
		t.Fatalf("status = %d, stream = %q", status, stream)
	}
	for !strings.Contains(stream, "event: ping\n") {
		select {
		case <-recorder.flushed:
			_, stream = recorder.snapshot()
		case <-time.After(time.Second):
			t.Fatal("ping event was not flushed")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not complete")
	}
	_, stream = recorder.snapshot()
	progressAt := strings.Index(stream, "event: object_verified\n")
	completeAt := strings.Index(stream, "event: complete\n")
	if progressAt < 0 || completeAt < progressAt || strings.Count(stream, "event: complete\n") != 1 ||
		strings.Contains(stream, "event: error\n") {
		t.Fatalf("stream = %q", stream)
	}
}

func TestDeploymentFinalizationStreamCancelsWorkAfterDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/finalize", nil).WithContext(ctx)
	recorder := newDeploymentFinalizeStreamRecorder()
	finishCanceled := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamDeploymentFinalization(recorder, request, discardTestLogger(), deploymentFinalizePingEvery, "sha256:bundle", func(
			ctx context.Context,
			_ func(string) error,
		) (api.DeploymentResponse, error) {
			<-ctx.Done()
			close(finishCanceled)
			return api.DeploymentResponse{}, ctx.Err()
		})
	}()
	select {
	case <-recorder.flushed:
	case <-time.After(time.Second):
		t.Fatal("started event was not flushed")
	}
	cancel()
	select {
	case <-finishCanceled:
	case <-time.After(time.Second):
		t.Fatal("finalizer context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop")
	}
	_, stream := recorder.snapshot()
	if !strings.Contains(stream, "event: started\n") ||
		strings.Contains(stream, "event: complete\n") || strings.Contains(stream, "event: error\n") {
		t.Fatalf("stream = %q", stream)
	}
}

func TestDeploymentFinalizationStreamEmitsOneSanitizedError(t *testing.T) {
	recorder := newDeploymentFinalizeStreamRecorder()
	request := httptest.NewRequest(http.MethodPost, "/finalize", nil)
	streamDeploymentFinalization(recorder, request, discardTestLogger(), deploymentFinalizePingEvery, "sha256:bundle", func(
		context.Context,
		func(string) error,
	) (api.DeploymentResponse, error) {
		return api.DeploymentResponse{}, deployment.InvalidObjectError{Err: errors.New("secret-sentinel")}
	})
	_, stream := recorder.snapshot()
	if strings.Count(stream, "event: error\n") != 1 || strings.Contains(stream, "event: complete\n") ||
		strings.Contains(stream, "secret-sentinel") || !strings.Contains(stream, "deployment object failed verification") {
		t.Fatalf("stream = %q", stream)
	}
}

func TestDeploymentFinalizationStreamReportsCanceledVerificationAsUnavailable(t *testing.T) {
	recorder := newDeploymentFinalizeStreamRecorder()
	request := httptest.NewRequest(http.MethodPost, "/finalize", nil)
	canceled := fmt.Errorf("read deployment object sha256:object: %w", context.Canceled)
	if got := publicDeploymentFinalizeError(canceled).Code; got != "deployment_finalization_unavailable" {
		t.Fatalf("public error code = %s", got)
	}
	streamDeploymentFinalization(recorder, request, discardTestLogger(), deploymentFinalizePingEvery, "sha256:bundle", func(
		context.Context,
		func(string) error,
	) (api.DeploymentResponse, error) {
		return api.DeploymentResponse{}, canceled
	})
	_, stream := recorder.snapshot()
	if strings.Count(stream, "event: error\n") != 1 || strings.Contains(stream, "event: complete\n") ||
		!strings.Contains(stream, `"code":"deployment_finalization_unavailable"`) {
		t.Fatalf("stream = %q", stream)
	}
}

func TestDeploymentFinalizationStreamContainsFinalizerPanic(t *testing.T) {
	recorder := newDeploymentFinalizeStreamRecorder()
	request := httptest.NewRequest(http.MethodPost, "/finalize", nil)
	streamDeploymentFinalization(recorder, request, discardTestLogger(), deploymentFinalizePingEvery, "sha256:bundle", func(
		context.Context,
		func(string) error,
	) (api.DeploymentResponse, error) {
		panic("secret-panic-sentinel")
	})
	_, stream := recorder.snapshot()
	if strings.Count(stream, "event: error\n") != 1 || strings.Contains(stream, "event: complete\n") ||
		strings.Contains(stream, "secret-panic-sentinel") || !strings.Contains(stream, "deployment finalization is unavailable") {
		t.Fatalf("stream = %q", stream)
	}
}

func TestPublicDeploymentFinalizeErrorUsesClosedMessages(t *testing.T) {
	const secret = "secret-sentinel"
	for name, test := range map[string]struct {
		err     error
		code    string
		message string
	}{
		"invalid object": {
			err:     deployment.InvalidObjectError{Err: errors.New(secret)},
			code:    "invalid_deployment_object",
			message: "deployment object failed verification",
		},
		"idempotency conflict": {
			err:     deployment.ErrFinalizationConflict,
			code:    "idempotency_conflict",
			message: "idempotency key conflicts with another deployment bundle",
		},
		"wrapped idempotency conflict": {
			err:     fmt.Errorf("context: %w", deployment.ErrFinalizationConflict),
			code:    "idempotency_conflict",
			message: "idempotency key conflicts with another deployment bundle",
		},
		"infrastructure": {
			err:     errors.New(secret),
			code:    "deployment_finalization_unavailable",
			message: "deployment finalization is unavailable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := publicDeploymentFinalizeError(test.err)
			if value.Code != test.code || value.Message != test.message || strings.Contains(value.Message, secret) {
				t.Fatalf("error = %+v", value)
			}
		})
	}
}
