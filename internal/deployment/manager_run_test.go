package deployment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestManagerRunnerUsesOneShotDependencyGuest(t *testing.T) {
	request := managerProbeRequest()
	response := appendFramedManagerTree(
		t,
		managerSuccessMetadata(request, "", nil),
		nil,
	)
	connector := &managerRunTestConnector{
		newStream: func() *managerRunTestStream {
			return newManagerRunTestStream(response)
		},
	}
	runner := ManagerRunner{
		Connector: connector,
		Toolchains: authenticatedToolchainCatalogForTest(
			t,
			[]Toolchain{managerRequestToolchain(request)},
		),
	}

	for attempt := 0; attempt < 2; attempt++ {
		metadata, tree, err := runner.Run(
			context.Background(),
			request,
			managerProbeDrives(),
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if tree != nil {
			t.Fatal("manager probe returned a tree")
		}
		if metadata.Outcome != ManagerSucceeded ||
			metadata.ObservedVersion == nil ||
			*metadata.ObservedVersion != request.DependencyPlan.PackageManager.Version {
			t.Fatalf("manager metadata = %#v", metadata)
		}
	}
	if len(connector.requests) != 2 {
		t.Fatalf("connect requests = %d, want 2", len(connector.requests))
	}
	connect := connector.requests[0]
	if connect.OwnerKind != vm.OwnerBuild ||
		connect.Resources != compute.BuildGuestResources() ||
		connect.PIDsMax != compute.DependencyGuestPIDsMax ||
		!connect.Networkless ||
		connect.Network.Internet ||
		len(connect.Network.Allow) != 0 ||
		len(connect.Network.Deny) != 0 {
		t.Fatalf("connect request = %#v", connect)
	}
	if len(connect.BuildDrives) != 3 ||
		connect.BuildDrives[0].ID != vm.ManagerDrive ||
		connect.BuildDrives[1].ID != vm.ManagedRuntimeDrive ||
		connect.BuildDrives[2].ID != vm.ToolchainDrive {
		t.Fatalf("build drives = %#v", connect.BuildDrives)
	}

	for index, stream := range connector.streams {
		if !stream.writeClosed {
			t.Fatalf("manager request %d write direction was not half-closed", index)
		}
		if !stream.closed {
			t.Fatalf("manager stream %d was not closed with its guest", index)
		}
	}
	stream := connector.streams[0]
	parsed, err := ReadManagerRequest(
		context.Background(),
		&managerBuffer{Buffer: *bytes.NewBuffer(stream.request.Bytes())},
	)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DependencyPlanDigest != request.DependencyPlanDigest ||
		parsed.Operation != request.Operation {
		t.Fatalf("written manager request = %#v", parsed)
	}
}

func TestManagerRunnerReturnsDeterministicManagerFailure(t *testing.T) {
	request := managerProbeRequest()
	reason := ManagerProcessFailed
	message := "manager rejected its fixed activation"
	response := appendFramedManagerTree(t, ManagerMetadata{
		FormatVersion: ManagerFormatVersion,
		Operation:     request.Operation,
		Outcome:       ManagerFailed,
		RequestDigest: mustManagerRequestDigest(t, request),
		Reason:        &reason,
		Message:       &message,
	}, nil)
	connector := &managerRunTestConnector{
		newStream: func() *managerRunTestStream {
			return newManagerRunTestStream(response)
		},
	}
	runner := managerProbeRunner(t, connector, request)

	metadata, tree, err := runner.Run(
		context.Background(),
		request,
		managerProbeDrives(),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tree != nil {
		t.Fatal("failed manager response returned a tree")
	}
	if metadata.Outcome != ManagerFailed ||
		metadata.Reason == nil ||
		*metadata.Reason != reason {
		t.Fatalf("manager metadata = %#v", metadata)
	}
}

func TestManagerRunnerPreparesResolveTransportBeforeManagerConnection(
	t *testing.T,
) {
	request := managerResolveRequest()
	reason := ManagerProcessFailed
	message := "manager rejected its fixed activation"
	response := appendFramedManagerTree(t, ManagerMetadata{
		FormatVersion: ManagerFormatVersion,
		Operation:     request.Operation,
		Outcome:       ManagerFailed,
		RequestDigest: mustManagerRequestDigest(t, request),
		Reason:        &reason,
		Message:       &message,
	}, nil)
	managerStream := newManagerRunTestStream(response)
	bootstrapStream := &relayTestStream{
		response: bytes.NewReader(resolveBootstrapAck[:]),
	}
	prepared := &managerRunPreparedSession{
		endpoint:  &relayTestEndpoint{},
		bootstrap: bootstrapStream,
		manager:   &managerRunTestSession{stream: managerStream},
	}
	connector := &managerRunPreparedConnector{prepared: prepared}
	runner := ManagerRunner{
		Connector: connector,
		Toolchains: authenticatedToolchainCatalogForTest(
			t,
			[]Toolchain{managerRequestToolchain(request)},
		),
		TempDir: t.TempDir(),
		random:  bytes.NewReader(bytes.Repeat([]byte{0x5a}, relayTokenBytes)),
	}
	source := managerRunDriveSource{}

	metadata, tree, err := runner.Run(
		context.Background(),
		request,
		ManagerRunDrives{
			Manager:           source,
			Runtime:           source,
			StandardToolchain: source,
			Project:           source,
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tree != nil {
		t.Fatal("failed resolve returned a tree")
	}
	if metadata.Outcome != ManagerFailed {
		t.Fatalf("manager metadata = %#v", metadata)
	}
	wantEvents := []string{"prepare", "bind", "bootstrap", "open", "close"}
	if len(connector.events) != len(wantEvents) {
		t.Fatalf("events = %v, want %v", connector.events, wantEvents)
	}
	for index := range wantEvents {
		if connector.events[index] != wantEvents[index] {
			t.Fatalf("events = %v, want %v", connector.events, wantEvents)
		}
	}
	if len(connector.requests) != 1 ||
		len(connector.requests[0].BuildDrives) != 4 {
		t.Fatalf("prepare requests = %#v", connector.requests)
	}
	if managerStream.request.Len() == 0 {
		t.Fatal("manager request was not written after transport preparation")
	}
}

func TestManagerRunnerClosesBlockedResponseOnCancellation(t *testing.T) {
	request := managerProbeRequest()
	stream := newManagerRunTestStream(nil)
	stream.blockRead = true
	connector := &managerRunTestConnector{
		newStream: func() *managerRunTestStream { return stream },
	}
	runner := managerProbeRunner(t, connector, request)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(ctx, request, managerProbeDrives(), nil)
		done <- err
	}()
	<-stream.readStarted
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("manager run error = %v, want context canceled", err)
	}
	var guestErr *vm.GuestError
	if !errors.As(err, &guestErr) {
		t.Fatalf("manager run error = %T, want guest error", err)
	}
	if !stream.closed {
		t.Fatal("canceled manager stream was not closed")
	}
}

func TestManagerRunnerClosesBlockedRequestOnCancellation(t *testing.T) {
	request := managerProbeRequest()
	stream := newManagerRunTestStream(nil)
	stream.blockWrite = true
	connector := &managerRunTestConnector{
		newStream: func() *managerRunTestStream { return stream },
	}
	runner := managerProbeRunner(t, connector, request)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(ctx, request, managerProbeDrives(), nil)
		done <- err
	}()
	<-stream.writeStarted
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("manager run error = %v, want context canceled", err)
	}
	var guestErr *vm.GuestError
	if !errors.As(err, &guestErr) {
		t.Fatalf("manager run error = %T, want guest error", err)
	}
	if !stream.closed {
		t.Fatal("canceled manager stream was not closed")
	}
}

