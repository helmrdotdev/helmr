package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestManagerRequestCanonicalRoundTripAndFraming(t *testing.T) {
	t.Parallel()
	for _, request := range []ManagerRequest{
		managerProbeRequest(),
		managerResolveRequest(),
		managerLifecycleRequest(),
	} {
		request := request
		t.Run(string(request.Operation), func(t *testing.T) {
			t.Parallel()
			toolchain := managerRequestToolchain(request)
			canonical, err := CanonicalManagerRequest(request, toolchain)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseManagerRequest(canonical, toolchain)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(parsed, request) {
				t.Fatalf("parsed request = %#v", parsed)
			}

			var framed bytes.Buffer
			if err := WriteManagerRequest(&framed, request, toolchain); err != nil {
				t.Fatal(err)
			}
			source := bytes.NewReader(framed.Bytes())
			decoded, err := ReadManagerRequest(
				context.Background(),
				io.NopCloser(source),
				toolchain,
			)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Operation != request.Operation {
				t.Fatalf("decoded operation = %q, want %q", decoded.Operation, request.Operation)
			}
			if source.Len() != 0 {
				t.Fatalf("framed request has %d trailing bytes", source.Len())
			}
		})
	}
}

func TestManagerRequestDigestIsDomainSeparatedAndStable(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	toolchain := managerRequestToolchain(request)
	digest, err := ManagerRequestDigest(request, toolchain)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalManagerRequest(request, toolchain)
	if err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256(canonical)
	if digest == "sha256:"+hex.EncodeToString(plain[:]) {
		t.Fatal("manager request digest is not domain separated")
	}
	const want = "sha256:933a144bf5ce95eafc2b4c645c6e7aee82f13cc2402feace9b029716aaa96990"
	if digest != want {
		t.Fatalf("manager request digest = %q, want %q", digest, want)
	}
}

func TestManagerRequestRejectsInvalidShapes(t *testing.T) {
	t.Parallel()
	tests := map[string]func(ManagerRequest) ManagerRequest{
		"unsupported operation": func(request ManagerRequest) ManagerRequest {
			request.Operation = "install"
			return request
		},
		"bad project media": func(request ManagerRequest) ManagerRequest {
			request.Project.MediaType = ManagerTreeMediaType
			return request
		},
		"bad manager tree media": func(request ManagerRequest) ManagerRequest {
			request.ManagerTree.MediaType = ManagerProjectMediaType
			return request
		},
		"invalid dependency plan digest": func(request ManagerRequest) ManagerRequest {
			request.DependencyPlanDigest = "sha256:invalid"
			return request
		},
		"mismatched manager tree": func(request ManagerRequest) ManagerRequest {
			request.ManagerTree.Digest = managerDigest("other manager tree")
			return request
		},
		"zero project size": func(request ManagerRequest) ManagerRequest {
			request.Project.SizeBytes = 0
			return request
		},
		"mismatched runtime": func(request ManagerRequest) ManagerRequest {
			request.Runtime.Digest = managerDigest("other runtime")
			return request
		},
		"resolve graph": func(request ManagerRequest) ManagerRequest {
			file := managerGraphFile()
			request.PackageGraph = &file
			return request
		},
		"lifecycle without store": func(request ManagerRequest) ManagerRequest {
			request.Operation = ManagerLifecycle
			file := managerGraphFile()
			request.PackageGraph = &file
			return request
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := mutate(managerResolveRequest())
			if _, err := CanonicalManagerRequest(
				request,
				managerRequestToolchain(request),
			); err == nil {
				t.Fatal("CanonicalManagerRequest returned nil error")
			}
		})
	}
	probe := managerProbeRequest()
	project := managerProjectArtifact()
	probe.Project = &project
	if _, err := CanonicalManagerRequest(
		probe,
		managerRequestToolchain(probe),
	); err == nil {
		t.Fatal("CanonicalManagerRequest accepted a probe project")
	}
	request := managerResolveRequest()
	toolchain := managerRequestToolchain(request)
	toolchain.ToolchainClosure.Digest = managerDigest("other toolchain closure")
	if _, err := CanonicalManagerRequest(request, toolchain); err == nil {
		t.Fatal("CanonicalManagerRequest accepted a divergent registered toolchain")
	}
}

