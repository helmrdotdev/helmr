package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestSaveLoginPersistsConfigAndKeyring(t *testing.T) {
	temp := t.TempDir()
	keyring := newMemoryKeyring()
	store := NewStore(filepath.Join(temp, "helmr"), keyring)

	if err := store.SaveLogin(" https://helmr.example.test ", " session_token_test "); err != nil {
		t.Fatalf("SaveLogin() error = %v", err)
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DefaultHost != "https://helmr.example.test" {
		t.Fatalf("Load().DefaultHost = %q", cfg.DefaultHost)
	}

	token, err := store.Token(cfg.DefaultHost)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "session_token_test" {
		t.Fatalf("Token() = %q", token)
	}

	data, err := os.ReadFile(store.ConfigPath())
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if filepath.Base(store.ConfigPath()) != "config.toml" {
		t.Fatalf("config path = %s", store.ConfigPath())
	}
	if !strings.Contains(string(data), `default_host = 'https://helmr.example.test'`) && !strings.Contains(string(data), `default_host = "https://helmr.example.test"`) {
		t.Fatalf("config TOML = %q", data)
	}
	if strings.Contains(string(data), "session_token_test") {
		t.Fatalf("config unexpectedly contains CLI token")
	}
}

func TestSaveConfigPermissions(t *testing.T) {
	temp := t.TempDir()
	store := NewStore(filepath.Join(temp, "helmr"), newMemoryKeyring())

	if err := store.Save(Config{DefaultHost: "https://helmr.example.test"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	dirInfo, err := os.Stat(filepath.Dir(store.ConfigPath()))
	if err != nil {
		t.Fatalf("Stat(config dir) error = %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("config dir permissions = %o, want 700", got)
	}

	fileInfo, err := os.Stat(store.ConfigPath())
	if err != nil {
		t.Fatalf("Stat(config file) error = %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("config file permissions = %o, want 600", got)
	}
}

func TestLoadMissingConfig(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "helmr"), newMemoryKeyring())

	_, err := store.Load()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load() error = %v, want ErrNotFound", err)
	}
}

func TestDefaultConfigDirUsesOverride(t *testing.T) {
	t.Setenv(configDirEnv, filepath.Join(t.TempDir(), "custom"))
	t.Setenv(xdgConfigEnv, filepath.Join(t.TempDir(), "xdg"))

	dir, err := DefaultConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, "custom") {
		t.Fatalf("dir = %s", dir)
	}
}

func TestDefaultConfigDirUsesXDG(t *testing.T) {
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv(configDirEnv, "")
	t.Setenv(xdgConfigEnv, xdg)

	dir, err := DefaultConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(xdg, "helmr") {
		t.Fatalf("dir = %s", dir)
	}
}

func TestTokenMissing(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "helmr"), newMemoryKeyring())

	_, err := store.Token("https://helmr.example.test")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Token() error = %v, want ErrNotFound", err)
	}
}

func TestDeleteToken(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "helmr"), newMemoryKeyring())
	if err := store.SaveToken("https://helmr.example.test", "session-token"); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteToken("https://helmr.example.test"); err != nil {
		t.Fatalf("DeleteToken() error = %v", err)
	}
	if _, err := store.Token("https://helmr.example.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Token() after DeleteToken error = %v, want ErrNotFound", err)
	}
	if err := store.DeleteToken("https://helmr.example.test"); err != nil {
		t.Fatalf("DeleteToken() should ignore missing token: %v", err)
	}
}

type memoryKeyring struct {
	values map[string]string
}

func newMemoryKeyring() *memoryKeyring {
	return &memoryKeyring{values: map[string]string{}}
}

func (m *memoryKeyring) Set(service, user, password string) error {
	m.values[service+"\x00"+user] = password
	return nil
}

func (m *memoryKeyring) Get(service, user string) (string, error) {
	value, ok := m.values[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (m *memoryKeyring) Delete(service, user string) error {
	key := service + "\x00" + user
	if _, ok := m.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(m.values, key)
	return nil
}
