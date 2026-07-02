package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	magicLinkRateLimitWindow = 15 * time.Minute
	magicLinkRateLimitCount  = int64(5)
)

type magicLinkMessage struct {
	Email     string
	Purpose   db.MagicLinkPurpose
	URL       string
	ExpiresAt time.Time
}

func magicLinkSubject(purpose db.MagicLinkPurpose) string {
	switch purpose {
	case db.MagicLinkPurposeInviteAccept:
		return "Accept your Helmr invitation"
	default:
		return "Sign in to Helmr"
	}
}

func (s *Server) magicLinkStart(w http.ResponseWriter, r *http.Request) {
	var request api.MagicLinkStartRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid magic link request JSON: %w", err)))
		return
	}
	if request.Token != "" {
		s.magicLinkInviteStart(w, r, request)
		return
	}
	s.magicLinkLoginStart(w, r, request)
}

func (s *Server) magicLinkDeliveryConfigured() bool {
	_, unconfigured := s.mailer.(email.Unconfigured)
	return !unconfigured
}

func (s *Server) magicLinkInviteStartRoute(w http.ResponseWriter, r *http.Request) {
	var request api.MagicLinkStartRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid invite magic link request JSON: %w", err)))
		return
	}
	s.magicLinkInviteStart(w, r, request)
}

func (s *Server) magicLinkInviteStart(w http.ResponseWriter, r *http.Request, request api.MagicLinkStartRequest) {
	if !s.magicLinkDeliveryConfigured() {
		writeError(w, unavailable(errors.New("magic link mailer is not configured")))
		return
	}
	tokenHash, err := s.validateInvitationToken(r, request.Token)
	if err != nil {
		writeAuthError(w, authStartStatus(err), err)
		return
	}
	invite, err := s.db.GetActiveInvitation(r.Context(), tokenHash)
	if err != nil {
		if isNoRows(err) {
			writeAuthError(w, http.StatusBadRequest, errInvalidOrExpiredToken)
			return
		}
		writeError(w, errors.New("load invitation"))
		return
	}
	debugURL, err := s.sendMagicLink(r, db.MagicLinkPurposeInviteAccept, invite.InviteeEmail, invite.OrgID, invite.ID, "")
	if err != nil {
		writeError(w, errors.New("send magic link"))
		return
	}
	writeJSON(w, http.StatusOK, api.MagicLinkStartResponse{Sent: true, Email: invite.InviteeEmail, DebugURL: debugURL})
}

func (s *Server) magicLinkLoginStart(w http.ResponseWriter, r *http.Request, request api.MagicLinkStartRequest) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	if !s.magicLinkDeliveryConfigured() {
		writeError(w, unavailable(errors.New("magic link mailer is not configured")))
		return
	}
	email, err := normalizeInviteEmail(request.Email)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	redirectAfter := validateRedirectAfter(request.Next)
	debugURL, err := s.sendMagicLink(r, db.MagicLinkPurposeLogin, email, pgtype.UUID{}, pgtype.UUID{}, redirectAfter)
	if err != nil {
		s.log.Warn("send login magic link failed", "error", err)
		writeJSON(w, http.StatusOK, api.MagicLinkStartResponse{Sent: true})
		return
	}
	writeJSON(w, http.StatusOK, api.MagicLinkStartResponse{Sent: true, DebugURL: debugURL})
}

func (s *Server) sendMagicLink(r *http.Request, purpose db.MagicLinkPurpose, email string, orgID pgtype.UUID, invitationID pgtype.UUID, redirectAfter string) (string, error) {
	if err := s.userAuthConfigured(); err != nil {
		return "", err
	}
	link, linkURL, expiresAt, ok, err := s.createPendingMagicLink(r, purpose, email, orgID, invitationID, redirectAfter)
	if err != nil || !ok {
		return "", err
	}
	message := magicLinkMessage{
		Email:     email,
		Purpose:   purpose,
		URL:       linkURL,
		ExpiresAt: expiresAt,
	}
	if s.magicLinkDebugURLs {
		if err := s.deliverMagicLink(context.Background(), message, purpose, email, orgID, invitationID, link.ID); err != nil {
			return "", err
		}
		return linkURL, nil
	}
	go func() {
		if err := s.deliverMagicLink(context.Background(), message, purpose, email, orgID, invitationID, link.ID); err != nil {
			s.log.Warn("send magic link failed", "purpose", purpose, "error", err)
		}
	}()
	return "", nil
}

