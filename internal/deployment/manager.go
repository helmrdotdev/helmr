package deployment

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ManagerFormatVersion = 0

	ManagerResolve   = ManagerOperation("resolve")
	ManagerLifecycle = ManagerOperation("lifecycle")

	ManagerSucceeded = ManagerOutcome("succeeded")
	ManagerFailed    = ManagerOutcome("failed")

	ManagerOfflineStore    = ManagerTreeKind("offlineStore")
	ManagerRegistryClosure = ManagerTreeKind("registryClosure")

	ManagerInvalidInput  = ManagerFailure("invalidInput")
	ManagerProcessFailed = ManagerFailure("managerFailed")
	ManagerOutputInvalid = ManagerFailure("outputInvalid")

	ManagerDependencyToolsMediaType = "application/vnd.helmr.dependency-tools.v0+squashfs"
	ManagerProjectMediaType         = "application/vnd.helmr.package-manager-project.v0+squashfs"
	ManagerOfflineStoreMediaType    = "application/vnd.helmr.package-manager-offline-store.v0+squashfs"

	maxManagerRequestBytes  = 64 << 10
	maxManagerMetadataBytes = (16 << 20) + maxManagerRequestBytes
	maxManagerMessageBytes  = 16 << 10
	maxManagerProjectBytes  = 384 << 20
	maxManagerTreeBytes     = 9 << 30

	managerRequestDigestDomain = "helmr.dependency-manager-request.v0\x00"
)

var managerRequestHeader = []byte(`{"type":"dependency-manager"}`)

type ManagerOperation string
type ManagerOutcome string
type ManagerTreeKind string
type ManagerFailure string

type ManagerArtifact struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ManagerRequest struct {
	Architecture            RuntimeArchitecture `json:"architecture"`
	ComponentManifestDigest string              `json:"componentManifestDigest"`
	DependencyTools         ManagerArtifact     `json:"dependencyTools"`
	DependencyToolsDigest   string              `json:"dependencyToolsDigest"`
	FormatVersion           int                 `json:"formatVersion"`
	MaterializerVersion     string              `json:"materializerVersion"`
	Operation               ManagerOperation    `json:"operation"`
	PackageManager          PackageManager      `json:"packageManager"`
	Project                 ManagerArtifact     `json:"project"`
	PackageGraph            *ProgramFile        `json:"packageGraph,omitempty"`
	OfflineStore            *ManagerArtifact    `json:"offlineStore,omitempty"`
}

type ManagerTree struct {
	Digest    string          `json:"digest"`
	Kind      ManagerTreeKind `json:"kind"`
	SizeBytes int64           `json:"sizeBytes"`
}

type ManagerMetadata struct {
	FormatVersion      int              `json:"formatVersion"`
	Operation          ManagerOperation `json:"operation"`
	Outcome            ManagerOutcome   `json:"outcome"`
	RequestDigest      string           `json:"requestDigest"`
	PackageGraph       *PackageGraph    `json:"packageGraph,omitempty"`
	PackageGraphDigest *string          `json:"packageGraphDigest,omitempty"`
	Tree               *ManagerTree     `json:"tree,omitempty"`
	Reason             *ManagerFailure  `json:"reason,omitempty"`
	Message            *string          `json:"message,omitempty"`
}

type ManagerTreeContent interface {
	io.Reader
	io.Seeker
	io.Closer
}

func ParseManagerRequest(raw []byte) (ManagerRequest, error) {
	if len(raw) == 0 || len(raw) > maxManagerRequestBytes {
		return ManagerRequest{}, fmt.Errorf(
			"manager request size is outside [1,%d]",
			maxManagerRequestBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ManagerRequest{}, fmt.Errorf("canonicalize manager request: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ManagerRequest{}, errors.New("manager request is not RFC 8785 canonical JSON")
	}

	var request ManagerRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ManagerRequest{}, fmt.Errorf("decode manager request: %w", err)
	}
	if err := ensureEOF(decoder, "manager request"); err != nil {
		return ManagerRequest{}, err
	}
	if err := ValidateManagerRequest(request); err != nil {
		return ManagerRequest{}, err
	}
	complete, err := CanonicalManagerRequest(request)
	if err != nil {
		return ManagerRequest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerRequest{}, errors.New(
			"manager request does not match the complete canonical v0 shape",
		)
	}
	return request, nil
}

func CanonicalManagerRequest(request ManagerRequest) ([]byte, error) {
	if err := ValidateManagerRequest(request); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode manager request: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize manager request: %w", err)
	}
	if len(canonical) > maxManagerRequestBytes {
		return nil, fmt.Errorf(
			"manager request size is outside [1,%d]",
			maxManagerRequestBytes,
		)
	}
	return canonical, nil
}

