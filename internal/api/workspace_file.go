package api

import "time"

type WorkspaceFileEntryResponse struct {
	Path       string     `json:"path"`
	Name       string     `json:"name,omitempty"`
	Kind       string     `json:"kind"`
	SizeBytes  int64      `json:"size_bytes"`
	Mode       int64      `json:"mode"`
	LinkTarget string     `json:"link_target,omitempty"`
	ModTime    *time.Time `json:"mod_time,omitempty"`
}

type WorkspaceFileStatResponse struct {
	Entry WorkspaceFileEntryResponse `json:"entry"`
}

type ListWorkspaceFilesResponse struct {
	Path       string                       `json:"path"`
	Entries    []WorkspaceFileEntryResponse `json:"entries"`
	NextCursor string                       `json:"next_cursor,omitempty"`
}

type WorkspaceVersionResponse struct {
	ID                 string     `json:"id"`
	WorkspaceID        string     `json:"workspace_id"`
	ParentVersionID    string     `json:"parent_version_id,omitempty"`
	Kind               string     `json:"kind"`
	State              string     `json:"state"`
	ContentDigest      string     `json:"content_digest"`
	SizeBytes          int64      `json:"size_bytes"`
	ArtifactEncoding   string     `json:"artifact_encoding"`
	ArtifactEntryCount int32      `json:"artifact_entry_count"`
	Message            string     `json:"message,omitempty"`
	PromotedAt         *time.Time `json:"promoted_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

type WorkspaceVersionEnvelope struct {
	Version WorkspaceVersionResponse `json:"version"`
}

type ListWorkspaceVersionsResponse struct {
	Versions []WorkspaceVersionResponse `json:"versions"`
}
