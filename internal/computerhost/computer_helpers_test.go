package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// unsupportedMachineStarts provides failing Restore and Materialize defaults so
// test backends satisfy vm.Backend; a fake may override either method.
type unsupportedMachineStarts struct{}

func (unsupportedMachineStarts) Restore(context.Context, vm.RestoreRequest) (vm.Machine, error) {
	return nil, errors.New("test backend does not restore machines")
}

func (unsupportedMachineStarts) Materialize(context.Context, vm.MaterializeRequest) (vm.Machine, error) {
	return nil, errors.New("test backend does not materialize machines")
}

// errTestLiveCapture is what test runtimes return when the machines admits them as
// live-capture machines but a test never expects a Computer save to succeed.
var errTestLiveCapture = errors.New("test runtime does not capture a live Computer")

type scriptedGuestStream struct {
	read    *bytes.Reader
	written bytes.Buffer
}

func (s *scriptedGuestStream) Read(p []byte) (int, error) {
	return s.read.Read(p)
}

func (s *scriptedGuestStream) Write(p []byte) (int, error) {
	return s.written.Write(p)
}

func (s *scriptedGuestStream) Close() error {
	return nil
}

type fakeCAS struct {
	mu        sync.Mutex
	mediaType string
	content   []byte
	objects   map[string][]byte
	metadata  map[string]cas.Object
	getCalls  map[string]int
}

func (f *fakeCAS) Put(
	_ context.Context,
	mediaType string,
	body io.Reader,
) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	return f.put(mediaType, content), nil
}

func (f *fakeCAS) Stage(
	_ context.Context,
	mediaType string,
) (cas.Stage, error) {
	return &fakeCASStage{store: f, mediaType: mediaType}, nil
}

func (f *fakeCAS) put(mediaType string, content []byte) cas.Object {
	object := cas.Object{
		Digest:    sha256sum.DigestBytes(content),
		SizeBytes: int64(len(content)),
		MediaType: mediaType,
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mediaType = mediaType
	f.content = append([]byte(nil), content...)
	if f.objects != nil {
		f.objects[object.Digest] = append([]byte(nil), content...)
	}
	if f.metadata == nil {
		f.metadata = map[string]cas.Object{}
	}
	f.metadata[object.Digest] = object
	return object
}

type fakeCASStage struct {
	store     *fakeCAS
	mediaType string
	content   bytes.Buffer
	closed    bool
}

func (s *fakeCASStage) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("stage is closed")
	}
	return s.content.Write(p)
}

func (s *fakeCASStage) Close() error {
	s.closed = true
	return nil
}

func (s *fakeCASStage) Commit(context.Context) (cas.Object, error) {
	s.closed = true
	return s.store.put(s.mediaType, s.content.Bytes()), nil
}

func (s *fakeCASStage) Abort(context.Context) error {
	s.closed = true
	return nil
}

func (f *fakeCAS) Stat(
	_ context.Context,
	digest string,
) (cas.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.metadata[digest]
	if !ok {
		return cas.Object{}, os.ErrNotExist
	}
	return object, nil
}

func (f *fakeCAS) Get(
	_ context.Context,
	digest string,
) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getCalls == nil {
		f.getCalls = map[string]int{}
	}
	f.getCalls[digest]++
	return io.NopCloser(bytes.NewReader(
		append([]byte(nil), f.objects[digest]...),
	)), nil
}

func (f *fakeCAS) Delete(context.Context, string) error {
	return nil
}

func testCheckpointEncryptor(t *testing.T) *CheckpointEncryptor {
	t.Helper()
	encryptor, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return encryptor
}
