package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	invitationListLimit         = 200
	defaultInvitationExpiryDays = 7
)

var (
	errSelfMemberManagementLoss = errors.New("cannot remove your own member management access")
	errSelfMemberRemoval        = errors.New("cannot remove your own member access")
	errLastActiveOwner          = errors.New("cannot remove or demote the last active owner")
	errOwnerRoleRequired        = errors.New("owner role is required to manage owners")
	errMemberRoleChanged        = errors.New("member role changed")
)

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	rows, err := s.db.ListOrgMembers(r.Context(), ids.ToPG(actor.OrgID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list members"))
		return
	}
	items := make([]api.MemberSummary, 0, len(rows))
	for _, row := range rows {
		item, err := memberSummaryFromListRow(row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("format member"))
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, api.ListMembersResponse{Members: items})
}

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	rows, err := s.db.ListInvitations(r.Context(), db.ListInvitationsParams{
		OrgID:    ids.ToPG(actor.OrgID),
		RowLimit: invitationListLimit + 1,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list invitations"))
		return
	}
	hasMore := len(rows) > invitationListLimit
	if hasMore {
		rows = rows[:invitationListLimit]
	}
	items := make([]api.InvitationSummary, 0, len(rows))
	for _, row := range rows {
		item, err := invitationSummaryFromListRow(row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("format invitation"))
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, api.ListInvitationsResponse{Invitations: items, HasMore: hasMore})
}

