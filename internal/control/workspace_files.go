package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	workspaceFileReadMaxBytes     = int64(8 << 20)
	workspaceFileListDefaultLimit = int32(50)
	workspaceFileListMaxLimit     = int32(100)
	workspaceFileCursorMaxBytes   = 4 << 10
	workspaceFileCursorTTL        = 15 * time.Minute
)

type workspaceFileCursor struct {
	WorkspaceID string `json:"workspaceId"`
	VersionID   string `json:"versionId"`
	Path        string `json:"path"`
	After       string `json:"after"`
	ExpiresAt   int64  `json:"expiresAt"`
}

func (s *Server) readWorkspaceFileHTTP(w http.ResponseWriter, r *http.Request) {
	record, ok := s.loadPublicWorkspace(w, r, auth.PermissionWorkspaceFilesRead)
	if !ok {
		return
	}
	target, err := validateWorkspaceFilePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_file_request", message: err.Error()}))
		return
	}
	version, err := s.currentWorkspaceVersion(r, record)
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	body, empty, err := s.openWorkspaceVersion(r, version)
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	if empty {
		writeError(w, notFound(codedError{code: "workspace_file_not_found", message: "Workspace file was not found"}))
		return
	}
	defer body.Close()
	entry, err := archive.OpenTarEntry(body, target, archive.ExtractOptions{
		MaxBytes:   workspaceFileReadMaxBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	content, err := io.ReadAll(entry.Reader)
	if err != nil {
		s.writeWorkspaceFileError(w, fmt.Errorf("read Workspace file: %w", err))
		return
	}
	if int64(len(content)) != entry.Entry.Size {
		s.writeWorkspaceFileError(w, errors.New("Workspace file content is truncated"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceFileContent{
		DataBase64: base64.StdEncoding.EncodeToString(content),
	})
}

func (s *Server) statWorkspaceFileHTTP(w http.ResponseWriter, r *http.Request) {
	record, ok := s.loadPublicWorkspace(w, r, auth.PermissionWorkspaceFilesRead)
	if !ok {
		return
	}
	target, err := validateWorkspaceFilePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_file_request", message: err.Error()}))
		return
	}
	version, err := s.currentWorkspaceVersion(r, record)
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	if target == "." && !version.ArtifactID.Valid {
		writeJSON(w, http.StatusOK, workspaceFileEntry(archive.TarEntry{
			Path: ".",
			Kind: archive.TarEntryKindDir,
		}))
		return
	}
	body, empty, err := s.openWorkspaceVersion(r, version)
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	if empty {
		writeError(w, notFound(codedError{code: "workspace_file_not_found", message: "Workspace file was not found"}))
		return
	}
	defer body.Close()
	entry, err := archive.StatTarEntry(body, target, archive.ExtractOptions{MaxEntries: workspace.MaxArtifactEntries})
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceFileEntry(entry))
}

func (s *Server) listWorkspaceFilesHTTP(w http.ResponseWriter, r *http.Request) {
	record, ok := s.loadPublicWorkspace(w, r, auth.PermissionWorkspaceFilesRead)
	if !ok {
		return
	}
	target, err := validateWorkspaceFilePath(r.URL.Query().Get("path"))
	if err != nil {
		s.writeWorkspaceFileRequestError(w, err)
		return
	}
	limit, err := workspaceFileLimit(r.URL.Query().Get("limit"))
	if err != nil {
		s.writeWorkspaceFileRequestError(w, err)
		return
	}
	var version db.WorkspaceVersion
	var after string
	if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
		cursor, err := s.parseWorkspaceFileCursor(rawCursor, record.PublicID, target, time.Now())
		if err != nil {
			if errors.Is(err, errWorkspaceFileCursorExpired) {
				writeError(w, gone(codedError{
					code:    "workspace_file_cursor_expired",
					message: "Workspace file cursor expired",
				}))
			} else {
				s.writeWorkspaceFileRequestError(w, err)
			}
			return
		}
		version, err = s.db.GetWorkspaceVersionByPublicID(r.Context(), db.GetWorkspaceVersionByPublicIDParams{
			OrgID:         record.OrgID,
			ProjectID:     record.ProjectID,
			EnvironmentID: record.EnvironmentID,
			WorkspaceID:   record.ID,
			PublicID:      cursor.VersionID,
		})
		if err != nil {
			s.writeWorkspaceFileError(w, err)
			return
		}
		after = cursor.After
	} else {
		version, err = s.currentWorkspaceVersion(r, record)
		if err != nil {
			s.writeWorkspaceFileError(w, err)
			return
		}
	}

	var entries []archive.TarEntry
	if version.ArtifactID.Valid {
		body, _, err := s.openWorkspaceVersion(r, version)
		if err != nil {
			s.writeWorkspaceFileError(w, err)
			return
		}
		defer body.Close()
		entries, err = archive.ListTarEntries(body, target, archive.ExtractOptions{MaxEntries: workspace.MaxArtifactEntries})
		if err != nil {
			s.writeWorkspaceFileError(w, err)
			return
		}
	} else if target != "." {
		writeError(w, notFound(codedError{code: "workspace_file_not_found", message: "Workspace file was not found"}))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	items := make([]api.WorkspaceFileEntry, 0, min(len(entries), int(limit)))
	var nextCursor string
	for _, entry := range entries {
		if after != "" && entry.Path <= after {
			continue
		}
		if len(items) == int(limit) {
			nextCursor, err = s.signWorkspaceFileCursor(workspaceFileCursor{
				WorkspaceID: record.PublicID,
				VersionID:   version.PublicID,
				Path:        target,
				After:       items[len(items)-1].Path,
				ExpiresAt:   time.Now().Add(workspaceFileCursorTTL).Unix(),
			})
			if err != nil {
				s.writeWorkspaceFileError(w, err)
				return
			}
			break
		}
		items = append(items, workspaceFileEntry(entry))
	}
	writeJSON(w, http.StatusOK, api.WorkspaceFilePage{Items: items, NextCursor: nextCursor})
}

func (s *Server) loadPublicWorkspace(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.Workspace, bool) {
	workspaceID := chi.URLParam(r, "workspaceID")
	if publicid.ValidateFor(publicid.Workspace, workspaceID) != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: "Workspace ID is invalid"}))
		return db.Workspace{}, false
	}
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal, "", "")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_workspace_reference", message: err.Error()}))
		return db.Workspace{}, false
	}
	if !principal.HasPermission(permission, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return db.Workspace{}, false
	}
	record, err := s.db.GetWorkspaceByPublicID(r.Context(), db.GetWorkspaceByPublicIDParams{
		OrgID:         pgvalue.UUID(principal.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		PublicID:      workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "workspace_not_found", message: "Workspace was not found"}))
		return db.Workspace{}, false
	}
	if err != nil {
		writeError(w, unavailable(codedError{
			code: "workspace_authority_unavailable", message: "Workspace authority is unavailable", retryable: true,
		}))
		return db.Workspace{}, false
	}
	switch record.State {
	case db.WorkspaceStateDeleting:
		writeError(w, conflict(codedError{code: "workspace_deleting", message: "Workspace is deleting"}))
		return db.Workspace{}, false
	case db.WorkspaceStateRecoveryRequired:
		writeError(w, conflict(codedError{
			code:    "workspace_recovery_required",
			message: "Workspace requires recovery",
		}))
		return db.Workspace{}, false
	case db.WorkspaceStateActive:
	default:
		writeError(w, notFound(codedError{code: "workspace_not_found", message: "Workspace was not found"}))
		return db.Workspace{}, false
	}
	return record, true
}

