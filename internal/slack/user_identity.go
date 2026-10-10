package slack

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var ErrUserIdentity = errors.New("the Slack identity could not be verified; start linking again")

type UserIdentity struct {
	TeamID      string `json:"team_id"`
	SlackUserID string `json:"slack_user_id"`
	Name        string `json:"name"`
}

// Identity authorization is a separate OpenID flow, never an installation grant
// or Helmr sign-in. Email is neither requested nor used to associate accounts.
func (c *OAuthClient) IdentityAuthorizationURL(state, nonce, team, redirect string) string {
	query := url.Values{"response_type": {"code"}, "response_mode": {"form_post"}, "client_id": {c.clientID}, "scope": {"openid profile"}, "state": {state}, "nonce": {nonce}, "team": {team}, "redirect_uri": {redirect}}
	return "https://slack.com/openid/connect/authorize?" + query.Encode()
}

type slackIdentityClaims struct {
	jwt.RegisteredClaims
	Nonce string `json:"nonce"`
	Team  string `json:"https://slack.com/team_id"`
	User  string `json:"https://slack.com/user_id"`
	Name  string `json:"name"`
}

// VerifyIdentity verifies a one-use code's ID token using Slack's fixed key
// endpoint and the existing JWT implementation. Tokens are not stored, returned
// to the browser, or used for later Slack operations.
func (c *OAuthClient) VerifyIdentity(ctx context.Context, code, redirect, nonce, team string) (UserIdentity, error) {
	fail := func() (UserIdentity, error) { return UserIdentity{}, ErrUserIdentity }
	if code == "" || len(code) > 4096 || nonce == "" || team == "" || redirect == "" {
		return fail()
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/openid.connect.token", strings.NewReader(form.Encode()))
	if err != nil {
		return fail()
	}
	request.GetBody = nil
	request.SetBasicAuth(c.clientID, c.clientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(request)
	if err != nil {
		return fail()
	}
	body, err := readIdentityResponse(response)
	if err != nil {
		return fail()
	}
	var tokenResponse struct {
		OK      bool   `json:"ok"`
		IDToken string `json:"id_token"`
	}
	if json.Unmarshal(body, &tokenResponse) != nil || !tokenResponse.OK || len(tokenResponse.IDToken) > 16384 || tokenResponse.IDToken == "" {
		return fail()
	}
	keyRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/openid/connect/keys", nil)
	if err != nil {
		return fail()
	}
	keyResponse, err := c.http.Do(keyRequest)
	if err != nil {
		return fail()
	}
	keyBody, err := readIdentityResponse(keyResponse)
	if err != nil {
		return fail()
	}
	var keys struct {
		Keys []struct {
			ID        string `json:"kid"`
			Type      string `json:"kty"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			N         string `json:"n"`
			E         string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(keyBody, &keys) != nil || len(keys.Keys) == 0 || len(keys.Keys) > 32 {
		return fail()
	}
	claims := &slackIdentityClaims{}
	token, err := jwt.ParseWithClaims(tokenResponse.IDToken, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrUserIdentity
		}
		var matched *rsa.PublicKey
		for _, key := range keys.Keys {
			if key.ID != kid {
				continue
			}
			if matched != nil || key.Type != "RSA" || (key.Use != "" && key.Use != "sig") || (key.Algorithm != "" && key.Algorithm != "RS256") {
				return nil, ErrUserIdentity
			}
			n, err := base64.RawURLEncoding.DecodeString(key.N)
			if err != nil || len(n) < 256 || len(n) > 1024 {
				return nil, ErrUserIdentity
			}
			e, err := base64.RawURLEncoding.DecodeString(key.E)
			if err != nil || len(e) == 0 || len(e) > 4 {
				return nil, ErrUserIdentity
			}
			exponent := new(big.Int).SetBytes(e).Int64()
			if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
				return nil, ErrUserIdentity
			}
			matched = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
		}
		if matched == nil {
			return nil, ErrUserIdentity
		}
		return matched, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("https://slack.com"), jwt.WithAudience(c.clientID), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || claims.IssuedAt == nil || len(claims.Audience) != 1 || claims.Team != team || claims.User == "" || len(claims.User) > 100 || claims.Subject == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return fail()
	}
	// The OpenID subject is opaque; Slack documents its workspace identity in
	// the namespaced claims, which need not equal the subject.
	name := claims.Name
	if len(name) > 256 {
		name = ""
	}
	return UserIdentity{TeamID: claims.Team, SlackUserID: claims.User, Name: name}, nil
}
func readIdentityResponse(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrUserIdentity
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return nil, ErrUserIdentity
	}
	body, err = jsoncanon.Transform(body)
	if err != nil {
		return nil, ErrUserIdentity
	}
	return body, nil
}
