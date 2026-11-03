package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/sso"
)

// ssoAdminError maps SSO administration errors to HTTP errors.
func ssoAdminError(err error) error {
	var input identity.InputError
	switch {
	case errors.As(err, &input), errors.Is(err, identity.ErrSSOSessionLifetime):
		return badRequest(err)
	case errors.Is(err, sso.ErrEnforcerNotLinked):
		return conflict(codedError{code: "conflict", message: "Link your own company account from Sign-in and security and sign in through it first, so requiring single sign-on does not lock you out."})
	case errors.Is(err, sso.ErrCandidateExists), errors.Is(err, sso.ErrUntested), errors.Is(err, sso.ErrNoRecoveryCodes),
		errors.Is(err, identity.ErrSSORecoveryUnneeded):
		return conflict(codedError{code: "conflict", message: err.Error()})
	case errors.Is(err, sso.ErrConnectionNotFound):
		return notFound(err)
	}
	if mapped := identityError(err); mapped != err {
		return mapped
	}
	return orgError(err)
}

func (s *Server) ssoConfigured(w http.ResponseWriter) bool {
	if s.ssoStore == nil {
		writeError(w, identityError(sso.ErrConnectionNotFound))
		return false
	}
	return true
}

func (s *Server) ssoConnectionView(c *sso.Connection) *api.SSOConnection {
	if c == nil {
		return nil
	}
	view := &api.SSOConnection{ID: c.ID.String(), Protocol: c.Protocol, State: c.State, CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
		TestedAt: formatTimePtr(c.TestedAt), ActivatedAt: formatTimePtr(c.ActivatedAt)}
	switch c.Protocol {
	case sso.ProtocolOIDC:
		view.OIDCIssuer, view.OIDCClientID = c.OIDC.Issuer, c.OIDC.ClientID
		view.OIDCRedirectURL = s.appOrigin.JoinPath("/sso/oidc/callback").String()
	case sso.ProtocolSAML:
		view.SAMLEntityID, view.SAMLSSOURL, view.SAMLCertificates = c.SAML.EntityID, c.SAML.SSOURL, c.SAML.Certificates
		view.ServiceProviderEntityID, view.ServiceProviderACSURL = sso.ServiceProviderURLs(s.appOrigin, c.ID)
	}
	return view
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	value := t.UTC().Format(time.RFC3339)
	return &value
}

func (s *Server) ssoStatus(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	status, err := identity.ReadSSOStatus(r.Context(), s.tx, s.ssoStore, s.managingMember(principalFromContext(r.Context())))
	if err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.SSOStatusResponse{Required: status.Policy.Required, SessionSeconds: status.Policy.SessionSeconds,
		Active: s.ssoConnectionView(status.Active), Candidate: s.ssoConnectionView(status.Candidate),
		MembersLinked: status.MembersLinked, MembersTotal: status.MembersTotal,
		RecoveryCodes:   api.SSORecoveryCodesStatus{Remaining: status.RecoveryRemaining, GeneratedAt: formatTimePtr(status.RecoveryGenerated)},
		OIDCRedirectURL: s.appOrigin.JoinPath("/sso/oidc/callback").String()})
}

// samlInput reads pasted metadata, or the three manual fields.
func samlInput(metadata, entityID, ssoURL string, certificates []string) (sso.SAMLIdP, error) {
	if strings.TrimSpace(metadata) != "" {
		return sso.ParseSAMLMetadata([]byte(metadata))
	}
	idp := sso.SAMLIdP{EntityID: strings.TrimSpace(entityID), SSOURL: strings.TrimSpace(ssoURL), Certificates: certificates}
	if entityID == "" {
		// An update keeps the connection's entity ID.
		if strings.TrimSpace(idp.SSOURL) == "" || len(idp.Certificates) == 0 {
			return sso.SAMLIdP{}, errors.New("the SAML sign-in URL and at least one signing certificate are required")
		}
		return idp, nil
	}
	return idp, idp.Validate()
}

func (s *Server) createSSOConnection(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	var request api.SSOConnectionCreateRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, badRequest(fmt.Errorf("invalid SSO connection JSON: %w", err)))
		return
	}
	var input identity.SSOConnectionInput
	switch request.Protocol {
	case sso.ProtocolOIDC:
		cfg := sso.OIDCConfig{Issuer: strings.TrimSpace(request.Issuer), ClientID: strings.TrimSpace(request.ClientID), ClientSecret: request.ClientSecret}
		// Discovery proves the issuer before anything is stored.
		if _, err := sso.NewOIDC(r.Context(), s.ssoHTTP, cfg.Issuer, cfg.ClientID, cfg.ClientSecret, s.appOrigin.JoinPath("/sso/oidc/callback").String()); err != nil {
			s.writeOperationError(w, r, badRequest(fmt.Errorf("the OIDC issuer could not be verified: %w", err)))
			return
		}
		input.OIDC = &cfg
	case sso.ProtocolSAML:
		idp, err := samlInput(request.MetadataXML, request.EntityID, request.SSOURL, request.Certificates)
		if err == nil && idp.EntityID == "" {
			err = errors.New("the SAML identity provider entity ID is required")
		}
		if err != nil {
			s.writeOperationError(w, r, badRequest(err))
			return
		}
		input.SAML = &idp
	default:
		s.writeOperationError(w, r, badRequest(errors.New("protocol must be oidc or saml")))
		return
	}
	created, err := identity.CreateSSOConnection(r.Context(), s.tx, s.ssoStore, s.managingMember(principalFromContext(r.Context())), input)
	if err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	writeJSON(w, http.StatusCreated, s.ssoConnectionView(&created))
}

