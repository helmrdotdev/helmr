package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestBuildGuestUsesOneNetworkedVM(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	failed := buildGuestFailureForTest(t, BuildFailureDeclarationAnalysis)
	connector := &buildGuestTestConnector{
		handle: func(stream io.ReadWriter, bodyLen uint64) error {
			body := &io.LimitedReader{R: stream, N: int64(bodyLen)}
			requestRaw, err := frameio.ReadMessageFrameBounded(body, 64<<10)
			if err != nil {
				return err
			}
			actual, err := ParseBuildGuestRequest(requestRaw)
			if err != nil {
				return err
			}
			if actual.Manager.Artifact.Digest != request.Manager.Artifact.Digest {
				return errors.New("Manager changed")
			}
			actualSource, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			if string(actualSource) != string(source) {
				return errors.New("submitted source changed")
			}
			return frameio.WriteMessageFrame(stream, failed)
		},
	}
	_, err := (BuildGuest{Connector: connector}).Execute(
		context.Background(),
		"run",
		request,
		strings.NewReader(string(source)),
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
	)
	var failure BuildFailure
	if !errors.As(err, &failure) ||
		failure.Reason != BuildFailureDeclarationAnalysis {
		t.Fatalf("build error = %v", err)
	}
	if failure.Logs == nil ||
		failure.Logs.ExitStatus != 23 ||
		failure.Logs.StderrBase64 != base64.StdEncoding.EncodeToString(
			[]byte("build stderr"),
		) {
		t.Fatalf("build failure logs = %+v", failure.Logs)
	}
	if connector.statusCount != 1 || connector.closeWriteCount != 1 {
		t.Fatalf(
			"network status count = %d, close-write count = %d",
			connector.statusCount,
			connector.closeWriteCount,
		)
	}
	vmRequest := connector.request
	if vmRequest.Networkless ||
		!vmRequest.Network.Internet ||
		len(vmRequest.Network.Allow) != 0 ||
		len(vmRequest.Network.Deny) != 0 ||
		vmRequest.Resources != compute.BuildGuestResources() ||
		vmRequest.PIDsMax != compute.BuildGuestPIDsMax {
		t.Fatalf("build VM request = %+v", vmRequest)
	}
	wantDrives := []string{
		vm.ManagerDrive,
		vm.ManagedRuntimeDrive,
		vm.ToolchainDrive,
	}
	if len(vmRequest.ReadOnlyDrives) != len(wantDrives) {
		t.Fatalf("build drives = %+v", vmRequest.ReadOnlyDrives)
	}
	for index, drive := range vmRequest.ReadOnlyDrives {
		if drive.ID != wantDrives[index] {
			t.Fatalf("build drives = %+v", vmRequest.ReadOnlyDrives)
		}
	}
}

func TestBuildGuestNetworkLimitOverridesGuestResult(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	connector := &buildGuestTestConnector{
		network: vm.BuildNetworkStatus{
			DeniedPackets: 4,
			LimitPackets:  1,
		},
		handle: writeBuildGuestResultForTest(
			buildGuestFailureForTest(t, BuildFailureInstallLifecycle),
		),
	}
	_, err := (BuildGuest{Connector: connector}).Execute(
		context.Background(),
		"run",
		request,
		strings.NewReader(string(source)),
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
	)
	var failure BuildFailure
	if !errors.As(err, &failure) ||
		failure.Reason != BuildFailureNetworkLimit {
		t.Fatalf("build error = %v", err)
	}
}

func TestBuildGuestDeniedPacketsDoNotOverrideGuestResult(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	connector := &buildGuestTestConnector{
		network: vm.BuildNetworkStatus{DeniedPackets: 1},
		handle: writeBuildGuestResultForTest(
			buildGuestFailureForTest(t, BuildFailureDeclarationAnalysis),
		),
	}
	_, err := (BuildGuest{Connector: connector}).Execute(
		context.Background(),
		"run",
		request,
		strings.NewReader(string(source)),
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
	)
	var failure BuildFailure
	if !errors.As(err, &failure) ||
		failure.Reason != BuildFailureDeclarationAnalysis {
		t.Fatalf("build error = %v", err)
	}
}

