package sso

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/helmrdotdev/helmr/internal/sso/ssotest"
)

const attacker = "mallory@evil.test"

func TestSAMLAcceptsEachIdPSigningStyle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	idp := ssotest.NewIdP(t)
	for _, tc := range []struct {
		name string
		edit func(*responseOptions)
	}{
		{"assertion signed (Entra, Google default)", func(o *responseOptions) { o.Sign = signAssertion }},
		{"response signed (ADFS style)", func(o *responseOptions) { o.Sign = signResponse }},
		{"both signed (Keycloak default)", func(o *responseOptions) { o.Sign = signAssertion | signResponse }},
		{"SHA-1 signature (Entra option)", func(o *responseOptions) { o.SHA1 = true }},
		{"persistent NameID with email attribute (Okta, Entra pairwise)", func(o *responseOptions) {
			o.NameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent"
			o.NameID = "00u1abcd"
			o.Attributes = map[string]string{"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": "alice@example.test"}
		}},
		{"email NameID without attributes (minimal Okta app)", func(o *responseOptions) { o.Attributes = nil }},
		{"unspecified NameID holding a UPN (Entra default)", func(o *responseOptions) {
			o.NameIDFormat = "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified"
			o.Attributes = map[string]string{"email": "alice@example.test"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := testSP(t, idp, now)
			_, requestID, err := sp.AuthnRequest(false)
			if err != nil {
				t.Fatal(err)
			}
			o := defaultOptions(sp, requestID, now)
			tc.edit(&o)
			got, err := sp.Validate(ssotest.Encode(t, idp.Response(t, o)), requestID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != o.NameID || got.Email != "alice@example.test" || got.ID != o.AssertionID {
				t.Fatalf("assertion = %+v", got)
			}
			if want, _ := time.Parse(time.RFC3339, o.AuthnInstant); !got.AuthenticatedAt.Equal(want) {
				t.Fatalf("authenticated at %v, want IdP AuthnInstant %v", got.AuthenticatedAt, want)
			}
		})
	}
}

func TestSAMLRefusesInvalidResponses(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	idp := ssotest.NewIdP(t)
	other := ssotest.NewIdP(t)
	for _, tc := range []struct {
		name    string
		edit    func(*responseOptions)
		signer  *ssotest.IdP
		request string
		want    error
	}{
		{name: "answers another request", request: "_other", want: ErrSAMLWrongRequest},
		{name: "transient NameID", edit: func(o *responseOptions) {
			o.NameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
		}, want: ErrTransientNameID},
		{name: "another audience", edit: func(o *responseOptions) { o.Audience = "https://other.example.test" }, want: ErrSAMLResponse},
		{name: "another recipient", edit: func(o *responseOptions) { o.Recipient = "https://other.example.test/acs" }, want: ErrSAMLResponse},
		{name: "another issuer", edit: func(o *responseOptions) { o.Issuer = "https://other-idp.example.test" }, want: ErrSAMLResponse},
		{name: "expired", edit: func(o *responseOptions) {
			o.NotBefore, o.NotOnOrAfter = now.Add(-time.Hour), now.Add(-30*time.Minute)
		}, want: ErrSAMLResponse},
		{name: "not yet valid", edit: func(o *responseOptions) { o.NotBefore = now.Add(time.Hour) }, want: ErrSAMLResponse},
		{name: "no AuthnInstant", edit: func(o *responseOptions) { o.AuthnInstant = "" }, want: ErrSAMLResponse},
		{name: "signed by an unknown key", signer: &other, want: ErrSAMLResponse},
		{name: "signed assertion answers another request", edit: func(o *responseOptions) { o.SCDRequestID = "_other" }, want: ErrSAMLWrongRequest},
		{name: "signed assertion has no request ID (IdP-initiated)", edit: func(o *responseOptions) { o.SCDRequestID = "-" }, want: ErrSAMLWrongRequest},
		{name: "no audience restriction", edit: func(o *responseOptions) { o.NoAudience = true }, want: ErrSAMLResponse},
		{name: "AuthnInstant in the future", edit: func(o *responseOptions) { o.AuthnInstant = now.Add(time.Hour).Format(time.RFC3339) }, want: ErrSAMLResponse},
		{name: "unsigned", edit: func(o *responseOptions) { o.Sign = 0 }, want: ErrSAMLResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := testSP(t, idp, now)
			_, requestID, err := sp.AuthnRequest(false)
			if err != nil {
				t.Fatal(err)
			}
			o := defaultOptions(sp, requestID, now)
			if tc.edit != nil {
				tc.edit(&o)
			}
			signer := idp
			if tc.signer != nil {
				signer = *tc.signer
			}
			expected := requestID
			if tc.request != "" {
				expected = tc.request
			}
			if _, err := sp.Validate(ssotest.Encode(t, signer.Response(t, o)), expected); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// With only the assertion signed, the Response envelope is attacker-writable,
// so its InResponseTo must not stand in for the signed one.
func TestSAMLIgnoresForgedEnvelopeRequestID(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	idp := ssotest.NewIdP(t)
	sp := testSP(t, idp, now)
	_, requestID, err := sp.AuthnRequest(false)
	if err != nil {
		t.Fatal(err)
	}
	o := defaultOptions(sp, "_unsolicited", now)
	o.SCDRequestID = "-"
	doc := idp.Response(t, o)
	doc.Root().CreateAttr("InResponseTo", requestID)
	if _, err := sp.Validate(ssotest.Encode(t, doc), requestID); !errors.Is(err, ErrSAMLWrongRequest) {
		t.Fatalf("forged envelope err = %v", err)
	}
}

// Signature-wrapping and tampering attacks against each signing style. Each
// must be refused, or at worst still yield the legitimate subject.
func TestSAMLRefusesSignatureWrapping(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	idp := ssotest.NewIdP(t)
	attacks := []struct {
		name string
		fn   func(*etree.Document)
	}{
		{"tampered NameID", func(d *etree.Document) { setNameID(assertionOf(d), attacker) }},
		{"unsigned assertion before the signed one", func(d *etree.Document) {
			a := assertionOf(d)
			d.Root().InsertChildAt(a.Index(), evilAssertion(a))
		}},
		{"unsigned assertion after the signed one", func(d *etree.Document) {
			a := assertionOf(d)
			d.Root().InsertChildAt(a.Index()+1, evilAssertion(a))
		}},
		{"signed assertion moved into Extensions", func(d *etree.Document) {
			a := assertionOf(d)
			e := evilAssertion(a)
			d.Root().InsertChildAt(a.Index(), e)
			d.Root().RemoveChild(a)
			ext := etree.NewElement("samlp:Extensions")
			ext.AddChild(a)
			d.Root().InsertChildAt(e.Index(), ext)
		}},
		{"signed assertion nested in an evil one", func(d *etree.Document) {
			a := assertionOf(d)
			e := evilAssertion(a)
			d.Root().InsertChildAt(a.Index(), e)
			d.Root().RemoveChild(a)
			e.AddChild(a)
		}},
		{"signatures stripped", func(d *etree.Document) {
			for _, s := range d.FindElements("//Signature") {
				if p := s.Parent(); p != nil {
					p.RemoveChild(s)
				}
			}
		}},
		{"comment injected into NameID", func(d *etree.Document) {
			n := assertionOf(d).FindElement(".//NameID")
			v := n.Text()
			n.SetText("")
			n.CreateText(attacker)
			n.CreateComment("x")
			n.CreateText(v)
		}},
	}
	for _, mode := range []struct {
		name string
		sign int
	}{{"assertion signed", signAssertion}, {"response signed", signResponse}, {"both signed", signAssertion | signResponse}} {
		for _, a := range attacks {
			t.Run(mode.name+"/"+a.name, func(t *testing.T) {
				sp := testSP(t, idp, now)
				_, requestID, err := sp.AuthnRequest(false)
				if err != nil {
					t.Fatal(err)
				}
				o := defaultOptions(sp, requestID, now)
				o.Sign = mode.sign
				doc := idp.Response(t, o)
				a.fn(doc)
				got, err := sp.Validate(ssotest.Encode(t, doc), requestID)
				if err == nil && strings.Contains(got.Subject+got.Email, "evil") {
					t.Fatalf("accepted as %+v", got)
				}
				if err == nil {
					t.Fatalf("accepted a modified response as %+v", got)
				}
			})
		}
	}
}

func TestSAMLAuthnRequest(t *testing.T) {
	idp := ssotest.NewIdP(t)
	sp := testSP(t, idp, time.Now())
	for _, force := range []bool{false, true} {
		redirect, requestID, err := sp.AuthnRequest(force)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(redirect)
		if err != nil || u.Host != "idp.example.test" || u.Query().Get("SAMLRequest") == "" || requestID == "" {
			t.Fatalf("redirect %q id %q err %v", redirect, requestID, err)
		}
	}
	entity, acs := ServiceProviderURLs(&url.URL{Scheme: "https", Host: "helmr.example.test"}, [16]byte{1})
	if !strings.HasSuffix(entity, "/metadata") || !strings.HasSuffix(acs, "/acs") || !strings.HasPrefix(acs, "https://helmr.example.test/api/auth/sso/saml/") {
		t.Fatal(entity, acs)
	}
}

func assertionOf(d *etree.Document) *etree.Element { return d.Root().FindElement("./Assertion") }

func setNameID(a *etree.Element, v string) {
	if n := a.FindElement(".//NameID"); n != nil {
		n.SetText(v)
	}
}

func evilAssertion(a *etree.Element) *etree.Element {
	c := a.Copy()
	c.CreateAttr("ID", "_evil")
	if s := c.FindElement("./Signature"); s != nil {
		c.RemoveChild(s)
	}
	setNameID(c, attacker)
	return c
}

func TestSAMLForcedReauthentication(t *testing.T) {
	sp := testSP(t, ssotest.NewIdP(t), time.Now())
	for _, force := range []bool{false, true} {
		redirect, requestID, err := sp.AuthnRequest(force)
		if err != nil || requestID == "" {
			t.Fatalf("request = %q, %v", requestID, err)
		}
		u, _ := url.Parse(redirect)
		deflated, err := base64.StdEncoding.DecodeString(u.Query().Get("SAMLRequest"))
		if err != nil {
			t.Fatal(err)
		}
		inflated, err := io.ReadAll(flate.NewReader(bytes.NewReader(deflated)))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(inflated), `ForceAuthn="true"`); got != force {
			t.Fatalf("force=%v request %s", force, inflated)
		}
	}
}
