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
	DatabaseURL             string
	RedisURL                string
	AsyncBusURI             string
	CASURI                  string
	WorkerTokenSigningKey   string
	WorkerBootstrapToken    string
	SetupToken              string
	AuthSecret              string
	SecretEncryptionKey     string
	PublicURL               string
	MagicLinkDebugURLs      bool
	EmailProvider           string
	ResendAPIKey            string
	SMTPAddr                string
	SMTPUsername            string
	SMTPPassword            string
	EmailFrom               string
	GitHubAppID             string
	GitHubAppSlug           string
	GitHubAppPrivateKeyPath string
	GitHubAppPrivateKeyEnv  string
	GitHubWebhookSecret     string
	GitHubAppClientID       string
	GitHubAppClientSecret   string
}

type Dispatcher struct {
	DatabaseURL                    string
	RedisURL                       string
	AsyncBusURI                    string
	AuthSecret                     string
	SecretEncryptionKey            string
	PublicURL                      string
	EmailProvider                  string
	ResendAPIKey                   string
	SMTPAddr                       string
	SMTPUsername                   string
	SMTPPassword                   string
	EmailFrom                      string
	GitHubAppID                    string
	GitHubAppSlug                  string
	GitHubAppPrivateKeyPath        string
	GitHubAppPrivateKeyEnv         string
	ScheduleSweepEvery             time.Duration
	ScheduleSweepLimit             int
	ScheduleMaterializeConcurrency int
	ScheduleFireConcurrency        int
	ScheduleLease                  time.Duration
	ScheduleMaxAttempts            int
	ScheduleJitter                 time.Duration
}

type Database struct {
	URL string
}

type Worker struct {
	ControlURL                   string
	CASURI                       string
	WorkerBootstrapToken         string
	WorkerBootstrapTokenPath     string
	WorkerInstanceCredentialPath string
	CheckpointKey                string
	WorkerResourceID             string
	WorkerRegion                 string
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
	WorkerDiskMiB                int64
	VMHealthTimeout              time.Duration
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

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "worker"
	}
	return name
}
