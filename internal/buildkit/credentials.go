package buildkit

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	authutil "github.com/containerd/containerd/v2/core/remotes/docker/auth"
	remoteserrors "github.com/containerd/containerd/v2/core/remotes/errors"
	"github.com/docker/cli/cli/config/types"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/session/auth/authprovider"
	"golang.org/x/crypto/nacl/sign"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type registryCredentials struct {
	values map[string]registryCredential
}

type registryCredential struct {
	username string
	password []byte
}

func NewWithRegistryCredentials(
	client buildkitSolver,
	outputRoot string,
	credentials []imagebuild.RegistryCredentialValue,
) (*Builder, func(), error) {
	provider := &registryCredentials{values: make(map[string]registryCredential, len(credentials))}
	for _, credential := range credentials {
		provider.values[credential.Authority] = registryCredential{
			username: credential.Username,
			password: append([]byte(nil), credential.Password...),
		}
	}
	builder := New(client, outputRoot)
	delegate := authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: provider.authConfig,
	})
	authServer, ok := delegate.(auth.AuthServer)
	if !ok {
		panic("BuildKit Docker auth provider does not implement its auth server contract")
	}
	tokenSeed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(tokenSeed); err != nil {
		provider.close()
		return nil, nil, fmt.Errorf("create image-build token authority: %w", err)
	}
	sessionProvider := &registryAuthProvider{
		delegate:    authServer,
		credentials: provider,
		tokenSeed:   tokenSeed,
	}
	builder.sessions = []session.Attachable{sessionProvider}
	closeProvider := func() {
		sessionProvider.close()
		provider.close()
	}
	return builder, closeProvider, nil
}

type registryAuthProvider struct {
	auth.UnimplementedAuthServer
	delegate           auth.AuthServer
	credentials        *registryCredentials
	tokenSeed          []byte
	fetchAuthenticated func(context.Context, *auth.FetchTokenRequest, string, registryCredential) (*auth.FetchTokenResponse, error)
}

func (provider *registryAuthProvider) Register(server *grpc.Server) {
	auth.RegisterAuthServer(server, provider)
}

func (provider *registryAuthProvider) Credentials(
	ctx context.Context,
	request *auth.CredentialsRequest,
) (*auth.CredentialsResponse, error) {
	return provider.delegate.Credentials(ctx, request)
}

func (provider *registryAuthProvider) FetchToken(
	ctx context.Context,
	request *auth.FetchTokenRequest,
) (*auth.FetchTokenResponse, error) {
	registryAuthority, err := imagebuild.CanonicalRegistryAuthority(request.Host)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "registry token host is invalid")
	}
	realmAuthority, err := canonicalHTTPSRealmAuthority(request.Realm)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if _, authenticated := provider.credentials.values[registryAuthority]; authenticated && realmAuthority != registryAuthority {
		return nil, status.Error(codes.PermissionDenied, "credential-bearing registry token realm crosses authority")
	}
	if credential, authenticated := provider.credentials.values[registryAuthority]; authenticated {
		fetch := provider.fetchAuthenticated
		if fetch == nil {
			fetch = fetchBoundRegistryToken
		}
		return fetch(ctx, request, registryAuthority, credential)
	}
	return provider.delegate.FetchToken(ctx, request)
}

func fetchBoundRegistryToken(
	ctx context.Context,
	request *auth.FetchTokenRequest,
	registryAuthority string,
	credential registryCredential,
) (*auth.FetchTokenResponse, error) {
	client := cleanhttp.DefaultPooledClient()
	client.CheckRedirect = boundTokenRedirect(registryAuthority)
	options := authutil.TokenOptions{
		Realm:    request.Realm,
		Service:  request.Service,
		Scopes:   request.Scopes,
		Username: credential.username,
		Secret:   string(credential.password),
	}
	response, err := authutil.FetchTokenWithOAuth(ctx, client, nil, "buildkit-client", options)
	if err == nil {
		return tokenResponse(response.AccessToken, response.IssuedAt, response.ExpiresInSeconds), nil
	}
	var unexpected remoteserrors.ErrUnexpectedStatus
	if !errors.As(err, &unexpected) ||
		unexpected.StatusCode != http.StatusMethodNotAllowed &&
			unexpected.StatusCode != http.StatusNotFound &&
			unexpected.StatusCode != http.StatusUnauthorized {
		return nil, fmt.Errorf("fetch registry OAuth token: %w", err)
	}
	responseWithBasicAuth, err := authutil.FetchToken(ctx, client, nil, options)
	if err != nil {
		return nil, fmt.Errorf("fetch registry token: %w", err)
	}
	return tokenResponse(
		responseWithBasicAuth.Token,
		responseWithBasicAuth.IssuedAt,
		responseWithBasicAuth.ExpiresInSeconds,
	), nil
}

