package guestd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type workspaceOperationRegistry struct {
	mu      sync.RWMutex
	entries map[string]*workspaceMaterializationEntry
}

type workspaceMaterializationEntry struct {
	channelToken      string
	workspaceID       string
	fencingGeneration uint64
	imageRoot         string
	workspaceRoot     string
	cleanup           func()
	active            int
	retired           bool
}

func newWorkspaceOperationRegistry() *workspaceOperationRegistry {
	return &workspaceOperationRegistry{entries: map[string]*workspaceMaterializationEntry{}}
}

func (r *workspaceOperationRegistry) register(materializationID string, entry workspaceMaterializationEntry) {
	r.mu.Lock()
	previous := r.entries[materializationID]
	r.entries[materializationID] = &entry
	var cleanup func()
	if previous != nil {
		previous.retired = true
		if previous.active == 0 {
			cleanup = previous.cleanup
		}
	}
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func (r *workspaceOperationRegistry) acquire(materializationID string, workspaceID string, token string, fencingGeneration uint64) (*workspaceMaterializationEntry, func(), bool) {
	r.mu.Lock()
	entry, ok := r.entries[materializationID]
	workspaceID = strings.TrimSpace(workspaceID)
	token = strings.TrimSpace(token)
	if !(ok &&
		workspaceID != "" &&
		token != "" &&
		fencingGeneration != 0 &&
		entry.workspaceID == workspaceID &&
		entry.fencingGeneration <= fencingGeneration &&
		!entry.retired &&
		subtle.ConstantTimeCompare([]byte(entry.channelToken), []byte(token)) == 1) {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	if fencingGeneration > entry.fencingGeneration {
		entry.fencingGeneration = fencingGeneration
	}
	entry.active++
	r.mu.Unlock()
	return entry, func() { r.release(entry) }, true
}

func (r *workspaceOperationRegistry) release(entry *workspaceMaterializationEntry) {
	r.mu.Lock()
	if entry.active > 0 {
		entry.active--
	}
	var cleanup func()
	if entry.retired && entry.active == 0 {
		cleanup = entry.cleanup
		entry.cleanup = nil
	}
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func handleWorkspaceMaterializeConnection(_ context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.MaterializeWorkspaceRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace materialize request: %w", err)
	}
	envelope := request.GetEnvelope()
	if envelope == nil {
		return errors.New("workspace materialize envelope is required")
	}
	if strings.TrimSpace(envelope.MaterializationId) == "" {
		return errors.New("workspace materialize materialization_id is required")
	}
	if strings.TrimSpace(envelope.WorkspaceId) == "" {
		return errors.New("workspace materialize workspace_id is required")
	}
	if strings.TrimSpace(envelope.ChannelToken) == "" {
		return errors.New("workspace materialize channel_token is required")
	}
	if envelope.FencingGeneration == 0 {
		return errors.New("workspace materialize fencing_generation is required")
	}
	entry, err := restoreMaterializedWorkspace(conn, &request)
	if err != nil {
		return fmt.Errorf("restore materialized workspace: %w", err)
	}
	entry.channelToken = envelope.ChannelToken
	entry.workspaceID = strings.TrimSpace(envelope.WorkspaceId)
	entry.fencingGeneration = envelope.FencingGeneration
	registry.register(envelope.MaterializationId, entry)
	return transport.WriteProtoFrame(conn, &workspacev0.MaterializeWorkspaceResponse{
		State:                  "running",
		GuestdChannelTokenHash: sha256sum.HexBytes([]byte(strings.TrimSpace(envelope.ChannelToken))),
	})
}

func restoreMaterializedWorkspace(conn io.Reader, request *workspacev0.MaterializeWorkspaceRequest) (workspaceMaterializationEntry, error) {
	entry := workspaceMaterializationEntry{}
	mountPath := filepath.Clean(strings.TrimSpace(request.GetMountPath()))
	if mountPath == "" || mountPath == "." || mountPath == string(filepath.Separator) || !filepath.IsAbs(mountPath) {
		return entry, fmt.Errorf("workspace materialize mount_path %q is invalid", request.GetMountPath())
	}
	if strings.TrimSpace(request.GetBaseVersionId()) == "" {
		return entry, errors.New("workspace materialize base_version_id is required")
	}
	artifact := request.GetBaseArtifact()
	if artifact == nil {
		return entry, errors.New("workspace materialize base_artifact is required")
	}
	if strings.TrimSpace(artifact.GetDigest()) == "" {
		return entry, errors.New("workspace materialize base_artifact digest is required")
	}
	if strings.TrimSpace(artifact.GetMediaType()) != workspace.ArtifactMediaType {
		return entry, fmt.Errorf("workspace materialize base_artifact media_type %q is not supported", artifact.GetMediaType())
	}
	if strings.TrimSpace(artifact.GetEncoding()) != workspace.ArtifactEncoding {
		return entry, fmt.Errorf("workspace materialize base_artifact encoding %q is not supported", artifact.GetEncoding())
	}
	if artifact.GetSizeBytes() == 0 {
		return entry, errors.New("workspace materialize base_artifact size_bytes is required")
	}
	if artifact.GetSizeBytes() > uint64(workspace.MaxArtifactArchiveBytes) {
		return entry, fmt.Errorf("workspace materialize base_artifact size_bytes %d exceeds max %d", artifact.GetSizeBytes(), workspace.MaxArtifactArchiveBytes)
	}
	if artifact.GetEntryCount() > uint32(workspace.MaxArtifactEntries) {
		return entry, fmt.Errorf("workspace materialize base_artifact entry_count %d exceeds max %d", artifact.GetEntryCount(), workspace.MaxArtifactEntries)
	}
	sandbox := request.GetSandboxArtifact()
	if sandbox == nil {
		return entry, errors.New("workspace materialize sandbox_artifact is required")
	}
	if strings.TrimSpace(sandbox.GetDigest()) == "" {
		return entry, errors.New("workspace materialize sandbox_artifact digest is required")
	}
	if strings.TrimSpace(sandbox.GetMediaType()) != "application/vnd.helmr.sandbox-image.v0.oci-tar" {
		return entry, fmt.Errorf("workspace materialize sandbox_artifact media_type %q is not supported", sandbox.GetMediaType())
	}
	if strings.TrimSpace(sandbox.GetEncoding()) != "oci-tar" {
		return entry, fmt.Errorf("workspace materialize sandbox_artifact encoding %q is not supported", sandbox.GetEncoding())
	}
	if sandbox.GetSizeBytes() == 0 {
		return entry, errors.New("workspace materialize sandbox_artifact size_bytes is required")
	}
	imageRoot, cleanupImage, err := restoreMaterializedSandboxImage(conn, request)
	if err != nil {
		return entry, err
	}
	entry.imageRoot = imageRoot
	entry.cleanup = cleanupImage
	workspaceRoot, err := workspaceRootForImage(imageRoot, mountPath)
	if err != nil {
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("resolve workspace mount: %w", err)
	}
	entry.workspaceRoot = workspaceRoot
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("read workspace artifact stream header: %w", err)
	}
	if header.Type != transport.StreamTypeWorkspaceArtifact {
		drainStreamBody(conn, bodyLen)
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("unsupported workspace materialize input type %q", header.Type)
	}
	if header.WorkspaceID != strings.TrimSpace(request.GetEnvelope().GetWorkspaceId()) {
		drainStreamBody(conn, bodyLen)
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("workspace artifact workspace_id %q does not match materialize workspace_id %q", header.WorkspaceID, request.GetEnvelope().GetWorkspaceId())
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(artifact.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("workspace artifact digest %q does not match frame digest %q", artifact.GetDigest(), frameDigest)
	}
	if artifact.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("workspace artifact size_bytes %d does not match frame size %d", artifact.GetSizeBytes(), bodyLen)
	}
	workspaceParent := filepath.Dir(workspaceRoot)
	if err := os.MkdirAll(workspaceParent, 0o755); err != nil {
		drainStreamBody(conn, bodyLen)
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("create workspace mount parent: %w", err)
	}
	stagingRoot, err := os.MkdirTemp(workspaceParent, ".helmr-workspace-restore-*")
	if err != nil {
		drainStreamBody(conn, bodyLen)
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("create workspace restore staging dir: %w", err)
	}
	cleanupStaging := func() { _ = os.RemoveAll(stagingRoot) }
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	hashedBody := newDigestingReader(body)
	stats, err := archive.ExtractTarWithStats(hashedBody, stagingRoot, archive.ExtractOptions{
		MaxBytes:   workspace.MaxArtifactExtractedBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanupStaging()
			cleanupImage()
			return workspaceMaterializationEntry{}, errors.Join(fmt.Errorf("extract workspace artifact: %w", err), fmt.Errorf("drain workspace artifact: %w", drainErr))
		}
		cleanupStaging()
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("extract workspace artifact: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanupStaging()
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("drain workspace artifact: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(artifact.GetDigest()) {
		cleanupStaging()
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("workspace artifact body digest %q does not match declared digest %q", digest, artifact.GetDigest())
	}
	if stats.EntryCount != int(artifact.GetEntryCount()) {
		cleanupStaging()
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("workspace artifact entry_count %d does not match declared entry_count %d", stats.EntryCount, artifact.GetEntryCount())
	}
	if err := os.RemoveAll(workspaceRoot); err != nil {
		cleanupStaging()
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("replace workspace mount: remove existing mount: %w", err)
	}
	if err := os.Rename(stagingRoot, workspaceRoot); err != nil {
		cleanupStaging()
		cleanupImage()
		return workspaceMaterializationEntry{}, fmt.Errorf("replace workspace mount: %w", err)
	}
	return entry, nil
}

func restoreMaterializedSandboxImage(conn io.Reader, request *workspacev0.MaterializeWorkspaceRequest) (string, func(), error) {
	cleanup := func() {}
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		return "", cleanup, fmt.Errorf("read sandbox image stream header: %w", err)
	}
	if header.Type != transport.StreamTypeRunImage {
		drainStreamBody(conn, bodyLen)
		return "", cleanup, fmt.Errorf("unsupported workspace materialize sandbox input type %q", header.Type)
	}
	if header.WorkspaceID != strings.TrimSpace(request.GetEnvelope().GetWorkspaceId()) {
		drainStreamBody(conn, bodyLen)
		return "", cleanup, fmt.Errorf("sandbox image workspace_id %q does not match materialize workspace_id %q", header.WorkspaceID, request.GetEnvelope().GetWorkspaceId())
	}
	sandbox := request.GetSandboxArtifact()
	if sandbox.GetSizeBytes() != bodyLen {
		drainStreamBody(conn, bodyLen)
		return "", cleanup, fmt.Errorf("sandbox image artifact size_bytes %d does not match frame size %d", sandbox.GetSizeBytes(), bodyLen)
	}
	frameDigest := ""
	if header.BodyDigest != nil {
		frameDigest = strings.TrimSpace(*header.BodyDigest)
	}
	if frameDigest != "" && frameDigest != strings.TrimSpace(sandbox.GetDigest()) {
		drainStreamBody(conn, bodyLen)
		return "", cleanup, fmt.Errorf("sandbox image artifact digest %q does not match frame digest %q", sandbox.GetDigest(), frameDigest)
	}
	imageRoot, err := os.MkdirTemp("", "helmr-workspace-image-*")
	if err != nil {
		drainStreamBody(conn, bodyLen)
		return "", cleanup, fmt.Errorf("create sandbox image root: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(imageRoot) }
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	hashedBody := newDigestingReader(body)
	if _, err := unpackOCIImage(hashedBody, imageRoot); err != nil {
		if _, drainErr := io.Copy(io.Discard, hashedBody); drainErr != nil {
			cleanup()
			return "", func() {}, errors.Join(fmt.Errorf("extract sandbox image artifact: %w", err), fmt.Errorf("drain sandbox image artifact: %w", drainErr))
		}
		cleanup()
		return "", func() {}, fmt.Errorf("extract sandbox image artifact: %w", err)
	}
	if _, err := io.Copy(io.Discard, hashedBody); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("drain sandbox image artifact: %w", err)
	}
	if digest := hashedBody.Digest(); digest != strings.TrimSpace(sandbox.GetDigest()) {
		cleanup()
		return "", func() {}, fmt.Errorf("sandbox image artifact body digest %q does not match declared digest %q", digest, sandbox.GetDigest())
	}
	return imageRoot, cleanup, nil
}

func handleWorkspaceOperationConnection(_ context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.WorkspaceOperationRequest
	if err := transport.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace operation request: %w", err)
	}
	envelope := request.GetEnvelope()
	if envelope == nil {
		return errors.New("workspace operation envelope is required")
	}
	if strings.TrimSpace(envelope.OperationId) == "" {
		return errors.New("workspace operation operation_id is required")
	}
	if strings.TrimSpace(envelope.MaterializationId) == "" {
		return errors.New("workspace operation materialization_id is required")
	}
	if strings.TrimSpace(envelope.WorkspaceId) == "" {
		return errors.New("workspace operation workspace_id is required")
	}
	entry, release, ok := registry.acquire(envelope.MaterializationId, envelope.WorkspaceId, envelope.ChannelToken, envelope.FencingGeneration)
	if !ok {
		return errors.New("workspace operation channel token or fencing generation is invalid")
	}
	defer release()
	_ = entry
	if envelope.OperationExpiresAtUnixNano <= 0 {
		return errors.New("workspace operation operation_expires_at is required")
	}
	if time.Now().UnixNano() >= envelope.OperationExpiresAtUnixNano {
		return errors.New("workspace operation expired")
	}
	fingerprint := strings.TrimSpace(envelope.RequestFingerprint)
	if fingerprint == "" {
		return errors.New("workspace operation request_fingerprint is required")
	}
	if actual := workspaceOperationRequestFingerprint(request.OperationKind, request.RequestJson); actual != fingerprint {
		return fmt.Errorf("workspace operation request_fingerprint %q does not match request %q", fingerprint, actual)
	}
	switch strings.TrimSpace(request.OperationKind) {
	case "noop":
		return transport.WriteProtoFrame(conn, &workspacev0.WorkspaceOperationResult{
			ResultJson: `{"ok":true}`,
		})
	default:
		return transport.WriteProtoFrame(conn, &workspacev0.WorkspaceOperationResult{
			ErrorJson: fmt.Sprintf(`{"message":"unsupported workspace operation %q"}`, strings.TrimSpace(request.OperationKind)),
		})
	}
}

func workspaceOperationRequestFingerprint(operationKind string, requestJSON string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(operationKind)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.TrimSpace(requestJSON)))
	return hex.EncodeToString(hash.Sum(nil))
}
