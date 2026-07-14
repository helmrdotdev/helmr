package config

import (
	"errors"
	"fmt"
	"net"
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
	WorkerTokenSigningKey   string
	WorkerGroupsJSON        string
	SetupToken              string
	AuthSecret              string
	SecretEncryptionKey     string
	SecretEncryptionKeyOld  string
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
	RuntimePrepareTarget    int
	RuntimePrepareLimit     int
}

type Dispatcher struct {
	WorkerFleets               []WorkerFleet
	FleetMetricsNamespace      string
	DatabaseURL                string
	RedisURL                   string
	WorkerGroupID              string
	ClickHouseURL              string
	ClickHouseUser             string
	ClickHousePassword         string
	AuthSecret                 string
	SecretEncryptionKey        string
	SecretEncryptionKeyOld     string
	PublicURL                  string
	EmailProvider              string
	ResendAPIKey               string
	SMTPAddr                   string
	SMTPUsername               string
	SMTPPassword               string
	EmailFrom                  string
	ScheduleRepairEvery        time.Duration
	ScheduleRepairLimit        int
	ScheduleTriggerConcurrency int
	ScheduleRepairLookahead    time.Duration
	ScheduleLease              time.Duration
	ScheduleMaxAttempts        int
	ScheduleJitter             time.Duration
	RuntimePrepareTarget       int
	RuntimePrepareLimit        int
	RuntimePrepareEvery        time.Duration
}

type WorkerFleet struct {
	GroupID               string
	Role                  string
	ASGName               string
	CompatibilityKeys     []string
	MilliCPU              uint64
	MemoryBytes           uint64
	WorkloadDiskBytes     uint64
	ScratchBytes          uint64
	BuildCacheBytes       uint64
	ArtifactCacheBytes    uint64
	VMSlots               uint64
	BuildExecutors        uint64
	QueuedRunScratchBytes uint64
	MinWorkers            int
	WarmWorkers           int
	MaxWorkers            int
	MaxScaleOutPerCycle   int
	MaxPending            int
	MaxPackingItems       int
	ScaleOutCooldown      time.Duration
	ScaleInCooldown       time.Duration
	ScaleInHysteresis     time.Duration
	StaleWorkerTimeout    time.Duration
	ReadinessTimeout      time.Duration
	DrainTimeout          time.Duration
	EmergencyStop         bool
	ControllerInterval    time.Duration
	MetricsInterval       time.Duration
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
	CASURI                       string
	WorkerInstanceCredentialPath string
	CheckpointKey                string
	WorkerProviderRegion         string
	WorkerLabels                 map[string]string
	WorkDir                      string
	ImagesDir                    string
	GitPath                      string
	BuildKitAddr                 string
	BuildKitCacheNS              string
	FirecrackerPath              string
	JailerPath                   string
	JailerUID                    int
	JailerGID                    int
	JailerNumaNode               int
	JailerChrootDir              string
	CgroupVersion                string
	CNINetworkName               string
	CNIProfile                   string
	CNIConfDir                   string
	CNIBinDir                    string
	CNICacheDir                  string
	IPPath                       string
	NFTPath                      string
	NetworkBlockedIPv4CIDRs      []string
	NetworkBlockedIPv6CIDRs      []string
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
	WorkerCertificationTTL       time.Duration
	VMHealthTimeout              time.Duration
	VMHealthAttemptTimeout       time.Duration
	WorkspaceMountStartupTimeout time.Duration
	PreparedRuntimePoolSize      int
	PollEvery                    time.Duration
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

func envList(name string) []string {
	value := envString(name)
	if value == "" {
		return nil
	}
	if strings.EqualFold(value, "none") {
		return []string{}
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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