func boundTokenRedirect(registryAuthority string) func(*http.Request, []*http.Request) error {
	return func(redirect *http.Request, _ []*http.Request) error {
		authority, err := canonicalHTTPSRealmAuthority(redirect.URL.String())
		if err != nil || authority != registryAuthority {
			return errors.New("credential-bearing registry token redirect crosses authority")
		}
		return nil
	}
}

func tokenResponse(token string, issuedAt time.Time, expiresIn int) *auth.FetchTokenResponse {
	if expiresIn == 0 {
		expiresIn = 60
	}
	response := &auth.FetchTokenResponse{
		Token:     token,
		ExpiresIn: int64(expiresIn),
	}
	if !issuedAt.IsZero() {
		response.IssuedAt = issuedAt.Unix()
	}
	return response
}

func (provider *registryAuthProvider) GetTokenAuthority(
	_ context.Context,
	request *auth.GetTokenAuthorityRequest,
) (*auth.GetTokenAuthorityResponse, error) {
	privateKey, err := provider.tokenKey(request.Host, request.Salt)
	if err != nil {
		return nil, err
	}
	defer clear(privateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &auth.GetTokenAuthorityResponse{PublicKey: append([]byte(nil), publicKey...)}, nil
}

func (provider *registryAuthProvider) VerifyTokenAuthority(
	_ context.Context,
	request *auth.VerifyTokenAuthorityRequest,
) (*auth.VerifyTokenAuthorityResponse, error) {
	privateKey, err := provider.tokenKey(request.Host, request.Salt)
	if err != nil {
		return nil, err
	}
	defer clear(privateKey)
	key := new([ed25519.PrivateKeySize]byte)
	copy(key[:], privateKey)
	signed := sign.Sign(nil, request.Payload, key)
	clear(key[:])
	return &auth.VerifyTokenAuthorityResponse{Signed: signed}, nil
}

func (provider *registryAuthProvider) tokenKey(host string, salt []byte) (ed25519.PrivateKey, error) {
	authority, err := imagebuild.CanonicalRegistryAuthority(host)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "registry token host is invalid")
	}
	if len(provider.tokenSeed) != ed25519.SeedSize {
		return nil, status.Error(codes.Unavailable, "image-build token authority is closed")
	}
	mac := hmac.New(sha256.New, provider.tokenSeed)
	_, _ = mac.Write([]byte("helmr.image-build.token-authority.v0\x00"))
	_, _ = mac.Write([]byte(authority))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(salt)
	seed := mac.Sum(nil)
	privateKey := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return privateKey, nil
}

func (provider *registryAuthProvider) close() {
	clear(provider.tokenSeed)
	provider.tokenSeed = nil
}

func canonicalHTTPSRealmAuthority(raw string) (string, error) {
	realm, err := url.Parse(raw)
	if err != nil || realm.Scheme != "https" || realm.Host == "" || realm.User != nil || realm.Fragment != "" {
		return "", fmt.Errorf("registry token realm must be an HTTPS URL without user info or fragment")
	}
	if strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("registry token realm must not contain surrounding whitespace")
	}
	authority, err := imagebuild.CanonicalRegistryAuthority(realm.Host)
	if err != nil {
		return "", fmt.Errorf("registry token realm authority is invalid")
	}
	return authority, nil
}

func (credentials *registryCredentials) authConfig(
	_ context.Context,
	host string,
	_ []string,
	_ authprovider.ExpireCachedAuthCheck,
) (types.AuthConfig, error) {
	authority, err := imagebuild.CanonicalRegistryAuthority(host)
	if err != nil {
		return types.AuthConfig{}, nil
	}
	credential, ok := credentials.values[authority]
	if !ok {
		return types.AuthConfig{}, nil
	}
	return types.AuthConfig{
		ServerAddress: authority,
		Username:      credential.username,
		Password:      string(credential.password),
	}, nil
}

func (credentials *registryCredentials) close() {
	for authority, credential := range credentials.values {
		clear(credential.password)
		delete(credentials.values, authority)
	}
}