func TestManagerRunnerClosesBlockedHalfCloseOnCancellation(t *testing.T) {
	request := managerProbeRequest()
	stream := newManagerRunTestStream(nil)
	stream.blockCloseWrite = true
	connector := &managerRunTestConnector{
		newStream: func() *managerRunTestStream { return stream },
	}
	runner := managerProbeRunner(t, connector, request)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.Run(ctx, request, managerProbeDrives(), nil)
		done <- err
	}()
	<-stream.closeWriteStarted
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("manager run error = %v, want context canceled", err)
	}
	var guestErr *vm.GuestError
	if !errors.As(err, &guestErr) {
		t.Fatalf("manager run error = %T, want guest error", err)
	}
	if !stream.closed {
		t.Fatal("canceled manager stream was not closed")
	}
}

func TestManagerRunnerTreatsMalformedResponseAndCloseFailureAsGuestErrors(
	t *testing.T,
) {
	request := managerProbeRequest()
	closeFailure := errors.New("cleanup failed")
	connector := &managerRunTestConnector{
		closeErr: closeFailure,
		newStream: func() *managerRunTestStream {
			return newManagerRunTestStream([]byte("malformed"))
		},
	}
	runner := managerProbeRunner(t, connector, request)

	_, _, err := runner.Run(
		context.Background(),
		request,
		managerProbeDrives(),
		nil,
	)
	if err == nil {
		t.Fatal("manager run returned nil error")
	}
	if !errors.Is(err, closeFailure) {
		t.Fatalf("manager run error = %v, want close failure", err)
	}
	var guestErr *vm.GuestError
	if !errors.As(err, &guestErr) {
		t.Fatalf("manager run error = %T, want guest error", err)
	}
}

