package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// requestedMaxAge makes IdPs that honor max_age return auth_time. Helmr's own
// browser SSO proof lifetime is enforced separately.
const requestedMaxAge = 30 * 24 * time.Hour

// clockSkew bounds how far an IdP authentication time may lie in the future.
const clockSkew = 5 * time.Minute

var ErrOIDCToken = errors.New("the identity provider token is not valid")

// OIDC is Helmr's relying party for one connection.
type OIDC struct {
	issuer   string
	config   oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// NewOIDC discovers the issuer through client. The issuer in the discovery
// document must equal the configured issuer exactly.
func NewOIDC(ctx context.Context, client *http.Client, issuer, clientID, clientSecret, redirectURL string) (*OIDC, error) {
	if u, err := url.Parse(issuer); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the OIDC issuer must be an HTTPS URL")
	}
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("the OIDC client ID and secret are required")
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	return &OIDC{
		issuer: issuer,
		config: oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: provider.Endpoint(),
			RedirectURL: redirectURL, Scopes: []string{oidc.ScopeOpenID, "email", "profile"}},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		client:   client,
	}, nil
}

// AuthURL starts a sign-in. reauthenticate asks the IdP to authenticate the
// person again even when it has a session.
func (o *OIDC) AuthURL(state, nonce, pkceVerifier string, reauthenticate bool) string {
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier)}
	if reauthenticate {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	} else {
		opts = append(opts, oauth2.SetAuthURLParam("max_age", strconv.Itoa(int(requestedMaxAge.Seconds()))))
	}
	return o.config.AuthCodeURL(state, opts...)
}

// Exchange redeems a callback code and validates its ID token. go-oidc checks
// the signature, audience and expiry; Helmr checks the exact issuer (go-oidc
// also accepts Google's scheme-less issuer), the nonce and the authorized
// party. The proof time is auth_time when present, otherwise the token's iat
// (Entra omits auth_time unless configured).
func (o *OIDC) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, error) {
	ctx = oidc.ClientContext(ctx, o.client)
	token, err := o.config.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: code exchange failed: %v", ErrOIDCToken, err)
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, fmt.Errorf("%w: no ID token", ErrOIDCToken)
	}
	return o.verify(ctx, raw, nonce)
}

func (o *OIDC) verify(ctx context.Context, raw, nonce string) (Identity, error) {
	id, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrOIDCToken, err)
	}
	if nonce == "" || id.Nonce != nonce {
		return Identity{}, fmt.Errorf("%w: nonce mismatch", ErrOIDCToken)
	}
	if id.Issuer != o.issuer {
		return Identity{}, fmt.Errorf("%w: issuer mismatch", ErrOIDCToken)
	}
	var claims struct {
		AZP      string          `json:"azp"`
		AuthTime json.RawMessage `json:"auth_time"`
		Email    string          `json:"email"`
	}
	if err := id.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrOIDCToken, err)
	}
	if claims.AZP != "" && claims.AZP != o.config.ClientID {
		return Identity{}, fmt.Errorf("%w: issued to another client", ErrOIDCToken)
	}
	if claims.AZP == "" && len(id.Audience) > 1 {
		return Identity{}, fmt.Errorf("%w: multiple audiences without authorized party", ErrOIDCToken)
	}
	if strings.TrimSpace(id.Subject) == "" {
		return Identity{}, fmt.Errorf("%w: missing subject", ErrOIDCToken)
	}
	at := id.IssuedAt
	if len(claims.AuthTime) > 0 {
		var seconds int64
		if err := json.Unmarshal(claims.AuthTime, &seconds); err != nil || seconds <= 0 {
			return Identity{}, fmt.Errorf("%w: invalid auth_time", ErrOIDCToken)
		}
		at = time.Unix(seconds, 0)
	}
	if at.IsZero() || at.After(time.Now().Add(clockSkew)) {
		return Identity{}, fmt.Errorf("%w: authentication time is missing or in the future", ErrOIDCToken)
	}
	return Identity{Subject: id.Subject, Email: strings.TrimSpace(claims.Email), AuthenticatedAt: at.UTC()}, nil
}
