package sso

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/russellhaering/gosaml2/types"
)

const samlRedirectBinding = "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"

// maxMetadataBytes bounds pasted IdP metadata.
const maxMetadataBytes = 256 << 10

// SAMLIdP is the identity-provider side of a SAML connection: its immutable
// entity ID, its sign-in URL and the certificates that may sign its responses.
type SAMLIdP struct {
	EntityID     string
	SSOURL       string
	Certificates []string // PEM
}

// ParseSAMLMetadata reads pasted IdP metadata. Metadata is never fetched from a
// URL, so no certificate is refreshed without an administrator.
func ParseSAMLMetadata(raw []byte) (SAMLIdP, error) {
	if len(raw) == 0 || len(raw) > maxMetadataBytes {
		return SAMLIdP{}, errors.New("SAML metadata is empty or too large")
	}
	var ed types.EntityDescriptor
	if err := xml.Unmarshal(raw, &ed); err != nil {
		return SAMLIdP{}, fmt.Errorf("SAML metadata is not valid XML: %w", err)
	}
	if ed.IDPSSODescriptor == nil {
		return SAMLIdP{}, errors.New("SAML metadata has no identity provider descriptor")
	}
	idp := SAMLIdP{EntityID: strings.TrimSpace(ed.EntityID)}
	for _, s := range ed.IDPSSODescriptor.SingleSignOnServices {
		if s.Binding == samlRedirectBinding {
			idp.SSOURL = strings.TrimSpace(s.Location)
		}
	}
	for _, kd := range ed.IDPSSODescriptor.KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, c := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(c.Data), ""))
			if err != nil {
				return SAMLIdP{}, errors.New("SAML metadata has an unreadable certificate")
			}
			idp.Certificates = append(idp.Certificates, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
		}
	}
	return idp, idp.Validate()
}

// normalized trims manually entered values so they compare exactly at sign-in.
func (idp SAMLIdP) normalized() SAMLIdP {
	idp.EntityID, idp.SSOURL = strings.TrimSpace(idp.EntityID), strings.TrimSpace(idp.SSOURL)
	return idp
}

// Validate checks a manually entered or parsed IdP configuration.
func (idp SAMLIdP) Validate() error {
	if idp.EntityID == "" {
		return errors.New("the SAML identity provider entity ID is required")
	}
	if u, err := url.Parse(idp.SSOURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("the SAML sign-in URL must be an HTTPS URL with the HTTP-Redirect binding")
	}
	if len(idp.Certificates) == 0 {
		return errors.New("at least one SAML signing certificate is required")
	}
	_, err := parseCertificates(idp.Certificates)
	return err
}

func parseCertificates(pems []string) ([]*x509.Certificate, error) {
	certs := make([]*x509.Certificate, 0, len(pems))
	for _, p := range pems {
		block, rest := pem.Decode([]byte(strings.TrimSpace(p)))
		if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
			return nil, errors.New("each SAML signing certificate must be one PEM certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("a SAML signing certificate is invalid: %w", err)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}
