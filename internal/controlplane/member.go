package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/identity"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/email"
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
	principal := principalFromContext(r.Context())
	rows, err := org.ListMembers(r.Context(), s.tx, s.managingMember(principal))
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	items := make([]api.MemberSummary, 0, len(rows))
	for _, row := range rows {
		item, err := memberSummaryFromListRow(row)
		if err != nil {
			s.writeOperationError(w, r, errors.New("format member"))
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, api.ListMembersResponse{Members: items})
}

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var after *org.InvitationPosition
	if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
		cursor, err := decodeInvitationListCursor(rawCursor)
		if err != nil || cursor.OrgID != principal.OrgID.String() {
			s.writeOperationError(w, r, badRequest(errors.New("invitation cursor is invalid")))
			return
		}
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			s.writeOperationError(w, r, badRequest(errors.New("invitation cursor is invalid")))
			return
		}
		after = &org.InvitationPosition{CreatedAt: createdAt, ID: uuid.MustParse(cursor.ID)}
	}
	rows, hasMore, err := org.ListInvitations(r.Context(), s.tx, s.managingMember(principal), invitationListLimit, after)
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	items := make([]api.InvitationSummary, 0, len(rows))
	for _, row := range rows {
		item, err := invitationSummaryFromListRow(row)
		if err != nil {
			s.writeOperationError(w, r, errors.New("format invitation"))
			return
		}
		items = append(items, item)
	}
	response := api.ListInvitationsResponse{Invitations: items}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeInvitationListCursor(invitationListCursor{
			OrgID: principal.OrgID.String(), CreatedAt: last.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			s.writeOperationError(w, r, errors.New("list invitations"))
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

// createInvitations invites the requested addresses by email, which requires
// configured email delivery. The invitations and their audit facts are created
// in one transaction and their emails are sent as one batch before it
// commits: a failed send rolls every invitation back and fails those
// addresses. Two rare cases leave emails whose links do not resolve: the
// commit failing after the emails were sent, and an SMTP server refusing a
// recipient mid-batch after accepting the earlier messages (the refused
// address is reported as such). Both report every address of the batch failed.
func (s *Server) createInvitations(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		s.writeOperationError(w, r, unavailable(err))
		return
	}
	principal := principalFromContext(r.Context())
	manager := s.managingMember(principal)
	if err := manager.Authorize(r.Context(), s.tx, auth.PermissionInvitationsCreate); err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	if !s.emailConfigured() {
		s.writeOperationError(w, r, unavailable(codedError{code: "service_unavailable", message: "email delivery is not configured, so members cannot be invited"}))
		return
	}
	var input api.CreateInvitationsRequest
	if err := decodeRequestJSON(r, &input); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid invitation request JSON: %w", err))
		return
	}
	inviter, err := identity.LoadAccount(r.Context(), s.db, principal)
	if err != nil {
		s.writeOperationError(w, r, identityError(err))
		return
	}
	// The send and the commit outlive a closed tab or a proxy timeout, so a
	// sent batch is not rolled back by the caller leaving, and together they
	// are bounded regardless of the provider or batch size.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), invitationRequestTimeout)
	defer cancel()
	created, err := org.CreateInvitations(ctx, s.tx, s.authKeys.Invitation, manager, org.InvitationInput{
		Emails:        input.Emails,
		ExpiresInDays: input.ExpiresInDays,
	}, func(ctx context.Context, invitations []org.NewInvitation) error {
		messages := make([]email.Message, len(invitations))
		for i, created := range invitations {
			messages[i] = invitationEmailMessage(inviter.DisplayName, inviter.OrgName, created.Invitation, s.inviteURL(created.RawToken))
		}
		return s.mailer.SendEmails(ctx, messages)
	})
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	results := make([]api.InvitationResult, len(created))
	var failure error
	for i, result := range created {
		results[i] = api.InvitationResult{Email: result.Email, Status: api.InvitationResultInvited}
		if result.Err == nil {
			continue
		}
		results[i].Status = api.InvitationResultFailed
		results[i].Error = invitationResultError(result.Email, result.Err)
		if code := results[i].Error.Code; failure == nil && (code == "email_failed" || code == "email_rejected" || code == "internal_error") {
			failure = result.Err
		}
	}
	if failure != nil {
		s.log.Warn("create invitations failed", "error", failure)
	}
	writeJSON(w, http.StatusOK, api.CreateInvitationsResponse{Results: results})
}