func (s *Server) deliverMagicLink(ctx context.Context, message magicLinkMessage, purpose db.MagicLinkPurpose, email string, orgID pgtype.UUID, invitationID pgtype.UUID, linkID pgtype.UUID) error {
	emailMessage := magicLinkEmailMessage(message)
	emailMessage.IdempotencyKey = "magic-link/" + pgvalue.MustUUIDValue(linkID).String()
	if err := s.mailer.SendEmail(ctx, emailMessage); err != nil {
		if markErr := s.markMagicLinkDeliveryFailed(ctx, linkID); markErr != nil {
			return fmt.Errorf("send magic link: %w; mark delivery failed: %v", err, markErr)
		}
		return err
	}
	return s.markMagicLinkSent(ctx, purpose, email, orgID, invitationID, linkID)
}

func magicLinkEmailMessage(message magicLinkMessage) email.Message {
	return email.Message{
		To:      message.Email,
		Subject: magicLinkSubject(message.Purpose),
		PlainText: fmt.Sprintf(
			"Open this link to continue signing in to Helmr:\n\n%s\n\nThis link expires at %s.\n",
			message.URL,
			message.ExpiresAt.Format(time.RFC3339),
		),
		MagicLink: &email.MagicLink{
			Email:     message.Email,
			Purpose:   string(message.Purpose),
			URL:       message.URL,
			ExpiresAt: message.ExpiresAt,
		},
	}
}

func (s *Server) createPendingMagicLink(r *http.Request, purpose db.MagicLinkPurpose, email string, orgID pgtype.UUID, invitationID pgtype.UUID, redirectAfter string) (db.MagicLink, string, time.Time, bool, error) {
	if s.tx == nil {
		return db.MagicLink{}, "", time.Time{}, false, errors.New("transactional storage is not configured")
	}
	tx, err := s.tx.Begin(r.Context())
	if err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	defer tx.Rollback(r.Context())
	queries := db.New(tx)
	if err := lockMagicLinkRecipient(r.Context(), tx, purpose, email, orgID, invitationID); err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	count, err := queries.CountRecentMagicLinks(r.Context(), db.CountRecentMagicLinksParams{
		Purpose: purpose,
		Email:   email,
		Since:   pgvalue.Timestamptz(time.Now().Add(-magicLinkRateLimitWindow)),
	})
	if err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	if count >= magicLinkRateLimitCount {
		if err := tx.Commit(r.Context()); err != nil {
			return db.MagicLink{}, "", time.Time{}, false, err
		}
		return db.MagicLink{}, "", time.Time{}, false, nil
	}
	rawToken, err := token.GenerateOpaque(32)
	if err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	tokenHash, err := auth.HashToken(s.authSecret, rawToken)
	if err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	redirect := pgtype.Text{}
	if redirectAfter != "" {
		redirect = pgtype.Text{String: redirectAfter, Valid: true}
	}
	expiresAt := time.Now().Add(s.effectiveMagicLinkTTL())
	link, err := queries.CreateMagicLink(r.Context(), db.CreateMagicLinkParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Purpose:       purpose,
		TokenHash:     tokenHash,
		Email:         email,
		OrgID:         orgID,
		InvitationID:  invitationID,
		RedirectAfter: redirect,
		ExpiresAt:     pgvalue.Timestamptz(expiresAt),
	})
	if err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	linkURL := s.magicLinkURL(rawToken)
	if err := tx.Commit(r.Context()); err != nil {
		return db.MagicLink{}, "", time.Time{}, false, err
	}
	return link, linkURL, expiresAt, true, nil
}

