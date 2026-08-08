package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPublicURL = "https://helmr.dev"

const (
	DeploymentModeSelfHosted   = "self-hosted"
	DeploymentModeManagedCloud = "managed-cloud"
)

const (
	EmailProviderNone   = "none"
	EmailProviderLog    = "log"
	EmailProviderSMTP   = "smtp"
	EmailProviderResend = "resend"
)

type ControlPlane struct {
	Addr                    string
	DeploymentMode          string
	DatabaseURL             string
	RedisURL                string
	ClickHouseURL           string
	ClickHouseUser          string
	ClickHousePassword      string
	CASURI                  string
	BuildPolicyPath         string
	PlatformStoreURI        string
	WorkerTokenSigningKey   []byte
	Bootstrap               Bootstrap
	CapacityToken           string
	SetupToken              string
	AuthKey                 []byte
	EncryptionKey           []byte
	WorkspaceFencingKey     []byte
	TokenCredentialKey      []byte
	PublicURL               string
	APIOrigin               string
	MagicLinkDebugURLs      bool
	AdminEmails             []string
	EmailProvider           string
	ResendAPIKey            string
	SMTPAddr                string
	SMTPUsername            string
	SMTPPassword            string
	EmailFrom               string
	GitHubOAuthClientID     string
	GitHubOAuthClientSecret string
	ImageCache              *ImageCache
}

type Bootstrap struct {
	Enabled           bool
	RegionID          string
	RegionDisplayName string
	RegionLocation    string
	WorkerGroupName   string
	WorkerToken       string
}

type Dispatcher struct {
	DatabaseURL         string
	RedisURL            string
	ClickHouseURL       string
	ClickHouseUser      string
	ClickHousePassword  string
	WorkspaceFencingKey []byte
}

type Database struct {
	URL string
}

type ClickHouse struct {
	URL      string
	User     string
	Password string
}

type Worker struct {
	ControlPlaneURL              string
	WorkerResourceID             string
	WorkerEnrollmentTokenFile    string
	CASURI                       string
	WorkerInstanceCredentialPath string
	CheckpointKey                []byte
	BuildPolicyPath              string
	PlatformStoreURI             string
	WorkDir                      string
	BuildCacheDir                string
	BuildScratchDir              string
	ImagesDir                    string
	FirecrackerPath              string
	JailerPath                   string
	JailerUID                    int
	JailerGID                    int
	JailerNumaNode               int
	JailerChrootDir              string
	CgroupVersion                string
	NetworkLinkPool              string
	NetworkTranslationPool       string
	NetworkResolverIPv4          string
	NetworkBlockedIPv4CIDRs      []netip.Prefix
	IPPath                       string
	NFTPath                      string
	VMVCPUCount                  int64
	VMMemoryMiB                  int64
	VMScratchDiskMiB             int64
	WorkerCapacityVCPUs          int64
	WorkerCapacityMemoryMiB      int64
	WorkerDiskMiB                int64
	WorkerDiskReserveMiB         int64
	SubstrateCacheMaxMiB         int64
	ArtifactCacheMaxMiB          int64
	WorkerExecutionSlots         int32
	WorkerRoles                  []string
	VMInitTimeout                time.Duration
	VMHealthTimeout              time.Duration
	PollEvery                    time.Duration
	ImageCache                   *ImageCache
}

// ImageCache is shared entry configuration for the Control Plane provisioner and
// Worker credential adapter. It is either completely configured or absent.
type ImageCache struct {
	RegistryAuthority   string
	RepositoryPrefix    string
	CacheRoleARN        string
	RepositoryARNPrefix string
}

type WorkerControlPlane struct {
	ControlPlaneURL              string
	WorkerInstanceCredentialPath string
	WorkDir                      string
	PollEvery                    time.Duration
}

func LoadDatabase() (Database, error) {
	cfg := Database{URL: envText("DATABASE_URL")}
	if cfg.URL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}

func LoadClickHouse() (ClickHouse, error) {
	cfg := ClickHouse{
		URL:      envText("CLICKHOUSE_URL"),
		User:     envText("CLICKHOUSE_USER"),
		Password: envSecret("CLICKHOUSE_PASSWORD"),
	}
	if cfg.URL == "" {
		return cfg, errors.New("CLICKHOUSE_URL is required")
	}
	return cfg, nil
}

func loadImageCache() (*ImageCache, error) {
	config := ImageCache{
		RegistryAuthority:   envText("IMAGE_CACHE_REGISTRY_AUTHORITY"),
		RepositoryPrefix:    envText("IMAGE_CACHE_REPOSITORY_PREFIX"),
		CacheRoleARN:        envText("IMAGE_CACHE_ROLE_ARN"),
		RepositoryARNPrefix: envText("IMAGE_CACHE_REPOSITORY_ARN_PREFIX"),
	}
	values := []string{
		config.RegistryAuthority, config.RepositoryPrefix,
		config.CacheRoleARN, config.RepositoryARNPrefix,
	}
	configured := 0
	for _, value := range values {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != len(values) {
		return nil, errors.New("IMAGE_CACHE_REGISTRY_AUTHORITY, IMAGE_CACHE_REPOSITORY_PREFIX, IMAGE_CACHE_ROLE_ARN, and IMAGE_CACHE_REPOSITORY_ARN_PREFIX must be configured together")
	}
	return &config, nil
}

func normalizeOrigin(name string, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute URL", name)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("%s must use http or https", name)
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("%s must use https for non-loopback hosts", name)
	}
	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must be an origin without credentials, path, query, or fragment", name)
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func env(name, fallback string) string {
	if value := envText(name); value != "" {
		return value
	}
	return fallback
}

func envText(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func envSecret(name string) string {
	return os.Getenv(name)
}

func envLower(name string) string {
	return strings.ToLower(envText(name))
}

func envInt64(name string, fallback int64) (int64, error) {
	value := envText(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := envText(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := envText(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value := envText(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}