func TestManagerRequestRejectsNonCanonicalUnknownAndDuplicateJSON(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	toolchain := managerRequestToolchain(request)
	canonical, err := CanonicalManagerRequest(request, toolchain)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(canonical, &value); err != nil {
		t.Fatal(err)
	}
	value["unknown"] = true
	unknown, err := jsoncanon.Transform(mustJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManagerRequest(unknown, toolchain); err == nil {
		t.Fatal("ParseManagerRequest accepted an unknown member")
	}

	nonCanonical := append([]byte("{\n"), canonical[1:]...)
	if _, err := ParseManagerRequest(nonCanonical, toolchain); err == nil {
		t.Fatal("ParseManagerRequest accepted non-canonical JSON")
	}

	duplicate := bytes.Replace(
		canonical,
		[]byte(`"formatVersion":0,"managerCapsule"`),
		[]byte(`"formatVersion":0,"formatVersion":0,"managerCapsule"`),
		1,
	)
	if _, err := ParseManagerRequest(duplicate, toolchain); err == nil {
		t.Fatal("ParseManagerRequest accepted a duplicate member")
	}
}

func TestManagerRequestRejectsWrongStreamHeader(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	toolchain := managerRequestToolchain(request)
	body, err := CanonicalManagerRequest(request, toolchain)
	if err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	if err := frameio.WriteStreamFrameHeader(
		&framed,
		[]byte(`{"type":"deployment-source"}`),
		uint64(len(body)),
	); err != nil {
		t.Fatal(err)
	}
	framed.Write(body)
	if _, err := ReadManagerRequest(
		context.Background(),
		io.NopCloser(bytes.NewReader(framed.Bytes())),
		toolchain,
	); err == nil {
		t.Fatal("ReadManagerRequest accepted the wrong stream header")
	}
}

func TestManagerRequestRejectsTrailingInput(t *testing.T) {
	t.Parallel()
	var framed bytes.Buffer
	request := managerResolveRequest()
	toolchain := managerRequestToolchain(request)
	if err := WriteManagerRequest(&framed, request, toolchain); err != nil {
		t.Fatal(err)
	}
	framed.WriteByte(0)
	if _, err := ReadManagerRequest(
		context.Background(),
		io.NopCloser(bytes.NewReader(framed.Bytes())),
		toolchain,
	); err == nil {
		t.Fatal("ReadManagerRequest accepted trailing input")
	}
}

func TestManagerMetadataRoundTripsAllOutcomes(t *testing.T) {
	t.Parallel()
	probe := managerProbeRequest()
	resolve := managerResolveRequest()
	lifecycle := managerLifecycleRequest()
	failureMessage := "manifest projection is invalid"
	failureReason := ManagerInvalidInput
	for _, test := range []struct {
		name     string
		request  ManagerRequest
		metadata ManagerMetadata
	}{
		{
			name:     "probe",
			request:  probe,
			metadata: managerSuccessMetadata(probe, "", nil),
		},
		{
			name:    "resolve",
			request: resolve,
			metadata: managerSuccessMetadata(
				resolve,
				ManagerOfflineStore,
				[]byte("tree"),
			),
		},
		{
			name:    "lifecycle",
			request: lifecycle,
			metadata: managerSuccessMetadata(
				lifecycle,
				ManagerRegistryClosure,
				[]byte("tree"),
			),
		},
		{
			name:    "failed",
			request: resolve,
			metadata: ManagerMetadata{
				FormatVersion: ManagerFormatVersion,
				Operation:     ManagerResolve,
				Outcome:       ManagerFailed,
				RequestDigest: mustManagerRequestDigest(t, resolve),
				Reason:        &failureReason,
				Message:       &failureMessage,
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			canonical, err := CanonicalManagerMetadata(test.metadata)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseManagerMetadata(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateManagerMetadataForRequest(
				parsed,
				test.request,
				managerRequestToolchain(test.request),
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagerFailureMappingIsClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		operation ManagerOperation
		reason    ManagerFailure
		want      BuildFailureReason
	}{
		{ManagerProbe, ManagerInvalidInput, BuildFailureManagerUnsupported},
		{ManagerProbe, ManagerProcessFailed, BuildFailureManagerUnsupported},
		{ManagerProbe, ManagerOutputInvalid, BuildFailureManagerUnsupported},
		{ManagerResolve, ManagerProcessFailed, BuildFailureDependencyFailed},
		{ManagerLifecycle, ManagerProcessFailed, BuildFailureDependencyFailed},
		{ManagerResolve, ManagerInvalidInput, BuildFailureOutputInvalid},
		{ManagerResolve, ManagerOutputInvalid, BuildFailureOutputInvalid},
		{ManagerLifecycle, ManagerInvalidInput, BuildFailureOutputInvalid},
		{ManagerLifecycle, ManagerOutputInvalid, BuildFailureOutputInvalid},
	} {
		got, err := ManagerBuildFailure(test.operation, test.reason)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf(
				"ManagerBuildFailure(%q, %q) = %q, want %q",
				test.operation,
				test.reason,
				got,
				test.want,
			)
		}
	}
	if _, err := ManagerBuildFailure(ManagerResolve, "unknown"); err == nil {
		t.Fatal("ManagerBuildFailure accepted an unknown reason")
	}
}

func TestManagerResponseStreamsAndChecksTreeIdentity(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	body := managerOfflineTree(t)
	metadata := managerSuccessMetadata(request, ManagerOfflineStore, body)
	response := &managerBuffer{}
	if err := WriteManagerResponse(
		context.Background(),
		response,
		request,
		managerRequestToolchain(request),
		metadata,
		io.NopCloser(bytes.NewReader(body)),
		nil,
	); err != nil {
		t.Fatal(err)
	}
	parsed, tree, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		t.TempDir(),
		request,
		managerRequestToolchain(request),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	treeBytes, err := io.ReadAll(tree)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RequestDigest != metadata.RequestDigest || !bytes.Equal(treeBytes, body) {
		t.Fatalf("parsed = %#v tree bytes = %d", parsed, len(treeBytes))
	}
}

func TestManagerProbeResponseRequiresImmediateEOF(t *testing.T) {
	t.Parallel()
	request := managerProbeRequest()
	metadata := managerSuccessMetadata(request, "", nil)
	response := &managerBuffer{}
	if err := WriteManagerResponse(
		context.Background(),
		response,
		request,
		managerRequestToolchain(request),
		metadata,
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	parsed, tree, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		"",
		request,
		managerRequestToolchain(request),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if tree != nil || parsed.ObservedVersion == nil ||
		*parsed.ObservedVersion != request.DependencyPlan.PackageManager.Version {
		t.Fatalf("probe response = %#v, tree = %#v", parsed, tree)
	}
	response.WriteByte(0)
	if _, _, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		"",
		request,
		managerRequestToolchain(request),
		nil,
	); err == nil {
		t.Fatal("ReadManagerResponse accepted bytes after probe metadata")
	}
}

func TestManagerResponseRejectsTreeDivergence(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	valid := managerOfflineTree(t)
	tests := map[string][]byte{
		"short":     valid[:len(valid)-1],
		"long":      append(append([]byte(nil), valid...), 'x'),
		"different": bytes.Repeat([]byte{'x'}, len(valid)),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			metadata := managerSuccessMetadata(request, ManagerOfflineStore, valid)
			var response bytes.Buffer
			canonical, err := CanonicalManagerMetadata(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := frameio.WriteMessageFrame(&response, canonical); err != nil {
				t.Fatal(err)
			}
			response.Write(body)
			if _, tree, err := ReadManagerResponse(
				context.Background(),
				io.NopCloser(bytes.NewReader(response.Bytes())),
				t.TempDir(),
				request,
				managerRequestToolchain(request),
				nil,
			); err == nil {
				if tree != nil {
					tree.Close()
				}
				t.Fatal("ReadManagerResponse returned nil error")
			}
		})
	}
}

func TestManagerFailedResponseRequiresImmediateEOF(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	reason := ManagerProcessFailed
	message := "manager rejected the project"
	metadata := ManagerMetadata{
		FormatVersion: ManagerFormatVersion,
		Operation:     ManagerResolve,
		Outcome:       ManagerFailed,
		RequestDigest: mustManagerRequestDigest(t, request),
		Reason:        &reason,
		Message:       &message,
	}
	response := &managerBuffer{}
	if err := WriteManagerResponse(
		context.Background(),
		response,
		request,
		managerRequestToolchain(request),
		metadata,
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	response.WriteByte(0)
	if _, tree, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		t.TempDir(),
		request,
		managerRequestToolchain(request),
		nil,
	); err == nil {
		if tree != nil {
			tree.Close()
		}
		t.Fatal("ReadManagerResponse accepted bytes after failed metadata")
	}
}

func TestManagerMetadataRejectsTargetDivergence(t *testing.T) {
	t.Parallel()
	request := managerLifecycleRequest()
	metadata := managerSuccessMetadata(request, ManagerRegistryClosure, []byte("tree"))
	other := managerDigest("other graph")
	metadata.PackageGraphDigest = &other
	if err := ValidateManagerMetadataForRequest(
		metadata,
		request,
		managerRequestToolchain(request),
	); err == nil {
		t.Fatal("ValidateManagerMetadataForRequest accepted graph divergence")
	}
	probe := managerProbeRequest()
	probeMetadata := managerSuccessMetadata(probe, "", nil)
	otherVersion := "1.3.11"
	probeMetadata.ObservedVersion = &otherVersion
	if err := ValidateManagerMetadataForRequest(
		probeMetadata,
		probe,
		managerRequestToolchain(probe),
	); err == nil {
		t.Fatal("ValidateManagerMetadataForRequest accepted version divergence")
	}
}

func TestManagerResponseRejectsInvalidTreeProfiles(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	escapingLink := managerTree(t, []treeEntry{
		{Path: "dir", Kind: artifactEntryDirectory, Mode: 0755},
		{
			Path:       "dir/link",
			Kind:       artifactEntrySymlink,
			Mode:       0777,
			LinkTarget: "../../outside",
		},
	})
	valid := managerOfflineTree(t)
	tests := map[string][]byte{
		"not tar":            []byte("not a tar stream"),
		"missing end marker": valid[:len(valid)-int(tarBlockBytes)],
		"extra end marker":   append(append([]byte(nil), valid...), make([]byte, tarBlockBytes)...),
		"escaping link":      escapingLink,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			metadata := managerSuccessMetadata(request, ManagerOfflineStore, body)
			var response bytes.Buffer
			canonical, err := CanonicalManagerMetadata(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := frameio.WriteMessageFrame(&response, canonical); err != nil {
				t.Fatal(err)
			}
			response.Write(body)
			stageDirectory := t.TempDir()
			_, tree, err := ReadManagerResponse(
				context.Background(),
				io.NopCloser(bytes.NewReader(response.Bytes())),
				stageDirectory,
				request,
				managerRequestToolchain(request),
				nil,
			)
			if err == nil {
				tree.Close()
				t.Fatal("ReadManagerResponse accepted an invalid manager tree")
			}
			if tree != nil {
				tree.Close()
				t.Fatal("ReadManagerResponse exposed a rejected manager tree")
			}
			remaining, readErr := os.ReadDir(stageDirectory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(remaining) != 0 {
				t.Fatal("ReadManagerResponse retained a rejected manager tree")
			}
		})
	}
}

func TestManagerLifecycleResponseUsesInputGraphProfile(t *testing.T) {
	t.Parallel()
	request := managerLifecycleRequest()
	graph := managerPackageGraph()
	body := managerTree(t, nil)
	metadata := managerSuccessMetadata(request, ManagerRegistryClosure, body)
	response := &managerBuffer{}
	if err := WriteManagerResponse(
		context.Background(),
		response,
		request,
		managerRequestToolchain(request),
		metadata,
		io.NopCloser(bytes.NewReader(body)),
		&graph,
	); err != nil {
		t.Fatal(err)
	}
	_, output, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		t.TempDir(),
		request,
		managerRequestToolchain(request),
		&graph,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	outputBytes, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(outputBytes, body) {
		t.Fatal("lifecycle tree changed")
	}
	if _, tree, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		t.TempDir(),
		request,
		managerRequestToolchain(request),
		nil,
	); err == nil {
		if tree != nil {
			tree.Close()
		}
		t.Fatal("ReadManagerResponse accepted lifecycle output without its graph")
	}
}

func TestManagerLifecycleResponseAcceptsListedNestedPackage(t *testing.T) {
	t.Parallel()
	graph := managerNestedPackageGraph()
	request := managerLifecycleRequestForGraph(graph)
	body := managerTree(t, []treeEntry{
		{Path: "parent", Kind: artifactEntryDirectory, Mode: 0755},
		{
			Path:      "parent/index.js",
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: 1,
			Content:   strings.NewReader("p"),
		},
		{Path: "parent/node_modules", Kind: artifactEntryDirectory, Mode: 0755},
		{Path: "parent/node_modules/child", Kind: artifactEntryDirectory, Mode: 0755},
		{
			Path:      "parent/node_modules/child/index.js",
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: 1,
			Content:   strings.NewReader("c"),
		},
	})
	metadata := managerSuccessMetadata(request, ManagerRegistryClosure, body)
	response := &managerBuffer{}
	if err := WriteManagerResponse(
		context.Background(),
		response,
		request,
		managerRequestToolchain(request),
		metadata,
		io.NopCloser(bytes.NewReader(body)),
		&graph,
	); err != nil {
		t.Fatal(err)
	}
	_, tree, err := ReadManagerResponse(
		context.Background(),
		io.NopCloser(bytes.NewReader(response.Bytes())),
		t.TempDir(),
		request,
		managerRequestToolchain(request),
		&graph,
	)
	if err != nil {
		t.Fatal(err)
	}
	tree.Close()

	empty := managerTree(t, nil)
	if err := WriteManagerResponse(
		context.Background(),
		&managerBuffer{},
		request,
		managerRequestToolchain(request),
		managerSuccessMetadata(request, ManagerRegistryClosure, empty),
		io.NopCloser(bytes.NewReader(empty)),
		&graph,
	); err == nil {
		t.Fatal("WriteManagerResponse accepted a closure missing listed package roots")
	}
}

func TestManagerResponseCancellationClosesBlockedSource(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	body := managerOfflineTree(t)
	metadata := managerSuccessMetadata(request, ManagerOfflineStore, body)
	canonical, err := CanonicalManagerMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var prefix bytes.Buffer
	if err := frameio.WriteMessageFrame(&prefix, canonical); err != nil {
		t.Fatal(err)
	}
	prefix.Write(body[:len(body)-1])

	for _, test := range []struct {
		name   string
		prefix []byte
	}{
		{name: "mid body", prefix: prefix.Bytes()},
		{name: "final EOF", prefix: appendFramedManagerTree(t, metadata, body)},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := newBlockingManagerReader(test.prefix)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			stageDirectory := t.TempDir()
			go func() {
				_, tree, err := ReadManagerResponse(
					ctx,
					source,
					stageDirectory,
					request,
					managerRequestToolchain(request),
					nil,
				)
				if tree != nil {
					tree.Close()
				}
				done <- err
			}()
			select {
			case <-source.blocked:
			case <-time.After(time.Second):
				t.Fatal("response reader did not block")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("ReadManagerResponse error = %v, want context cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("ReadManagerResponse did not stop after cancellation")
			}
		})
	}
}

func TestManagerResponseCancellationClosesBlockedDestination(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	body := managerOfflineTree(t)
	metadata := managerSuccessMetadata(request, ManagerOfflineStore, body)
	destination := newBlockingManagerWriter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- WriteManagerResponse(
			ctx,
			destination,
			request,
			managerRequestToolchain(request),
			metadata,
			io.NopCloser(bytes.NewReader(body)),
			nil,
		)
	}()
	select {
	case <-destination.blocked:
	case <-time.After(time.Second):
		t.Fatal("response writer did not block")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteManagerResponse error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WriteManagerResponse did not stop after cancellation")
	}
}