// invitationRequestTimeout bounds creating, emailing and committing the
// invitations of one request.
const invitationRequestTimeout = 30 * time.Second

// invitationResultError is the public error of one failed address. When an
// SMTP server refused one recipient, that address says so; the others of the
// batch share the delivery failure.
func invitationResultError(address string, err error) *api.HTTPError {
	var refused email.RecipientError
	var input org.InputError
	switch {
	case errors.As(err, &input):
		return &api.HTTPError{Code: "invalid_email", Message: err.Error()}
	case errors.Is(err, org.ErrInvitationActiveMember):
		return &api.HTTPError{Code: "already_member", Message: err.Error()}
	case errors.Is(err, org.ErrInvitationPending):
		return &api.HTTPError{Code: "invitation_pending", Message: err.Error()}
	case errors.Is(err, org.ErrOwnerRoleRequired):
		return &api.HTTPError{Code: "owner_role_required", Message: err.Error()}
	case errors.As(err, &refused) && refused.Address == address:
		return &api.HTTPError{Code: "email_rejected", Message: "The email server refused this address"}
	case errors.Is(err, org.ErrInvitationNotDelivered):
		return &api.HTTPError{Code: "email_failed", Message: "Invitation emails could not be sent"}
	default:
		return &api.HTTPError{Code: "internal_error", Message: "Invitation could not be created"}
	}
}

func invitationEmailMessage(inviterName string, orgName string, invitation db.Invitation, inviteURL string) email.Message {
	expiresAt := invitation.ExpiresAt.Time.UTC()
	return email.Message{
		To:      invitation.InviteeEmail,
		Subject: fmt.Sprintf("%s invited you to %s on Helmr", inviterName, orgName),
		PlainText: fmt.Sprintf(
			"%s invited you to join the %s organization on Helmr.\n\n"+
				"Open this link to accept the invitation:\n\n%s\n\n"+
				"This invitation expires at %s. If you did not expect it, you can ignore this email.\n",
			inviterName, orgName, inviteURL, expiresAt.Format("January 2, 2006 15:04 MST"),
		),
		Link: &email.Link{
			Email:     invitation.InviteeEmail,
			Purpose:   "invitation",
			URL:       inviteURL,
			ExpiresAt: expiresAt,
		},
	}
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	invitationID, err := parseUUIDParam(r, "id")
	if err != nil {
		s.writeOperationError(w, r, notFound(org.ErrInvitationNotFound))
		return
	}
	if err := org.RevokeInvitation(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())), invitationID); err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateMemberOwnership(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := parseUUIDParam(r, "memberID")
	if err != nil {
		s.writeOperationError(w, r, notFound(org.ErrMemberNotFound))
		return
	}
	var input api.UpdateMemberOwnershipRequest
	if err := decodeRequestJSON(r, &input); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid member role request JSON: %w", err))
		return
	}
	updated, err := org.UpdateMemberOwnership(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())), targetUserID, input.IsOwner, input.ExpectedIsOwner)
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	summary, err := memberSummaryFromUpdated(updated)
	if err != nil {
		s.writeOperationError(w, r, errors.New("format member"))
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := parseUUIDParam(r, "memberID")
	if err != nil {
		s.writeOperationError(w, r, notFound(org.ErrMemberNotFound))
		return
	}
	if err := identity.RemoveMember(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())), targetUserID); err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) managingMember(principal auth.Principal) org.ManagingMember {
	return org.ManagingMember{AuditPolicy: s.auditPolicy, Actor: audit.FromPrincipal(principal), OrgID: principal.OrgID, UserID: principal.UserID, MemberID: principal.MemberID}
}

