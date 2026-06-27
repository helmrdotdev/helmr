package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspaceVersionListDefaultLimit = int32(50)
	workspaceVersionListMaxLimit     = int32(200)
)

type workspaceFileSource string

const (
	workspaceFileSourceCurrent workspaceFileSource = "current"
	workspaceFileSourceVersion workspaceFileSource = "version"
	workspaceFileSourceLive    workspaceFileSource = "live"
)

var (
	errWorkspaceSourceLiveUnsupported       = codedError{code: "workspace_source_live_unsupported", message: "source=live is not implemented"}
	errWorkspaceVersionIDRequired           = codedError{code: "workspace_version_id_required", message: "version_id is required when source=version"}
	errWorkspaceVersionIDUnexpected         = codedError{code: "workspace_version_id_unexpected", message: "version_id is only valid when source=version"}
	errWorkspaceMaterializationIDUnexpected = codedError{code: "workspace_materialization_id_unexpected", message: "materialization_id is only valid when source=live"}
	errWorkspaceNoCurrentVersion            = codedError{code: "workspace_no_current_version", message: "workspace has no current version"}
	errWorkspaceVersionNotReadable          = codedError{code: "workspace_version_not_readable", message: "workspace version is not readable"}
)

func parseWorkspaceFileSource(r *http.Request) (workspaceFileSource, pgtype.UUID, error) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		source = string(workspaceFileSourceCurrent)
	}
	versionIDRaw := strings.TrimSpace(r.URL.Query().Get("version_id"))
	materializationIDRaw := strings.TrimSpace(r.URL.Query().Get("materialization_id"))
	switch workspaceFileSource(source) {
	case workspaceFileSourceCurrent:
		if versionIDRaw != "" {
			return "", pgtype.UUID{}, errWorkspaceVersionIDUnexpected
		}
		if materializationIDRaw != "" {
			return "", pgtype.UUID{}, errWorkspaceMaterializationIDUnexpected
		}
		return workspaceFileSourceCurrent, pgtype.UUID{}, nil
	case workspaceFileSourceVersion:
		if versionIDRaw == "" {
			return "", pgtype.UUID{}, errWorkspaceVersionIDRequired
		}
		if materializationIDRaw != "" {
			return "", pgtype.UUID{}, errWorkspaceMaterializationIDUnexpected
		}
		parsed, err := uuid.Parse(versionIDRaw)
		if err != nil {
			return "", pgtype.UUID{}, fmt.Errorf("version_id must be a UUID")
		}
		return workspaceFileSourceVersion, pgvalue.UUID(parsed), nil
	case workspaceFileSourceLive:
		return "", pgtype.UUID{}, errWorkspaceSourceLiveUnsupported
	default:
		return "", pgtype.UUID{}, fmt.Errorf("source must be current, version, or live")
	}
}

func (s *Server) resolveReadableWorkspaceVersion(ctx context.Context, row db.Workspace, source workspaceFileSource, versionID pgtype.UUID) (db.WorkspaceVersion, error) {
	switch source {
	case workspaceFileSourceCurrent:
		if !row.CurrentVersionID.Valid {
			return db.WorkspaceVersion{}, errWorkspaceNoCurrentVersion
		}
		versionID = row.CurrentVersionID
	case workspaceFileSourceVersion:
		if !versionID.Valid {
			return db.WorkspaceVersion{}, errWorkspaceVersionIDRequired
		}
	default:
		return db.WorkspaceVersion{}, errWorkspaceSourceLiveUnsupported
	}
	version, err := s.db.GetWorkspaceVersion(ctx, db.GetWorkspaceVersionParams{
		OrgID:         row.OrgID,
		ProjectID:     row.ProjectID,
		EnvironmentID: row.EnvironmentID,
		WorkspaceID:   row.ID,
		ID:            versionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkspaceVersion{}, errWorkspaceVersionNotReadable
	}
	return version, err
}

func (s *Server) getWorkspaceVersion(w http.ResponseWriter, r *http.Request) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionVersionsRead)
	if !ok {
		return
	}
	versionID, err := parseUUIDParam(r, "versionID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	version, err := s.resolveReadableWorkspaceVersion(r.Context(), workspace, workspaceFileSourceVersion, pgvalue.UUID(versionID))
	if err != nil {
		s.writeWorkspaceFileError(w, "get workspace version", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceVersionEnvelope{Version: workspaceVersionResponse(version)})
}

func (s *Server) listWorkspaceVersions(w http.ResponseWriter, r *http.Request) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionVersionsRead)
	if !ok {
		return
	}
	limit, err := parseWorkspaceVersionListLimit(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	kind, err := optionalWorkspaceVersionKind(r.URL.Query().Get("kind"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.ListWorkspaceVersions(r.Context(), db.ListWorkspaceVersionsParams{
		OrgID:         workspace.OrgID,
		ProjectID:     workspace.ProjectID,
		EnvironmentID: workspace.EnvironmentID,
		WorkspaceID:   workspace.ID,
		Kind:          kind,
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspaceFileError(w, "list workspace versions", err)
		return
	}
	out := make([]api.WorkspaceVersionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceVersionResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspaceVersionsResponse{Versions: out})
}

func parseWorkspaceVersionListLimit(r *http.Request) (int32, error) {
	limit, err := optionalLimitQuery(r, workspaceVersionListDefaultLimit)
	if err != nil {
		return 0, err
	}
	if limit > workspaceVersionListMaxLimit {
		return 0, fmt.Errorf("limit must be %d or less", workspaceVersionListMaxLimit)
	}
	return limit, nil
}

func optionalWorkspaceVersionKind(raw string) (db.NullWorkspaceVersionKind, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return db.NullWorkspaceVersionKind{}, nil
	}
	switch db.WorkspaceVersionKind(trimmed) {
	case db.WorkspaceVersionKindUser, db.WorkspaceVersionKindSystem:
		return db.NullWorkspaceVersionKind{WorkspaceVersionKind: db.WorkspaceVersionKind(trimmed), Valid: true}, nil
	default:
		return db.NullWorkspaceVersionKind{}, errors.New("kind must be user or system")
	}
}

func workspaceVersionResponse(row db.WorkspaceVersion) api.WorkspaceVersionResponse {
	response := api.WorkspaceVersionResponse{
		ID:                 pgvalue.MustUUIDValue(row.ID).String(),
		WorkspaceID:        pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		Kind:               string(row.Kind),
		State:              string(row.State),
		ContentDigest:      row.ContentDigest,
		SizeBytes:          row.SizeBytes,
		ArtifactEncoding:   row.ArtifactEncoding,
		ArtifactEntryCount: row.ArtifactEntryCount,
		Message:            row.Message,
		CreatedAt:          row.CreatedAt.Time,
	}
	if row.ParentVersionID.Valid {
		response.ParentVersionID = pgvalue.MustUUIDValue(row.ParentVersionID).String()
	}
	response.PromotedAt = optionalWorkspaceFileTime(row.PromotedAt)
	return response
}

func optionalWorkspaceFileTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
