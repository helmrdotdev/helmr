package api

type SSOStartRequest struct {
	// Organization is the organization slug. An invitation names its own
	// organization, and self-hosted deployments with one organization may omit it.
	OrganizationSlug string `json:"organization_slug,omitempty"`
	Next             string `json:"next,omitempty"`
	InvitationToken  string `json:"invitation_token,omitempty"`
	Reauthenticate   bool   `json:"reauthenticate,omitempty"`
}

type SSOStartResponse struct {
	RedirectURL string `json:"redirect_url"`
}

type SSOOIDCFinishRequest struct {
	Code             string `json:"code,omitempty"`
	State            string `json:"state,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

type SSOFinishResponse struct {
	RedirectAfter string `json:"redirect_after"`
}

type SSOConfirmationResponse struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	AssertedEmail    string `json:"asserted_email"`
	ExpiresAt        string `json:"expires_at"`
	SignedInEmail    string `json:"signed_in_email,omitempty"`
}

type SSOConfirmResponse struct {
	OrganizationID string `json:"organization_id"`
	RedirectAfter  string `json:"redirect_after"`
}

type AccountSSOLinkRequest struct {
	OrganizationID string `json:"organization_id"`
	Reauthenticate bool   `json:"reauthenticate,omitempty"`
}

type AccountSSOLink struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	Protocol         string `json:"protocol"`
	Subject          string `json:"subject"`
	LinkedAt         string `json:"linked_at"`
}

type AccountSSOLinksResponse struct {
	Links []AccountSSOLink `json:"links"`
}

type SSOConnection struct {
	ID               string   `json:"id"`
	Protocol         string   `json:"protocol"`
	State            string   `json:"state"`
	OIDCIssuer       string   `json:"oidc_issuer,omitempty"`
	OIDCClientID     string   `json:"oidc_client_id,omitempty"`
	SAMLEntityID     string   `json:"saml_entity_id,omitempty"`
	SAMLSSOURL       string   `json:"saml_sso_url,omitempty"`
	SAMLCertificates []string `json:"saml_certificates,omitempty"`
	// The values an administrator copies into the identity provider.
	ServiceProviderEntityID string  `json:"service_provider_entity_id,omitempty"`
	ServiceProviderACSURL   string  `json:"service_provider_acs_url,omitempty"`
	OIDCRedirectURL         string  `json:"oidc_redirect_url,omitempty"`
	TestedAt                *string `json:"tested_at"`
	CreatedAt               string  `json:"created_at"`
	ActivatedAt             *string `json:"activated_at"`
}

type SSOStatusResponse struct {
	Required        bool           `json:"required"`
	SessionSeconds  int32          `json:"session_seconds"`
	Active          *SSOConnection `json:"active"`
	Candidate       *SSOConnection `json:"candidate"`
	MembersLinked   int            `json:"members_linked"`
	MembersTotal    int            `json:"members_total"`
	OIDCRedirectURL string         `json:"oidc_redirect_url"`
}

// SSOConnectionCreateRequest creates a candidate: OIDC with issuer, client ID
// and secret, or SAML with pasted metadata or its three fields. Fields shared
// with SSOConnection use its names.
type SSOConnectionCreateRequest struct {
	Protocol         string   `json:"protocol"`
	OIDCIssuer       string   `json:"oidc_issuer,omitempty"`
	OIDCClientID     string   `json:"oidc_client_id,omitempty"`
	OIDCClientSecret string   `json:"oidc_client_secret,omitempty"`
	SAMLMetadataXML  string   `json:"saml_metadata_xml,omitempty"`
	SAMLEntityID     string   `json:"saml_entity_id,omitempty"`
	SAMLSSOURL       string   `json:"saml_sso_url,omitempty"`
	SAMLCertificates []string `json:"saml_certificates,omitempty"`
}

// SSOConnectionUpdateRequest rotates material: an OIDC client secret, or SAML
// metadata or sign-in URL and certificates.
type SSOConnectionUpdateRequest struct {
	OIDCClientSecret *string  `json:"oidc_client_secret,omitempty"`
	SAMLMetadataXML  string   `json:"saml_metadata_xml,omitempty"`
	SAMLSSOURL       string   `json:"saml_sso_url,omitempty"`
	SAMLCertificates []string `json:"saml_certificates,omitempty"`
}

type SSOPolicyResponse struct {
	Required           bool  `json:"required"`
	SessionSeconds     int32 `json:"session_seconds"`
	RevokedCLISessions int   `json:"revoked_cli_sessions"`
	RevokedSlackLinks  int   `json:"revoked_slack_links"`
}

type SSOMember struct {
	MemberID    string  `json:"member_id"`
	DisplayName string  `json:"display_name"`
	Email       string  `json:"email"`
	Subject     string  `json:"subject,omitempty"`
	LinkedAt    *string `json:"linked_at"`
}

type SSOMembersResponse struct {
	Members []SSOMember `json:"members"`
}