func (s *Server) markMagicLinkSent(ctx context.Context, purpose db.MagicLinkPurpose, email string, orgID pgtype.UUID, invitationID pgtype.UUID, linkID pgtype.UUID) error {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	if err := lockMagicLinkRecipient(ctx, tx, purpose, email, orgID, invitationID); err != nil {
		return err
	}
	rows, err := queries.MarkMagicLinkSent(ctx, linkID)
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("mark magic link sent")
	}
	if _, err := queries.RevokeOpenMagicLinksForRecipient(ctx, db.RevokeOpenMagicLinksForRecipientParams{
		Purpose:      purpose,
		Email:        email,
		OrgID:        orgID,
		InvitationID: invitationID,
		ExceptID:     linkID,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Server) markMagicLinkDeliveryFailed(ctx context.Context, linkID pgtype.UUID) error {
	rows, err := s.db.MarkMagicLinkDeliveryFailed(ctx, linkID)
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("mark magic link delivery failed")
	}
	return nil
}

func lockMagicLinkRecipient(ctx context.Context, tx pgx.Tx, purpose db.MagicLinkPurpose, email string, orgID pgtype.UUID, invitationID pgtype.UUID) error {
	_, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", magicLinkRecipientLockKey(purpose, email, orgID, invitationID))
	if err != nil {
		return fmt.Errorf("lock magic link recipient: %w", err)
	}
	return nil
}

func magicLinkRecipientLockKey(purpose db.MagicLinkPurpose, email string, orgID pgtype.UUID, invitationID pgtype.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("helmr.magic_link.start\x00"))
	_, _ = h.Write([]byte(purpose))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(email))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(orgID.Bytes[:])
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(invitationID.Bytes[:])
	return int64(h.Sum64() & math.MaxInt64)
}

func (s *Server) magicLinkFinish(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	var request api.MagicLinkFinishRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid magic link finish JSON: %w", err)))
		return
	}
	tokenHash, err := auth.HashToken(s.authSecret, request.Token)
	if err != nil {
		writeAuthError(w, http.StatusBadRequest, errInvalidOrExpiredToken)
		return
	}
	rawSession, redirectAfter, err := s.completeMagicLink(r, tokenHash)
	if err != nil {
		writeAuthError(w, callbackStatus(err), err)
		return
	}
	setSessionCookie(w, r, rawSession, s.effectiveSessionTTL())
	writeJSON(w, http.StatusOK, api.MagicLinkFinishResponse{RedirectAfter: redirectAfter})
}

