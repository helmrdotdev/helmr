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
		managerResolveRequest(),
		managerLifecycleRequest(),
	} {
		request := request
		t.Run(string(request.Operation), func(t *testing.T) {
			t.Parallel()
			canonical, err := CanonicalManagerRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseManagerRequest(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Operation != request.Operation ||
				parsed.PackageManager != request.PackageManager ||
				parsed.Project != request.Project ||
				parsed.ToolClosure != request.ToolClosure {
				t.Fatalf("parsed request = %#v", parsed)
			}

			var framed bytes.Buffer
			if err := WriteManagerRequest(&framed, request); err != nil {
				t.Fatal(err)
			}
			source := bytes.NewReader(framed.Bytes())
			decoded, err := ReadManagerRequest(
				context.Background(),
				io.NopCloser(source),
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
	digest, err := ManagerRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalManagerRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	plain := sha256.Sum256(canonical)
	if digest == "sha256:"+hex.EncodeToString(plain[:]) {
		t.Fatal("manager request digest is not domain separated")
	}
	const want = "sha256:647710ce0920875e087b05ed72637b96c4a216af0ed2d7f2946fd476dec47188"
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
			request.Project.MediaType = ManagerToolMediaType
			return request
		},
		"zero project size": func(request ManagerRequest) ManagerRequest {
			request.Project.SizeBytes = 0
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
			if _, err := CanonicalManagerRequest(request); err == nil {
				t.Fatal("CanonicalManagerRequest returned nil error")
			}
		})
	}
}

func TestManagerRequestRejectsNonCanonicalUnknownAndDuplicateJSON(t *testing.T) {
	t.Parallel()
	request := managerResolveRequest()
	canonical, err := CanonicalManagerRequest(request)
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
	if _, err := ParseManagerRequest(unknown); err == nil {
		t.Fatal("ParseManagerRequest accepted an unknown member")
	}

	nonCanonical := append([]byte("{\n"), canonical[1:]...)
	if _, err := ParseManagerRequest(nonCanonical); err == nil {
		t.Fatal("ParseManagerRequest accepted non-canonical JSON")
	}

	duplicate := append(
		[]byte(`{"architecture":"aarch64",`),
		canonical[1:]...,
	)
	if _, err := ParseManagerRequest(duplicate); err == nil {
		t.Fatal("ParseManagerRequest accepted a duplicate member")
	}
}

func TestManagerRequestRejectsWrongStreamHeader(t *testing.T) {
	t.Parallel()
	body, err := CanonicalManagerRequest(managerResolveRequest())
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
	); err == nil {
		t.Fatal("ReadManagerRequest accepted the wrong stream header")
	}
}

func TestManagerRequestRejectsTrailingInput(t *testing.T) {
	t.Parallel()
	var framed bytes.Buffer
	if err := WriteManagerRequest(&framed, managerResolveRequest()); err != nil {
		t.Fatal(err)
	}
	framed.WriteByte(0)
	if _, err := ReadManagerRequest(
		context.Background(),
		io.NopCloser(bytes.NewReader(framed.Bytes())),
	); err == nil {
		t.Fatal("ReadManagerRequest accepted trailing input")
	}
}

func TestManagerMetadataRoundTripsAllOutcomes(t *testing.T) {
	t.Parallel()
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
			if err := ValidateManagerMetadataForRequest(parsed, test.request); err != nil {
				t.Fatal(err)
			}
		})
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
	if err := ValidateManagerMetadataForRequest(metadata, request); err == nil {
		t.Fatal("ValidateManagerMetadataForRequest accepted graph divergence")
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
				_, tree, err := ReadManagerResponse(ctx, source, stageDirectory, request, nil)
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

func managerResolveRequest() ManagerRequest {
	return ManagerRequest{
		Architecture:        ArchitectureAArch64,
		FormatVersion:       ManagerFormatVersion,
		MaterializerVersion: DependencyMaterializerVersion,
		Operation:           ManagerResolve,
		PackageManager: PackageManager{
			Name:    PackageManagerBun,
			Version: "1.3.10",
		},
		Project: ManagerArtifact{
			Digest:    managerDigest("project"),
			MediaType: ManagerProjectMediaType,
			SizeBytes: 4096,
		},
		ToolClosure: ManagerArtifact{
			Digest:    managerDigest("tool closure"),
			MediaType: ManagerToolMediaType,
			SizeBytes: 8192,
		},
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
		Tree: &ManagerTree{
			Digest:    managerDigestBytes(body),
			Kind:      kind,
			SizeBytes: int64(len(body)),
		},
	}
	if request.Operation == ManagerResolve {
		graph := managerPackageGraph()
		metadata.PackageGraph = &graph
	} else {
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
	digest, err := ManagerRequestDigest(request)
	if err != nil {
		if t == nil {
			panic(err)
		}
		t.Fatal(err)
	}
	return digest
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