func (s *Server) currentWorkspaceVersion(r *http.Request, record db.Workspace) (db.WorkspaceVersion, error) {
	if !record.HeadVersionID.Valid {
		return db.WorkspaceVersion{}, errors.New("Workspace committed head is missing")
	}
	return s.db.GetWorkspaceVersion(r.Context(), db.GetWorkspaceVersionParams{
		OrgID:         record.OrgID,
		ProjectID:     record.ProjectID,
		EnvironmentID: record.EnvironmentID,
		WorkspaceID:   record.ID,
		ID:            record.HeadVersionID,
	})
}

func (s *Server) openWorkspaceVersion(r *http.Request, version db.WorkspaceVersion) (io.ReadCloser, bool, error) {
	if !version.ArtifactID.Valid {
		if version.ParentVersionID.Valid || version.SizeBytes != 0 || version.EntryCount != 0 {
			return nil, false, errors.New("Workspace version Artifact is missing")
		}
		return nil, true, nil
	}
	if s.cas == nil {
		return nil, false, errors.New("Workspace Artifact store is unavailable")
	}
	artifact, err := s.db.GetArtifact(r.Context(), db.GetArtifactParams{
		OrgID:         version.OrgID,
		ProjectID:     version.ProjectID,
		EnvironmentID: version.EnvironmentID,
		ID:            version.ArtifactID,
	})
	if err != nil {
		return nil, false, err
	}
	if artifact.Kind != db.ArtifactKindWorkspaceVersion || artifact.MediaType != workspace.ArtifactMediaType {
		return nil, false, errors.New("Workspace version Artifact is unsupported")
	}
	body, err := s.cas.Get(r.Context(), artifact.Digest)
	return body, false, err
}

