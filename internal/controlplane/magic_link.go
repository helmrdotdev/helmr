package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
)

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
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid magic link request JSON: %w", err))
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
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid invite magic link request JSON: %w", err))
		return
	}
	s.magicLinkInviteStart(w, r, request)
}

func (s *Server) magicLinkInviteStart(w http.ResponseWriter, r *http.Request, request api.MagicLinkStartRequest) {
	if !s.magicLinkDeliveryConfigured() {
		writeError(w, unavailable(errors.New("magic link mailer is not configured")))
		return
	}
	invitation, _, err := s.resolveInvitation(r, request.Token)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	debugURL, err := s.sendMagicLink(r, identity.MagicLinkRecipient{
		Purpose:      db.MagicLinkPurposeInviteAccept,
		Email:        invitation.InviteeEmail,
		OrgID:        invitation.OrgID,
		InvitationID: invitation.ID,
	}, "")
	if err != nil {
		writeError(w, errors.New("send magic link"))
		return
	}
	writeJSON(w, http.StatusOK, api.MagicLinkStartResponse{Sent: true, Email: invitation.InviteeEmail, DebugURL: debugURL})
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
	email, err := org.NormalizeEmail(request.Email)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	debugURL, err := s.sendMagicLink(r, identity.MagicLinkRecipient{Purpose: db.MagicLinkPurposeLogin, Email: email}, validateRedirectAfter(request.Next))
	if err != nil {
		s.log.Warn("send login magic link failed", "error", err)
		writeJSON(w, http.StatusOK, api.MagicLinkStartResponse{Sent: true})
		return
	}
	writeJSON(w, http.StatusOK, api.MagicLinkStartResponse{Sent: true, DebugURL: debugURL})
}

// sendMagicLink creates a pending magic link on its own transaction, then
// queues its delivery, which marks it sent or failed on further transactions.
// A rate-limited recipient gets no link and no error. With debug URLs, it
// waits for delivery and returns the link.
func (s *Server) sendMagicLink(r *http.Request, recipient identity.MagicLinkRecipient, redirectAfter string) (string, error) {
	if err := s.userAuthConfigured(); err != nil {
		return "", err
	}
	link, created, err := identity.CreateMagicLink(r.Context(), s.tx, s.identity, recipient, redirectAfter)
	if err != nil || !created {
		return "", err
	}
	linkURL := s.magicLinkURL(link.Token)
	job := magicLinkDeliveryJob{
		id:      link.ID.String(),
		purpose: string(recipient.Purpose),
		deliver: func(ctx context.Context) error {
			return s.deliverMagicLink(ctx, link, linkURL)
		},
		fail: func(ctx context.Context) error {
			_, err := identity.MarkMagicLinkDeliveryFailed(ctx, s.db, link.ID)
			return err
		},
	}
	if s.magicLinkDebugURLs {
		job.done = make(chan error, 1)
	}
	accepted := s.magicLinkDelivery.enqueue(job)
	if !accepted {
		failureCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.magicLinkDelivery.shutdownTimeout)
		defer cancel()
		if markErr := s.markMagicLinkDeliveryFailed(failureCtx, link.ID); markErr != nil {
			return "", fmt.Errorf("magic link delivery queue unavailable; mark delivery failed: %w", markErr)
		}
		return "", errors.New("magic link delivery queue unavailable")
	}
	if s.magicLinkDebugURLs {
		select {
		case err := <-job.done:
			if err != nil {
				return "", err
			}
			return linkURL, nil
		case <-r.Context().Done():
			return "", r.Context().Err()
		}
	}
	return "", nil
}

func (s *Server) deliverMagicLink(ctx context.Context, link identity.PendingMagicLink, linkURL string) error {
	message := magicLinkEmailMessage(link, linkURL)
	message.IdempotencyKey = "magic-link/" + link.ID.String()
	if err := s.mailer.SendEmail(ctx, message); err != nil {
		if markErr := s.markMagicLinkDeliveryFailed(ctx, link.ID); markErr != nil {
			return fmt.Errorf("send magic link: %w; mark delivery failed: %v", err, markErr)
		}
		return err
	}
	return identity.MarkMagicLinkSent(ctx, s.tx, link)
}

func magicLinkEmailMessage(link identity.PendingMagicLink, linkURL string) email.Message {
	return email.Message{
		To:      link.Recipient.Email,
		Subject: magicLinkSubject(link.Recipient.Purpose),
		PlainText: fmt.Sprintf(
			"Open this link to continue signing in to Helmr:\n\n%s\n\nThis link expires at %s.\n",
			linkURL,
			link.ExpiresAt.Format(time.RFC3339),
		),
		MagicLink: &email.MagicLink{
			Email:     link.Recipient.Email,
			Purpose:   string(link.Recipient.Purpose),
			URL:       linkURL,
			ExpiresAt: link.ExpiresAt,
		},
	}
}

// markMagicLinkDeliveryFailed revokes a magic link whose delivery this
// process gave up on; the link must still be pending.
func (s *Server) markMagicLinkDeliveryFailed(ctx context.Context, linkID uuid.UUID) error {
	marked, err := identity.MarkMagicLinkDeliveryFailed(ctx, s.db, linkID)
	if err != nil {
		return err
	}
	if !marked {
		return errors.New("mark magic link delivery failed")
	}
	return nil
}

func (s *Server) magicLinkFinish(w http.ResponseWriter, r *http.Request) {
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	var request api.MagicLinkFinishRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid magic link finish JSON: %w", err))
		return
	}
	completed, err := identity.CompleteMagicLink(r.Context(), s.tx, s.identity, request.Token)
	if err != nil {
		writeError(w, identityError(err))
		return
	}
	setSessionCookie(w, r, completed.Session, s.identity.Lifetimes().Session)
	writeJSON(w, http.StatusOK, api.MagicLinkFinishResponse{RedirectAfter: validateRedirectAfter(completed.RedirectAfter)})
}

func (s *Server) magicLinkURL(token string) string {
	values := url.Values{"token": []string{token}}
	return s.publicURL.ResolveReference(&url.URL{Path: "/auth/magic-link/callback", RawQuery: values.Encode()}).String()
}
