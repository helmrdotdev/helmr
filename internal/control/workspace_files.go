package control

import (
	"context"
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
	content, err := s.readWorkspaceFile(r.Context(), record, target)
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, content)
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
	entry, err := s.statWorkspaceFile(r.Context(), record, target)
	if err != nil {
		s.writeWorkspaceFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
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
	page, err := s.listWorkspaceFiles(
		r.Context(), record, target, r.URL.Query().Get("cursor"), limit, time.Now(),
	)
	if err != nil {
		if errors.Is(err, errWorkspaceFileCursorExpired) {
			writeError(w, gone(codedError{
				code:    "workspace_file_cursor_expired",
				message: "Workspace file cursor expired",
			}))
		} else if errors.Is(err, errWorkspaceFileCursorInvalid) {
			s.writeWorkspaceFileRequestError(w, err)
		} else {
			s.writeWorkspaceFileError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) readWorkspaceFile(
	ctx context.Context,
	record db.Workspace,
	target string,
) (api.WorkspaceFileContent, error) {
	return s.readWorkspaceFileWithQueries(ctx, s.db, record, target)
}

func (s *Server) readWorkspaceFileWithQueries(
	ctx context.Context,
	q db.Querier,
	record db.Workspace,
	target string,
) (api.WorkspaceFileContent, error) {
	version, err := s.currentWorkspaceVersion(ctx, q, record)
	if err != nil {
		return api.WorkspaceFileContent{}, err
	}
	body, empty, err := s.openWorkspaceVersion(ctx, q, version)
	if err != nil {
		return api.WorkspaceFileContent{}, err
	}
	if empty {
		return api.WorkspaceFileContent{}, archive.ErrTarEntryNotFound
	}
	defer body.Close()
	entry, err := archive.OpenTarEntry(body, target, archive.ExtractOptions{
		MaxBytes:   workspaceFileReadMaxBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		return api.WorkspaceFileContent{}, err
	}
	content, err := io.ReadAll(entry.Reader)
	if err != nil {
		return api.WorkspaceFileContent{}, fmt.Errorf("read Workspace file: %w", err)
	}
	if int64(len(content)) != entry.Entry.Size {
		return api.WorkspaceFileContent{}, errors.New("Workspace file content is truncated")
	}
	return api.WorkspaceFileContent{
		DataBase64: base64.StdEncoding.EncodeToString(content),
	}, nil
}

func (s *Server) statWorkspaceFile(
	ctx context.Context,
	record db.Workspace,
	target string,
) (api.WorkspaceFileEntry, error) {
	return s.statWorkspaceFileWithQueries(ctx, s.db, record, target)
}

func (s *Server) statWorkspaceFileWithQueries(
	ctx context.Context,
	q db.Querier,
	record db.Workspace,
	target string,
) (api.WorkspaceFileEntry, error) {
	version, err := s.currentWorkspaceVersion(ctx, q, record)
	if err != nil {
		return api.WorkspaceFileEntry{}, err
	}
	if target == "." && !version.ArtifactID.Valid {
		return workspaceFileEntry(archive.TarEntry{
			Path: ".",
			Kind: archive.TarEntryKindDir,
		}), nil
	}
	body, empty, err := s.openWorkspaceVersion(ctx, q, version)
	if err != nil {
		return api.WorkspaceFileEntry{}, err
	}
	if empty {
		return api.WorkspaceFileEntry{}, archive.ErrTarEntryNotFound
	}
	defer body.Close()
	entry, err := archive.StatTarEntry(body, target, archive.ExtractOptions{
		MaxEntries: workspace.MaxArtifactEntries,
	})
	if err != nil {
		return api.WorkspaceFileEntry{}, err
	}
	return workspaceFileEntry(entry), nil
}

func (s *Server) listWorkspaceFiles(
	ctx context.Context,
	record db.Workspace,
	target string,
	rawCursor string,
	limit int32,
	now time.Time,
) (api.WorkspaceFilePage, error) {
	return s.listWorkspaceFilesWithQueries(ctx, s.db, record, target, rawCursor, limit, now)
}

func (s *Server) listWorkspaceFilesWithQueries(
	ctx context.Context,
	q db.Querier,
	record db.Workspace,
	target string,
	rawCursor string,
	limit int32,
	now time.Time,
) (api.WorkspaceFilePage, error) {
	var version db.WorkspaceVersion
	var after string
	var err error
	if rawCursor != "" {
		cursor, parseErr := s.parseWorkspaceFileCursor(rawCursor, record.PublicID, target, now)
		if parseErr != nil {
			return api.WorkspaceFilePage{}, parseErr
		}
		version, err = q.GetWorkspaceVersionByPublicID(ctx, db.GetWorkspaceVersionByPublicIDParams{
			OrgID:         record.OrgID,
			ProjectID:     record.ProjectID,
			EnvironmentID: record.EnvironmentID,
			WorkspaceID:   record.ID,
			PublicID:      cursor.VersionID,
		})
		after = cursor.After
	} else {
		version, err = s.currentWorkspaceVersion(ctx, q, record)
	}
	if err != nil {
		return api.WorkspaceFilePage{}, err
	}

	var entries []archive.TarEntry
	if version.ArtifactID.Valid {
		body, _, openErr := s.openWorkspaceVersion(ctx, q, version)
		if openErr != nil {
			return api.WorkspaceFilePage{}, openErr
		}
		defer body.Close()
		entries, err = archive.ListTarEntries(body, target, archive.ExtractOptions{
			MaxEntries: workspace.MaxArtifactEntries,
		})
		if err != nil {
			return api.WorkspaceFilePage{}, err
		}
	} else if target != "." {
		return api.WorkspaceFilePage{}, archive.ErrTarEntryNotFound
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
				ExpiresAt:   now.Add(workspaceFileCursorTTL).Unix(),
			})
			if err != nil {
				return api.WorkspaceFilePage{}, err
			}
			break
		}
		items = append(items, workspaceFileEntry(entry))
	}
	return api.WorkspaceFilePage{Items: items, NextCursor: nextCursor}, nil
}