func validateWorkspaceFilePath(raw string) (string, error) {
	if !utf8.ValidString(raw) || len(raw) > 4096 || strings.IndexByte(raw, 0) >= 0 {
		return "", errors.New("Workspace file path must contain at most 4096 UTF-8 bytes")
	}
	if raw == "." {
		return ".", nil
	}
	if raw == "" || path.IsAbs(raw) {
		return "", fmt.Errorf("Workspace file path %q must be canonical and root-relative", raw)
	}
	for component := range strings.SplitSeq(raw, "/") {
		if component == ".." {
			return "", fmt.Errorf("Workspace file path %q must be canonical and root-relative", raw)
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean != raw {
		return "", fmt.Errorf("Workspace file path %q must be canonical and root-relative", raw)
	}
	return clean, nil
}

func workspaceFileLimit(raw string) (int32, error) {
	if raw == "" {
		return workspaceFileListDefaultLimit, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 1 || parsed > int64(workspaceFileListMaxLimit) {
		return 0, fmt.Errorf("Workspace file list limit must be between 1 and %d", workspaceFileListMaxLimit)
	}
	return int32(parsed), nil
}

func workspaceFileEntry(entry archive.TarEntry) api.WorkspaceFileEntry {
	response := api.WorkspaceFileEntry{
		Path:       entry.Path,
		Kind:       string(entry.Kind),
		Mode:       uint32(entry.Mode),
		LinkTarget: entry.LinkTarget,
	}
	if entry.Kind == archive.TarEntryKindFile {
		size := entry.Size
		response.SizeBytes = &size
	}
	return response
}

var errWorkspaceFileCursorExpired = errors.New("Workspace file cursor expired")

func (s *Server) signWorkspaceFileCursor(cursor workspaceFileCursor) (string, error) {
	if len(s.authSecret) == 0 {
		return "", errors.New("Workspace file cursor authority is unavailable")
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write(payload)
	token := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > workspaceFileCursorMaxBytes {
		return "", errors.New("Workspace file cursor exceeds its size limit")
	}
	return token, nil
}

func (s *Server) parseWorkspaceFileCursor(raw, workspaceID, target string, now time.Time) (workspaceFileCursor, error) {
	if len(raw) > workspaceFileCursorMaxBytes || len(s.authSecret) == 0 {
		return workspaceFileCursor{}, errors.New("Workspace file cursor is invalid")
	}
	payloadPart, signaturePart, ok := strings.Cut(raw, ".")
	if !ok {
		return workspaceFileCursor{}, errors.New("Workspace file cursor is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return workspaceFileCursor{}, errors.New("Workspace file cursor is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return workspaceFileCursor{}, errors.New("Workspace file cursor is invalid")
	}
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return workspaceFileCursor{}, errors.New("Workspace file cursor is invalid")
	}
	var cursor workspaceFileCursor
	if err := json.Unmarshal(payload, &cursor); err != nil ||
		cursor.WorkspaceID != workspaceID || cursor.Path != target ||
		publicid.ValidateFor(publicid.WorkspaceVersion, cursor.VersionID) != nil ||
		cursor.After == "" {
		return workspaceFileCursor{}, errors.New("Workspace file cursor is invalid")
	}
	if now.Unix() >= cursor.ExpiresAt {
		return workspaceFileCursor{}, errWorkspaceFileCursorExpired
	}
	return cursor, nil
}

func (s *Server) writeWorkspaceFileRequestError(w http.ResponseWriter, err error) {
	writeError(w, badRequest(codedError{code: "invalid_workspace_file_request", message: err.Error()}))
}

func (s *Server) writeWorkspaceFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, archive.ErrTarEntryNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, notFound(codedError{code: "workspace_file_not_found", message: "Workspace file was not found"}))
	case errors.Is(err, archive.ErrTarEntryNotFile), errors.Is(err, archive.ErrTarEntryNotDir):
		writeError(w, apiError{kind: errUnprocessable, err: codedError{
			code: "workspace_file_not_regular", message: "Workspace file is not a regular file",
		}})
	case errors.Is(err, archive.ErrTarEntryTooLarge):
		writeError(w, tooLarge(codedError{code: "workspace_file_too_large", message: "Workspace file is too large"}))
	default:
		s.log.Error("Workspace file operation failed", "error", err)
		writeError(w, unavailable(codedError{
			code: "workspace_authority_unavailable", message: "Workspace authority is unavailable", retryable: true,
		}))
	}
}
