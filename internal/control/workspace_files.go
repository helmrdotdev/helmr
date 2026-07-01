package control

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	workspaceFileReadMaxBytes     = int64(32 << 20)
	workspaceFileListDefaultLimit = int32(200)
)

var (
	errWorkspaceFileNotFound     = codedError{code: "workspace_file_not_found", message: "workspace file not found"}
	errWorkspaceFileNotRegular   = codedError{code: "workspace_file_not_regular", message: "workspace file is not a regular file"}
	errWorkspaceFileNotDirectory = codedError{code: "workspace_file_not_directory", message: "workspace file is not a directory"}
	errWorkspaceFileTooLarge     = codedError{code: "workspace_file_too_large", message: "workspace file is too large"}
)

func (s *Server) readWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	workspaceRow, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionFilesRead)
	if !ok {
		return
	}
	target, err := requiredWorkspaceFilePath(r, "path")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	source, versionID, err := parseWorkspaceFileSource(r)
	if err != nil {
		s.writeWorkspaceFileError(w, "read workspace file", err)
		return
	}
	version, err := s.resolveReadableWorkspaceVersion(r.Context(), workspaceRow, source, versionID)
	if err != nil {
		s.writeWorkspaceFileError(w, "read workspace file", err)
		return
	}
	body, err := s.openWorkspaceVersionArtifact(r, version)
	if err != nil {
		s.writeWorkspaceFileError(w, "read workspace file", err)
		return
	}
	defer body.Close()
	entry, err := archive.OpenTarEntry(body, target, archive.ExtractOptions{
		MaxBytes:   workspaceFileReadMaxBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		s.writeWorkspaceFileError(w, "read workspace file", err)
		return
	}
	contents, err := io.ReadAll(entry.Reader)
	if err != nil {
		s.writeWorkspaceFileError(w, "read workspace file", fmt.Errorf("read workspace file contents: %w", err))
		return
	}
	if int64(len(contents)) != entry.Entry.Size {
		s.writeWorkspaceFileError(w, "read workspace file", fmt.Errorf("workspace file content is truncated"))
		return
	}
	w.Header().Set("content-type", "application/octet-stream")
	w.Header().Set("x-helmr-workspace-version-id", workspaceVersionResponse(version).ID)
	w.Header().Set("x-helmr-workspace-file-path", entry.Entry.Path)
	w.Header().Set("x-helmr-workspace-file-size", strconv.FormatInt(entry.Entry.Size, 10))
	w.Header().Set("x-helmr-workspace-file-mode", strconv.FormatInt(entry.Entry.Mode, 10))
	w.Header().Set("content-length", strconv.FormatInt(entry.Entry.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contents)
}

func (s *Server) listWorkspaceFiles(w http.ResponseWriter, r *http.Request) {
	workspaceRow, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionFilesRead)
	if !ok {
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("path"))
	source, versionID, err := parseWorkspaceFileSource(r)
	if err != nil {
		s.writeWorkspaceFileError(w, "list workspace files", err)
		return
	}
	limit, err := parseWorkspaceFileListLimit(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	version, err := s.resolveReadableWorkspaceVersion(r.Context(), workspaceRow, source, versionID)
	if err != nil {
		s.writeWorkspaceFileError(w, "list workspace files", err)
		return
	}
	body, err := s.openWorkspaceVersionArtifact(r, version)
	if err != nil {
		s.writeWorkspaceFileError(w, "list workspace files", err)
		return
	}
	defer body.Close()
	entries, err := archive.ListTarEntries(body, target, archive.ExtractOptions{MaxEntries: workspace.MaxArtifactEntries})
	if err != nil {
		s.writeWorkspaceFileError(w, "list workspace files", err)
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	capacity := min(len(entries), int(limit))
	responses := make([]api.WorkspaceFileEntryResponse, 0, capacity)
	var nextCursor string
	for _, entry := range entries {
		if cursor != "" && entry.Path <= cursor {
			continue
		}
		if len(responses) >= int(limit) {
			if len(responses) > 0 {
				nextCursor = responses[len(responses)-1].Path
			}
			break
		}
		responses = append(responses, workspaceFileEntryResponse(entry, true))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspaceFilesResponse{
		Path:       cleanWorkspaceFileResponsePath(target),
		Entries:    responses,
		NextCursor: nextCursor,
	})
}

func (s *Server) statWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	workspaceRow, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionFilesRead)
	if !ok {
		return
	}
	target, err := requiredWorkspaceFilePath(r, "path")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	source, versionID, err := parseWorkspaceFileSource(r)
	if err != nil {
		s.writeWorkspaceFileError(w, "stat workspace file", err)
		return
	}
	version, err := s.resolveReadableWorkspaceVersion(r.Context(), workspaceRow, source, versionID)
	if err != nil {
		s.writeWorkspaceFileError(w, "stat workspace file", err)
		return
	}
	body, err := s.openWorkspaceVersionArtifact(r, version)
	if err != nil {
		s.writeWorkspaceFileError(w, "stat workspace file", err)
		return
	}
	defer body.Close()
	entry, err := archive.StatTarEntry(body, target, archive.ExtractOptions{MaxEntries: workspace.MaxArtifactEntries})
	if err != nil {
		s.writeWorkspaceFileError(w, "stat workspace file", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceFileStatResponse{Entry: workspaceFileEntryResponse(entry, false)})
}

func (s *Server) openWorkspaceVersionArtifact(r *http.Request, version db.WorkspaceVersion) (io.ReadCloser, error) {
	if s.cas == nil {
		return nil, errors.New("workspace artifact CAS is not configured")
	}
	if !version.ArtifactID.Valid {
		return nil, errors.New("workspace version artifact is missing")
	}
	if strings.TrimSpace(version.ArtifactEncoding) != workspace.ArtifactEncoding {
		return nil, fmt.Errorf("workspace version artifact encoding %q is unsupported", version.ArtifactEncoding)
	}
	artifact, err := s.db.GetArtifact(r.Context(), db.GetArtifactParams{
		OrgID:         version.OrgID,
		ProjectID:     version.ProjectID,
		EnvironmentID: version.EnvironmentID,
		ID:            version.ArtifactID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("workspace version artifact is missing")
	}
	if err != nil {
		return nil, err
	}
	if artifact.Kind != db.ArtifactKindWorkspaceVersion || strings.TrimSpace(artifact.MediaType) != workspace.ArtifactMediaType {
		return nil, errors.New("workspace version artifact is unsupported")
	}
	return s.cas.Get(r.Context(), artifact.Digest)
}

func requiredWorkspaceFilePath(r *http.Request, field string) (string, error) {
	value := strings.TrimSpace(r.URL.Query().Get(field))
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func parseWorkspaceFileListLimit(r *http.Request) (int32, error) {
	return optionalLimitQuery(r, workspaceFileListDefaultLimit)
}

func workspaceFileEntryResponse(entry archive.TarEntry, includeName bool) api.WorkspaceFileEntryResponse {
	response := api.WorkspaceFileEntryResponse{
		Path:       entry.Path,
		Kind:       string(entry.Kind),
		SizeBytes:  entry.Size,
		Mode:       entry.Mode,
		LinkTarget: entry.LinkTarget,
	}
	if includeName {
		response.Name = path.Base(entry.Path)
	}
	response.ModTime = optionalWorkspaceFileEntryTime(entry.ModTime)
	return response
}

func optionalWorkspaceFileEntryTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func cleanWorkspaceFileResponsePath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "."
	}
	return trimmed
}

func (s *Server) writeWorkspaceFileError(w http.ResponseWriter, operation string, err error) {
	var coded codedError
	switch {
	case errors.Is(err, archive.ErrTarEntryNotFound):
		writeError(w, notFound(errWorkspaceFileNotFound))
	case errors.Is(err, archive.ErrTarEntryNotFile):
		writeError(w, apiError{kind: errUnprocessable, err: errWorkspaceFileNotRegular})
	case errors.Is(err, archive.ErrTarEntryNotDir):
		writeError(w, apiError{kind: errUnprocessable, err: errWorkspaceFileNotDirectory})
	case errors.Is(err, archive.ErrTarEntryTooLarge):
		writeError(w, tooLarge(errWorkspaceFileTooLarge))
	case errors.As(err, &coded):
		switch coded.code {
		case errWorkspaceSourceLiveUnsupported.code:
			writeError(w, apiError{kind: errNotImplemented, err: err})
		case errWorkspaceVersionNotReadable.code, errWorkspaceNoCurrentVersion.code:
			writeError(w, notFound(err))
		default:
			writeError(w, badRequest(err))
		}
	case isNoRows(err):
		writeError(w, notFound(errWorkspaceFileNotFound))
	default:
		s.log.Error(operation+" failed", "error", err)
		writeError(w, errors.New(operation))
	}
}