func managerProbeRequest() ManagerRequest {
	capsule := managerCapsuleFixture(PackageManagerBun, ArchitectureAArch64)
	toolchain := dependencyPlanToolchain(ArchitectureAArch64)
	plan, err := NewDependencyPlan(
		capsule,
		toolchain,
		DependencyMaterializerVersion,
	)
	if err != nil {
		panic(err)
	}
	planDigest, err := DependencyPlanDigest(plan)
	if err != nil {
		panic(err)
	}
	return ManagerRequest{
		DependencyPlan:       plan,
		DependencyPlanDigest: planDigest,
		FormatVersion:        ManagerFormatVersion,
		ManagerCapsule:       capsule,
		ManagerTree:          capsule.Tree,
		Operation:            ManagerProbe,
		Runtime: ManagerArtifact{
			Digest:    plan.ManagedRuntimeDigest,
			MediaType: RuntimeArtifactMediaType,
			SizeBytes: 4096,
		},
		StandardToolchain: toolchain.ToolchainClosure,
	}
}

func managerResolveRequest() ManagerRequest {
	request := managerProbeRequest()
	request.Operation = ManagerResolve
	project := managerProjectArtifact()
	request.Project = &project
	return request
}

func managerProjectArtifact() ManagerArtifact {
	return ManagerArtifact{
		Digest:    managerDigest("project"),
		MediaType: ManagerProjectMediaType,
		SizeBytes: 4096,
	}
}

