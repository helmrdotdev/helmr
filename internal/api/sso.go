package api

type SSOStartRequest struct {
	// Organization is the organization slug. An invitation names its own
	// organization, and self-hosted deployments with one organization may omit it.
	Organization    string `json:"organization,omitempty"`
	Next            string `json:"next,omitempty"`
	InvitationToken string `json:"invitation_token,omitempty"`
	Reauthenticate  bool   `json:"reauthenticate,omitempty"`
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

type SSOLinkResponse struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	AssertedEmail    string `json:"asserted_email"`
	ExpiresAt        string `json:"expires_at"`
	SignedInEmail    string `json:"signed_in_email,omitempty"`
}

type SSOLinkConfirmResponse struct {
	OrganizationID string `json:"organization_id"`
	RedirectAfter  string `json:"redirect_after"`
}

type AccountSSOLinkRequest struct {
	OrganizationID string `json:"organization_id"`
	Reauthenticate bool   `json:"reauthenticate,omitempty"`
}

type AccountSSOBinding struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	Protocol         string `json:"protocol"`
	Subject          string `json:"subject"`
	LinkedAt         string `json:"linked_at"`
}

type AccountSSOBindingsResponse struct {
	Bindings []AccountSSOBinding `json:"bindings"`
}