func TestManagerRunnerRejectsAuthorityAndDriveShapeBeforeConnect(t *testing.T) {
	request := managerProbeRequest()
	connector := &managerRunTestConnector{}
	runner := managerProbeRunner(t, connector, request)

	unauthorized := request
	unauthorized.DependencyPlan.StandardToolchainDigest = managerDigest("other")
	if _, _, err := runner.Run(
		context.Background(),
		unauthorized,
		managerProbeDrives(),
		nil,
	); err == nil {
		t.Fatal("manager run accepted unauthorized request")
	}
	drives := managerProbeDrives()
	drives.Manager = nil
	if _, _, err := runner.Run(
		context.Background(),
		request,
		drives,
		nil,
	); err == nil {
		t.Fatal("manager run accepted a missing manager drive")
	}
	if len(connector.requests) != 0 {
		t.Fatalf("connect requests = %d, want 0", len(connector.requests))
	}
}

func TestManagerRunDrivesExactOperationShape(t *testing.T) {
	source := managerRunDriveSource{}
	for _, test := range []struct {
		name    string
		request ManagerRequest
		drives  ManagerRunDrives
		wantIDs []string
	}{
		{
			name:    "probe",
			request: managerProbeRequest(),
			drives:  managerProbeDrives(),
			wantIDs: []string{
				vm.ManagerDrive,
				vm.ManagedRuntimeDrive,
				vm.ToolchainDrive,
			},
		},
		{
			name:    "resolve",
			request: managerResolveRequest(),
			drives: ManagerRunDrives{
				Manager:           source,
				Runtime:           source,
				StandardToolchain: source,
				Project:           source,
			},
			wantIDs: []string{
				vm.ManagerDrive,
				vm.ManagedRuntimeDrive,
				vm.ToolchainDrive,
				vm.ProjectDrive,
			},
		},
		{
			name:    "lifecycle",
			request: managerLifecycleRequest(),
			drives: ManagerRunDrives{
				Manager:           source,
				Runtime:           source,
				StandardToolchain: source,
				Project:           source,
				OfflineStore:      source,
			},
			wantIDs: []string{
				vm.ManagerDrive,
				vm.ManagedRuntimeDrive,
				vm.ToolchainDrive,
				vm.ProjectDrive,
				vm.OfflineStoreDrive,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := managerRunDrives(test.request, test.drives)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.wantIDs) {
				t.Fatalf("drives = %#v", got)
			}
			for index, want := range test.wantIDs {
				if got[index].ID != want {
					t.Fatalf("drive %d ID = %q, want %q", index, got[index].ID, want)
				}
			}
		})
	}
}

func managerProbeRunner(
	t *testing.T,
	connector vm.Connector,
	request ManagerRequest,
) ManagerRunner {
	t.Helper()
	return ManagerRunner{
		Connector: connector,
		Toolchains: authenticatedToolchainCatalogForTest(
			t,
			[]Toolchain{managerRequestToolchain(request)},
		),
	}
}

func managerProbeDrives() ManagerRunDrives {
	source := managerRunDriveSource{}
	return ManagerRunDrives{
		Manager:           source,
		Runtime:           source,
		StandardToolchain: source,
	}
}

type managerRunDriveSource struct{}

func (managerRunDriveSource) LinkInto(string, string, int, int) error {
	return nil
}

type managerRunTestConnector struct {
	closeErr  error
	newStream func() *managerRunTestStream
	requests  []vm.ConnectRequest
	streams   []*managerRunTestStream
}

type managerRunPreparedConnector struct {
	prepared *managerRunPreparedSession
	requests []vm.ConnectRequest
	events   []string
}

func (*managerRunPreparedConnector) Connect(
	context.Context,
	vm.ConnectRequest,
) (vm.Session, error) {
	return nil, errors.New("resolve must not use eager connection")
}

