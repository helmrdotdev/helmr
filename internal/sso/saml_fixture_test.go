package sso

import (
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sso/ssotest"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	signAssertion = ssotest.SignAssertion
	signResponse  = ssotest.SignResponse
)

type responseOptions = ssotest.ResponseOptions

func samlIdP(i ssotest.IdP) SAMLIdP {
	return SAMLIdP{EntityID: i.EntityID, SSOURL: "https://idp.example.test/sso", Certificates: []string{i.CertPEM}}
}

func defaultOptions(sp *SAML, requestID string, now time.Time) responseOptions {
	return responseOptions{
		Sign: signAssertion, NameID: "alice@example.test",
		NameIDFormat: "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
		RequestID:    requestID, Audience: sp.sp.AudienceURI, Recipient: sp.sp.AssertionConsumerServiceURL,
		NotBefore: now.Add(-time.Minute), NotOnOrAfter: now.Add(5 * time.Minute),
		AuthnInstant: now.Add(-30 * time.Second).Format(time.RFC3339), AssertionID: "_a" + uuid.NewV7().String(),
	}
}

func testSP(t *testing.T, idp ssotest.IdP, now time.Time) *SAML {
	t.Helper()
	origin, _ := url.Parse("https://helmr.example.test")
	sp, err := NewSAML(origin, uuid.NewV7(), samlIdP(idp), dsig.NewFakeClockAt(now))
	if err != nil {
		t.Fatal(err)
	}
	return sp
}