func ValidateManagerRequest(request ManagerRequest) error {
	if request.FormatVersion != ManagerFormatVersion {
		return fmt.Errorf(
			"manager request formatVersion = %d, want %d",
			request.FormatVersion,
			ManagerFormatVersion,
		)
	}
	if request.MaterializerVersion != DependencyMaterializerVersion {
		return fmt.Errorf(
			"manager request materializerVersion = %q, want %q",
			request.MaterializerVersion,
			DependencyMaterializerVersion,
		)
	}
	if !validArchitecture(request.Architecture) {
		return fmt.Errorf("manager request architecture %q is unsupported", request.Architecture)
	}
	if !sha256DigestPattern.MatchString(request.ComponentManifestDigest) {
		return errors.New("manager request componentManifestDigest is not a lowercase SHA-256 digest")
	}
	if !sha256DigestPattern.MatchString(request.DependencyToolsDigest) {
		return errors.New("manager request dependencyToolsDigest is not a lowercase SHA-256 digest")
	}
	if err := validateManagerPackage(request.PackageManager); err != nil {
		return err
	}
	if err := validateManagerArtifact(
		request.Project,
		ManagerProjectMediaType,
		maxManagerProjectBytes,
		"project",
	); err != nil {
		return err
	}
	if err := validateManagerArtifact(
		request.DependencyTools,
		ManagerDependencyToolsMediaType,
		maxJSONSafeInteger,
		"dependencyTools",
	); err != nil {
		return err
	}

	switch request.Operation {
	case ManagerResolve:
		if request.PackageGraph != nil || request.OfflineStore != nil {
			return errors.New("manager resolve request forbids packageGraph and offlineStore")
		}
	case ManagerLifecycle:
		if request.PackageGraph == nil || request.OfflineStore == nil {
			return errors.New("manager lifecycle request requires packageGraph and offlineStore")
		}
		if err := validateManagerFile(*request.PackageGraph, "packageGraph"); err != nil {
			return err
		}
		if err := validateManagerArtifact(
			*request.OfflineStore,
			ManagerOfflineStoreMediaType,
			maxManagerTreeBytes,
			"offlineStore",
		); err != nil {
			return err
		}
	default:
		return fmt.Errorf("manager request operation %q is unsupported", request.Operation)
	}
	return nil
}

