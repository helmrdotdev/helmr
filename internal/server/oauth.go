package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"golang.org/x/oauth2"
)

type githubOAuthProvider struct {
	config        oauth2.Config
	userURL       string
	userEmailsURL string
}

func newGitHubOAuthProvider(clientID string, clientSecret string, publicURL *url.URL) authProvider {
	redirect := publicURL.ResolveReference(&url.URL{Path: "/auth/github/callback"}).String()
	return &githubOAuthProvider{
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

func (p *githubOAuthProvider) Resolve(ctx context.Context, code string, verifier string) (authIdentity, error) {
	identity, _, err := p.ResolveWithToken(ctx, code, verifier)
	return identity, err
}

func (p *githubOAuthProvider) ResolveWithToken(ctx context.Context, code string, verifier string) (authIdentity, *oauth2.Token, error) {
	token, err := p.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return authIdentity{}, nil, fmt.Errorf("exchange github oauth code: %w", err)
	}
	client := p.config.Client(ctx, token)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userURL, nil)
	if err != nil {
		return authIdentity{}, nil, err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "helmr-control")
	response, err := client.Do(request)
	if err != nil {
		return authIdentity{}, nil, fmt.Errorf("fetch github user: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return authIdentity{}, nil, fmt.Errorf("github user endpoint returned %s", response.Status)
	}
	var user struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil {
		return authIdentity{}, nil, fmt.Errorf("decode github user: %w", err)
	}
	if user.ID == 0 || user.Login == "" {
		return authIdentity{}, nil, fmt.Errorf("github user response is missing identity")
	}
	primaryEmail, verifiedEmails, emailErr := p.verifiedEmails(ctx, client)
	email := user.Email
	emailVerified := containsNormalizedEmail(verifiedEmails, email)
	if primaryEmail != "" {
		email = primaryEmail
		emailVerified = true
	}
	claims, err := json.Marshal(map[string]any{"login": user.Login})
	if err != nil {
		return authIdentity{}, nil, err
	}
	return authIdentity{
		Provider:        "github",
		Subject:         strconv.FormatInt(user.ID, 10),
		DisplayName:     user.Login,
		ProfileImageURL: user.AvatarURL,
		Email:           email,
		EmailVerified:   emailVerified,
		VerifiedEmails:  verifiedEmails,
		EmailLookupErr:  errorString(emailErr),
		Claims:          claims,
	}, token, nil
}

func (p *githubOAuthProvider) verifiedEmails(ctx context.Context, client *http.Client) (string, []string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userEmailsURL, nil)
	if err != nil {
		return "", nil, err
	}
	request.Header.Set("accept", "application/vnd.github+json")
	request.Header.Set("user-agent", "helmr-control")
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

func containsNormalizedEmail(emails []string, target string) bool {
	target = normalizeEmailAddress(target)
	if target == "" {
		return false
	}
	for _, email := range emails {
		if normalizeEmailAddress(email) == target {
			return true
		}
	}
	return false
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
