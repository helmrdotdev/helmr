package api

type PermissionScope struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Agent       string   `json:"agent,omitempty"`
	Permissions []string `json:"permissions"`
}

type MeResponse struct {
	UserID          string `json:"user_id"`
	DisplayName     string `json:"display_name"`
	ProfileImageURL string `json:"profile_image_url"`
	PrimaryEmail    string `json:"primary_email"`
}
type OrganizationContextResponse struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Slug           string                 `json:"slug"`
	CurrentMember  CurrentMember          `json:"current_member"`
	Authentication AuthenticationContext  `json:"authentication"`
	Onboarding     OrganizationOnboarding `json:"onboarding"`
}
type CurrentMember struct {
	ID               string            `json:"id"`
	UserID           string            `json:"user_id"`
	DisplayName      string            `json:"display_name"`
	IsOwner          bool              `json:"is_owner"`
	Permissions      []string          `json:"permissions"`
	PermissionScopes []PermissionScope `json:"permission_scopes"`
}
type AuthenticationContext struct {
	AuthenticatedAt string `json:"authenticated_at"`
}
type OrganizationOnboarding struct {
	ProjectRequired bool `json:"project_required"`
}
type OrganizationMembership struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	Slug                   string `json:"slug"`
	MembershipID           string `json:"membership_id"`
	AuthenticationRequired string `json:"authentication_required"`
}
type OrganizationsResponse struct {
	Organizations []OrganizationMembership `json:"organizations"`
}
type AuthConfigResponse struct {
	DeploymentMode              string                  `json:"deployment_mode"`
	Methods                     []string                `json:"methods"`
	EmailDelivery               bool                    `json:"email_delivery"`
	AppOrigin                   string                  `json:"app_origin"`
	SignupEnabled               bool                    `json:"signup_enabled"`
	OrganizationCreationEnabled bool                    `json:"organization_creation_enabled"`
	SingleOrganization          *OrganizationMembership `json:"single_organization,omitempty"`
	SetupRequired               bool                    `json:"setup_required"`
}

type GitHubAuthStartRequest struct {
	Next string `json:"next,omitempty"`
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

type EmailStartRequest struct {
	Email           string `json:"email"`
	Next            string `json:"next,omitempty"`
	InvitationToken string `json:"invitation_token,omitempty"`
}
type EmailVerifyRequest struct {
	Code string `json:"code"`
}
type AuthenticationFinishResponse struct {
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

type DeviceConsentView struct {
	AccountName      string `json:"account_name"`
	OrganizationName string `json:"org_name"`
	Proof            string `json:"proof"`
}
type DeviceStatusResponse struct {
	Consent   *DeviceConsentView `json:"consent,omitempty"`
	Status    string             `json:"status"`
	ExpiresAt string             `json:"expires_at,omitempty"`
}

type DeviceAuthorizeRequest struct {
	UserCode string `json:"user_code"`
}

type DeviceTokenRequest struct {
	DeviceCode string `json:"device_code"`
}

type DeviceTokenResponse struct {
	AccessToken string `json:"access_token,omitempty"`
	Error       string `json:"error,omitempty"`
}