func TestBuildGuestNetworkStatusFailureIsInfrastructureError(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	statusErr := errors.New("read counters")
	connector := &buildGuestTestConnector{
		statusErr: statusErr,
		handle: writeBuildGuestResultForTest(
			buildGuestFailureForTest(t, BuildFailureDeclarationAnalysis),
		),
	}
	_, err := (BuildGuest{Connector: connector}).Execute(
		context.Background(),
		"run",
		request,
		strings.NewReader(string(source)),
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&ArtifactSnapshot{content: &artifactSnapshot{}},
	)
	var fatal interface{ FatalWorker() bool }
	if !errors.Is(err, statusErr) || errors.As(err, &fatal) {
		t.Fatalf("network status error = %v", err)
	}
}

func TestBuildNetworkFailure(t *testing.T) {
	tests := []struct {
		name   string
		status vm.BuildNetworkStatus
		reason BuildFailureReason
	}{
		{name: "clean"},
		{name: "denied packets are observational", status: vm.BuildNetworkStatus{
			DeniedPackets: 1,
		}},
		{
			name: "limit",
			status: vm.BuildNetworkStatus{
				DeniedPackets: 4,
				LimitPackets:  1,
			},
			reason: BuildFailureNetworkLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := buildNetworkFailure(
				test.status,
				&BuildLogs{ExitStatus: 7},
			)
			if test.reason == "" {
				if failure != nil {
					t.Fatalf("unexpected network failure: %+v", failure)
				}
				return
			}
			if failure == nil || failure.Reason != test.reason {
				t.Fatalf(
					"network failure = %+v, want reason %q",
					failure,
					test.reason,
				)
			}
			if failure.Logs == nil || failure.Logs.ExitStatus != 7 {
				t.Fatalf("network failure lost build logs: %+v", failure)
			}
		})
	}
}

