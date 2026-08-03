package programbuild

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
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func buildWorkloadBindingForTest() vm.WorkloadBinding {
	return vm.WorkloadBinding{
		WorkerEpoch:       1,
		OwnerID:           "01926eae-b7ac-7c35-8000-000000000001",
		Generation:        1,
		RuntimeIdentityID: "01926eae-b7ac-7c35-8000-000000000002",
	}
}

func TestBuildGuestUsesOneNetworkedVM(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	failed := buildGuestFailureForTest(t, deployment.BuildFailureDeclarationAnalysis)
	connector := &buildGuestTestConnector{
		requireEOFBeforeStatus: true,
		handle: func(stream io.ReadWriter, bodyLen uint64) error {
			body := &io.LimitedReader{R: stream, N: int64(bodyLen)}
			requestRaw, err := frameio.ReadMessageFrameBounded(body, 64<<10)
			if err != nil {
				return err
			}
			actual, err := deployment.ParseBuildGuestRequest(requestRaw)
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
	_, err := (buildGuest{connector: connector}).execute(
		context.Background(),
		buildWorkloadBindingForTest(),
		request,
		strings.NewReader(string(source)),
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
	)
	var failure buildFailure
	if !errors.As(err, &failure) ||
		failure.reason != deployment.BuildFailureDeclarationAnalysis {
		t.Fatalf("build error = %v", err)
	}
	if failure.logs == nil ||
		failure.logs.ExitStatus != 23 ||
		failure.logs.StderrBase64 != base64.StdEncoding.EncodeToString(
			[]byte("build stderr"),
		) {
		t.Fatalf("build failure logs = %+v", failure.logs)
	}
	if connector.statusCount != 1 {
		t.Fatalf("network status count = %d", connector.statusCount)
	}
	vmRequest := connector.request
	if vmRequest.Resources != compute.BuildGuestResources() ||
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
			buildGuestFailureForTest(t, deployment.BuildFailureInstallLifecycle),
		),
	}
	_, err := (buildGuest{connector: connector}).execute(
		context.Background(),
		buildWorkloadBindingForTest(),
		request,
		strings.NewReader(string(source)),
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
	)
	var failure buildFailure
	if !errors.As(err, &failure) ||
		failure.reason != deployment.BuildFailureNetworkLimit {
		t.Fatalf("build error = %v", err)
	}
}

func TestBuildGuestDeniedPacketsDoNotOverrideGuestResult(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	connector := &buildGuestTestConnector{
		network: vm.BuildNetworkStatus{DeniedPackets: 1},
		handle: writeBuildGuestResultForTest(
			buildGuestFailureForTest(t, deployment.BuildFailureDeclarationAnalysis),
		),
	}
	_, err := (buildGuest{connector: connector}).execute(
		context.Background(),
		buildWorkloadBindingForTest(),
		request,
		strings.NewReader(string(source)),
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
	)
	var failure buildFailure
	if !errors.As(err, &failure) ||
		failure.reason != deployment.BuildFailureDeclarationAnalysis {
		t.Fatalf("build error = %v", err)
	}
}

func TestBuildGuestNetworkStatusFailureIsInfrastructureError(t *testing.T) {
	request, source := buildGuestRequestForTest(t)
	statusErr := errors.New("read counters")
	connector := &buildGuestTestConnector{
		statusErr: statusErr,
		handle: writeBuildGuestResultForTest(
			buildGuestFailureForTest(t, deployment.BuildFailureDeclarationAnalysis),
		),
	}
	_, err := (buildGuest{connector: connector}).execute(
		context.Background(),
		buildWorkloadBindingForTest(),
		request,
		strings.NewReader(string(source)),
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
		&deployment.ArtifactSnapshot{},
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
		reason deployment.BuildFailureReason
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
			reason: deployment.BuildFailureNetworkLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := buildNetworkFailure(
				test.status,
				&deployment.BuildLogs{ExitStatus: 7},
			)
			if test.reason == "" {
				if failure != nil {
					t.Fatalf("unexpected network failure: %+v", failure)
				}
				return
			}
			if failure == nil || failure.reason != test.reason {
				t.Fatalf(
					"network failure = %+v, want reason %q",
					failure,
					test.reason,
				)
			}
			if failure.logs == nil || failure.logs.ExitStatus != 7 {
				t.Fatalf("network failure lost build logs: %+v", failure)
			}
		})
	}
}

