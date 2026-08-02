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

type Control struct {
	Addr                    string
	DeploymentMode          string
	WorkerGroupID           string
	RegionID                string
	DefaultRegionID         string
	DatabaseURL             string
	RedisURL                string
	ClickHouseURL           string
	ClickHouseUser          string
	ClickHousePassword      string
	CASURI                  string
	BuildPolicyPath         string
	PlatformStoreURI        string
	WorkerTokenSigningKey   []byte
	WorkerGroupsJSON        string
	OperatorToken           string
	SetupToken              string
	AuthKey                 []byte
	EncryptionKey           []byte
	WorkspaceFencingKey     []byte
	TokenCredentialKey      []byte
	PublicURL               string
	MagicLinkDebugURLs      bool
	EmailProvider           string
	ResendAPIKey            string
	SMTPAddr                string
	SMTPUsername            string
	SMTPPassword            string
	EmailFrom               string
	GitHubOAuthClientID     string
	GitHubOAuthClientSecret string
	ScheduleJitter          time.Duration
	RunLeaseTTL             time.Duration
	RunFinalizationTTL      time.Duration
	ImageCache              *ImageCache
}

type Dispatcher struct {
	DatabaseURL           string
	RedisURL              string
	ClickHouseURL         string
	ClickHouseUser        string
	ClickHousePassword    string
	WorkspaceFencingKey   []byte
	RunPreparationLimit   int
	RunReservationTTL     time.Duration
	RunLeaseStartDeadline time.Duration
	RunLeaseTTL           time.Duration
	SchedulePollInterval  time.Duration
	ScheduleClaimLimit    int
	ScheduleConcurrency   int
	ScheduleClaimLease    time.Duration
}

type Database struct {
	URL string
}

type ClickHouse struct {
	URL      string
	User     string
	Password string
}

type WorkerGroupBootstrap struct {
	RegionID          string
	DefaultRegionID   string
	Provider          string
	ProviderRegion    string
	RegionDisplayName string
}

type Worker struct {
	ControlURL                   string
	WorkerGroupID                string
	WorkerResourceID             string
	WorkerEnrollmentSecretFile   string
	CASURI                       string
	WorkerInstanceCredentialPath string
	CheckpointKey                []byte
	BuildPolicyPath              string
	PlatformStoreURI             string
	WorkDir                      string
	BuildCacheDir                string
	BuildScratchDir              string
	ImagesDir                    string
	GitPath                      string
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
	WorkerBuildExecutors         int32
	WorkerRuntimeStarts          int32
	VMInitTimeout                time.Duration
	VMHealthTimeout              time.Duration
	VMHealthAttemptTimeout       time.Duration
	WorkspaceMountStartupTimeout time.Duration
	PreparedRuntimePoolSize      int
	PollEvery                    time.Duration
	ImageCache                   *ImageCache
}

// ImageCache is shared entry configuration for the Control provisioner and
// Worker credential adapter. It is either completely configured or absent.
type ImageCache struct {
	RegistryAuthority   string
	RepositoryPrefix    string
	CacheRoleARN        string
	RepositoryARNPrefix string
}

type WorkerControl struct {
	ControlURL                   string
	WorkerInstanceCredentialPath string
	WorkDir                      string
	PollEvery                    time.Duration
}

func LoadDatabase() (Database, error) {
	cfg := Database{URL: envString("HELMR_DATABASE_URL")}
	if cfg.URL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
	}
	return cfg, nil
}

func LoadClickHouse() (ClickHouse, error) {
	cfg := ClickHouse{
		URL:      envString("HELMR_CLICKHOUSE_URL"),
		User:     envString("HELMR_CLICKHOUSE_USER"),
		Password: envString("HELMR_CLICKHOUSE_PASSWORD"),
	}
	if cfg.URL == "" {
		return cfg, errors.New("HELMR_CLICKHOUSE_URL is required")
	}
	return cfg, nil
}

func loadImageCache() (*ImageCache, error) {
	config := ImageCache{
		RegistryAuthority:   envString("HELMR_IMAGE_CACHE_REGISTRY_AUTHORITY"),
		RepositoryPrefix:    envString("HELMR_IMAGE_CACHE_REPOSITORY_PREFIX"),
		CacheRoleARN:        envString("HELMR_IMAGE_CACHE_ROLE_ARN"),
		RepositoryARNPrefix: envString("HELMR_IMAGE_CACHE_REPOSITORY_ARN_PREFIX"),
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
		return nil, errors.New("HELMR_IMAGE_CACHE_REGISTRY_AUTHORITY, HELMR_IMAGE_CACHE_REPOSITORY_PREFIX, HELMR_IMAGE_CACHE_ROLE_ARN, and HELMR_IMAGE_CACHE_REPOSITORY_ARN_PREFIX must be configured together")
	}
	return &config, nil
}

func LoadWorkerGroupBootstrap() (WorkerGroupBootstrap, error) {
	regionID := envString("HELMR_REGION_ID")
	defaultRegionID := envString("HELMR_DEFAULT_REGION_ID")
	cfg := WorkerGroupBootstrap{
		RegionID:          regionID,
		DefaultRegionID:   defaultRegionID,
		Provider:          envString("HELMR_PROVIDER"),
		ProviderRegion:    envString("HELMR_PROVIDER_REGION"),
		RegionDisplayName: envString("HELMR_REGION_DISPLAY_NAME"),
	}
	if cfg.RegionID == "" {
		return cfg, errors.New("HELMR_REGION_ID is required")
	}
	if cfg.DefaultRegionID == "" {
		return cfg, errors.New("HELMR_DEFAULT_REGION_ID is required")
	}
	if cfg.Provider == "" {
		return cfg, errors.New("HELMR_PROVIDER is required")
	}
	if cfg.ProviderRegion == "" {
		return cfg, errors.New("HELMR_PROVIDER_REGION is required")
	}
	if cfg.RegionDisplayName == "" {
		cfg.RegionDisplayName = cfg.RegionID
	}
	return cfg, nil
}

func validatePublicURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("HELMR_PUBLIC_URL must be an absolute URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("HELMR_PUBLIC_URL must use http or https")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("HELMR_PUBLIC_URL must use https for non-loopback hosts")
	}
	return nil
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
	if value := envString(name); value != "" {
		return value
	}
	return fallback
}

func envString(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func envLower(name string) string {
	return strings.ToLower(envString(name))
}

func envInt64(name string, fallback int64) (int64, error) {
	value := envString(name)
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
	value := envString(name)
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
	value := envString(name)
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
	value := envString(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}
