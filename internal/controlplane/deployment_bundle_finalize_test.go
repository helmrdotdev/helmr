package controlplane

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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
	server := &Server{deploymentFinalizePingEvery: time.Millisecond}
	go func() {
		defer close(done)
		server.streamDeploymentFinalization(recorder, request, bundleDigest, func(
			ctx context.Context,
			progress func(deploymentFinalizeProgress) error,
		) (api.DeploymentResponse, error) {
			close(finishStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return api.DeploymentResponse{}, ctx.Err()
			}
			if err := progress(deploymentFinalizeProgress{digest: "sha256:object"}); err != nil {
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
		(&Server{}).streamDeploymentFinalization(recorder, request, "sha256:bundle", func(
			ctx context.Context,
			_ func(deploymentFinalizeProgress) error,
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
	(&Server{}).streamDeploymentFinalization(recorder, request, "sha256:bundle", func(
		context.Context,
		func(deploymentFinalizeProgress) error,
	) (api.DeploymentResponse, error) {
		return api.DeploymentResponse{}, invalidDeploymentObjectError{err: errors.New("secret-sentinel")}
	})
	_, stream := recorder.snapshot()
	if strings.Count(stream, "event: error\n") != 1 || strings.Contains(stream, "event: complete\n") ||
		strings.Contains(stream, "secret-sentinel") || !strings.Contains(stream, "deployment object failed verification") {
		t.Fatalf("stream = %q", stream)
	}
}

func TestDeploymentFinalizationStreamContainsFinalizerPanic(t *testing.T) {
	recorder := newDeploymentFinalizeStreamRecorder()
	request := httptest.NewRequest(http.MethodPost, "/finalize", nil)
	(&Server{}).streamDeploymentFinalization(recorder, request, "sha256:bundle", func(
		context.Context,
		func(deploymentFinalizeProgress) error,
	) (api.DeploymentResponse, error) {
		panic("secret-panic-sentinel")
	})
	_, stream := recorder.snapshot()
	if strings.Count(stream, "event: error\n") != 1 || strings.Contains(stream, "event: complete\n") ||
		strings.Contains(stream, "secret-panic-sentinel") || !strings.Contains(stream, "deployment finalization is unavailable") {
		t.Fatalf("stream = %q", stream)
	}
}

func TestFinishFinalizedDeploymentBundleStopsBeforeTransactionAfterDisconnect(t *testing.T) {
	image := deploymentFinalizeOCIFixture(t)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(image))
	descriptor := cas.Descriptor{
		Digest: digest, SizeBytes: int64(len(image)), MediaType: deployment.WorkspaceImageArtifactMediaType,
	}
	store := &deploymentFinalizeObjectStore{descriptor: descriptor, body: image}
	server := &Server{db: deploymentFinalizePossessionStore{}, deploymentVerifierSlots: make(chan struct{}, 1)}
	_, err := server.finishFinalizedDeploymentBundle(
		t.Context(), store, uuid.UUID{15: 1}, pgvalue.UUID(uuid.UUID{15: 2}), pgvalue.UUID(uuid.UUID{15: 3}),
		finalizedDeploymentBundle{
			root:    cas.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64)},
			bundle:  deployment.DeploymentBundle{},
			objects: []cas.Descriptor{descriptor},
		},
		nil,
		func(deploymentFinalizeProgress) error { return context.Canceled },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation before transaction", err)
	}
}

type deploymentFinalizePossessionStore struct{ db.Querier }

func (deploymentFinalizePossessionStore) GetCasObject(context.Context, db.GetCasObjectParams) (db.CasObject, error) {
	return db.CasObject{}, pgx.ErrNoRows
}

func (deploymentFinalizePossessionStore) GetDeploymentByBundleDigest(
	context.Context,
	db.GetDeploymentByBundleDigestParams,
) (db.Deployment, error) {
	return db.Deployment{}, pgx.ErrNoRows
}

type deploymentFinalizeObjectStore struct {
	cas.UploadStore
	descriptor cas.Descriptor
	body       []byte
}

func (s *deploymentFinalizeObjectStore) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{
		Digest: s.descriptor.Digest, SizeBytes: s.descriptor.SizeBytes, MediaType: s.descriptor.MediaType,
	}, nil
}

func (s *deploymentFinalizeObjectStore) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

func (s *deploymentFinalizeObjectStore) PromoteQuarantine(
	context.Context,
	string,
	cas.Descriptor,
) (cas.Object, error) {
	return s.Stat(context.Background(), s.descriptor.Digest)
}

func deploymentFinalizeOCIFixture(t *testing.T) []byte {
	t.Helper()
	layer := deploymentFinalizeTarFixture(t, "hello.txt", []byte("hello"))
	config := []byte(`{"Config":{"WorkingDir":"/workspace"}}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
	layerDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer))
	manifest, err := json.Marshal(oci.Manifest{
		Config: oci.Descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: int64(len(config)),
		},
		Layers: []oci.Descriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: int64(len(layer)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	index, err := json.Marshal(oci.Index{Manifests: []oci.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest,
		Size: int64(len(manifest)), Platform: &oci.Platform{Architecture: "amd64", OS: "linux"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for name, body := range map[string][]byte{
		"oci-layout":                         []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":                         index,
		"blobs/sha256/" + configDigest[7:]:   config,
		"blobs/sha256/" + layerDigest[7:]:    layer,
		"blobs/sha256/" + manifestDigest[7:]: manifest,
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func deploymentFinalizeTarFixture(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestPublicDeploymentFinalizeErrorUsesClosedMessages(t *testing.T) {
	const secret = "secret-sentinel"
	for name, test := range map[string]struct {
		err     error
		code    string
		message string
	}{
		"invalid object": {
			err:     invalidDeploymentObjectError{err: errors.New(secret)},
			code:    "invalid_deployment_object",
			message: "deployment object failed verification",
		},
		"idempotency conflict": {
			err:     idempotency.ConflictError{},
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
