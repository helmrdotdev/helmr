// Package ssotest provides an identity provider that signs SAML responses the
// way real IdPs do, for tests that need no vendor account.
package ssotest

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// IdP signs SAML responses the way real IdPs do, so validation can be
// exercised for each signing style without a vendor account.
type IdP struct {
	EntityID string
	Keys     dsig.X509KeyStore
	CertPEM  string
}

func NewIdP(t *testing.T) IdP {
	t.Helper()
	ks := dsig.RandomKeyStoreForTest()
	_, der, err := ks.GetKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return IdP{EntityID: "https://idp.example.test/metadata", Keys: ks,
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// Signing styles seen across IdPs.
const (
	SignAssertion = 1 << iota // Entra and Google default
	SignResponse              // ADFS-style response signature
)

type ResponseOptions struct {
	Sign         int
	NameID       string
	NameIDFormat string
	RequestID    string
	Audience     string
	Recipient    string
	Issuer       string
	NotBefore    time.Time
	NotOnOrAfter time.Time
	AuthnInstant string // empty omits the AuthnInstant attribute
	AssertionID  string
	SHA1         bool
	Attributes   map[string]string
	// scdRequestID overrides the signed SubjectConfirmationData InResponseTo;
	// "-" omits it (the IdP-initiated shape).
	SCDRequestID string
	NoAudience   bool
}

func (i IdP) Response(t *testing.T, o ResponseOptions) *etree.Document {
	t.Helper()
	issuer := i.EntityID
	if o.Issuer != "" {
		issuer = o.Issuer
	}
	authn := ""
	if o.AuthnInstant != "" {
		authn = fmt.Sprintf(` AuthnInstant="%s"`, o.AuthnInstant)
	}
	attrs := ""
	for k, v := range o.Attributes {
		attrs += fmt.Sprintf(`<saml:Attribute Name="%s"><saml:AttributeValue>%s</saml:AttributeValue></saml:Attribute>`, k, v)
	}
	if attrs != "" {
		attrs = "<saml:AttributeStatement>" + attrs + "</saml:AttributeStatement>"
	}
	f := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	scd := fmt.Sprintf(` InResponseTo="%s"`, o.RequestID)
	switch o.SCDRequestID {
	case "":
	case "-":
		scd = ""
	default:
		scd = fmt.Sprintf(` InResponseTo="%s"`, o.SCDRequestID)
	}
	audience := fmt.Sprintf(`<saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction>`, o.Audience)
	if o.NoAudience {
		audience = ""
	}
	raw := fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="_r%s" Version="2.0" IssueInstant="%s" Destination="%s" InResponseTo="%s"><saml:Issuer>%s</saml:Issuer><samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status><saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="%s" Version="2.0" IssueInstant="%s"><saml:Issuer>%s</saml:Issuer><saml:Subject><saml:NameID Format="%s">%s</saml:NameID><saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData%s NotOnOrAfter="%s" Recipient="%s"/></saml:SubjectConfirmation></saml:Subject><saml:Conditions NotBefore="%s" NotOnOrAfter="%s">%s</saml:Conditions><saml:AuthnStatement%s SessionIndex="_s1"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>%s</saml:Assertion></samlp:Response>`,
		uuid.NewV7(), f(o.NotBefore), o.Recipient, o.RequestID, issuer, o.AssertionID, f(o.NotBefore), issuer,
		o.NameIDFormat, o.NameID, scd, f(o.NotOnOrAfter), o.Recipient, f(o.NotBefore), f(o.NotOnOrAfter), audience, authn, attrs)
	doc := etree.NewDocument()
	if err := doc.ReadFromString(raw); err != nil {
		t.Fatal(err)
	}
	ctx := dsig.NewDefaultSigningContext(i.Keys)
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if o.SHA1 {
		if err := ctx.SetSignatureMethod(dsig.RSASHA1SignatureMethod); err != nil {
			t.Fatal(err)
		}
	}
	if o.Sign&SignAssertion != 0 {
		Sign(t, ctx, doc.Root().FindElement("./Assertion"))
	}
	if o.Sign&SignResponse != 0 {
		Sign(t, ctx, doc.Root())
	}
	return doc
}

// Sign inserts an enveloped signature right after Issuer, as the SAML schema
// requires and IdPs do.
func Sign(t *testing.T, ctx *dsig.SigningContext, el *etree.Element) {
	t.Helper()
	sig, err := ctx.ConstructSignature(el, true)
	if err != nil {
		t.Fatal(err)
	}
	el.InsertChildAt(1, sig)
}

func Encode(t *testing.T, doc *etree.Document) string {
	t.Helper()
	doc.WriteSettings = etree.WriteSettings{CanonicalAttrVal: true, CanonicalEndTags: true, CanonicalText: true}
	b, err := doc.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}