func managerLifecycleRequest() ManagerRequest {
	return managerLifecycleRequestForGraph(managerPackageGraph())
}

func managerLifecycleRequestForGraph(graph PackageGraph) ManagerRequest {
	request := managerResolveRequest()
	request.Operation = ManagerLifecycle
	graphFile := managerGraphFileForGraph(graph)
	request.PackageGraph = &graphFile
	store := ManagerArtifact{
		Digest:    managerDigest("offline store"),
		MediaType: ManagerOfflineStoreMediaType,
		SizeBytes: 16384,
	}
	request.OfflineStore = &store
	return request
}

func managerGraphFile() ProgramFile {
	return managerGraphFileForGraph(managerPackageGraph())
}

func managerGraphFileForGraph(graph PackageGraph) ProgramFile {
	canonical, err := CanonicalPackageGraph(graph)
	if err != nil {
		panic(err)
	}
	return ProgramFile{
		Digest:    managerDigestBytes(canonical),
		SizeBytes: int64(len(canonical)),
	}
}

func managerPackageGraph() PackageGraph {
	return PackageGraph{
		FormatVersion: PackageGraphFormatVersion,
		LocalPackages: []LocalPackage{
			{
				ManifestDigest: managerDigest("root manifest"),
				Path:           ".",
			},
		},
		RegistryPackages: []RegistryPackage{},
		Resolutions:      []PackageResolution{},
	}
}

