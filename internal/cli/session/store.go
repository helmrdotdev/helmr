package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/zalando/go-keyring"
)

const (
	appName        = "helmr"
	configFileName = "config.toml"
	configDirEnv   = "HELMR_CONFIG_DIR"
	xdgConfigEnv   = "XDG_CONFIG_HOME"
	keyringService = "helmr-cli"
)

// ErrNotFound reports that the requested CLI session state is missing.
var ErrNotFound = errors.New("helmr CLI session not found")

// Config is the non-secret CLI config stored on disk.
type Config struct {
	DefaultHost string `toml:"default_host,omitempty"`
}

// Store reads and writes CLI session config and credentials.
type Store struct {
	configDir string
	keyring   Keyring
}

// Keyring is the subset of OS keyring behavior used by Store.
type Keyring interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (osKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (osKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

func New() (*Store, error) {
	configDir, err := DefaultConfigDir()
	if err != nil {
		return nil, err
	}
	return NewStore(configDir, osKeyring{}), nil
}

func DefaultConfigDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(configDirEnv)); dir != "" {
		return dir, nil
	}
	if dir := strings.TrimSpace(os.Getenv(xdgConfigEnv)); dir != "" {
		return filepath.Join(dir, appName), nil
	}
	if runtime.GOOS == "windows" {
		if dir := strings.TrimSpace(os.Getenv("AppData")); dir != "" {
			return filepath.Join(dir, appName), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home dir: %w", err)
	}
	return filepath.Join(home, ".config", appName), nil
}

func NewStore(configDir string, keyring Keyring) *Store {
	if keyring == nil {
		keyring = osKeyring{}
	}
	return &Store{configDir: configDir, keyring: keyring}
}

func (s *Store) ConfigPath() string {
	return filepath.Join(s.configDir, configFileName)
}

func (s *Store) Load() (Config, error) {
	data, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, ErrNotFound
		}
		return Config{}, fmt.Errorf("read CLI config: %w", err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse CLI config: %w", err)
	}
	cfg.DefaultHost = strings.TrimSpace(cfg.DefaultHost)
	if cfg.DefaultHost == "" {
		return Config{}, ErrNotFound
	}
	return cfg, nil
}

func (s *Store) Save(cfg Config) error {
	cfg.DefaultHost = strings.TrimSpace(cfg.DefaultHost)
	if cfg.DefaultHost == "" {
		return errors.New("default host is required")
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode CLI config: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(s.ConfigPath(), data)
}

func (s *Store) Token(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", errors.New("base URL is required")
	}
	token, err := s.keyring.Get(keyringService, baseURL)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read CLI token from keyring: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", ErrNotFound
	}
	return token, nil
}

func (s *Store) SaveToken(baseURL, token string) error {
	baseURL = strings.TrimSpace(baseURL)
	token = strings.TrimSpace(token)
	if baseURL == "" {
		return errors.New("base URL is required")
	}
	if token == "" {
		return errors.New("CLI token is required")
	}
	if err := s.keyring.Set(keyringService, baseURL, token); err != nil {
		return fmt.Errorf("save CLI token to keyring: %w", err)
	}
	return nil
}

func (s *Store) DeleteToken(baseURL string) error {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return errors.New("base URL is required")
	}
	if err := s.keyring.Delete(keyringService, baseURL); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete CLI token from keyring: %w", err)
	}
	return nil
}

func (s *Store) SaveLogin(baseURL, token string) error {
	if err := s.SaveToken(baseURL, token); err != nil {
		return err
	}
	return s.Save(Config{DefaultHost: baseURL})
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create CLI config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set CLI config dir permissions: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary CLI config: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("set temporary CLI config permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary CLI config: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary CLI config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary CLI config: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace CLI config: %w", err)
	}
	_ = syncDir(dir)
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