func (s *Server) loadPublicWorkspace(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.Workspace, bool) {
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
	record, err := s.resolveWorkspaceReference(r.Context(), workspaceReference{
		OrgID:         principal.OrgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		PublicID:      chi.URLParam(r, "workspaceID"),
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

func (s *Server) currentWorkspaceVersion(
	ctx context.Context,
	q db.Querier,
	record db.Workspace,
) (db.WorkspaceVersion, error) {
	if !record.HeadVersionID.Valid {
		return db.WorkspaceVersion{}, errors.New("Workspace committed head is missing")
	}
	return q.GetWorkspaceVersion(ctx, db.GetWorkspaceVersionParams{
		OrgID:         record.OrgID,
		ProjectID:     record.ProjectID,
		EnvironmentID: record.EnvironmentID,
		WorkspaceID:   record.ID,
		ID:            record.HeadVersionID,
	})
}

func (s *Server) openWorkspaceVersion(
	ctx context.Context,
	q db.Querier,
	version db.WorkspaceVersion,
) (io.ReadCloser, bool, error) {
	if !version.ArtifactID.Valid {
		if version.ParentVersionID.Valid || version.SizeBytes != 0 || version.EntryCount != 0 {
			return nil, false, errors.New("Workspace version Artifact is missing")
		}
		return nil, true, nil
	}
	if s.cas == nil {
		return nil, false, errors.New("Workspace Artifact store is unavailable")
	}
	artifact, err := q.GetArtifact(ctx, db.GetArtifactParams{
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
	body, err := s.cas.Get(ctx, artifact.Digest)
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

var (
	errWorkspaceFileCursorExpired = errors.New("Workspace file cursor expired")
	errWorkspaceFileCursorInvalid = errors.New("Workspace file cursor is invalid")
)

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
		return workspaceFileCursor{}, errWorkspaceFileCursorInvalid
	}
	payloadPart, signaturePart, ok := strings.Cut(raw, ".")
	if !ok {
		return workspaceFileCursor{}, errWorkspaceFileCursorInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return workspaceFileCursor{}, errWorkspaceFileCursorInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return workspaceFileCursor{}, errWorkspaceFileCursorInvalid
	}
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return workspaceFileCursor{}, errWorkspaceFileCursorInvalid
	}
	var cursor workspaceFileCursor
	if err := json.Unmarshal(payload, &cursor); err != nil ||
		cursor.WorkspaceID != workspaceID || cursor.Path != target ||
		publicid.ValidateFor(publicid.WorkspaceVersion, cursor.VersionID) != nil ||
		cursor.After == "" {
		return workspaceFileCursor{}, errWorkspaceFileCursorInvalid
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