func buildGuestFailureForTest(
	t *testing.T,
	reason BuildFailureReason,
) []byte {
	t.Helper()
	raw, err := CanonicalBuildGuestResult(BuildGuestResult{
		FormatVersion: BuildGuestFormatVersion,
		Outcome:       BuildGuestFailed,
		Logs: &BuildLogs{
			ExitStatus: 23,
			StderrBase64: base64.StdEncoding.EncodeToString(
				[]byte("build stderr"),
			),
		},
		Error: &BuildError{
			ReasonCode: reason,
			Message:    "build failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeBuildGuestResultForTest(
	result []byte,
) func(io.ReadWriter, uint64) error {
	return func(stream io.ReadWriter, bodyLen uint64) error {
		if _, err := io.CopyN(io.Discard, stream, int64(bodyLen)); err != nil {
			return err
		}
		return frameio.WriteMessageFrame(stream, result)
	}
}

type buildGuestTestConnector struct {
	mu              sync.Mutex
	request         vm.ConnectRequest
	handle          func(io.ReadWriter, uint64) error
	network         vm.BuildNetworkStatus
	statusErr       error
	statusCount     int
	closeWriteCount int
}

func (connector *buildGuestTestConnector) Connect(
	_ context.Context,
	request vm.ConnectRequest,
) (vm.Session, error) {
	connector.mu.Lock()
	connector.request = request
	connector.mu.Unlock()
	host, guest := net.Pipe()
	session := &buildGuestTestProtocolSession{
		connector: connector,
		host:      host,
		done:      make(chan error, 1),
	}
	go func() {
		defer guest.Close()
		header, bodyLen, err := wire.ReadStreamFrameHeader(guest)
		if err == nil && header.Type != wire.StreamTypeBuild {
			err = errors.New("unexpected build guest stream type")
		}
		if err == nil {
			err = connector.handle(guest, bodyLen)
		}
		session.done <- err
	}()
	return session, nil
}

type buildGuestTestProtocolSession struct {
	connector *buildGuestTestConnector
	host      net.Conn
	done      chan error
	closeOnce sync.Once
	closeErr  error
}

func (session *buildGuestTestProtocolSession) Stream() vm.Stream {
	return buildGuestTestStream{
		Conn: session.host,
		closeWrite: func() {
			session.connector.mu.Lock()
			defer session.connector.mu.Unlock()
			session.connector.closeWriteCount++
		},
	}
}

func (*buildGuestTestProtocolSession) OpenStream(
	context.Context,
) (vm.Stream, error) {
	return nil, errors.New("unsupported")
}

func (*buildGuestTestProtocolSession) Wait(context.Context) error {
	return nil
}

func (session *buildGuestTestProtocolSession) Close(context.Context) error {
	session.closeOnce.Do(func() {
		session.closeErr = errors.Join(session.host.Close(), <-session.done)
	})
	return session.closeErr
}

func (session *buildGuestTestProtocolSession) BuildNetworkStatus(
	context.Context,
) (vm.BuildNetworkStatus, error) {
	session.connector.mu.Lock()
	defer session.connector.mu.Unlock()
	session.connector.statusCount++
	return session.connector.network, session.connector.statusErr
}

type buildGuestTestStream struct {
	net.Conn
	closeWrite func()
}

func (stream buildGuestTestStream) CloseWrite() error {
	stream.closeWrite()
	return nil
}

func buildGuestRequestForTest(
	t *testing.T,
) (BuildGuestRequest, []byte) {
	t.Helper()
	runtime := testRuntimeDescriptor()
	toolchain, _ := testToolchainForRuntime(t, runtime)
	manager := testManager(PackageManagerNPM, runtime.Architecture)
	source := []byte("x")
	sourceHash := sha256.Sum256(source)
	return BuildGuestRequest{
		FormatVersion:   BuildGuestFormatVersion,
		Manager:         buildManagerForTest(manager),
		Runtime:         buildRuntimeForTest(runtime),
		Toolchain:       buildToolchainForTest(toolchain),
		LockfileName:    "package-lock.json",
		SourceDigest:    "sha256:" + hex.EncodeToString(sourceHash[:]),
		SourceSizeBytes: int64(len(source)),
	}, source
}

func testManager(
	name PackageManagerName,
	architecture RuntimeArchitecture,
) Manager {
	manager := PackageManager{Name: name, Version: "11.4.2"}
	if name == PackageManagerPNPM {
		manager.Version = "11.1.0"
	}
	if name == PackageManagerBun {
		manager.Version = "1.3.10"
	}
	kind, entrypoint, origin, err := managerDistribution(manager)
	if err != nil {
		panic(err)
	}
	return Manager{
		AdapterVersion: ManagerAdapterVersion,
		Architecture:   architecture,
		Entrypoint: ManagerEntrypoint{
			Kind: kind,
			Path: entrypoint,
		},
		PackageManager: manager,
		Source: ManagerSource{
			Digest:    "sha256:" + strings.Repeat("1", 64),
			Origin:    origin,
			SizeBytes: 1,
		},
		Tree: ArtifactDescriptor{
			Digest:    "sha256:" + strings.Repeat("2", 64),
			MediaType: ManagerTreeMediaType,
			SizeBytes: 1,
		},
	}
}

func buildManagerForTest(manager Manager) BuildManager {
	return BuildManager{
		Artifact:       manager.Tree,
		Entrypoint:     manager.Entrypoint,
		PackageManager: manager.PackageManager,
	}
}

func buildRuntimeForTest(runtime RuntimeDescriptor) BuildRuntime {
	return BuildRuntime{
		Artifact: ArtifactDescriptor{
			Digest:    runtime.Digest,
			MediaType: runtime.MediaType,
			SizeBytes: runtime.SizeBytes,
		},
		NodeVersion: "24.16.0",
	}
}

func buildToolchainForTest(toolchain Toolchain) BuildToolchain {
	return BuildToolchain{
		Artifact:      toolchain.ToolchainClosure,
		RuntimeDigest: toolchain.ManagedRuntimeDigest,
	}
}

func testToolchainForRuntime(
	t *testing.T,
	runtime RuntimeDescriptor,
) (Toolchain, string) {
	t.Helper()
	toolchain := Toolchain{
		Architecture:         runtime.Architecture,
		FormatVersion:        ToolchainFormatVersion,
		ManagedRuntimeDigest: runtime.Digest,
		ToolchainClosure: ArtifactDescriptor{
			Digest:    "sha256:" + strings.Repeat("3", 64),
			MediaType: ToolchainMediaType,
			SizeBytes: squashFSPhysicalAlign,
		},
	}
	digest, err := ToolchainDigest(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	return toolchain, digest
}
