package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/sso"
	"github.com/jackc/pgx/v5"
)

const (
	browserAuthSSOSignIn   browserAuthKind = "sso_sign_in"
	browserAuthSSOSelfLink browserAuthKind = "sso_self_link"
	browserAuthSSOConfirm  browserAuthKind = "sso_confirm"
)

// SSO flows use the ordinary Lax auth-flow cookie, except two:
//   - A SAML start also sets a SameSite=None cookie, because the IdP posts its
//     response to the ACS cross-site. The ACS validates the response, consumes
//     its assertion ID and hands the validated identity to the Lax cookie; a
//     same-site Console page then finishes with the browser's session cookie,
//     which a cross-site post does not carry.
//   - First association keeps the validated identity in its own cookie, so a
//     sign-in or account switch on the way does not replace it.
func samlFlowCookieName(r *http.Request) string {
	if isSecureRequest(r) {
		return "__Host-helmr_saml_flow"
	}
	return "helmr_saml_flow_dev"
}

func samlFlowCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	c := &http.Cookie{Name: samlFlowCookieName(r), Value: value, Path: "/", HttpOnly: true, MaxAge: maxAge, Secure: isSecureRequest(r), SameSite: http.SameSiteLaxMode}
	if c.Secure {
		c.SameSite = http.SameSiteNoneMode
	}
	return c
}

func ssoConfirmCookieName(r *http.Request) string {
	if isSecureRequest(r) {
		return "__Host-helmr_sso_confirm"
	}
	return "helmr_sso_confirm_dev"
}

func ssoConfirmCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: ssoConfirmCookieName(r), Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Secure: isSecureRequest(r)}
}

func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	var request api.SSOStartRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid SSO start request JSON: %w", err))
		return
	}
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, unavailable(err))
		return
	}
	flow := browserAuthFlow{Kind: browserAuthSSOSignIn, RedirectAfter: validateRedirectAfter(request.Next)}
	var connection sso.Connection
	var err error
	if request.InvitationToken != "" {
		// An invitation names its organization.
		invitation, tokenHash, err := s.resolveInvitation(r, request.InvitationToken)
		if err != nil {
			s.writeIdentityError(w, r, err)
			return
		}
		if connection, err = s.invitationConnection(r.Context(), invitation.OrgID); err != nil {
			writeError(w, identityError(err))
			return
		}
		flow.TokenHash = base64.RawURLEncoding.EncodeToString(tokenHash)
	} else if connection, err = s.startConnection(r.Context(), request.OrganizationSlug); err != nil {
		writeError(w, identityError(err))
		return
	}
	s.writeSSOStart(w, r, connection, flow, request.Reauthenticate)
}

// startConnection finds the active connection by organization slug. A
// self-hosted deployment with one organization may omit the slug.
func (s *Server) startConnection(ctx context.Context, slug string) (sso.Connection, error) {
	if s.ssoStore == nil {
		return sso.Connection{}, sso.ErrConnectionNotFound
	}
	if slug = strings.TrimSpace(slug); slug != "" {
		return s.ssoStore.ActiveBySlug(ctx, s.tx, slug)
	}
	if !s.selfHostedMode() {
		return sso.Connection{}, sso.ErrConnectionNotFound
	}
	ids, err := s.db.ListOrganizationIDs(ctx, 2)
	if err != nil {
		return sso.Connection{}, err
	}
	if len(ids) != 1 {
		return sso.Connection{}, sso.ErrConnectionNotFound
	}
	return s.ssoStore.Active(ctx, s.tx, uuid.UUID(ids[0].Bytes))
}

func (s *Server) invitationConnection(ctx context.Context, org uuid.UUID) (sso.Connection, error) {
	if s.ssoStore == nil {
		return sso.Connection{}, sso.ErrConnectionNotFound
	}
	return s.ssoStore.Active(ctx, s.tx, org)
}

