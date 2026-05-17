package api

type MeResponse struct {
	UserID          string   `json:"user_id"`
	DisplayName     string   `json:"display_name"`
	ProfileImageURL string   `json:"profile_image_url"`
	OrgID           string   `json:"org_id"`
	Role            string   `json:"role"`
	Permissions     []string `json:"permissions"`
}

type GitHubAuthStartRequest struct {
	Next string `json:"next,omitempty"`
}

type BootstrapStatusResponse struct {
	SetupEnabled                  bool `json:"setup_enabled"`
	BootstrapRequired             bool `json:"bootstrap_required"`
	BootstrapOwnerEmailConfigured bool `json:"bootstrap_owner_email_configured"`
}

type GitHubAuthInviteStartRequest struct {
	Token string `json:"token"`
}

type GitHubAuthStartResponse struct {
	RedirectURL string `json:"redirect_url"`
}

type GitHubAuthFinishRequest struct {
	Code             string `json:"code,omitempty"`
	State            string `json:"state,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

type GitHubAuthFinishResponse struct {
	RedirectAfter string `json:"redirect_after"`
}

type MagicLinkStartRequest struct {
	Email string `json:"email,omitempty"`
	Next  string `json:"next,omitempty"`
	Token string `json:"token,omitempty"`
}

type MagicLinkStartResponse struct {
	Sent     bool   `json:"sent"`
	Email    string `json:"email,omitempty"`
	DebugURL string `json:"debug_url,omitempty"`
}

type MagicLinkFinishRequest struct {
	Token string `json:"token"`
}

type MagicLinkFinishResponse struct {
	RedirectAfter string `json:"redirect_after"`
}

type DeviceStartResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresInSeconds        int64  `json:"expires_in_seconds"`
	IntervalSeconds         int64  `json:"interval_seconds"`
}

type DeviceStatusResponse struct {
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type DeviceAuthorizeRequest struct {
	UserCode string `json:"user_code"`
}

type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

type DeviceTokenResponse struct {
	AccessToken      string `json:"access_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
	Error            string `json:"error,omitempty"`
}
