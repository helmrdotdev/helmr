package sso

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"uuid"

	saml2 "github.com/russellhaering/gosaml2"
	dsig "github.com/russellhaering/goxmldsig"
)

// samlEmailAttributes are the attribute names IdPs commonly use for the email
// address when the NameID is not an email.
var samlEmailAttributes = []string{"email", "mail", "Email", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"}

// Identity is what a validated SSO response proves: a stable subject within the
// connection, the email the IdP asserted (untrusted for account ownership) and
// the time the person authenticated at the IdP.
type Identity struct {
	Subject         string
	Email           string
	AuthenticatedAt time.Time
}

// SAMLAssertion is a validated SAML sign-in. The caller consumes its ID with
// ConsumeAssertion in the transaction that acts on it.
type SAMLAssertion struct {
	Identity
	ID           string
	NotOnOrAfter time.Time
}

// SAML is Helmr's service provider for one connection.
type SAML struct {
	sp *saml2.SAMLServiceProvider
}

// ServiceProviderURLs are the values an administrator copies into the IdP.
func ServiceProviderURLs(appOrigin *url.URL, connection uuid.UUID) (entityID, acsURL string) {
	base := strings.TrimRight(appOrigin.String(), "/") + "/api/auth/sso/saml/" + connection.String()
	return base + "/metadata", base + "/acs"
}

// NewSAML builds the service provider. clock is nil outside tests.
func NewSAML(appOrigin *url.URL, connection uuid.UUID, idp SAMLIdP, clock *dsig.Clock) (*SAML, error) {
	certs, err := parseCertificates(idp.Certificates)
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = dsig.NewRealClock()
	}
	entityID, acsURL := ServiceProviderURLs(appOrigin, connection)
	return &SAML{sp: &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL:      idp.SSOURL,
		IdentityProviderIssuer:      idp.EntityID,
		ServiceProviderIssuer:       entityID,
		AssertionConsumerServiceURL: acsURL,
		AudienceURI:                 entityID,
		IDPCertificateStore:         &dsig.MemoryX509CertificateStore{Roots: certs},
		Clock:                       clock,
		// A minimal Okta app with an email NameID sends no attributes.
		AllowMissingAttributes: true,
	}}, nil
}

// AuthnRequest returns the IdP redirect URL and the request ID that the
// response must answer. forceAuthn asks the IdP to authenticate again.
func (s *SAML) AuthnRequest(forceAuthn bool) (redirectURL, requestID string, err error) {
	s.sp.ForceAuthn = forceAuthn
	doc, err := s.sp.BuildAuthRequestDocumentNoSig()
	if err != nil {
		return "", "", err
	}
	requestID = doc.Root().SelectAttrValue("ID", "")
	if requestID == "" {
		return "", "", errors.New("SAML request has no ID")
	}
	redirectURL, err = s.sp.BuildAuthURLRedirect("", doc)
	return redirectURL, requestID, err
}

var (
	ErrSAMLResponse      = errors.New("the SAML response is not valid")
	ErrSAMLWrongRequest  = errors.New("the SAML response does not answer this sign-in")
	ErrTransientNameID   = errors.New("the identity provider sent a transient NameID; configure a persistent or email NameID")
	ErrAssertionReplayed = errors.New("the SAML assertion was already used")
)

// Validate checks a posted SAMLResponse for the request it must answer.
// gosaml2 verifies the signature (on the response or the assertion), issuer,
// status, bearer confirmation, recipient and its expiry, and allows exactly one
// signed top-level assertion. Helmr adds the request ID, audience and time
// conditions (which gosaml2 only reports), the NameID format and AuthnInstant.
func (s *SAML) Validate(encoded, requestID string) (SAMLAssertion, error) {
	resp, err := s.sp.ValidateEncodedResponse(encoded)
	if err != nil {
		return SAMLAssertion{}, fmt.Errorf("%w: %v", ErrSAMLResponse, err)
	}
	if requestID == "" || resp.InResponseTo != requestID {
		return SAMLAssertion{}, ErrSAMLWrongRequest
	}
	info, err := s.sp.RetrieveAssertionInfo(encoded)
	if err != nil {
		return SAMLAssertion{}, fmt.Errorf("%w: %v", ErrSAMLResponse, err)
	}
	if info.WarningInfo == nil || info.WarningInfo.InvalidTime || info.WarningInfo.NotInAudience {
		return SAMLAssertion{}, fmt.Errorf("%w: assertion is outside its validity window or audience", ErrSAMLResponse)
	}
	if len(info.Assertions) != 1 {
		return SAMLAssertion{}, fmt.Errorf("%w: expected one assertion", ErrSAMLResponse)
	}
	a := info.Assertions[0]
	if a.Subject == nil || a.Subject.SubjectConfirmation == nil || a.Subject.SubjectConfirmation.SubjectConfirmationData == nil {
		return SAMLAssertion{}, fmt.Errorf("%w: missing subject confirmation", ErrSAMLResponse)
	}
	data := a.Subject.SubjectConfirmation.SubjectConfirmationData
	if data.InResponseTo != "" && data.InResponseTo != requestID {
		return SAMLAssertion{}, ErrSAMLWrongRequest
	}
	if info.NameIDFormat == saml2.NameIdFormatTransient {
		return SAMLAssertion{}, ErrTransientNameID
	}
	if strings.TrimSpace(info.NameID) == "" || a.ID == "" {
		return SAMLAssertion{}, fmt.Errorf("%w: missing NameID or assertion ID", ErrSAMLResponse)
	}
	if info.AuthnInstant == nil || info.AuthnInstant.IsZero() {
		return SAMLAssertion{}, fmt.Errorf("%w: missing AuthnInstant", ErrSAMLResponse)
	}
	notOnOrAfter, err := time.Parse(time.RFC3339, data.NotOnOrAfter)
	if err != nil {
		return SAMLAssertion{}, fmt.Errorf("%w: invalid NotOnOrAfter", ErrSAMLResponse)
	}
	return SAMLAssertion{
		Identity:     Identity{Subject: info.NameID, Email: samlEmail(info), AuthenticatedAt: info.AuthnInstant.UTC()},
		ID:           a.ID,
		NotOnOrAfter: notOnOrAfter.UTC(),
	}, nil
}

func samlEmail(info *saml2.AssertionInfo) string {
	if info.NameIDFormat == saml2.NameIdFormatEmailAddress {
		return strings.TrimSpace(info.NameID)
	}
	for _, name := range samlEmailAttributes {
		if v := strings.TrimSpace(info.Values.Get(name)); v != "" {
			return v
		}
	}
	return ""
}
