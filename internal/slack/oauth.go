package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type OAuthClient struct {
	clientID, clientSecret string
	http                   *http.Client
}

func NewOAuthClient(clientID, clientSecret string, transport http.RoundTripper) (*OAuthClient, error) {
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("the Slack OAuth client credentials are required")
	}
	return &OAuthClient{clientID: clientID, clientSecret: clientSecret, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type oauthGrant struct {
	bundle                    credentialBundle
	app, team, bot, workspace string
	scopes                    []string
}

type oauthResult struct {
	grant      oauthGrant
	code       string
	retryAfter time.Duration
}

// exchange makes exactly one request. Only an explicit 429 and valid Retry-After
// permit repeating an exchange; all other failures require recovery/reauthorization.
func (c *OAuthClient) exchange(ctx context.Context, form url.Values) oauthResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/oauth.v2.access", strings.NewReader(form.Encode()))
	if err != nil {
		return oauthResult{code: "oauth_request_invalid"}
	}
	request.GetBody = nil
	request.SetBasicAuth(c.clientID, c.clientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	started := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		return oauthResult{code: "oauth_outcome_unknown"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		seconds, err := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 32)
		if err == nil && seconds > 0 {
			return oauthResult{code: "oauth_rate_limited", retryAfter: time.Duration(seconds) * time.Second}
		}
		return oauthResult{code: "oauth_outcome_unknown"}
	}
	if response.StatusCode != http.StatusOK {
		return oauthResult{code: "oauth_outcome_unknown"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return oauthResult{code: "oauth_outcome_unknown"}
	}
	body, err = jsoncanon.Transform(body)
	if err != nil {
		return oauthResult{code: "oauth_response_invalid"}
	}
	var result struct {
		OK           *bool  `json:"ok"`
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    *int64 `json:"expires_in"`
		TokenType    string `json:"token_type"`
		App          string `json:"app_id"`
		Bot          string `json:"bot_user_id"`
		Scope        string `json:"scope"`
		Enterprise   bool   `json:"is_enterprise_install"`
		Team         struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
	}
	if json.Unmarshal(body, &result) != nil || result.OK == nil {
		return oauthResult{code: "oauth_response_invalid"}
	}
	if !*result.OK {
		switch result.Error {
		case "invalid_refresh_token", "invalid_grant", "token_revoked", "invalid_auth":
			return oauthResult{code: "oauth_credential_rejected"}
		default:
			return oauthResult{code: "oauth_outcome_unknown"}
		}
	}
	bundle := credentialBundle{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken}
	if result.ExpiresIn != nil {
		if *result.ExpiresIn < 1 || *result.ExpiresIn > 86400 {
			return oauthResult{code: "oauth_response_invalid"}
		}
		expires := started.Add(time.Duration(*result.ExpiresIn) * time.Second)
		bundle.ExpiresAt = &expires
	}
	if result.TokenType != "bot" || result.Enterprise || !bundle.valid() {
		return oauthResult{code: "oauth_response_invalid"}
	}
	if form.Get("grant_type") == "refresh_token" && bundle.RefreshToken == "" {
		return oauthResult{code: "oauth_response_invalid"}
	}
	return oauthResult{grant: oauthGrant{bundle: bundle, app: result.App, bot: result.Bot, team: result.Team.ID, workspace: result.Team.Name, scopes: strings.FieldsFunc(result.Scope, func(r rune) bool { return r == ',' })}}
}
