package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func storeWorkspaceArtifactFrame(ctx context.Context, store cas.Store, reader io.Reader, header wire.StreamHeader, bodyLen uint64, runID string) (workspace.WorkspaceArtifact, error) {
	if header.Type != wire.StreamTypeWorkspaceArtifact {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("unsupported runtime stream type %q", header.Type)
	}
	if strings.TrimSpace(header.RunID) != strings.TrimSpace(runID) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact run_id %q did not match run %q", header.RunID, runID)
	}
	if header.BodyDigest == nil || strings.TrimSpace(*header.BodyDigest) == "" {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame body_digest is required")
	}
	if header.EntryCount == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame entry_count is required")
	}
	if *header.EntryCount < 0 {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame entry_count must be non-negative")
	}
	if *header.EntryCount > workspace.MaxArtifactEntries {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact entry_count %d exceeds max %d", *header.EntryCount, workspace.MaxArtifactEntries)
	}
	if bodyLen == 0 {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact frame body is required")
	}
	if bodyLen > uint64(workspace.MaxArtifactArchiveBytes) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact size_bytes %d exceeds max %d", bodyLen, workspace.MaxArtifactArchiveBytes)
	}
	if store == nil {
		return workspace.WorkspaceArtifact{}, errors.New("workspace artifact CAS is required")
	}
	body := &io.LimitedReader{R: reader, N: int64(bodyLen)}
	object, err := store.Put(ctx, workspace.ArtifactMediaType, body)
	if err != nil {
		_, _ = io.Copy(io.Discard, body)
		return workspace.WorkspaceArtifact{}, fmt.Errorf("put workspace artifact: %w", err)
	}
	if body.N > 0 {
		if _, err := io.Copy(io.Discard, body); err != nil {
			return workspace.WorkspaceArtifact{}, fmt.Errorf("drain workspace artifact: %w", err)
		}
	}
	if object.Digest != strings.TrimSpace(*header.BodyDigest) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact digest mismatch: got %s, want %s", object.Digest, *header.BodyDigest)
	}
	if object.SizeBytes != int64(bodyLen) {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact size mismatch: got %d, want %d", object.SizeBytes, bodyLen)
	}
	if strings.TrimSpace(object.MediaType) != workspace.ArtifactMediaType {
		return workspace.WorkspaceArtifact{}, fmt.Errorf("workspace artifact media_type mismatch: got %q, want %q", object.MediaType, workspace.ArtifactMediaType)
	}
	return workspace.WorkspaceArtifact{
		Digest:     object.Digest,
		MediaType:  object.MediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  object.SizeBytes,
		EntryCount: *header.EntryCount,
	}, nil
}
