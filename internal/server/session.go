package server

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
)

var uuidNil uuid.UUID

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	if actor.UserID == uuidNil {
		writeError(w, http.StatusUnauthorized, errors.New("session authentication is required"))
		return
	}
	member, err := s.db.GetOrgMember(r.Context(), db.GetOrgMemberParams{
		OrgID:  ids.ToPG(actor.OrgID),
		UserID: ids.ToPG(actor.UserID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("authentication is required"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("load current user"))
		return
	}
	displayName := member.UserDisplayName
	if member.DisplayName.Valid {
		displayName = member.DisplayName.String
	}
	writeJSON(w, http.StatusOK, api.MeResponse{
		UserID:          actor.UserID.String(),
		DisplayName:     displayName,
		ProfileImageURL: member.ProfileImageUrl.String,
		OrgID:           actor.OrgID.String(),
		Role:            string(member.Role),
		Permissions:     sessionPermissions(auth.Role(member.Role)),
	})
}

func sessionPermissions(role auth.Role) []string {
	all := []auth.Permission{
		auth.PermissionAPIKeysManage,
		auth.PermissionGitHubManage,
		auth.PermissionMembersManage,
		auth.PermissionProjectsManage,
		auth.PermissionRunsCreate,
		auth.PermissionRunsRead,
		auth.PermissionSecretsUse,
		auth.PermissionSecretsWrite,
		auth.PermissionTasksDeploy,
		auth.PermissionWaitpointsRespond,
		auth.PermissionWorkersManage,
	}
	permissions := make([]string, 0, len(all))
	for _, permission := range all {
		if auth.RoleAllows(role, permission) {
			permissions = append(permissions, string(permission))
		}
	}
	return permissions
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName(r)); err == nil {
		s.revokeSessionToken(r, cookie.Value)
	}
	if token, ok := bearerToken(r.Header.Get("authorization")); ok {
		s.revokeSessionToken(r, token)
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeSessionToken(r *http.Request, raw string) {
	tokenHash, err := auth.HashToken(s.authSecret, raw)
	if err != nil {
		return
	}
	_, _ = s.db.RevokeSessionByTokenHash(r.Context(), tokenHash)
}