func buildGuestFailureForTest(
	t *testing.T,
	reason deployment.BuildFailureReason,
) []byte {
	t.Helper()
	raw, err := deployment.CanonicalBuildGuestResult(deployment.BuildGuestResult{
		FormatVersion: deployment.BuildGuestFormatVersion,
		Outcome:       deployment.BuildGuestFailed,
		Logs: &deployment.BuildLogs{
			ExitStatus: 23,
			StderrBase64: base64.StdEncoding.EncodeToString(
				[]byte("build stderr"),
			),
		},
		Error: &deployment.BuildError{
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
	mu                     sync.Mutex
	request                vm.ConnectRequest
	handle                 func(io.ReadWriter, uint64) error
	network                vm.BuildNetworkStatus
	statusErr              error
	statusCount            int
	eofRead                bool
	requireEOFBeforeStatus bool
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
		Conn:      session.host,
		connector: session.connector,
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
	if session.connector.requireEOFBeforeStatus && !session.connector.eofRead {
		return vm.BuildNetworkStatus{}, errors.New(
			"network status read before response EOF",
		)
	}
	return session.connector.network, session.connector.statusErr
}

type buildGuestTestStream struct {
	net.Conn
	connector *buildGuestTestConnector
}

func (stream buildGuestTestStream) Read(buffer []byte) (int, error) {
	count, err := stream.Conn.Read(buffer)
	if errors.Is(err, io.EOF) {
		stream.connector.mu.Lock()
		stream.connector.eofRead = true
		stream.connector.mu.Unlock()
	}
	return count, err
}

func buildGuestRequestForTest(
	t *testing.T,
) (deployment.BuildGuestRequest, []byte) {
	t.Helper()
	source := []byte("x")
	sourceHash := sha256.Sum256(source)
	runtimeDigest := "sha256:" + strings.Repeat("1", 64)
	return deployment.BuildGuestRequest{
		FormatVersion: deployment.BuildGuestFormatVersion,
		Manager: deployment.BuildManager{
			Artifact: deployment.ArtifactDescriptor{
				Digest:    "sha256:" + strings.Repeat("2", 64),
				MediaType: deployment.ManagerTreeMediaType,
				SizeBytes: 1,
			},
			Entrypoint: deployment.ManagerEntrypoint{
				Kind: deployment.ManagerEntrypointNode,
				Path: "/opt/helmr/manager/lib/npm/bin/npm-cli.js",
			},
			PackageManager: deployment.PackageManager{
				Name:    deployment.PackageManagerNPM,
				Version: "11.4.2",
			},
		},
		Runtime: deployment.BuildRuntime{
			Artifact: deployment.ArtifactDescriptor{
				Digest:    runtimeDigest,
				MediaType: deployment.RuntimeArtifactMediaType,
				SizeBytes: 1,
			},
			NodeVersion: "24.16.0",
		},
		Toolchain: deployment.BuildToolchain{
			Artifact: deployment.ArtifactDescriptor{
				Digest:    "sha256:" + strings.Repeat("3", 64),
				MediaType: deployment.ToolchainMediaType,
				SizeBytes: 1,
			},
			RuntimeDigest: runtimeDigest,
		},
		LockfileName:    "package-lock.json",
		SourceDigest:    "sha256:" + hex.EncodeToString(sourceHash[:]),
		SourceSizeBytes: int64(len(source)),
	}, source
}