func managerNestedPackageGraph() PackageGraph {
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512DigestBytes))
	return PackageGraph{
		FormatVersion: PackageGraphFormatVersion,
		LocalPackages: []LocalPackage{
			{
				ManifestDigest: managerDigest("root manifest"),
				Path:           ".",
			},
		},
		RegistryPackages: []RegistryPackage{
			{
				InstallPath: "parent",
				Integrity:   integrity,
				Name:        "parent",
				Version:     "1.0.0",
			},
			{
				InstallPath: "parent/node_modules/child",
				Integrity:   integrity,
				Name:        "child",
				Version:     "1.0.0",
			},
		},
		Resolutions: []PackageResolution{},
	}
}

func managerSuccessMetadata(
	request ManagerRequest,
	kind ManagerTreeKind,
	body []byte,
) ManagerMetadata {
	metadata := ManagerMetadata{
		FormatVersion: ManagerFormatVersion,
		Operation:     request.Operation,
		Outcome:       ManagerSucceeded,
		RequestDigest: mustManagerRequestDigest(nil, request),
	}
	switch request.Operation {
	case ManagerProbe:
		version := request.DependencyPlan.PackageManager.Version
		metadata.ObservedVersion = &version
	case ManagerResolve:
		metadata.Tree = &ManagerTree{
			Digest:    managerDigestBytes(body),
			Kind:      kind,
			SizeBytes: int64(len(body)),
		}
		graph := managerPackageGraph()
		metadata.PackageGraph = &graph
	case ManagerLifecycle:
		metadata.Tree = &ManagerTree{
			Digest:    managerDigestBytes(body),
			Kind:      kind,
			SizeBytes: int64(len(body)),
		}
		digest := request.PackageGraph.Digest
		metadata.PackageGraphDigest = &digest
	}
	return metadata
}