func (s *Server) writeSSOStart(w http.ResponseWriter, r *http.Request, connection sso.Connection, flow browserAuthFlow, reauthenticate bool) {
	session, err := s.browserSessionID(r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	flow.SessionID, flow.StartedAt = session, time.Now()
	flow.ConnectionID, flow.OrganizationID, flow.SSOProtocol = connection.ID.String(), connection.OrgID.String(), connection.Protocol
	var redirect string
	switch connection.Protocol {
	case sso.ProtocolOIDC:
		if flow.State, err = auth.GenerateOpaque(32); err == nil {
			if flow.Nonce, err = auth.GenerateOpaque(32); err == nil {
				flow.Verifier, err = auth.GenerateOpaque(64)
			}
		}
		if err != nil {
			writeError(w, errors.New("generate SSO state"))
			return
		}
		client, err := s.oidcClient(r.Context(), connection)
		if err != nil {
			s.log.Warn("OIDC discovery failed", "connection_id", connection.ID, "error", err)
			writeError(w, unavailable(errors.New("the identity provider is unavailable")))
			return
		}
		lifetime, err := sso.ProofLifetime(r.Context(), s.tx, connection.OrgID)
		if err != nil {
			writeError(w, err)
			return
		}
		redirect = client.AuthURL(flow.State, flow.Nonce, flow.Verifier, lifetime, reauthenticate)
		encoded, err := s.encodeAuthFlow(flow)
		if err != nil {
			writeError(w, errors.New("encode auth flow"))
			return
		}
		http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	case sso.ProtocolSAML:
		sp, err := sso.NewSAML(s.appOrigin, connection.ID, connection.SAML, nil)
		if err == nil {
			redirect, flow.SSORequestID, err = sp.AuthnRequest(reauthenticate)
		}
		if err != nil {
			writeError(w, fmt.Errorf("build SAML request: %w", err))
			return
		}
		encoded, err := s.encodeAuthFlow(flow)
		if err != nil {
			writeError(w, errors.New("encode auth flow"))
			return
		}
		http.SetCookie(w, samlFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	}
	w.Header().Set("referrer-policy", "no-referrer")
	writeJSON(w, http.StatusOK, api.SSOStartResponse{RedirectURL: redirect})
}

func (s *Server) oidcClient(ctx context.Context, c sso.Connection) (*sso.OIDC, error) {
	callback := s.appOrigin.JoinPath("/sso/oidc/callback").String()
	return sso.NewOIDC(ctx, s.ssoHTTP, c.OIDC.Issuer, c.OIDC.ClientID, c.OIDC.ClientSecret, callback)
}

// flowConnection reads the flow's connection, which must still be active for
// the same organization.
func (s *Server) flowConnection(ctx context.Context, flow browserAuthFlow) (sso.Connection, error) {
	id, err := uuid.Parse(flow.ConnectionID)
	if err != nil || s.ssoStore == nil {
		return sso.Connection{}, sso.ErrConnectionNotFound
	}
	connection, err := s.ssoStore.ActiveByID(ctx, s.tx, id)
	if err != nil {
		return sso.Connection{}, err
	}
	if connection.OrgID.String() != flow.OrganizationID || connection.Protocol != flow.SSOProtocol {
		return sso.Connection{}, sso.ErrConnectionNotFound
	}
	return connection, nil
}

func ssoStartKind(kind browserAuthKind) bool {
	return kind == browserAuthSSOSignIn || kind == browserAuthSSOSelfLink
}

func (s *Server) ssoOIDCFinish(w http.ResponseWriter, r *http.Request) {
	var request api.SSOOIDCFinishRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid SSO callback JSON: %w", err))
		return
	}
	flow, err := s.decodeAuthFlow(r)
	if err == nil && (!ssoStartKind(flow.Kind) || flow.SSOProtocol != sso.ProtocolOIDC || flow.SSOSubject != "" || request.State == "" || request.State != flow.State) {
		err = errors.New("auth state mismatch")
	}
	if err != nil {
		if s.refuseOperation(w, r, "authentication_invalid") {
			writeError(w, badRequest(err))
		}
		return
	}
	clearAuthFlowCookie(w, r)
	if request.Error != "" || request.Code == "" {
		if s.refuseOperation(w, r, "authentication_invalid") {
			message := strings.TrimSpace(request.ErrorDescription)
			if message == "" {
				message = "the identity provider did not complete the sign-in"
			}
			writeError(w, badRequest(errors.New(message)))
		}
		return
	}
	connection, err := s.flowConnection(r.Context(), flow)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	client, err := s.oidcClient(r.Context(), connection)
	if err != nil {
		s.log.Warn("OIDC discovery failed", "connection_id", connection.ID, "error", err)
		writeError(w, unavailable(errors.New("the identity provider is unavailable")))
		return
	}
	validated, err := client.Exchange(r.Context(), request.Code, flow.Verifier, flow.Nonce)
	if err != nil {
		s.log.Warn("OIDC sign-in refused", "connection_id", connection.ID, "error", err)
		if s.refuseOperation(w, r, "authentication_invalid") {
			writeError(w, badRequest(errors.New("the identity provider sign-in could not be verified")))
		}
		return
	}
	s.completeSSO(w, r, flow, connection, validated)
}

// ssoSAMLACS receives the IdP's cross-site post. It answers with a redirect to
// the Console in every case.
func (s *Server) ssoSAMLACS(w http.ResponseWriter, r *http.Request) {
	var flow browserAuthFlow
	fail := func(code string) {
		page := "/login"
		if flow.Kind == browserAuthSSOSelfLink {
			page = "/settings/security"
		}
		http.Redirect(w, r, page+"?sso_error="+url.QueryEscape(code), http.StatusSeeOther)
	}
	cookie, err := r.Cookie(samlFlowCookieName(r))
	if err == nil {
		flow, err = s.decodeAuthFlowValue(cookie.Value)
	}
	http.SetCookie(w, samlFlowCookie(r, "", -1))
	if err != nil || !ssoStartKind(flow.Kind) || flow.SSOProtocol != sso.ProtocolSAML || flow.ConnectionID != chi.URLParam(r, "connectionID") {
		fail("authentication_invalid")
		return
	}
	connection, err := s.flowConnection(r.Context(), flow)
	if err != nil {
		fail("sso_not_available")
		return
	}
	sp, err := sso.NewSAML(s.appOrigin, connection.ID, connection.SAML, nil)
	if err != nil {
		fail("sso_not_available")
		return
	}
	response, err := requestFormValue(r, "SAMLResponse")
	if err != nil {
		fail("authentication_invalid")
		return
	}
	assertion, err := sp.Validate(response, flow.SSORequestID)
	if err == nil {
		err = db.RunTx(r.Context(), s.tx, func(tx pgx.Tx) error {
			if _, err := sso.LockActive(r.Context(), tx, connection.ID); err != nil {
				return err
			}
			return sso.ConsumeAssertion(r.Context(), tx, connection.ID, assertion)
		})
	}
	if err != nil {
		s.log.Warn("SAML sign-in refused", "connection_id", connection.ID, "error", err)
		fail("authentication_invalid")
		return
	}
	flow.SSOSubject, flow.SSOEmail, flow.SSOAuthenticatedAt = assertion.Subject, assertion.Email, assertion.AuthenticatedAt
	encoded, err := s.encodeAuthFlow(flow)
	if err != nil {
		fail("authentication_invalid")
		return
	}
	http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	http.Redirect(w, r, "/sso/saml/callback", http.StatusSeeOther)
}

// ssoSAMLFinish completes a SAML sign-in that the ACS validated.
func (s *Server) ssoSAMLFinish(w http.ResponseWriter, r *http.Request) {
	flow, err := s.decodeAuthFlow(r)
	if err == nil && (!ssoStartKind(flow.Kind) || flow.SSOProtocol != sso.ProtocolSAML || flow.SSOSubject == "") {
		err = errors.New("auth state mismatch")
	}
	if err != nil {
		if s.refuseOperation(w, r, "authentication_invalid") {
			writeError(w, badRequest(err))
		}
		return
	}
	clearAuthFlowCookie(w, r)
	connection, err := s.flowConnection(r.Context(), flow)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.completeSSO(w, r, flow, connection, sso.Identity{Subject: flow.SSOSubject, Email: flow.SSOEmail, AuthenticatedAt: flow.SSOAuthenticatedAt})
}

func (s *Server) completeSSO(w http.ResponseWriter, r *http.Request, flow browserAuthFlow, connection sso.Connection, validated sso.Identity) {
	a := identity.SSOAssertion{ConnectionID: connection.ID, Protocol: connection.Protocol, Identity: validated}
	if flow.Kind == browserAuthSSOSelfLink {
		p := s.optionalAccount(r)
		member, err := uuid.Parse(flow.MemberID)
		if err != nil || flow.SessionID == uuid.Nil() || p.SessionID != flow.SessionID {
			s.writeIdentityError(w, r, identity.ErrWrongAccount)
			return
		}
		if err := identity.LinkSSO(r.Context(), s.tx, s.identity, p, member, a); err != nil {
			s.writeIdentityError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, api.SSOFinishResponse{RedirectAfter: "/settings/security"})
		return
	}
	result, err := identity.SignInSSO(r.Context(), s.tx, s.identity, a, rawBrowserSession(r), flow.SessionID)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	if result.Unlinked {
		confirmation := browserAuthFlow{Kind: browserAuthSSOConfirm, StartedAt: time.Now(), ConnectionID: flow.ConnectionID, OrganizationID: flow.OrganizationID,
			SSOProtocol: connection.Protocol, SSOSubject: validated.Subject, SSOEmail: validated.Email, SSOAuthenticatedAt: validated.AuthenticatedAt,
			TokenHash: flow.TokenHash, RedirectAfter: flow.RedirectAfter}
		encoded, err := s.encodeAuthFlow(confirmation)
		if err != nil {
			writeError(w, errors.New("encode SSO confirmation"))
			return
		}
		http.SetCookie(w, ssoConfirmCookie(r, encoded, int(authFlowTTL.Seconds())))
		writeJSON(w, http.StatusOK, api.SSOFinishResponse{RedirectAfter: "/sso/confirm"})
		return
	}
	if result.Session != "" {
		setSessionCookie(w, r, result.Session, 30*24*time.Hour)
	}
	writeJSON(w, http.StatusOK, api.SSOFinishResponse{RedirectAfter: validateRedirectAfter(flow.RedirectAfter)})
}

func (s *Server) decodeSSOConfirmation(r *http.Request) (browserAuthFlow, error) {
	cookie, err := r.Cookie(ssoConfirmCookieName(r))
	if err != nil {
		return browserAuthFlow{}, identity.ErrInvalidToken
	}
	flow, err := s.decodeAuthFlowValue(cookie.Value)
	if err != nil || flow.Kind != browserAuthSSOConfirm || flow.SSOSubject == "" {
		return browserAuthFlow{}, identity.ErrInvalidToken
	}
	return flow, nil
}

func (s *Server) ssoConfirmation(w http.ResponseWriter, r *http.Request) {
	flow, err := s.decodeSSOConfirmation(r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	response := api.SSOConfirmationResponse{OrganizationID: flow.OrganizationID, AssertedEmail: flow.SSOEmail,
		ExpiresAt: flow.StartedAt.Add(authFlowTTL).UTC().Format(time.RFC3339)}
	if err := s.tx.QueryRow(r.Context(), `SELECT name FROM organizations WHERE id=$1`, flow.OrganizationID).Scan(&response.OrganizationName); err != nil {
		writeError(w, err)
		return
	}
	if p := s.optionalAccount(r); p.UserID != uuid.Nil() {
		if err := s.tx.QueryRow(r.Context(), `SELECT COALESCE(primary_email,'') FROM users WHERE id=$1`, p.UserID).Scan(&response.SignedInEmail); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) ssoConfirm(w http.ResponseWriter, r *http.Request) {
	flow, err := s.decodeSSOConfirmation(r)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	connection, err := uuid.Parse(flow.ConnectionID)
	if err != nil {
		s.writeIdentityError(w, r, identity.ErrInvalidToken)
		return
	}
	association := identity.SSOAssociation{ConnectionID: connection, Protocol: flow.SSOProtocol, Subject: flow.SSOSubject, Email: flow.SSOEmail, AuthenticatedAt: flow.SSOAuthenticatedAt}
	if flow.TokenHash != "" {
		if association.InvitationTokenHash, err = decodeFlowTokenHash(flow); err != nil {
			s.writeIdentityError(w, r, identity.ErrInvalidToken)
			return
		}
	}
	orgID, err := identity.ConfirmSSOAssociation(r.Context(), s.tx, s.identity, principalFromContext(r.Context()), association)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	http.SetCookie(w, ssoConfirmCookie(r, "", -1))
	writeJSON(w, http.StatusOK, api.SSOConfirmResponse{OrganizationID: orgID.String(), RedirectAfter: validateRedirectAfter(flow.RedirectAfter)})
}

// accountSSOLinkStart starts linking the member's own membership to company
// SSO. Like linking GitHub, it needs recent proof.
func (s *Server) accountSSOLinkStart(w http.ResponseWriter, r *http.Request) {
	var request api.AccountSSOLinkRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid SSO link request JSON: %w", err))
		return
	}
	p := principalFromContext(r.Context())
	if err := auth.RequireRecentProof(p); err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	orgID, err := uuid.Parse(request.OrganizationID)
	if err != nil {
		writeError(w, badRequest(errors.New("organization_id is invalid")))
		return
	}
	member, err := access.SessionPrincipal(r.Context(), s.tx, p.SessionID, orgID)
	if errors.Is(err, access.ErrDenied) {
		s.writeIdentityError(w, r, identity.ErrSSONotEligible)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if s.ssoStore == nil {
		s.writeIdentityError(w, r, sso.ErrConnectionNotFound)
		return
	}
	connection, err := s.ssoStore.Active(r.Context(), s.tx, orgID)
	if err != nil {
		s.writeIdentityError(w, r, err)
		return
	}
	s.writeSSOStart(w, r, connection, browserAuthFlow{Kind: browserAuthSSOSelfLink, MemberID: member.MemberID.String(), UserID: p.UserID.String()}, request.Reauthenticate)
}

func (s *Server) accountSSOLinks(w http.ResponseWriter, r *http.Request) {
	links, err := identity.AccountSSOLinks(r.Context(), s.tx, principalFromContext(r.Context()))
	if err != nil {
		writeError(w, err)
		return
	}
	response := api.AccountSSOLinksResponse{Links: []api.AccountSSOLink{}}
	for _, b := range links {
		response.Links = append(response.Links, api.AccountSSOLink{OrganizationID: b.OrgID.String(), OrganizationName: b.OrgName,
			Protocol: b.Protocol, Subject: b.Subject, LinkedAt: b.LinkedAt.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, response)
}

// ssoSAMLMetadata publishes Helmr's service-provider metadata for one connection.
func (s *Server) ssoSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "connectionID"))
	if err != nil {
		writeError(w, notFound(errors.New("SSO connection not found")))
		return
	}
	entityID, acsURL := sso.ServiceProviderURLs(s.appOrigin, id)
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
 <md:SPSSODescriptor AuthnRequestsSigned="false" WantAssertionsSigned="true" protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
  <md:NameIDFormat>urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress</md:NameIDFormat>
  <md:NameIDFormat>urn:oasis:names:tc:SAML:2.0:nameid-format:persistent</md:NameIDFormat>
  <md:AssertionConsumerService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="%s" index="0" isDefault="true"/>
 </md:SPSSODescriptor>
</md:EntityDescriptor>
`, html.EscapeString(entityID), html.EscapeString(acsURL))
}
