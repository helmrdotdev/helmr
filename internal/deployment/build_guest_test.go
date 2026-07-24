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

func TestBuildGuestInstallUsesOneNetworkedManagerVM(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, _ := testToolchainForRuntime(t, runtime)
	capsule := managerCapsuleFixture(PackageManagerNPM, runtime.Architecture)
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("x")
	sourceHash := sha256.Sum256(source)
	request := BuildInstallRequest{
		FormatVersion:        BuildGuestFormatVersion,
		Manager:              capsule,
		ManagerCapsuleDigest: capsuleDigest,
		Runtime:              runtime,
		StandardToolchain:    toolchain,
		SourceDigest:         "sha256:" + hex.EncodeToString(sourceHash[:]),
		SourceSizeBytes:      int64(len(source)),
	}
	response, err := CanonicalBuildInstallResult(BuildInstallResult{
		FormatVersion: BuildGuestFormatVersion,
		Outcome:       BuildInstallFailed,
		Logs: &BuildLogs{
			ExitStatus:   23,
			StderrBase64: base64.StdEncoding.EncodeToString([]byte("manager stderr")),
			StdoutBase64: base64.StdEncoding.EncodeToString([]byte("manager stdout")),
		},
		Error: &BuildError{
			ReasonCode: BuildFailureManagerFailed,
			Message:    "manager exited unsuccessfully",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connector := &buildGuestTestConnector{
		handle: func(stream io.ReadWriter, bodyLen uint64) error {
			body := &io.LimitedReader{R: stream, N: int64(bodyLen)}
			requestRaw, err := frameio.ReadMessageFrameBounded(
				body,
				maxBuildGuestRequestBytes,
			)
			if err != nil {
				return err
			}
			actual, err := ParseBuildInstallRequest(requestRaw)
			if err != nil {
				return err
			}
			if actual.ManagerCapsuleDigest != capsuleDigest {
				return errors.New("Manager Capsule changed")
			}
			actualSource, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			if string(actualSource) != string(source) {
				return errors.New("submitted source changed")
			}
			return frameio.WriteMessageFrame(stream, response)
		},
	}
	_, err = (BuildGuest{Connector: connector}).Install(
		context.Background(),
		"run",
		request,
		strings.NewReader(string(source)),
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&RuntimeArtifactSnapshot{content: &artifactSnapshot{}},
		&toolchainSnapshot{content: &artifactSnapshot{}},
	)
	var failure BuildFailure
	if !errors.As(err, &failure) ||
		failure.Reason != BuildFailureManagerFailed {
		t.Fatalf("build install error = %v", err)
	}
	if failure.Logs == nil ||
		failure.Logs.ExitStatus != 23 ||
		failure.Logs.StdoutBase64 != base64.StdEncoding.EncodeToString([]byte("manager stdout")) ||
		failure.Logs.StderrBase64 != base64.StdEncoding.EncodeToString([]byte("manager stderr")) {
		t.Fatalf("build install failure logs = %+v", failure.Logs)
	}
	vmRequest := connector.request
	if vmRequest.Networkless ||
		!vmRequest.Network.Internet ||
		len(vmRequest.Network.Allow) != 0 ||
		len(vmRequest.Network.Deny) != 0 ||
		vmRequest.Resources != compute.BuildGuestResources() ||
		vmRequest.PIDsMax != compute.BuildGuestPIDsMax {
		t.Fatalf("build install VM request = %+v", vmRequest)
	}
	wantDrives := []string{
		vm.ManagerDrive,
		vm.ManagedRuntimeDrive,
		vm.ToolchainDrive,
	}
	if len(vmRequest.ReadOnlyDrives) != len(wantDrives) {
		t.Fatalf("build install drives = %+v", vmRequest.ReadOnlyDrives)
	}
	for index, drive := range vmRequest.ReadOnlyDrives {
		if drive.ID != wantDrives[index] {
			t.Fatalf("build install drives = %+v", vmRequest.ReadOnlyDrives)
		}
	}
}

func TestBuildGuestInstallNetworkPolicyOverridesManagerFailure(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, _ := testToolchainForRuntime(t, runtime)
	capsule := managerCapsuleFixture(PackageManagerNPM, runtime.Architecture)
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("x")
	sourceHash := sha256.Sum256(source)
	response, err := CanonicalBuildInstallResult(BuildInstallResult{
		FormatVersion: BuildGuestFormatVersion,
		Outcome:       BuildInstallFailed,
		Logs:          &BuildLogs{ExitStatus: 1},
		Error: &BuildError{
			ReasonCode: BuildFailureManagerFailed,
			Message:    "manager exited unsuccessfully",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connector := &buildGuestTestConnector{
		network: vm.BuildNetworkStatus{DeniedPackets: 1},
		handle: func(stream io.ReadWriter, bodyLen uint64) error {
			if _, err := io.CopyN(io.Discard, stream, int64(bodyLen)); err != nil {
				return err
			}
			return frameio.WriteMessageFrame(stream, response)
		},
	}
	_, err = (BuildGuest{Connector: connector}).Install(
		context.Background(),
		"run",
		BuildInstallRequest{
			FormatVersion:        BuildGuestFormatVersion,
			Manager:              capsule,
			ManagerCapsuleDigest: capsuleDigest,
			Runtime:              runtime,
			StandardToolchain:    toolchain,
			SourceDigest:         "sha256:" + hex.EncodeToString(sourceHash[:]),
			SourceSizeBytes:      int64(len(source)),
		},
		strings.NewReader(string(source)),
		&ArtifactSnapshot{content: &artifactSnapshot{}},
		&RuntimeArtifactSnapshot{content: &artifactSnapshot{}},
		&toolchainSnapshot{content: &artifactSnapshot{}},
	)
	var failure BuildFailure
	if !errors.As(err, &failure) ||
		failure.Reason != BuildFailureNetworkDenied {
		t.Fatalf("build install error = %v", err)
	}
}

func TestBuildGuestAnalyzeUsesFreshIsolatedTree(t *testing.T) {
	runtime := testRuntimeDescriptor()
	toolchain, _ := testToolchainForRuntime(t, runtime)
	treeDescriptor := BuildTreeDescriptor{
		Digest:    "sha256:" + strings.Repeat("3", 64),
		SizeBytes: squashFSPhysicalAlign,
	}
	want := testWorkspaceAnalysisResult(t)
	raw, err := CanonicalAnalysisResult(want)
	if err != nil {
		t.Fatal(err)
	}
	connector := &buildGuestTestConnector{
		handle: func(stream io.ReadWriter, bodyLen uint64) error {
			body := &io.LimitedReader{R: stream, N: int64(bodyLen)}
			requestRaw, err := frameio.ReadMessageFrameBounded(
				body,
				maxBuildGuestRequestBytes,
			)
			if err != nil {
				return err
			}
			if body.N != 0 {
				return errors.New("analysis request has trailing data")
			}
			request, err := ParseBuildAnalysisRequest(requestRaw)
			if err != nil {
				return err
			}
			if request.Tree != treeDescriptor {
				return errors.New("analysis tree descriptor changed")
			}
			return frameio.WriteMessageFrame(stream, raw)
		},
	}
	result, err := (BuildGuest{Connector: connector}).Analyze(
		context.Background(),
		"run",
		BuildAnalysisRequest{
			FormatVersion:     BuildGuestFormatVersion,
			Runtime:           runtime,
			StandardToolchain: toolchain,
			Tree:              treeDescriptor,
		},
		&RuntimeArtifactSnapshot{content: &artifactSnapshot{}},
		&toolchainSnapshot{content: &artifactSnapshot{}},
		&BuildTree{
			content:   &artifactSnapshot{},
			inspected: &inspectedArtifact{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != AnalysisOutcomeSucceeded {
		t.Fatalf("analysis outcome = %q", result.Outcome)
	}
	request := connector.request
	if !request.Networkless ||
		request.Resources != compute.BuildGuestResources() ||
		request.PIDsMax != compute.BuildGuestPIDsMax {
		t.Fatalf("analysis VM request = %+v", request)
	}
	wantDrives := []string{
		vm.ManagedRuntimeDrive,
		vm.ToolchainDrive,
		vm.BuildTreeDrive,
	}
	if len(request.ReadOnlyDrives) != len(wantDrives) {
		t.Fatalf("analysis drives = %+v", request.ReadOnlyDrives)
	}
	for index, drive := range request.ReadOnlyDrives {
		if index >= len(wantDrives) || drive.ID != wantDrives[index] {
			t.Fatalf("analysis drives = %+v", request.ReadOnlyDrives)
		}
	}
}

func TestBuildGuestProveUsesFreshIsolatedProgram(t *testing.T) {
	runtime := testRuntimeDescriptor()
	programDescriptor := ProgramDescriptor{
		Digest:    "sha256:" + strings.Repeat("4", 64),
		MediaType: ProgramArtifactMediaType,
		SizeBytes: squashFSPhysicalAlign,
	}
	proof := ProgramProofResult{
		FormatVersion: ProgramProofFormatVersion,
		Outcome:       ProgramProofSucceeded,
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
	}
	raw, err := CanonicalProgramProofResult(proof)
	if err != nil {
		t.Fatal(err)
	}
	connector := &buildGuestTestConnector{
		handle: func(stream io.ReadWriter, bodyLen uint64) error {
			body := &io.LimitedReader{R: stream, N: int64(bodyLen)}
			requestRaw, err := frameio.ReadMessageFrameBounded(
				body,
				maxBuildGuestRequestBytes,
			)
			if err != nil {
				return err
			}
			if body.N != 0 {
				return errors.New("Program proof request has trailing data")
			}
			request, err := ParseProgramProofRequest(requestRaw)
			if err != nil {
				return err
			}
			if request.Program != programDescriptor {
				return errors.New("Program proof descriptors changed")
			}
			return frameio.WriteMessageFrame(stream, raw)
		},
	}
	result, err := (BuildGuest{Connector: connector}).Prove(
		context.Background(),
		"run",
		ProgramProofRequest{
			FormatVersion: BuildGuestFormatVersion,
			Runtime:       runtime,
			Program:       programDescriptor,
		},
		&RuntimeArtifactSnapshot{content: &artifactSnapshot{}},
		&EncodedProgram{
			artifact: &artifactSnapshot{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ProgramProofSucceeded {
		t.Fatalf("Program proof outcome = %q", result.Outcome)
	}
	request := connector.request
	if !request.Networkless ||
		request.Resources != compute.BuildGuestResources() ||
		request.PIDsMax != compute.BuildGuestPIDsMax {
		t.Fatalf("Program proof VM request = %+v", request)
	}
	wantDrives := []string{
		vm.ProgramRuntimeDrive,
		vm.ProgramDrive,
	}
	if len(request.ReadOnlyDrives) != len(wantDrives) {
		t.Fatalf("Program proof drives = %+v", request.ReadOnlyDrives)
	}
	for index, drive := range request.ReadOnlyDrives {
		if index >= len(wantDrives) || drive.ID != wantDrives[index] {
			t.Fatalf("Program proof drives = %+v", request.ReadOnlyDrives)
		}
	}
}

func TestReadBuildNetworkFailure(t *testing.T) {
	tests := []struct {
		name   string
		status vm.BuildNetworkStatus
		reason BuildFailureReason
	}{
		{name: "clean"},
		{
			name:   "denied",
			status: vm.BuildNetworkStatus{DeniedPackets: 1},
			reason: BuildFailureNetworkDenied,
		},
		{
			name: "limit takes precedence",
			status: vm.BuildNetworkStatus{
				DeniedPackets: 4,
				LimitPackets:  1,
			},
			reason: BuildFailureNetworkLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure, err := readBuildNetworkFailure(
				buildNetworkTestSession{status: test.status},
				&BuildLogs{ExitStatus: 7},
			)
			if err != nil {
				t.Fatal(err)
			}
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
				t.Fatalf("network failure lost Manager logs: %+v", failure)
			}
		})
	}
}

type buildGuestTestConnector struct {
	mu      sync.Mutex
	request vm.ConnectRequest
	handle  func(io.ReadWriter, uint64) error
	network vm.BuildNetworkStatus
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
		host:    host,
		done:    make(chan error, 1),
		network: connector.network,
	}
	go func() {
		defer guest.Close()
		header, bodyLen, err := wire.ReadStreamFrameHeader(guest)
		if err == nil {
			switch header.Type {
			case wire.StreamTypeBuildInstall,
				wire.StreamTypeBuildAnalyze,
				wire.StreamTypeProgramProof:
			default:
				err = errors.New("unexpected build guest stream type")
			}
		}
		if err == nil {
			err = connector.handle(guest, bodyLen)
		}
		session.done <- err
	}()
	return session, nil
}

type buildGuestTestProtocolSession struct {
	host      net.Conn
	done      chan error
	closeOnce sync.Once
	closeErr  error
	network   vm.BuildNetworkStatus
}

func (session *buildGuestTestProtocolSession) Stream() vm.Stream {
	return buildGuestTestStream{Conn: session.host}
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
	return session.network, nil
}

type buildGuestTestStream struct {
	net.Conn
}

func (buildGuestTestStream) CloseWrite() error {
	return nil
}

func TestReadBuildNetworkFailureRequiresAccounting(t *testing.T) {
	if _, err := readBuildNetworkFailure(buildNetworklessTestSession{}, nil); err == nil {
		t.Fatal("build session without network accounting was accepted")
	}
	want := errors.New("counter unavailable")
	if _, err := readBuildNetworkFailure(
		buildNetworkTestSession{err: want},
		nil,
	); !errors.Is(err, want) {
		t.Fatalf("network accounting error = %v, want %v", err, want)
	}
}

type buildNetworkTestSession struct {
	buildNetworklessTestSession
	status vm.BuildNetworkStatus
	err    error
}

func (session buildNetworkTestSession) BuildNetworkStatus(
	context.Context,
) (vm.BuildNetworkStatus, error) {
	return session.status, session.err
}

type buildNetworklessTestSession struct{}

func (buildNetworklessTestSession) Stream() vm.Stream {
	return nil
}

func (buildNetworklessTestSession) OpenStream(
	context.Context,
) (vm.Stream, error) {
	return nil, errors.New("unsupported")
}

func (buildNetworklessTestSession) Wait(context.Context) error {
	return nil
}

func (buildNetworklessTestSession) Close(context.Context) error {
	return nil
}