func managerOfflineTree(t *testing.T) []byte {
	t.Helper()
	return managerTree(t, []treeEntry{
		{Path: "store", Kind: artifactEntryDirectory, Mode: 0755},
		{
			Path:      "store/package.tgz",
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: 7,
			Content:   strings.NewReader("package"),
		},
	})
}

func managerTree(t *testing.T, entries []treeEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	sequence := func(yield func(treeEntry, error) bool) {
		for _, entry := range entries {
			if !yield(entry, nil) {
				return
			}
		}
	}
	if err := writeTreeArchive(
		context.Background(),
		&output,
		dependencyArtifact,
		iter.Seq2[treeEntry, error](sequence),
		true,
	); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func appendFramedManagerTree(
	t *testing.T,
	metadata ManagerMetadata,
	body []byte,
) []byte {
	t.Helper()
	canonical, err := CanonicalManagerMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := frameio.WriteMessageFrame(&output, canonical); err != nil {
		t.Fatal(err)
	}
	output.Write(body)
	return output.Bytes()
}

type managerBuffer struct {
	bytes.Buffer
}

func (*managerBuffer) Close() error {
	return nil
}

type blockingManagerReader struct {
	reader  *bytes.Reader
	blocked chan struct{}
	closed  chan struct{}
	once    sync.Once
}

type blockingManagerWriter struct {
	blocked chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingManagerReader(prefix []byte) *blockingManagerReader {
	return &blockingManagerReader{
		reader:  bytes.NewReader(prefix),
		blocked: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (reader *blockingManagerReader) Read(destination []byte) (int, error) {
	if reader.reader.Len() != 0 {
		return reader.reader.Read(destination)
	}
	reader.once.Do(func() {
		close(reader.blocked)
	})
	<-reader.closed
	return 0, io.ErrClosedPipe
}

func (reader *blockingManagerReader) Close() error {
	select {
	case <-reader.closed:
	default:
		close(reader.closed)
	}
	return nil
}

func newBlockingManagerWriter() *blockingManagerWriter {
	return &blockingManagerWriter{
		blocked: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (writer *blockingManagerWriter) Write([]byte) (int, error) {
	writer.once.Do(func() {
		close(writer.blocked)
	})
	<-writer.closed
	return 0, io.ErrClosedPipe
}

func (writer *blockingManagerWriter) Close() error {
	select {
	case <-writer.closed:
	default:
		close(writer.closed)
	}
	return nil
}

func mustManagerRequestDigest(t *testing.T, request ManagerRequest) string {
	if t != nil {
		t.Helper()
	}
	digest, err := ManagerRequestDigest(
		request,
		managerRequestToolchain(request),
	)
	if err != nil {
		if t == nil {
			panic(err)
		}
		t.Fatal(err)
	}
	return digest
}

func managerRequestToolchain(request ManagerRequest) Toolchain {
	return dependencyPlanToolchain(request.DependencyPlan.Architecture)
}

func managerDigest(value string) string {
	return managerDigestBytes([]byte(value))
}

func managerDigestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