func (connector *managerRunPreparedConnector) Prepare(
	_ context.Context,
	request vm.ConnectRequest,
) (vm.PreparedSession, error) {
	connector.events = append(connector.events, "prepare")
	connector.requests = append(connector.requests, request)
	connector.prepared.events = &connector.events
	return connector.prepared, nil
}

type managerRunPreparedSession struct {
	endpoint  vm.HostEndpoint
	bootstrap vm.Stream
	manager   vm.Session
	events    *[]string
}

func (session *managerRunPreparedSession) Open(
	context.Context,
) (vm.Session, error) {
	*session.events = append(*session.events, "open")
	return session.manager, nil
}

func (session *managerRunPreparedSession) DialGuest(
	_ context.Context,
	port uint32,
) (vm.Stream, error) {
	if port != ResolveBootstrapPort {
		return nil, errors.New("unexpected guest port")
	}
	*session.events = append(*session.events, "bootstrap")
	return session.bootstrap, nil
}

func (session *managerRunPreparedSession) BindHost(
	_ context.Context,
	port uint32,
) (vm.HostEndpoint, error) {
	if port != ResolveHostPort {
		return nil, errors.New("unexpected host port")
	}
	*session.events = append(*session.events, "bind")
	return session.endpoint, nil
}

func (session *managerRunPreparedSession) Close(context.Context) error {
	*session.events = append(*session.events, "close")
	return session.manager.Close(context.Background())
}

func (connector *managerRunTestConnector) Connect(
	_ context.Context,
	request vm.ConnectRequest,
) (vm.Session, error) {
	connector.requests = append(connector.requests, request)
	if connector.newStream == nil {
		return nil, errors.New("unexpected manager guest connection")
	}
	stream := connector.newStream()
	connector.streams = append(connector.streams, stream)
	return &managerRunTestSession{
		closeErr: connector.closeErr,
		stream:   stream,
	}, nil
}

type managerRunTestSession struct {
	closeErr error
	stream   *managerRunTestStream
}

func (session *managerRunTestSession) Stream() vm.Stream {
	return session.stream
}

func (*managerRunTestSession) OpenStream(context.Context) (vm.Stream, error) {
	return nil, errors.New("open stream is unsupported")
}

func (*managerRunTestSession) Wait(context.Context) error {
	return nil
}

func (session *managerRunTestSession) Close(context.Context) error {
	return errors.Join(session.stream.Close(), session.closeErr)
}

type managerRunTestStream struct {
	mu                sync.Mutex
	request           bytes.Buffer
	response          *bytes.Reader
	readStarted       chan struct{}
	writeStarted      chan struct{}
	closeWriteStarted chan struct{}
	readOnce          sync.Once
	writeOnce         sync.Once
	closeWriteOnce    sync.Once
	closeOnce         sync.Once
	closedSignal      chan struct{}
	blockRead         bool
	blockWrite        bool
	blockCloseWrite   bool
	closed            bool
	writeClosed       bool
}

func newManagerRunTestStream(response []byte) *managerRunTestStream {
	return &managerRunTestStream{
		response:          bytes.NewReader(response),
		readStarted:       make(chan struct{}),
		writeStarted:      make(chan struct{}),
		closeWriteStarted: make(chan struct{}),
		closedSignal:      make(chan struct{}),
	}
}

func (stream *managerRunTestStream) Read(buffer []byte) (int, error) {
	stream.readOnce.Do(func() { close(stream.readStarted) })
	if stream.blockRead {
		<-stream.closedSignal
		return 0, io.ErrClosedPipe
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.response.Read(buffer)
}

func (stream *managerRunTestStream) Write(buffer []byte) (int, error) {
	stream.writeOnce.Do(func() { close(stream.writeStarted) })
	if stream.blockWrite {
		<-stream.closedSignal
		return 0, io.ErrClosedPipe
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed || stream.writeClosed {
		return 0, io.ErrClosedPipe
	}
	return stream.request.Write(buffer)
}

func (stream *managerRunTestStream) CloseWrite() error {
	stream.closeWriteOnce.Do(func() { close(stream.closeWriteStarted) })
	if stream.blockCloseWrite {
		<-stream.closedSignal
		return io.ErrClosedPipe
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.closed {
		return io.ErrClosedPipe
	}
	stream.writeClosed = true
	return nil
}

func (stream *managerRunTestStream) Close() error {
	stream.closeOnce.Do(func() {
		stream.mu.Lock()
		stream.closed = true
		stream.mu.Unlock()
		close(stream.closedSignal)
	})
	return nil
}
