package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const invitationListLimit = int32(200)

type invitationListCursor struct {
	OrgID     string `json:"org_id"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	rows, err := org.ListMembers(r.Context(), s.db, managingMember(actor))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	items := make([]api.MemberSummary, 0, len(rows))
	for _, row := range rows {
		item, err := memberSummaryFromListRow(row)
		if err != nil {
			writeError(w, errors.New("format member"))
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, api.ListMembersResponse{Members: items})
}

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	var after *org.InvitationPosition
	if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
		cursor, err := decodeInvitationListCursor(rawCursor)
		if err != nil || cursor.OrgID != actor.OrgID.String() {
			writeError(w, badRequest(errors.New("invitation cursor is invalid")))
			return
		}
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			writeError(w, badRequest(errors.New("invitation cursor is invalid")))
			return
		}
		after = &org.InvitationPosition{CreatedAt: createdAt, ID: uuid.MustParse(cursor.ID)}
	}
	rows, hasMore, err := org.ListInvitations(r.Context(), s.db, managingMember(actor), invitationListLimit, after)
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	items := make([]api.InvitationSummary, 0, len(rows))
	for _, row := range rows {
		item, err := invitationSummaryFromListRow(row)
		if err != nil {
			writeError(w, errors.New("format invitation"))
			return
		}
		items = append(items, item)
	}
	response := api.ListInvitationsResponse{Invitations: items}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeInvitationListCursor(invitationListCursor{
			OrgID: actor.OrgID.String(), CreatedAt: last.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list invitations"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func encodeInvitationListCursor(cursor invitationListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeInvitationListCursor(raw string) (invitationListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return invitationListCursor{}, errors.New("invitation cursor is invalid")
	}
	var cursor invitationListCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.OrgID == "" || cursor.CreatedAt == "" || ids.Validate(cursor.ID) != nil {
		return invitationListCursor{}, errors.New("invitation cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	var input api.CreateInvitationRequest
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, fmt.Errorf("invalid invitation request JSON: %w", err))
		return
	}
	invitation, rawToken, err := org.CreateInvitation(r.Context(), s.db, s.authKeys.Invitation, managingMember(actorFromContext(r.Context())), org.InvitationInput{
		Email:         input.Email,
		Role:          input.Role,
		ExpiresInDays: input.ExpiresInDays,
	})
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	summary, err := invitationSummaryFromRecord(invitation)
	if err != nil {
		writeError(w, errors.New("format invitation"))
		return
	}
	writeJSON(w, http.StatusCreated, api.CreateInvitationResponse{
		InvitationSummary: summary,
		InviteURL:         s.inviteURL(rawToken),
	})
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	invitationID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, notFound(org.ErrInvitationNotFound))
		return
	}
	if err := org.RevokeInvitation(r.Context(), s.db, managingMember(actorFromContext(r.Context())), invitationID); err != nil {
		writeError(w, orgError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := parseUUIDParam(r, "userID")
	if err != nil {
		writeError(w, notFound(org.ErrMemberNotFound))
		return
	}
	var input api.UpdateMemberRoleRequest
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, fmt.Errorf("invalid member role request JSON: %w", err))
		return
	}
	updated, err := org.UpdateMemberRole(r.Context(), s.db, managingMember(actorFromContext(r.Context())), targetUserID, input.Role, input.ExpectedRole)
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	summary, err := memberSummaryFromUpdated(updated)
	if err != nil {
		writeError(w, errors.New("format member"))
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := parseUUIDParam(r, "userID")
	if err != nil {
		writeError(w, notFound(org.ErrMemberNotFound))
		return
	}
	if err := org.RemoveMember(r.Context(), s.db, managingMember(actorFromContext(r.Context())), targetUserID); err != nil {
		writeError(w, orgError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func managingMember(actor auth.Actor) org.ManagingMember {
	return org.ManagingMember{OrgID: actor.OrgID, UserID: actor.UserID, Role: actor.Role}
}

func (s *Server) inviteURL(token string) string {
	values := url.Values{"token": []string{token}}
	return s.publicURL.ResolveReference(&url.URL{Path: "/invite", RawQuery: values.Encode()}).String()
}

func memberSummaryFromListRow(row db.ListOrgMembersRow) (api.MemberSummary, error) {
	userID, err := pgvalue.UUIDValue(row.UserID)
	if err != nil {
		return api.MemberSummary{}, err
	}
	status := api.MemberStatusActive
	disabledAt := row.DisabledAt
	if row.UserDisabledAt.Valid && (!disabledAt.Valid || row.UserDisabledAt.Time.Before(disabledAt.Time)) {
		disabledAt = row.UserDisabledAt
	}
	if disabledAt.Valid {
		status = api.MemberStatusDisabled
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
		CreatedAt:   pgvalue.Time(row.CreatedAt),
		UpdatedAt:   pgvalue.Time(row.UpdatedAt),
		DisabledAt:  pgvalue.TimePtr(disabledAt),
	}, nil
}

func memberSummaryFromUpdated(updated org.UpdatedMember) (api.MemberSummary, error) {
	member := updated.Member
	userID, err := pgvalue.UUIDValue(member.UserID)
	if err != nil {
		return api.MemberSummary{}, err
	}
	status := api.MemberStatusActive
	if member.DisabledAt.Valid {
		status = api.MemberStatusDisabled
	}
	return api.MemberSummary{
		UserID:      userID.String(),
		DisplayName: updated.DisplayName.String,
		Email:       updated.PrimaryEmail.String,
		Role:        string(member.Role),
		Status:      status,
		CreatedAt:   pgvalue.Time(member.CreatedAt),
		UpdatedAt:   pgvalue.Time(member.UpdatedAt),
		DisabledAt:  pgvalue.TimePtr(member.DisabledAt),
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
	parsedID, err := pgvalue.UUIDValue(id)
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
		InvitedByUserID:  pgvalue.UUIDString(invitedByUserID),
		AcceptedByUserID: pgvalue.UUIDString(acceptedByUserID),
		RevokedByUserID:  pgvalue.UUIDString(revokedByUserID),
		CreatedAt:        pgvalue.Time(createdAt),
		ExpiresAt:        pgvalue.Time(expiresAt),
		AcceptedAt:       pgvalue.TimePtr(acceptedAt),
		RevokedAt:        pgvalue.TimePtr(revokedAt),
	}, nil
}
