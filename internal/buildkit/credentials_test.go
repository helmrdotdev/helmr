package buildkit

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/moby/buildkit/session/auth"
	"golang.org/x/crypto/nacl/sign"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRegistryCredentialsAreExactAuthorityAndCleared(t *testing.T) {
	password := []byte("private-token")
	provider := &registryCredentials{values: map[string]registryCredential{
		"docker.io": {username: "user", password: append([]byte(nil), password...)},
	}}
	auth, err := provider.authConfig(t.Context(), "registry-1.docker.io", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Username != "user" || auth.Password != string(password) {
		t.Fatalf("auth = %#v", auth)
	}
	redirected, err := provider.authConfig(t.Context(), "ghcr.io", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if redirected.Username != "" || redirected.Password != "" {
		t.Fatalf("cross-authority auth = %#v", redirected)
	}
	owned := provider.values["docker.io"].password
	provider.close()
	if len(provider.values) != 0 || !bytes.Equal(owned, make([]byte, len(owned))) {
		t.Fatal("owned registry password was not cleared")
	}
}

func TestNewWithRegistryCredentialsCopiesCallerBytes(t *testing.T) {
	password := []byte("private-token")
	_, clearProvider, err := NewWithRegistryCredentials(nil, "", []imagebuild.RegistryCredentialValue{{
		Authority: "docker.io",
		Username:  "user",
		Password:  password,
	}})
	if err != nil {
		t.Fatal(err)
	}
	clearProvider()
	if string(password) != "private-token" {
		t.Fatal("provider mutated caller-owned password bytes")
	}
}

func TestRegistryAuthProviderRejectsCredentialBearingCrossAuthorityRealm(t *testing.T) {
	delegate := &recordingAuthServer{}
	provider := &registryAuthProvider{
		delegate: delegate,
		credentials: &registryCredentials{values: map[string]registryCredential{
			"ghcr.io": {username: "user", password: []byte("private-token")},
		}},
	}
	_, err := provider.FetchToken(t.Context(), &auth.FetchTokenRequest{
		Host:  "ghcr.io",
		Realm: "https://tokens.example.com/oauth/token",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("FetchToken error = %v, want permission denied", err)
	}
	if delegate.fetchTokenCalls != 0 {
		t.Fatal("cross-authority credential request reached the upstream auth provider")
	}
}

func TestRegistryAuthProviderAllowsSameAuthorityAndAnonymousCrossAuthorityRealms(t *testing.T) {
	delegate := &recordingAuthServer{}
	authenticatedFetches := 0
	provider := &registryAuthProvider{
		delegate: delegate,
		credentials: &registryCredentials{values: map[string]registryCredential{
			"ghcr.io": {username: "user", password: []byte("private-token")},
		}},
		fetchAuthenticated: func(
			context.Context,
			*auth.FetchTokenRequest,
			string,
			registryCredential,
		) (*auth.FetchTokenResponse, error) {
			authenticatedFetches++
			return &auth.FetchTokenResponse{}, nil
		},
	}
	for _, request := range []*auth.FetchTokenRequest{
		{Host: "ghcr.io", Realm: "https://ghcr.io/token"},
		{Host: "registry-1.docker.io", Realm: "https://auth.docker.io/token"},
	} {
		if _, err := provider.FetchToken(t.Context(), request); err != nil {
			t.Fatalf("FetchToken(%q, %q): %v", request.Host, request.Realm, err)
		}
	}
	if authenticatedFetches != 1 || delegate.fetchTokenCalls != 1 {
		t.Fatalf("authenticated fetches = %d, delegated anonymous fetches = %d", authenticatedFetches, delegate.fetchTokenCalls)
	}
}

func TestRegistryAuthProviderRejectsNonHTTPSRealm(t *testing.T) {
	provider := &registryAuthProvider{
		delegate:    &recordingAuthServer{},
		credentials: &registryCredentials{values: map[string]registryCredential{}},
	}
	_, err := provider.FetchToken(t.Context(), &auth.FetchTokenRequest{
		Host:  "ghcr.io",
		Realm: "http://ghcr.io/token",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("FetchToken error = %v, want permission denied", err)
	}
}

func TestCredentialBearingTokenRedirectStaysOnHTTPSRegistryAuthority(t *testing.T) {
	check := boundTokenRedirect("ghcr.io")
	for raw, wantError := range map[string]bool{
		"https://ghcr.io/oauth/token":      false,
		"https://tokens.example.com/token": true,
		"http://ghcr.io/oauth/token":       true,
	} {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := check(request, nil); (err != nil) != wantError {
			t.Fatalf("redirect %q error = %v, wantError = %t", raw, err, wantError)
		}
	}
}

func TestRegistryAuthProviderUsesAttemptLocalInMemoryTokenAuthority(t *testing.T) {
	provider := &registryAuthProvider{tokenSeed: bytes.Repeat([]byte{7}, 32)}
	salt := []byte("daemon-salt")
	public, err := provider.GetTokenAuthority(t.Context(), &auth.GetTokenAuthorityRequest{
		Host: "ghcr.io",
		Salt: salt,
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := provider.VerifyTokenAuthority(t.Context(), &auth.VerifyTokenAuthorityRequest{
		Host:    "ghcr.io",
		Salt:    salt,
		Payload: []byte("challenge"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var publicKey [32]byte
	copy(publicKey[:], public.PublicKey)
	message, ok := sign.Open(nil, signed.Signed, &publicKey)
	if !ok || string(message) != "challenge" {
		t.Fatal("token authority signature did not verify")
	}
	ownedSeed := provider.tokenSeed
	provider.close()
	if !bytes.Equal(ownedSeed, make([]byte, len(ownedSeed))) {
		t.Fatal("token authority seed was not cleared")
	}
	if _, err := provider.GetTokenAuthority(t.Context(), &auth.GetTokenAuthorityRequest{Host: "ghcr.io", Salt: salt}); status.Code(err) != codes.Unavailable {
		t.Fatalf("closed token authority error = %v", err)
	}
}

type recordingAuthServer struct {
	auth.UnimplementedAuthServer
	fetchTokenCalls int
}

func (server *recordingAuthServer) FetchToken(
	context.Context,
	*auth.FetchTokenRequest,
) (*auth.FetchTokenResponse, error) {
	server.fetchTokenCalls++
	return &auth.FetchTokenResponse{}, nil
}

func (server *recordingAuthServer) Credentials(
	context.Context,
	*auth.CredentialsRequest,
) (*auth.CredentialsResponse, error) {
	return nil, errors.New("not implemented in test")
}