func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	var input api.CreateInvitationRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid invitation request JSON: %w", err))
		return
	}
	email, err := normalizeInviteEmail(input.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	role, err := normalizeMemberRole(input.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	if role == db.OrgMemberRoleOwner && actor.Role != auth.RoleOwner {
		writeMemberManagementError(w, errOwnerRoleRequired)
		return
	}
	if _, err := s.db.RevokeExpiredInvitationsByEmail(r.Context(), db.RevokeExpiredInvitationsByEmailParams{
		OrgID:        ids.ToPG(actor.OrgID),
		InviteeEmail: email,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("expire invitations"))
		return
	}
	pending, err := s.db.GetPendingInvitationByEmail(r.Context(), db.GetPendingInvitationByEmailParams{
		OrgID:        ids.ToPG(actor.OrgID),
		InviteeEmail: email,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, errors.New("load invitation"))
		return
	}
	if err == nil && pending.Role == db.OrgMemberRoleOwner && actor.Role != auth.RoleOwner {
		writeMemberManagementError(w, errOwnerRoleRequired)
		return
	}
	if err == nil {
		writeError(w, http.StatusConflict, errors.New("pending invitation already exists for email"))
		return
	}
	expiresInDays := defaultInvitationExpiryDays
	if input.ExpiresInDays != nil {
		expiresInDays = *input.ExpiresInDays
	}
	if expiresInDays < 1 || expiresInDays > 30 {
		writeError(w, http.StatusBadRequest, errors.New("expires_in_days must be between 1 and 30"))
		return
	}
	rawToken, err := auth.GenerateOpaqueToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate invitation token"))
		return
	}
	tokenHash, err := auth.HashToken(s.authSecret, rawToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("hash invitation token"))
		return
	}
	record, err := s.db.CreateInvitation(r.Context(), db.CreateInvitationParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           ids.ToPG(actor.OrgID),
		InviteeEmail:    email,
		Role:            role,
		InvitedByUserID: ids.ToPG(actor.UserID),
		TokenHash:       tokenHash,
		ExpiresAt:       pgTimeToPG(time.Now().AddDate(0, 0, expiresInDays)),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			pending, pendingErr := s.db.GetPendingInvitationByEmail(r.Context(), db.GetPendingInvitationByEmailParams{
				OrgID:        ids.ToPG(actor.OrgID),
				InviteeEmail: email,
			})
			if pendingErr == nil && pending.Role == db.OrgMemberRoleOwner && actor.Role != auth.RoleOwner {
				writeMemberManagementError(w, errOwnerRoleRequired)
				return
			}
			writeError(w, http.StatusConflict, errors.New("active member already exists for email"))
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, errors.New("pending invitation already exists for email"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("create invitation"))
		return
	}
	summary, err := invitationSummaryFromRecord(record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("format invitation"))
		return
	}
	writeJSON(w, http.StatusCreated, api.CreateInvitationResponse{
		InvitationSummary: summary,
		InviteURL:         s.inviteURL(rawToken),
	})
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	invitationID, err := ids.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("invitation not found"))
		return
	}
	actor := actorFromContext(r.Context())
	invitation, err := s.db.GetRevocableInvitation(r.Context(), db.GetRevocableInvitationParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(invitationID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("invitation not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("load invitation"))
		return
	}
	if invitation.Role == db.OrgMemberRoleOwner && actor.Role != auth.RoleOwner {
		writeMemberManagementError(w, errOwnerRoleRequired)
		return
	}
	rows, err := s.db.RevokeInvitation(r.Context(), db.RevokeInvitationParams{
		OrgID:           ids.ToPG(actor.OrgID),
		ID:              ids.ToPG(invitationID),
		RevokedByUserID: ids.ToPG(actor.UserID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("revoke invitation"))
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, errors.New("invitation not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := ids.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("member not found"))
		return
	}
	var input api.UpdateMemberRoleRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid member role request JSON: %w", err))
		return
	}
	newRole, err := normalizeMemberRole(input.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	expectedRole, err := normalizeMemberRole(input.ExpectedRole)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("expected_role must be owner, admin, developer, or viewer"))
		return
	}
	actor := actorFromContext(r.Context())
	target, err := s.db.GetOrgMemberForManagement(r.Context(), db.GetOrgMemberForManagementParams{
		OrgID:  ids.ToPG(actor.OrgID),
		UserID: ids.ToPG(targetUserID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("member not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("load member"))
		return
	}
	if target.DisabledAt.Valid || target.UserDisabledAt.Valid {
		writeError(w, http.StatusNotFound, errors.New("member not found"))
		return
	}
	if target.Role != expectedRole {
		writeMemberManagementError(w, errMemberRoleChanged)
		return
	}
	if err := s.authorizeMemberRoleChange(r, actor, target, expectedRole, newRole); err != nil {
		writeMemberManagementError(w, err)
		return
	}
	updated, err := s.db.UpdateOrgMemberRole(r.Context(), db.UpdateOrgMemberRoleParams{
		OrgID:        ids.ToPG(actor.OrgID),
		UserID:       ids.ToPG(targetUserID),
		Role:         newRole,
		ExpectedRole: expectedRole,
		ActorIsOwner: actor.Role == auth.RoleOwner,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if expectedRole == db.OrgMemberRoleOwner && newRole != db.OrgMemberRoleOwner {
				writeMemberManagementError(w, errLastActiveOwner)
				return
			}
			writeMemberManagementError(w, errMemberRoleChanged)
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("update member role"))
		return
	}
	summary, err := memberSummaryFromOrgMember(updated, target.DisplayName, target.PrimaryEmail, pgtype.Timestamptz{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("format member"))
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := ids.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("member not found"))
		return
	}
	actor := actorFromContext(r.Context())
	if actor.UserID == targetUserID {
		writeError(w, http.StatusForbidden, errSelfMemberRemoval)
		return
	}
	target, err := s.db.GetOrgMemberForManagement(r.Context(), db.GetOrgMemberForManagementParams{
		OrgID:  ids.ToPG(actor.OrgID),
		UserID: ids.ToPG(targetUserID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("member not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("load member"))
		return
	}
	if target.DisabledAt.Valid || target.UserDisabledAt.Valid {
		writeError(w, http.StatusNotFound, errors.New("member not found"))
		return
	}
	if target.Role == db.OrgMemberRoleOwner {
		if actor.Role != auth.RoleOwner {
			writeMemberManagementError(w, errOwnerRoleRequired)
			return
		}
	}
	if _, err := s.db.DisableOrgMemberAndRevokeOrgSessions(r.Context(), db.DisableOrgMemberAndRevokeOrgSessionsParams{
		OrgID:        ids.ToPG(actor.OrgID),
		UserID:       ids.ToPG(targetUserID),
		ExpectedRole: target.Role,
		ActorIsOwner: actor.Role == auth.RoleOwner,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if target.Role == db.OrgMemberRoleOwner {
				writeMemberManagementError(w, errLastActiveOwner)
				return
			}
			writeError(w, http.StatusNotFound, errors.New("member not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("remove member"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizeMemberRoleChange(r *http.Request, actor auth.Actor, target db.GetOrgMemberForManagementRow, expectedRole db.OrgMemberRole, newRole db.OrgMemberRole) error {
	targetUserID, err := ids.FromPG(target.UserID)
	if err != nil {
		return err
	}
	if actor.UserID == targetUserID && !roleCanManageMembers(newRole) {
		return errSelfMemberManagementLoss
	}
	if (expectedRole == db.OrgMemberRoleOwner || newRole == db.OrgMemberRoleOwner) && actor.Role != auth.RoleOwner {
		return errOwnerRoleRequired
	}
	return nil
}

func writeMemberManagementError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSelfMemberManagementLoss), errors.Is(err, errLastActiveOwner), errors.Is(err, errOwnerRoleRequired):
		writeError(w, http.StatusForbidden, err)
	case errors.Is(err, errMemberRoleChanged):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, errors.New("manage member"))
	}
}

func roleCanManageMembers(role db.OrgMemberRole) bool {
	return role == db.OrgMemberRoleOwner || role == db.OrgMemberRoleAdmin
}

func normalizeMemberRole(value string) (db.OrgMemberRole, error) {
	switch strings.TrimSpace(value) {
	case string(db.OrgMemberRoleOwner):
		return db.OrgMemberRoleOwner, nil
	case string(db.OrgMemberRoleAdmin):
		return db.OrgMemberRoleAdmin, nil
	case string(db.OrgMemberRoleDeveloper):
		return db.OrgMemberRoleDeveloper, nil
	case string(db.OrgMemberRoleViewer):
		return db.OrgMemberRoleViewer, nil
	default:
		return "", errors.New("role must be owner, admin, developer, or viewer")
	}
}

func normalizeInviteEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 320 {
		return "", errors.New("email is required")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value {
		return "", errors.New("email must be a valid address")
	}
	return normalizeEmailAddress(address.Address), nil
}

func normalizeEmailAddress(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (s *Server) inviteURL(token string) string {
	values := url.Values{"token": []string{token}}
	return s.publicURL.ResolveReference(&url.URL{Path: "/invite", RawQuery: values.Encode()}).String()
}

func memberSummaryFromListRow(row db.ListOrgMembersRow) (api.MemberSummary, error) {
	userID, err := ids.FromPG(row.UserID)
	if err != nil {
		return api.MemberSummary{}, err
	}
	status := "active"
	disabledAt := row.DisabledAt
	if row.UserDisabledAt.Valid && (!disabledAt.Valid || row.UserDisabledAt.Time.Before(disabledAt.Time)) {
		disabledAt = row.UserDisabledAt
	}
	if disabledAt.Valid {
		status = "disabled"
	}
	email := ""
	if row.PrimaryEmail.Valid {
		email = row.PrimaryEmail.String
	}
	return api.MemberSummary{
		UserID:      userID.String(),
		DisplayName: row.DisplayName,
		Email:       email,
		Role:        string(row.Role),
		Status:      status,
		CreatedAt:   pgTime(row.CreatedAt),
		UpdatedAt:   pgTime(row.UpdatedAt),
		DisabledAt:  pgTimePtr(disabledAt),
	}, nil
}

func memberSummaryFromOrgMember(member db.OrgMember, displayName pgtype.Text, email pgtype.Text, userDisabledAt pgtype.Timestamptz) (api.MemberSummary, error) {
	userID, err := ids.FromPG(member.UserID)
	if err != nil {
		return api.MemberSummary{}, err
	}
	name := ""
	if displayName.Valid {
		name = displayName.String
	}
	disabledAt := member.DisabledAt
	if userDisabledAt.Valid && (!disabledAt.Valid || userDisabledAt.Time.Before(disabledAt.Time)) {
		disabledAt = userDisabledAt
	}
	status := "active"
	if disabledAt.Valid {
		status = "disabled"
	}
	emailValue := ""
	if email.Valid {
		emailValue = email.String
	}
	return api.MemberSummary{
		UserID:      userID.String(),
		DisplayName: name,
		Email:       emailValue,
		Role:        string(member.Role),
		Status:      status,
		CreatedAt:   pgTime(member.CreatedAt),
		UpdatedAt:   pgTime(member.UpdatedAt),
		DisabledAt:  pgTimePtr(disabledAt),
	}, nil
}

func invitationSummaryFromRecord(record db.Invitation) (api.InvitationSummary, error) {
	return invitationSummary(
		record.ID,
		record.InviteeEmail,
		record.Role,
		record.InvitedByUserID,
		record.CreatedAt,
		record.ExpiresAt,
		record.AcceptedAt,
		record.AcceptedByUserID,
		record.RevokedAt,
		record.RevokedByUserID,
	)
}

func invitationSummaryFromListRow(row db.ListInvitationsRow) (api.InvitationSummary, error) {
	return invitationSummary(
		row.ID,
		row.InviteeEmail,
		row.Role,
		row.InvitedByUserID,
		row.CreatedAt,
		row.ExpiresAt,
		row.AcceptedAt,
		row.AcceptedByUserID,
		row.RevokedAt,
		row.RevokedByUserID,
	)
}

func invitationSummary(id pgtype.UUID, email string, role db.OrgMemberRole, invitedByUserID pgtype.UUID, createdAt pgtype.Timestamptz, expiresAt pgtype.Timestamptz, acceptedAt pgtype.Timestamptz, acceptedByUserID pgtype.UUID, revokedAt pgtype.Timestamptz, revokedByUserID pgtype.UUID) (api.InvitationSummary, error) {
	parsedID, err := ids.FromPG(id)
	if err != nil {
		return api.InvitationSummary{}, err
	}
	status := api.InvitationStatusPending
	if revokedAt.Valid {
		status = api.InvitationStatusRevoked
	} else if acceptedAt.Valid {
		status = api.InvitationStatusAccepted
	} else if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		status = api.InvitationStatusExpired
	}
	return api.InvitationSummary{
		ID:               parsedID.String(),
		Email:            email,
		Role:             string(role),
		Status:           status,
		InvitedByUserID:  nullableUUIDString(invitedByUserID),
		AcceptedByUserID: nullableUUIDString(acceptedByUserID),
		RevokedByUserID:  nullableUUIDString(revokedByUserID),
		CreatedAt:        pgTime(createdAt),
		ExpiresAt:        pgTime(expiresAt),
		AcceptedAt:       pgTimePtr(acceptedAt),
		RevokedAt:        pgTimePtr(revokedAt),
	}, nil
}

func nullableUUIDString(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	parsed, err := ids.FromPG(value)
	if err != nil {
		return ""
	}
	return parsed.String()
}