func (s *Server) inviteURL(token string) string {
	values := url.Values{"token": []string{token}}
	return s.appOrigin.ResolveReference(&url.URL{Path: "/invite", RawQuery: values.Encode()}).String()
}

func memberSummaryFromListRow(row db.ListOrgMembersRow) (api.MemberSummary, error) {
	userID, err := pgvalue.UUIDValue(row.UserID)
	if err != nil {
		return api.MemberSummary{}, err
	}
	status := api.MemberStatusActive
	disabledAt := row.RemovedAt
	if disabledAt.Valid {
		status = api.MemberStatusRemoved
	}
	email := ""
	if row.PrimaryEmail.Valid {
		email = row.PrimaryEmail.String
	}
	return api.MemberSummary{
		UserID:       userID.String(),
		DisplayName:  row.EffectiveDisplayName,
		Email:        email,
		MembershipID: pgvalue.MustUUIDValue(row.ID).String(),
		IsOwner:      row.IsOwner,
		Status:       status,
		CreatedAt:    pgvalue.Time(row.CreatedAt),
		UpdatedAt:    pgvalue.Time(row.UpdatedAt),
		RemovedAt:    pgvalue.TimePtr(disabledAt),
	}, nil
}

func memberSummaryFromUpdated(updated org.UpdatedMember) (api.MemberSummary, error) {
	member := updated.Member
	userID, err := pgvalue.UUIDValue(member.UserID)
	if err != nil {
		return api.MemberSummary{}, err
	}
	status := api.MemberStatusActive
	if member.RemovedAt.Valid {
		status = api.MemberStatusRemoved
	}
	return api.MemberSummary{
		UserID:       userID.String(),
		DisplayName:  updated.DisplayName.String,
		Email:        updated.PrimaryEmail.String,
		MembershipID: pgvalue.MustUUIDValue(member.ID).String(),
		IsOwner:      member.IsOwner,
		Status:       status,
		CreatedAt:    pgvalue.Time(member.CreatedAt),
		UpdatedAt:    pgvalue.Time(member.UpdatedAt),
		RemovedAt:    pgvalue.TimePtr(member.RemovedAt),
	}, nil
}

func invitationSummaryFromListRow(row db.ListInvitationsRow) (api.InvitationSummary, error) {
	return invitationSummary(
		row.ID,
		row.InviteeEmail,
		row.InvitedByMemberID,
		row.CreatedAt,
		row.ExpiresAt,
		row.AcceptedAt,
		row.AcceptedByMemberID,
		row.RevokedAt,
		row.RevokedByMemberID,
	)
}

func invitationSummary(id pgtype.UUID, email string, invitedByMemberID pgtype.UUID, createdAt pgtype.Timestamptz, expiresAt pgtype.Timestamptz, acceptedAt pgtype.Timestamptz, acceptedByMemberID pgtype.UUID, revokedAt pgtype.Timestamptz, revokedByMemberID pgtype.UUID) (api.InvitationSummary, error) {
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
		ID:                 parsedID.String(),
		Email:              email,
		Status:             status,
		InvitedByMemberID:  pgvalue.UUIDString(invitedByMemberID),
		AcceptedByMemberID: pgvalue.UUIDString(acceptedByMemberID),
		RevokedByMemberID:  pgvalue.UUIDString(revokedByMemberID),
		CreatedAt:          pgvalue.Time(createdAt),
		ExpiresAt:          pgvalue.Time(expiresAt),
		AcceptedAt:         pgvalue.TimePtr(acceptedAt),
		RevokedAt:          pgvalue.TimePtr(revokedAt),
	}, nil
}

func (s *Server) recoverOwnAccess(w http.ResponseWriter, r *http.Request) {
	result, err := org.RecoverOwnAccess(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())))
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) ownershipMembers(w http.ResponseWriter, r *http.Request) {
	result, err := org.OwnershipMembers(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())))
	if err != nil {
		s.writeOperationError(w, r, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": result})
}