func (s *Server) completeMagicLink(r *http.Request, tokenHash []byte) (string, string, error) {
	var rawSession string
	redirectAfter := "/"
	err := s.inTx(r.Context(), func(work *txWork) error {
		queries := work.q
		link, err := queries.GetActiveMagicLinkByTokenHash(r.Context(), tokenHash)
		if err != nil {
			if isNoRows(err) {
				return errInvalidOrExpiredToken
			}
			return err
		}
		identity := magicLinkIdentity(link.Email)
		var userID pgtype.UUID
		switch link.Purpose {
		case db.MagicLinkPurposeInviteAccept:
			rawSession, userID, err = s.completeMagicLinkInvite(r, queries, link, identity)
		case db.MagicLinkPurposeLogin:
			rawSession, userID, err = s.completeMagicLinkLogin(r, queries, identity)
		default:
			err = errors.New("unknown magic link purpose")
		}
		if err != nil {
			return err
		}
		rows, err := queries.ConsumeMagicLink(r.Context(), db.ConsumeMagicLinkParams{
			ID:               link.ID,
			ConsumedByUserID: userID,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return errInvalidOrExpiredToken
		}
		if link.RedirectAfter.Valid {
			redirectAfter = validateRedirectAfter(link.RedirectAfter.String)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return rawSession, redirectAfter, nil
}

func (s *Server) completeMagicLinkInvite(r *http.Request, queries db.Querier, link db.GetActiveMagicLinkByTokenHashRow, identity authIdentity) (string, pgtype.UUID, error) {
	if !link.InvitationID.Valid {
		return "", pgtype.UUID{}, errInvalidOrExpiredToken
	}
	invite, err := queries.GetActiveInvitationByID(r.Context(), link.InvitationID)
	if err != nil {
		if isNoRows(err) {
			return "", pgtype.UUID{}, errInvalidOrExpiredToken
		}
		return "", pgtype.UUID{}, err
	}
	if !identityMatchesInvitationEmail(identity, invite.InviteeEmail) {
		return "", pgtype.UUID{}, errWrongAccount
	}
	user, err := s.upsertMagicLinkAuthIdentity(r, queries, identity)
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	if user.DisabledAt.Valid {
		return "", pgtype.UUID{}, errDisabledMember
	}
	existingMember, err := queries.GetOrgMemberForManagement(r.Context(), db.GetOrgMemberForManagementParams{
		OrgID:  invite.OrgID,
		UserID: user.ID,
	})
	if err != nil && !isNoRows(err) {
		return "", pgtype.UUID{}, err
	}
	if err == nil && !existingMember.DisabledAt.Valid {
		if existingMember.UserDisabledAt.Valid {
			return "", pgtype.UUID{}, errDisabledMember
		}
		return "", pgtype.UUID{}, errAlreadyMember
	}
	if rows, err := queries.AcceptInvitation(r.Context(), db.AcceptInvitationParams{
		OrgID:  invite.OrgID,
		ID:     invite.ID,
		UserID: user.ID,
	}); err != nil {
		return "", pgtype.UUID{}, err
	} else if rows == 0 {
		return "", pgtype.UUID{}, errInvalidOrExpiredToken
	}
	if _, err := queries.RevokeAuthSessionsForUser(r.Context(), user.ID); err != nil {
		return "", pgtype.UUID{}, err
	}
	if _, err := queries.EnsureOrgMember(r.Context(), db.EnsureOrgMemberParams{
		OrgID:       invite.OrgID,
		UserID:      user.ID,
		Role:        invite.Role,
		DisplayName: pgtype.Text{String: identity.DisplayName, Valid: identity.DisplayName != ""},
	}); err != nil {
		return "", pgtype.UUID{}, err
	}
	rawSession, err := s.issueSessionForOrg(r, queries, user.ID, invite.OrgID)
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	return rawSession, user.ID, nil
}

func (s *Server) completeMagicLinkLogin(r *http.Request, queries db.Querier, identity authIdentity) (string, pgtype.UUID, error) {
	user, err := s.upsertMagicLinkAuthIdentity(r, queries, identity)
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	if user.DisabledAt.Valid {
		return "", pgtype.UUID{}, errDisabledMember
	}
	rawSession, err := s.issueSession(r, queries, user.ID)
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	return rawSession, user.ID, nil
}

func (s *Server) upsertMagicLinkAuthIdentity(r *http.Request, queries db.Querier, identity authIdentity) (db.UpsertMagicLinkAuthIdentityRow, error) {
	claims := identity.Claims
	if len(claims) == 0 || !json.Valid(claims) {
		claims = []byte(`{}`)
	}
	return queries.UpsertMagicLinkAuthIdentity(r.Context(), db.UpsertMagicLinkAuthIdentityParams{
		UserID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		IdentityID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		IdentityProvider: identity.Provider,
		IdentitySubject:  identity.Subject,
		DisplayName:      identity.DisplayName,
		ProfileImageUrl:  pgtype.Text{String: identity.ProfileImageURL, Valid: identity.ProfileImageURL != ""},
		Email:            pgtype.Text{String: identity.Email, Valid: true},
		Claims:           claims,
	})
}

func magicLinkIdentity(email string) authIdentity {
	return authIdentity{
		Provider:       "magic-link",
		Subject:        email,
		DisplayName:    email,
		Email:          email,
		EmailVerified:  true,
		VerifiedEmails: []string{email},
		Claims:         json.RawMessage(`{"email_verified":true}`),
	}
}

func (s *Server) magicLinkURL(token string) string {
	values := url.Values{"token": []string{token}}
	return s.publicURL.ResolveReference(&url.URL{Path: "/auth/magic-link/callback", RawQuery: values.Encode()}).String()
}
