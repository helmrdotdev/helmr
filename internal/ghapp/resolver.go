package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	ghapi "github.com/google/go-github/v75/github"
	"github.com/helmrdotdev/helmr/internal/api"
)

type ResolvedSource struct {
	Source             api.GitHubSource
	InstallationID     int64
	GitHubRepositoryID int64
}

type InstallationToken struct {
	Token     string
	ExpiresAt time.Time
}

type VerifiedInstallation struct {
	InstallationID      int64
	AccountLogin        string
	AccountType         string
	RepositorySelection string
	HTMLURL             string
	Suspended           bool
	Repositories        []Repository
}

type Repository struct {
	ID            int64
	OwnerLogin    string
	Name          string
	FullName      string
	Private       bool
	Archived      bool
	DefaultBranch string
	HTMLURL       string
}

type Resolver struct {
	appID      string
	appSlug    string
	privateKey *rsa.PrivateKey
	httpClient *http.Client
	baseURL    *url.URL
	now        func() time.Time
}

func NewResolver(appID string, appSlug string, privateKeyPEM []byte) (*Resolver, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return nil, errors.New("github app id is required")
	}
	appSlug = strings.TrimSpace(appSlug)
	if appSlug == "" {
		return nil, errors.New("github app slug is required")
	}
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		appID:      appID,
		appSlug:    appSlug,
		privateKey: privateKey,
		httpClient: http.DefaultClient,
		now:        time.Now,
	}, nil
}

func (r *Resolver) InstallURL() string {
	if r == nil || strings.TrimSpace(r.appSlug) == "" {
		return ""
	}
	return "https://github.com/apps/" + url.PathEscape(r.appSlug) + "/installations/new"
}

func (r *Resolver) VerifyUserInstallation(ctx context.Context, userAccessToken string, installationID int64) (VerifiedInstallation, error) {
	if r == nil {
		return VerifiedInstallation{}, errors.New("github resolver is not configured")
	}
	if strings.TrimSpace(userAccessToken) == "" {
		return VerifiedInstallation{}, errors.New("github user token is required")
	}
	if installationID <= 0 {
		return VerifiedInstallation{}, InvalidSourceError{Err: errors.New("github installation id is required")}
	}
	expectedAppID, err := parseAppID(r.appID)
	if err != nil {
		return VerifiedInstallation{}, err
	}
	client := r.githubClient(userAccessToken)
	opts := &ghapi.ListOptions{PerPage: 100}
	for {
		installations, response, err := client.Apps.ListUserInstallations(ctx, opts)
		if err != nil {
			return VerifiedInstallation{}, fmt.Errorf("list github user installations: %w", err)
		}
		for _, installation := range installations {
			if installation.GetID() != installationID {
				continue
			}
			if installation.GetAppID() != expectedAppID {
				return VerifiedInstallation{}, InvalidSourceError{Err: errors.New("github installation belongs to a different app")}
			}
			account := installation.GetAccount()
			verified := VerifiedInstallation{
				InstallationID:      installation.GetID(),
				AccountLogin:        account.GetLogin(),
				AccountType:         account.GetType(),
				RepositorySelection: installation.GetRepositorySelection(),
				HTMLURL:             installation.GetHTMLURL(),
				Suspended:           !installation.GetSuspendedAt().Time.IsZero(),
			}
			if strings.TrimSpace(verified.AccountLogin) == "" || strings.TrimSpace(verified.AccountType) == "" {
				return VerifiedInstallation{}, errors.New("github installation response is missing account")
			}
			repositories, err := listUserInstallationRepositories(ctx, client, installationID)
			if err != nil {
				return VerifiedInstallation{}, err
			}
			verified.Repositories = repositories
			return verified, nil
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}
	return VerifiedInstallation{}, InvalidSourceError{Err: fmt.Errorf("github installation %d is not accessible to this user", installationID)}
}

func listUserInstallationRepositories(ctx context.Context, client *ghapi.Client, installationID int64) ([]Repository, error) {
	opts := &ghapi.ListOptions{PerPage: 100}
	var repositories []Repository
	for {
		page, response, err := client.Apps.ListUserRepos(ctx, installationID, opts)
		if err != nil {
			return nil, fmt.Errorf("list github installation repositories: %w", err)
		}
		if page != nil {
			for _, repo := range page.Repositories {
				normalized, err := normalizeGitHubRepository(repo)
				if err != nil {
					return nil, err
				}
				repositories = append(repositories, normalized)
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}
	return repositories, nil
}

func normalizeGitHubRepository(repo *ghapi.Repository) (Repository, error) {
	if repo == nil || repo.GetID() <= 0 {
		return Repository{}, errors.New("github repository response is missing id")
	}
	ownerLogin := repo.GetOwner().GetLogin()
	name := repo.GetName()
	fullName := repo.GetFullName()
	if fullName == "" && ownerLogin != "" && name != "" {
		fullName = ownerLogin + "/" + name
	}
	if ownerLogin == "" {
		ownerLogin, _, _ = strings.Cut(fullName, "/")
	}
	if name == "" {
		_, name, _ = strings.Cut(fullName, "/")
	}
	if strings.TrimSpace(ownerLogin) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(fullName) == "" {
		return Repository{}, errors.New("github repository response is missing name")
	}
	return Repository{
		ID:            repo.GetID(),
		OwnerLogin:    ownerLogin,
		Name:          name,
		FullName:      fullName,
		Private:       repo.GetPrivate(),
		Archived:      repo.GetArchived(),
		DefaultBranch: repo.GetDefaultBranch(),
		HTMLURL:       repo.GetHTMLURL(),
	}, nil
}

func (r *Resolver) ResolveCommit(ctx context.Context, installationID int64, githubRepositoryID int64, source api.GitHubSource) (ResolvedSource, error) {
	normalized, err := NormalizeSource(source)
	if err != nil {
		return ResolvedSource{}, err
	}
	if installationID <= 0 {
		return ResolvedSource{}, InvalidSourceError{Err: errors.New("github installation id is required")}
	}
	if githubRepositoryID <= 0 {
		return ResolvedSource{}, InvalidSourceError{Err: errors.New("github repository id is required")}
	}
	owner, repo, _ := strings.Cut(normalized.Repository, "/")
	token, err := r.CreateRepositoryToken(ctx, installationID, githubRepositoryID)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("create github installation token for %s: %w", normalized.Repository, err)
	}
	client := r.githubClient(token.Token)
	enriched, err := enrichResolvedSource(ctx, client, owner, repo, normalized)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("resolve github ref %q for %s: %w", normalized.Ref, normalized.Repository, err)
	}
	sha, err := normalizeResolvedSHA(enriched.SHA)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("resolve github ref %q for %s: %w", normalized.Ref, normalized.Repository, err)
	}
	enriched.SHA = sha
	return ResolvedSource{Source: enriched, InstallationID: installationID, GitHubRepositoryID: githubRepositoryID}, nil
}