func ManagerRequestDigest(request ManagerRequest) (string, error) {
	canonical, err := CanonicalManagerRequest(request)
	if err != nil {
		return "", err
	}
	digest := domainDigest(managerRequestDigestDomain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func WriteManagerRequest(destination io.Writer, request ManagerRequest) error {
	if destination == nil {
		return errors.New("manager request destination is nil")
	}
	body, err := CanonicalManagerRequest(request)
	if err != nil {
		return err
	}
	if err := frameio.WriteStreamFrameHeader(
		destination,
		managerRequestHeader,
		uint64(len(body)),
	); err != nil {
		return fmt.Errorf("write manager request header: %w", err)
	}
	if _, err := destination.Write(body); err != nil {
		return fmt.Errorf("write manager request body: %w", err)
	}
	return nil
}

func ReadManagerRequest(
	ctx context.Context,
	source io.ReadCloser,
) (ManagerRequest, error) {
	if ctx == nil {
		return ManagerRequest{}, errors.New("manager request context is nil")
	}
	if source == nil {
		return ManagerRequest{}, errors.New("manager request source is nil")
	}
	stop := closeOnCancellation(ctx, source)
	defer stop()
	header, bodyLen, err := frameio.ReadStreamFrameHeaderBounded(
		source,
		uint32(len(managerRequestHeader)),
		maxManagerRequestBytes,
	)
	if err != nil {
		return ManagerRequest{}, fmt.Errorf("read manager request header: %w", err)
	}
	if !bytes.Equal(header, managerRequestHeader) {
		return ManagerRequest{}, errors.New("manager request header is not the exact v0 header")
	}
	if bodyLen == 0 {
		return ManagerRequest{}, errors.New("manager request body is empty")
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(source, body); err != nil {
		return ManagerRequest{}, fmt.Errorf("read manager request body: %w", err)
	}
	request, err := ParseManagerRequest(body)
	if err != nil {
		return ManagerRequest{}, err
	}
	if err := copyTreeContent(ctx, io.Discard, source, 0); err != nil {
		return ManagerRequest{}, fmt.Errorf("read manager request EOF: %w", preferContextError(ctx, err))
	}
	return request, nil
}

func ParseManagerMetadata(raw []byte) (ManagerMetadata, error) {
	if len(raw) == 0 || len(raw) > maxManagerMetadataBytes {
		return ManagerMetadata{}, fmt.Errorf(
			"manager metadata size is outside [1,%d]",
			maxManagerMetadataBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ManagerMetadata{}, fmt.Errorf("canonicalize manager metadata: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ManagerMetadata{}, errors.New("manager metadata is not RFC 8785 canonical JSON")
	}

	var metadata ManagerMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return ManagerMetadata{}, fmt.Errorf("decode manager metadata: %w", err)
	}
	if err := ensureEOF(decoder, "manager metadata"); err != nil {
		return ManagerMetadata{}, err
	}
	if err := ValidateManagerMetadata(metadata); err != nil {
		return ManagerMetadata{}, err
	}
	complete, err := CanonicalManagerMetadata(metadata)
	if err != nil {
		return ManagerMetadata{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerMetadata{}, errors.New(
			"manager metadata does not match the complete canonical v0 shape",
		)
	}
	return metadata, nil
}

func CanonicalManagerMetadata(metadata ManagerMetadata) ([]byte, error) {
	if err := ValidateManagerMetadata(metadata); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode manager metadata: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize manager metadata: %w", err)
	}
	if len(canonical) > maxManagerMetadataBytes {
		return nil, fmt.Errorf(
			"manager metadata size is outside [1,%d]",
			maxManagerMetadataBytes,
		)
	}
	return canonical, nil
}

func ValidateManagerMetadata(metadata ManagerMetadata) error {
	if metadata.FormatVersion != ManagerFormatVersion {
		return fmt.Errorf(
			"manager metadata formatVersion = %d, want %d",
			metadata.FormatVersion,
			ManagerFormatVersion,
		)
	}
	if metadata.Operation != ManagerResolve && metadata.Operation != ManagerLifecycle {
		return fmt.Errorf("manager metadata operation %q is unsupported", metadata.Operation)
	}
	if !sha256DigestPattern.MatchString(metadata.RequestDigest) {
		return errors.New("manager metadata requestDigest is not a lowercase SHA-256 digest")
	}

	switch metadata.Outcome {
	case ManagerSucceeded:
		if metadata.Tree == nil || metadata.Reason != nil || metadata.Message != nil {
			return errors.New("successful manager metadata has an invalid outcome shape")
		}
		if err := validateManagerTree(*metadata.Tree); err != nil {
			return err
		}
		if metadata.Operation == ManagerResolve {
			if metadata.PackageGraph == nil || metadata.PackageGraphDigest != nil {
				return errors.New("manager resolve success requires only packageGraph")
			}
			if metadata.Tree.Kind != ManagerOfflineStore {
				return fmt.Errorf(
					"manager resolve tree kind = %q, want %q",
					metadata.Tree.Kind,
					ManagerOfflineStore,
				)
			}
			if err := ValidatePackageGraph(*metadata.PackageGraph); err != nil {
				return fmt.Errorf("manager metadata packageGraph: %w", err)
			}
		} else {
			if metadata.PackageGraph != nil || metadata.PackageGraphDigest == nil {
				return errors.New("manager lifecycle success requires only packageGraphDigest")
			}
			if !sha256DigestPattern.MatchString(*metadata.PackageGraphDigest) {
				return errors.New(
					"manager metadata packageGraphDigest is not a lowercase SHA-256 digest",
				)
			}
			if metadata.Tree.Kind != ManagerRegistryClosure {
				return fmt.Errorf(
					"manager lifecycle tree kind = %q, want %q",
					metadata.Tree.Kind,
					ManagerRegistryClosure,
				)
			}
		}
	case ManagerFailed:
		if metadata.PackageGraph != nil ||
			metadata.PackageGraphDigest != nil ||
			metadata.Tree != nil ||
			metadata.Reason == nil ||
			metadata.Message == nil {
			return errors.New("failed manager metadata has an invalid outcome shape")
		}
		switch *metadata.Reason {
		case ManagerInvalidInput, ManagerProcessFailed, ManagerOutputInvalid:
		default:
			return fmt.Errorf("manager metadata failure reason %q is unsupported", *metadata.Reason)
		}
		if len(*metadata.Message) == 0 ||
			len(*metadata.Message) > maxManagerMessageBytes ||
			!utf8.ValidString(*metadata.Message) {
			return fmt.Errorf(
				"manager metadata failure message is outside [1,%d] UTF-8 bytes",
				maxManagerMessageBytes,
			)
		}
	default:
		return fmt.Errorf("manager metadata outcome %q is unsupported", metadata.Outcome)
	}
	return nil
}

func WriteManagerMetadata(destination io.Writer, metadata ManagerMetadata) error {
	if destination == nil {
		return errors.New("manager metadata destination is nil")
	}
	body, err := CanonicalManagerMetadata(metadata)
	if err != nil {
		return err
	}
	if err := frameio.WriteMessageFrame(destination, body); err != nil {
		return fmt.Errorf("write manager metadata: %w", err)
	}
	return nil
}

func ReadManagerMetadata(source io.Reader) (ManagerMetadata, error) {
	if source == nil {
		return ManagerMetadata{}, errors.New("manager metadata source is nil")
	}
	body, err := frameio.ReadMessageFrameBounded(source, maxManagerMetadataBytes)
	if err != nil {
		return ManagerMetadata{}, fmt.Errorf("read manager metadata: %w", err)
	}
	return ParseManagerMetadata(body)
}

func WriteManagerResponse(
	ctx context.Context,
	destination io.WriteCloser,
	request ManagerRequest,
	metadata ManagerMetadata,
	tree io.ReadCloser,
	graph *PackageGraph,
) error {
	if ctx == nil {
		return errors.New("manager response context is nil")
	}
	if destination == nil {
		return errors.New("manager response destination is nil")
	}
	stopDestination := closeOnCancellation(ctx, destination)
	defer stopDestination()
	if err := ValidateManagerMetadataForRequest(metadata, request); err != nil {
		return err
	}
	if err := validateManagerTreeGraph(request, metadata, graph); err != nil {
		return err
	}
	if metadata.Outcome == ManagerFailed {
		if tree != nil {
			return errors.New("failed manager response forbids a tree")
		}
		if err := WriteManagerMetadata(destination, metadata); err != nil {
			return err
		}
		return nil
	}
	if tree == nil {
		return errors.New("successful manager response requires a tree")
	}
	stopTree := closeOnCancellation(ctx, tree)
	defer stopTree()
	if err := WriteManagerMetadata(destination, metadata); err != nil {
		return preferContextError(ctx, err)
	}
	if err := rewriteManagerTree(
		ctx,
		destination,
		tree,
		*metadata.Tree,
		graph,
	); err != nil {
		return fmt.Errorf("write manager response tree: %w", preferContextError(ctx, err))
	}
	return nil
}

func ReadManagerResponse(
	ctx context.Context,
	source io.ReadCloser,
	stageDirectory string,
	request ManagerRequest,
	graph *PackageGraph,
) (ManagerMetadata, ManagerTreeContent, error) {
	if ctx == nil {
		return ManagerMetadata{}, nil, errors.New("manager response context is nil")
	}
	if source == nil {
		return ManagerMetadata{}, nil, errors.New("manager response source is nil")
	}
	if stageDirectory == "" {
		return ManagerMetadata{}, nil, errors.New("manager tree stage directory is empty")
	}
	stop := closeOnCancellation(ctx, source)
	defer stop()
	metadata, err := ReadManagerMetadata(source)
	if err != nil {
		return ManagerMetadata{}, nil, preferContextError(ctx, err)
	}
	if err := ValidateManagerMetadataForRequest(metadata, request); err != nil {
		return ManagerMetadata{}, nil, err
	}
	if err := validateManagerTreeGraph(request, metadata, graph); err != nil {
		return ManagerMetadata{}, nil, err
	}
	if metadata.Outcome == ManagerFailed {
		if err := copyTreeContent(ctx, io.Discard, source, 0); err != nil {
			return ManagerMetadata{}, nil, fmt.Errorf(
				"read failed manager response EOF: %w",
				preferContextError(ctx, err),
			)
		}
		return metadata, nil, nil
	}

	stage, err := os.CreateTemp(stageDirectory, "helmr-manager-tree-*")
	if err != nil {
		return ManagerMetadata{}, nil, fmt.Errorf("create manager tree stage: %w", err)
	}
	if err := os.Remove(stage.Name()); err != nil {
		_ = stage.Close()
		return ManagerMetadata{}, nil, fmt.Errorf("unlink manager tree stage: %w", err)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = stage.Close()
		}
	}()
	stopStage := closeOnCancellation(ctx, stage)
	if err := rewriteManagerTree(
		ctx,
		stage,
		source,
		*metadata.Tree,
		graph,
	); err != nil {
		stopStage()
		return ManagerMetadata{}, nil, fmt.Errorf(
			"read manager response tree: %w",
			preferContextError(ctx, err),
		)
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		stopStage()
		return ManagerMetadata{}, nil, fmt.Errorf(
			"rewind manager tree stage: %w",
			preferContextError(ctx, err),
		)
	}
	if !stopStage() {
		return ManagerMetadata{}, nil, preferContextError(ctx, errors.New("manager tree stage closed"))
	}
	keepStage = true
	return metadata, stage, nil
}

func closeOnCancellation(ctx context.Context, closer io.Closer) func() bool {
	return context.AfterFunc(ctx, func() {
		_ = closer.Close()
	})
}

func preferContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func ValidateManagerMetadataForRequest(
	metadata ManagerMetadata,
	request ManagerRequest,
) error {
	if err := ValidateManagerRequest(request); err != nil {
		return err
	}
	if err := ValidateManagerMetadata(metadata); err != nil {
		return err
	}
	if metadata.Operation != request.Operation {
		return fmt.Errorf(
			"manager metadata operation = %q, want %q",
			metadata.Operation,
			request.Operation,
		)
	}
	digest, err := ManagerRequestDigest(request)
	if err != nil {
		return err
	}
	if metadata.RequestDigest != digest {
		return fmt.Errorf(
			"manager metadata requestDigest = %q, want %q",
			metadata.RequestDigest,
			digest,
		)
	}
	if request.Operation == ManagerLifecycle &&
		metadata.Outcome == ManagerSucceeded &&
		*metadata.PackageGraphDigest != request.PackageGraph.Digest {
		return fmt.Errorf(
			"manager metadata packageGraphDigest = %q, want %q",
			*metadata.PackageGraphDigest,
			request.PackageGraph.Digest,
		)
	}
	return nil
}

func validateManagerPackage(manager PackageManager) error {
	if manager.Name != PackageManagerBun && manager.Name != PackageManagerNPM {
		return fmt.Errorf("manager request packageManager.name %q is unsupported", manager.Name)
	}
	if len(manager.Version) == 0 ||
		len(manager.Version) > maxPackageManagerVersionBytes ||
		!packageManagerVersionPattern.MatchString(manager.Version) {
		return fmt.Errorf(
			"manager request packageManager.version %q is not an admitted SemVer",
			manager.Version,
		)
	}
	return nil
}

func validateManagerArtifact(
	artifact ManagerArtifact,
	mediaType string,
	maxSize int64,
	label string,
) error {
	if !sha256DigestPattern.MatchString(artifact.Digest) {
		return fmt.Errorf("manager request %s.digest is not a lowercase SHA-256 digest", label)
	}
	if artifact.MediaType != mediaType {
		return fmt.Errorf(
			"manager request %s.mediaType = %q, want %q",
			label,
			artifact.MediaType,
			mediaType,
		)
	}
	if artifact.SizeBytes < 1 || artifact.SizeBytes > maxSize {
		return fmt.Errorf(
			"manager request %s.sizeBytes is outside [1,%d]",
			label,
			maxSize,
		)
	}
	return nil
}

func validateManagerFile(file ProgramFile, label string) error {
	if !sha256DigestPattern.MatchString(file.Digest) {
		return fmt.Errorf("manager request %s.digest is not a lowercase SHA-256 digest", label)
	}
	if file.SizeBytes < 1 || file.SizeBytes > maxProgramFileSizeBytes {
		return fmt.Errorf(
			"manager request %s.sizeBytes is outside [1,%d]",
			label,
			maxProgramFileSizeBytes,
		)
	}
	return nil
}

func validateManagerTree(tree ManagerTree) error {
	if tree.Kind != ManagerOfflineStore && tree.Kind != ManagerRegistryClosure {
		return fmt.Errorf("manager metadata tree kind %q is unsupported", tree.Kind)
	}
	if !sha256DigestPattern.MatchString(tree.Digest) {
		return errors.New("manager metadata tree digest is not a lowercase SHA-256 digest")
	}
	if tree.SizeBytes < 1 || tree.SizeBytes > maxManagerTreeBytes {
		return fmt.Errorf(
			"manager metadata tree sizeBytes is outside [1,%d]",
			maxManagerTreeBytes,
		)
	}
	return nil
}