func (s *Server) updateSSOConnection(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	id, err := parseUUIDParam(r, "connectionID")
	if err != nil {
		s.writeOperationError(w, r, notFound(sso.ErrConnectionNotFound))
		return
	}
	var request api.SSOConnectionUpdateRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, badRequest(fmt.Errorf("invalid SSO connection JSON: %w", err)))
		return
	}
	update := identity.SSOConnectionUpdate{ClientSecret: request.ClientSecret}
	if request.MetadataXML != "" || request.SSOURL != "" || len(request.Certificates) > 0 {
		idp, err := samlInput(request.MetadataXML, "", request.SSOURL, request.Certificates)
		if err != nil {
			s.writeOperationError(w, r, badRequest(err))
			return
		}
		update.SAML = &idp
	}
	if err := identity.UpdateSSOConnection(r.Context(), s.tx, s.ssoStore, s.managingMember(principalFromContext(r.Context())), id, update); err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testSSOConnection(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	id, err := parseUUIDParam(r, "connectionID")
	if err != nil {
		s.writeOperationError(w, r, notFound(sso.ErrConnectionNotFound))
		return
	}
	p := principalFromContext(r.Context())
	c, err := identity.StartSSOTest(r.Context(), s.tx, s.ssoStore, s.managingMember(p), id)
	if err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	s.writeSSOStart(w, r, c, browserAuthFlow{Kind: browserAuthSSOTest, MemberID: p.MemberID.String(), UserID: p.UserID.String(),
		SSOFingerprint: base64.RawURLEncoding.EncodeToString(c.Fingerprint())}, false)
}

func (s *Server) activateSSOConnection(w http.ResponseWriter, r *http.Request) {
	s.ssoConnectionAction(w, r, identity.ActivateSSOConnection)
}

func (s *Server) deleteSSOConnection(w http.ResponseWriter, r *http.Request) {
	s.ssoConnectionAction(w, r, identity.DeleteSSOConnection)
}

func (s *Server) ssoConnectionAction(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, database db.TxBeginner, store *sso.Store, m org.ManagingMember, id uuid.UUID) error) {
	if !s.ssoConfigured(w) {
		return
	}
	id, err := parseUUIDParam(r, "connectionID")
	if err != nil {
		s.writeOperationError(w, r, notFound(sso.ErrConnectionNotFound))
		return
	}
	if err := action(r.Context(), s.tx, s.ssoStore, s.managingMember(principalFromContext(r.Context())), id); err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateSSOPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	var request struct {
		Required       *bool           `json:"required"`
		SessionSeconds optionalSeconds `json:"session_seconds"`
	}
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, badRequest(fmt.Errorf("invalid SSO policy JSON: %w", err)))
		return
	}
	change := identity.SSOPolicyChange{Required: request.Required}
	if request.SessionSeconds.set {
		change.SessionSeconds, change.ResetSessionSeconds = request.SessionSeconds.value, request.SessionSeconds.value == nil
	}
	result, err := identity.UpdateSSOPolicy(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())), change)
	if err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.SSOPolicyResponse{Required: result.Policy.Required, SessionSeconds: result.Policy.SessionSeconds,
		RevokedCLISessions: result.RevokedCLISessions, RevokedSlackLinks: result.RevokedSlackLinks})
}

func (s *Server) ssoMembers(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	members, err := identity.LinkedSSOMembers(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())))
	if err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	response := api.SSOMembersResponse{Members: []api.SSOMember{}}
	for _, m := range members {
		response.Members = append(response.Members, api.SSOMember{MemberID: m.MemberID.String(), DisplayName: m.DisplayName, Email: m.Email, Subject: m.Subject, LinkedAt: formatTimePtr(m.LinkedAt)})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) unlinkSSOMember(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	member, err := parseUUIDParam(r, "memberID")
	if err != nil {
		s.writeOperationError(w, r, notFound(sso.ErrConnectionNotFound))
		return
	}
	if err := identity.UnlinkSSOMember(r.Context(), s.tx, s.managingMember(principalFromContext(r.Context())), member); err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) generateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if !s.ssoConfigured(w) {
		return
	}
	codes, err := identity.GenerateRecoveryCodes(r.Context(), s.tx, s.identity, s.managingMember(principalFromContext(r.Context())))
	if err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.SSORecoveryCodesResponse{Codes: codes})
}

// accountSSORecover lets an owner signed in with an ordinary method enter a
// recovery code for an organization whose identity provider is unavailable.
func (s *Server) accountSSORecover(w http.ResponseWriter, r *http.Request) {
	var request api.AccountSSORecoverRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid recovery JSON: %w", err)))
		return
	}
	orgID, err := uuid.Parse(request.OrganizationID)
	if err != nil {
		writeError(w, badRequest(errors.New("organization_id is invalid")))
		return
	}
	if err := identity.RecoverSSO(r.Context(), s.tx, s.identity, principalFromContext(r.Context()), orgID, request.Code); err != nil {
		s.writeOperationError(w, r, ssoAdminError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// optionalSeconds tells an absent session lifetime (keep it) from null (the
// default) and a number of seconds.
type optionalSeconds struct {
	set   bool
	value *int32
}

func (o *optionalSeconds) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		return nil
	}
	v, err := strconv.ParseInt(string(b), 10, 32)
	if err != nil {
		return identity.ErrSSOSessionLifetime
	}
	seconds := int32(v)
	o.value = &seconds
	return nil
}
