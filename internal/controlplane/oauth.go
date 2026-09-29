package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/identity"
	"golang.org/x/oauth2"
)

type githubOAuthProvider struct {
	log           *slog.Logger
	config        oauth2.Config
	userURL       string
	userEmailsURL string
}

func NewGitHubOAuthProvider(log *slog.Logger, clientID string, clientSecret string, publicURL *url.URL) AuthProvider {
	redirect := publicURL.ResolveReference(&url.URL{Path: "/auth/github/callback"}).String()
	return &githubOAuthProvider{
		log: log,
		config: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirect,
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://github.com/login/oauth/authorize",
				TokenURL: "https://github.com/login/oauth/access_token",
			},
			Scopes: []string{"user:email"},
		},
		userURL:       "https://api.github.com/user",
		userEmailsURL: "https://api.github.com/user/emails",
	}
}

func (p *githubOAuthProvider) RedirectURL(state string, verifier string) string {
	return p.config.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(verifier),
	)
}

// Resolve exchanges the authorization code and reads the GitHub user and the
// addresses GitHub verified for it. A failed address lookup leaves the
// identity without verified addresses.
func (p *githubOAuthProvider) Resolve(ctx context.Context, code string, verifier string) (identity.ExternalIdentity, error) {
	token, err := p.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return identity.ExternalIdentity{}, fmt.Errorf("exchange github oauth code: %w", err)
	}
	client := p.config.Client(ctx, token)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userURL, nil)
	if err != nil {
		return identity.ExternalIdentity{}, err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "helmr-controlplane")
	response, err := client.Do(request)
	if err != nil {
		return identity.ExternalIdentity{}, fmt.Errorf("fetch github user: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return identity.ExternalIdentity{}, fmt.Errorf("github user endpoint returned %s", response.Status)
	}
	var user struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil {
		return identity.ExternalIdentity{}, fmt.Errorf("decode github user: %w", err)
	}
	if user.ID == 0 || user.Login == "" {
		return identity.ExternalIdentity{}, fmt.Errorf("github user response is missing identity")
	}
	primaryEmail, verifiedEmails, err := p.verifiedEmails(ctx, client)
	if err != nil {
		p.log.Warn("github verified email lookup failed", "error", err)
	}
	external := identity.ExternalIdentity{
		Provider:        "github",
		Subject:         strconv.FormatInt(user.ID, 10),
		DisplayName:     user.Login,
		ProfileImageURL: user.AvatarURL,
		Email:           user.Email,
		VerifiedEmails:  verifiedEmails,
	}
	if primaryEmail != "" {
		external.Email = primaryEmail
		external.EmailVerified = true
	} else {
		external.EmailVerified = external.Verifies(user.Email)
	}
	return external, nil
}

func (p *githubOAuthProvider) verifiedEmails(ctx context.Context, client *http.Client) (string, []string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userEmailsURL, nil)
	if err != nil {
		return "", nil, err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "helmr-controlplane")
	response, err := client.Do(request)
	if err != nil {
		return "", nil, fmt.Errorf("fetch github user emails: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", nil, fmt.Errorf("github user emails endpoint returned %s", response.Status)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(response.Body).Decode(&emails); err != nil {
		return "", nil, fmt.Errorf("decode github user emails: %w", err)
	}
	verified := make([]string, 0, len(emails))
	primary := ""
	for _, email := range emails {
		if !email.Verified || email.Email == "" {
			continue
		}
		verified = append(verified, email.Email)
		if email.Primary {
			primary = email.Email
		}
	}
	return primary, verified, nil
}
