package sso

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// testIdP signs SAML responses the way real IdPs do, so validation can be
// exercised for each signing style without a vendor account.
type testIdP struct {
	entityID string
	keys     dsig.X509KeyStore
	certPEM  string
}

func newTestIdP(t *testing.T) testIdP {
	t.Helper()
	ks := dsig.RandomKeyStoreForTest()
	_, der, err := ks.GetKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return testIdP{entityID: "https://idp.example.test/metadata", keys: ks,
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (i testIdP) samlIdP() SAMLIdP {
	return SAMLIdP{EntityID: i.entityID, SSOURL: "https://idp.example.test/sso", Certificates: []string{i.certPEM}}
}

// signing styles seen across IdPs.
const (
	signAssertion = 1 << iota // Entra and Google default
	signResponse              // ADFS-style response signature
)

type responseOptions struct {
	sign         int
	nameID       string
	nameIDFormat string
	requestID    string
	audience     string
	recipient    string
	issuer       string
	notBefore    time.Time
	notOnOrAfter time.Time
	authnInstant string // empty omits the AuthnInstant attribute
	assertionID  string
	sha1         bool
	attributes   map[string]string
	// scdRequestID overrides the signed SubjectConfirmationData InResponseTo;
	// "-" omits it (the IdP-initiated shape).
	scdRequestID string
	noAudience   bool
}

func defaultOptions(sp *SAML, requestID string, now time.Time) responseOptions {
	return responseOptions{
		sign: signAssertion, nameID: "alice@example.test",
		nameIDFormat: "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
		requestID:    requestID, audience: sp.sp.AudienceURI, recipient: sp.sp.AssertionConsumerServiceURL,
		notBefore: now.Add(-time.Minute), notOnOrAfter: now.Add(5 * time.Minute),
		authnInstant: now.Add(-30 * time.Second).Format(time.RFC3339), assertionID: "_a" + uuid.NewV7().String(),
	}
}

func (i testIdP) response(t *testing.T, o responseOptions) *etree.Document {
	t.Helper()
	issuer := i.entityID
	if o.issuer != "" {
		issuer = o.issuer
	}
	authn := ""
	if o.authnInstant != "" {
		authn = fmt.Sprintf(` AuthnInstant="%s"`, o.authnInstant)
	}
	attrs := ""
	for k, v := range o.attributes {
		attrs += fmt.Sprintf(`<saml:Attribute Name="%s"><saml:AttributeValue>%s</saml:AttributeValue></saml:Attribute>`, k, v)
	}
	if attrs != "" {
		attrs = "<saml:AttributeStatement>" + attrs + "</saml:AttributeStatement>"
	}
	f := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	scd := fmt.Sprintf(` InResponseTo="%s"`, o.requestID)
	switch o.scdRequestID {
	case "":
	case "-":
		scd = ""
	default:
		scd = fmt.Sprintf(` InResponseTo="%s"`, o.scdRequestID)
	}
	audience := fmt.Sprintf(`<saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction>`, o.audience)
	if o.noAudience {
		audience = ""
	}
	raw := fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="_r%s" Version="2.0" IssueInstant="%s" Destination="%s" InResponseTo="%s"><saml:Issuer>%s</saml:Issuer><samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status><saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="%s" Version="2.0" IssueInstant="%s"><saml:Issuer>%s</saml:Issuer><saml:Subject><saml:NameID Format="%s">%s</saml:NameID><saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData%s NotOnOrAfter="%s" Recipient="%s"/></saml:SubjectConfirmation></saml:Subject><saml:Conditions NotBefore="%s" NotOnOrAfter="%s">%s</saml:Conditions><saml:AuthnStatement%s SessionIndex="_s1"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>%s</saml:Assertion></samlp:Response>`,
		uuid.NewV7(), f(o.notBefore), o.recipient, o.requestID, issuer, o.assertionID, f(o.notBefore), issuer,
		o.nameIDFormat, o.nameID, scd, f(o.notOnOrAfter), o.recipient, f(o.notBefore), f(o.notOnOrAfter), audience, authn, attrs)
	doc := etree.NewDocument()
	if err := doc.ReadFromString(raw); err != nil {
		t.Fatal(err)
	}
	ctx := dsig.NewDefaultSigningContext(i.keys)
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if o.sha1 {
		if err := ctx.SetSignatureMethod(dsig.RSASHA1SignatureMethod); err != nil {
			t.Fatal(err)
		}
	}
	if o.sign&signAssertion != 0 {
		sign(t, ctx, doc.Root().FindElement("./Assertion"))
	}
	if o.sign&signResponse != 0 {
		sign(t, ctx, doc.Root())
	}
	return doc
}

// sign inserts an enveloped signature right after Issuer, as the SAML schema
// requires and IdPs do.
func sign(t *testing.T, ctx *dsig.SigningContext, el *etree.Element) {
	t.Helper()
	sig, err := ctx.ConstructSignature(el, true)
	if err != nil {
		t.Fatal(err)
	}
	el.InsertChildAt(1, sig)
}

func encode(t *testing.T, doc *etree.Document) string {
	t.Helper()
	doc.WriteSettings = etree.WriteSettings{CanonicalAttrVal: true, CanonicalEndTags: true, CanonicalText: true}
	b, err := doc.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func testSP(t *testing.T, idp testIdP, now time.Time) *SAML {
	t.Helper()
	origin, _ := url.Parse("https://helmr.example.test")
	sp, err := NewSAML(origin, uuid.NewV7(), idp.samlIdP(), dsig.NewFakeClockAt(now))
	if err != nil {
		t.Fatal(err)
	}
	return sp
}