func (r *Resolver) CreateRepositoryToken(ctx context.Context, installationID int64, githubRepositoryID int64) (InstallationToken, error) {
	if r == nil {
		return InstallationToken{}, errors.New("github resolver is not configured")
	}
	if githubRepositoryID <= 0 {
		return InstallationToken{}, errors.New("github repository id is required")
	}
	appJWT, err := r.appJWT()
	if err != nil {
		return InstallationToken{}, err
	}
	token, _, err := r.githubClient(appJWT).Apps.CreateInstallationToken(ctx, installationID, &ghapi.InstallationTokenOptions{
		RepositoryIDs: []int64{githubRepositoryID},
		Permissions: &ghapi.InstallationPermissions{
			Contents: ghapi.Ptr("read"),
			Metadata: ghapi.Ptr("read"),
		},
	})
	if err != nil {
		return InstallationToken{}, err
	}
	value := strings.TrimSpace(token.GetToken())
	if value == "" {
		return InstallationToken{}, errors.New("github returned an empty installation token")
	}
	return InstallationToken{Token: value, ExpiresAt: token.GetExpiresAt().Time}, nil
}

func (r *Resolver) appJWT() (string, error) {
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	issuedAt := now().Add(-1 * time.Minute).Unix()
	expiresAt := now().Add(9 * time.Minute).Unix()
	return signJWT(r.privateKey, map[string]any{"alg": "RS256", "typ": "JWT"}, map[string]any{
		"iat": issuedAt,
		"exp": expiresAt,
		"iss": r.appID,
	})
}

func parseAppID(value string) (int64, error) {
	appID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || appID <= 0 {
		return 0, errors.New("github app id must be a positive integer")
	}
	return appID, nil
}

func (r *Resolver) githubClient(token string) *ghapi.Client {
	client := ghapi.NewClient(r.httpClient).WithAuthToken(token)
	if r.baseURL != nil {
		client.BaseURL = r.baseURL
	}
	return client
}

func resolveCommitSHA(ctx context.Context, client *ghapi.Client, owner string, repo string, ref string) (string, error) {
	commit, _, err := client.Repositories.GetCommit(ctx, owner, repo, ref, nil)
	if err == nil {
		return commit.GetSHA(), nil
	}
	if !IsNotFound(err) {
		return "", err
	}

	sha, fallbackErr := resolveGitRef(ctx, client, owner, repo, ref)
	if fallbackErr == nil {
		return sha, nil
	}
	return "", err
}

func resolveGitRef(ctx context.Context, client *ghapi.Client, owner string, repo string, ref string) (string, error) {
	var candidates []string
	if strings.HasPrefix(ref, "refs/") {
		candidates = []string{strings.TrimPrefix(ref, "refs/")}
	} else {
		candidates = []string{"heads/" + ref, "tags/" + ref}
	}
	var lastErr error
	for _, candidate := range candidates {
		reference, _, err := client.Git.GetRef(ctx, owner, repo, candidate)
		if err != nil {
			lastErr = err
			continue
		}
		object := reference.GetObject()
		switch object.GetType() {
		case "commit":
			return object.GetSHA(), nil
		case "tag":
			tag, _, err := client.Git.GetTag(ctx, owner, repo, object.GetSHA())
			if err != nil {
				return "", err
			}
			if tag.GetObject().GetType() != "commit" {
				return "", fmt.Errorf("git tag %q points to unsupported object type %q", candidate, tag.GetObject().GetType())
			}
			return tag.GetObject().GetSHA(), nil
		default:
			return "", fmt.Errorf("git ref %q points to unsupported object type %q", candidate, object.GetType())
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("git ref %q did not resolve", ref)
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github app private key must be PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app private key must be an RSA key")
	}
	return rsaKey, nil
}

func signJWT(privateKey *rsa.PrivateKey, header map[string]any, claims map[string]any) (string, error) {
	encodedHeader, err := encodeJWTPart(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeJWTPart(claims)
	if err != nil {
		return "", err
	}
	unsigned := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign github app jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func encodeJWTPart(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func IsNotFound(err error) bool {
	var githubError *ghapi.ErrorResponse
	return errors.As(err, &githubError) && githubError.Response != nil && githubError.Response.StatusCode == http.StatusNotFound
}

func IsTerminalAccessError(err error) bool {
	var githubError *ghapi.ErrorResponse
	if !errors.As(err, &githubError) || githubError.Response == nil {
		return false
	}
	switch githubError.Response.StatusCode {
	case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func isFullGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
