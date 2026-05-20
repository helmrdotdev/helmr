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

type Control struct {
	Addr                    string
	DatabaseURL             string
	RedisURL                string
	CASURI                  string
	WorkerTokenSigningKey   string
	WorkerRegistrationToken string
	AuthSecret              string
	SecretEncryptionKey     string
	PublicURL               string
	SetupEnabled            bool
	BootstrapOwnerEmail     string
	MagicLinkDebugURLs      bool
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
	DatabaseURL string
	RedisURL    string
}

type Database struct {
	URL string
}

type Worker struct {
	ControlURL                  string
	CASURI                      string
	WorkerRegistrationToken     string
	WorkerRegistrationTokenPath string
	WorkerSecret                string
	WorkerCredentialPath        string
	CheckpointKey               string
	WorkerHostID                string
	WorkerExternalID            string
	WorkerRegion                string
	WorkerLabels                map[string]string
	WorkDir                     string
	ImagesDir                   string
	GitPath                     string
	BuildKitAddr                string
	BuildKitCacheNS             string
	FirecrackerPath             string
	JailerPath                  string
	JailerUID                   int
	JailerGID                   int
	JailerNumaNode              int
	JailerChrootDir             string
	CgroupVersion               string
	CNINetworkName              string
	CNIProfile                  string
	CNIConfDir                  string
	CNIBinDir                   string
	CNICacheDir                 string
	IPPath                      string
	NFTPath                     string
	NetworkBlockedIPv4CIDRs     []string
	NetworkBlockedIPv6CIDRs     []string
	VMVCPUCount                 int64
	VMMemoryMiB                 int64
	WorkerDiskMiB               int64
	VMHealthTimeout             time.Duration
	PollEvery                   time.Duration
}

type WorkerControl struct {
	ControlURL           string
	WorkerSecret         string
	WorkerCredentialPath string
	WorkerHostID         string
	WorkerExternalID     string
	WorkDir              string
	PollEvery            time.Duration
}

func LoadDatabase() (Database, error) {
	cfg := Database{URL: os.Getenv("HELMR_DATABASE_URL")}
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

func defaultSetupEnabled(publicURL string) bool {
	parsed, err := url.Parse(publicURL)
	if err != nil {
		return true
	}
	managed, err := url.Parse(DefaultPublicURL)
	if err != nil {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, managed.Scheme) || !strings.EqualFold(parsed.Host, managed.Host) {
		return true
	}
	return parsed.Path != "" && parsed.Path != "/"
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envList(name string) []string {
	value := strings.TrimSpace(os.Getenv(name))
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
	value := os.Getenv(name)
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
	value := os.Getenv(name)
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
	value := os.Getenv(name)
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
	value := os.Getenv(name)
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
