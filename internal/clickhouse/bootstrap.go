package clickhouse

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
)

// ReaderLimits bounds one historical query. Values are selected alongside the
// historical workload and shared by query settings and account provisioning.
type ReaderLimits struct {
	MaxRowsToRead  uint64
	MaxBytesToRead uint64
	MaxMemoryUsage uint64
}

// DefaultReaderLimits leaves headroom above the measured maximum historical
// page and million-row rare-filter workloads; it is not a Cloud capacity SLO.
func DefaultReaderLimits() ReaderLimits {
	return ReaderLimits{MaxRowsToRead: 10_000_000, MaxBytesToRead: 1 << 30, MaxMemoryUsage: 512 << 20}
}

type BootstrapCredential struct {
	User     string
	Password string
}

type BootstrapConfig struct {
	URL          string
	Admin        BootstrapCredential
	Reader       BootstrapCredential
	Ingester     BootstrapCredential
	Migration    BootstrapCredential
	ReaderLimits ReaderLimits
}

var bootstrapIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func (cfg BootstrapConfig) Validate() error {
	endpoint, err := url.Parse(cfg.URL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return errors.New("ClickHouse bootstrap requires an HTTP endpoint without embedded credentials, query or fragment")
	}
	if endpoint.Scheme == "http" && endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1" && endpoint.Hostname() != "localhost" {
		return errors.New("ClickHouse bootstrap requires HTTPS outside loopback")
	}
	users, passwords := map[string]bool{}, map[string]bool{}
	for index, identity := range []BootstrapCredential{cfg.Admin, cfg.Reader, cfg.Ingester, cfg.Migration} {
		if !bootstrapIdentifier.MatchString(identity.User) || identity.Password == "" {
			return errors.New("ClickHouse bootstrap requires explicit usernames and passwords for all four identities")
		}
		if index > 0 && identity.User == "default" {
			return errors.New("ClickHouse application identities must not use default")
		}
		if users[identity.User] || passwords[identity.Password] {
			return errors.New("ClickHouse bootstrap identities and passwords must be distinct")
		}
		users[identity.User], passwords[identity.Password] = true, true
	}
	if cfg.ReaderLimits.MaxRowsToRead == 0 || cfg.ReaderLimits.MaxBytesToRead == 0 || cfg.ReaderLimits.MaxMemoryUsage == 0 {
		return errors.New("ClickHouse reader scan and memory limits must be positive")
	}
	return nil
}

type bootstrapIdentity struct {
	credential BootstrapCredential
	privileges []string
	settings   string
}

// Bootstrap provisions only missing users and expected privileges. Existing
// credentials and privilege boundaries are checked for all users before writes.
// It never changes a password or removes a user, privilege or database.
func Bootstrap(ctx context.Context, admin *Client, cfg BootstrapConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	identities := []bootstrapIdentity{
		{cfg.Reader, []string{"SELECT"}, readerBootstrapSettings(cfg.ReaderLimits)},
		{cfg.Ingester, []string{"INSERT"}, ""},
		{cfg.Migration, []string{"CREATE DATABASE", "CREATE TABLE"}, ""},
	}
	exists := make([]bool, len(identities))
	for index, identity := range identities {
		found, err := preflightBootstrapIdentity(ctx, admin, cfg.URL, identity)
		if err != nil {
			return err
		}
		exists[index] = found
	}
	for index, identity := range identities {
		user := "`" + identity.credential.User + "`"
		if !exists[index] {
			hash := sha256.Sum256([]byte(identity.credential.Password))
			query := fmt.Sprintf("CREATE USER %s IDENTIFIED WITH sha256_hash BY '%x'", user, hash)
			if identity.settings != "" {
				query += " SETTINGS " + identity.settings
			}
			if err := admin.Exec(ctx, query); err != nil {
				return bootstrapError("create application user", err)
			}
		} else if identity.settings != "" {
			if err := admin.Exec(ctx, "ALTER USER "+user+" SETTINGS "+identity.settings); err != nil {
				return bootstrapError("set reader constraints", err)
			}
		}
		query := "GRANT " + strings.Join(identity.privileges, ", ") + " ON helmr_telemetry.* TO " + user
		if err := admin.Exec(ctx, query); err != nil {
			return bootstrapError("grant telemetry privileges", err)
		}
	}
	for _, identity := range identities {
		if _, err := preflightBootstrapIdentity(ctx, admin, cfg.URL, identity); err != nil {
			return err
		}
	}
	return verifyReaderConstraints(ctx, cfg)
}

func readerBootstrapSettings(limits ReaderLimits) string {
	return fmt.Sprintf("readonly=2 READONLY, max_execution_time=35 MIN 1 MAX 35, timeout_overflow_mode='throw' READONLY, timeout_before_checking_execution_speed=0 READONLY, max_rows_to_read=%d MIN 1 MAX %d, max_bytes_to_read=%d MIN 1 MAX %d, max_memory_usage=%d MIN 1 MAX %d, read_overflow_mode='throw' READONLY, result_overflow_mode='throw' READONLY", limits.MaxRowsToRead, limits.MaxRowsToRead, limits.MaxBytesToRead, limits.MaxBytesToRead, limits.MaxMemoryUsage, limits.MaxMemoryUsage)
}

type bootstrapGrant struct {
	Access      string  `ch:"access_type"`
	Database    *string `ch:"database"`
	Table       *string `ch:"table"`
	Column      *string `ch:"column"`
	Partial     uint8   `ch:"is_partial_revoke"`
	GrantOption uint8   `ch:"grant_option"`
}

func preflightBootstrapIdentity(ctx context.Context, admin *Client, endpoint string, identity bootstrapIdentity) (bool, error) {
	var profiles []struct {
		Name string `ch:"name"`
	}
	if err := admin.Select(ctx, &profiles, "SELECT name FROM system.settings_profiles WHERE (apply_to_all OR has(apply_to_list, ?)) AND NOT has(apply_to_except, ?) LIMIT 1", identity.credential.User, identity.credential.User); err != nil {
		return false, bootstrapError("inspect assigned application settings profiles", err)
	}
	if len(profiles) != 0 {
		return false, errors.New("ClickHouse application user has an externally assigned settings profile")
	}

	var users []struct {
		Name string   `ch:"name"`
		Auth []string `ch:"auth"`
	}
	if err := admin.Select(ctx, &users, "SELECT name, arrayMap(x -> toString(x), auth_type) AS auth FROM system.users WHERE name = ? LIMIT 1", identity.credential.User); err != nil {
		return false, bootstrapError("inspect application user", err)
	}
	if len(users) == 0 {
		return false, nil
	}
	if len(users[0].Auth) != 1 || users[0].Auth[0] != "sha256_password" {
		return false, errors.New("ClickHouse application user has unexpected authentication methods")
	}
	client, err := New(Config{URL: endpoint, User: identity.credential.User, Password: identity.credential.Password})
	if err != nil {
		return false, bootstrapError("configure application credential check", err)
	}
	defer client.Close()
	if err := client.Ping(ctx); err != nil {
		return false, bootstrapError("stored application credential does not authenticate existing user", err)
	}
	var roles []struct {
		Role string `ch:"granted_role_name"`
	}
	if err := admin.Select(ctx, &roles, "SELECT granted_role_name FROM system.role_grants WHERE user_name = ? LIMIT 1", identity.credential.User); err != nil {
		return false, bootstrapError("inspect application role membership", err)
	}
	if len(roles) != 0 {
		return false, errors.New("ClickHouse application user has unexpected role membership")
	}
	var grants []bootstrapGrant
	if err := admin.Select(ctx, &grants, "SELECT toString(access_type) AS access_type, database, table, column, is_partial_revoke, grant_option FROM system.grants WHERE user_name = ? LIMIT 100", identity.credential.User); err != nil {
		return false, bootstrapError("inspect application privileges", err)
	}
	if len(grants) >= 100 {
		return false, errors.New("ClickHouse application user has unexpected privileges")
	}
	for _, grant := range grants {
		allowed := false
		for _, privilege := range identity.privileges {
			if grant.Access == privilege {
				allowed = true
			}
		}
		if !allowed || grant.Database == nil || *grant.Database != "helmr_telemetry" || grant.Table != nil || grant.Column != nil || grant.Partial != 0 || grant.GrantOption != 0 {
			return false, errors.New("ClickHouse application user has unexpected privileges")
		}
	}
	var settings []struct {
		Name    *string `ch:"setting_name"`
		Inherit *string `ch:"inherit_profile"`
	}
	if err := admin.Select(ctx, &settings, "SELECT setting_name, inherit_profile FROM system.settings_profile_elements WHERE user_name = ? LIMIT 100", identity.credential.User); err != nil {
		return false, bootstrapError("inspect application settings", err)
	}
	if len(settings) >= 100 {
		return false, errors.New("ClickHouse application user has unexpected settings")
	}
	for _, setting := range settings {
		if setting.Inherit != nil || setting.Name == nil || identity.settings == "" || !readerBootstrapSettingNames[*setting.Name] {
			return false, errors.New("ClickHouse application user has unexpected settings or inherited profile")
		}
	}
	return true, nil
}

var readerBootstrapSettingNames = map[string]bool{
	"readonly": true, "max_execution_time": true, "timeout_overflow_mode": true,
	"timeout_before_checking_execution_speed": true, "max_rows_to_read": true,
	"max_bytes_to_read": true, "max_memory_usage": true, "read_overflow_mode": true,
	"result_overflow_mode": true,
}

func verifyReaderConstraints(ctx context.Context, cfg BootstrapConfig) error {
	client, err := New(Config{URL: cfg.URL, User: cfg.Reader.User, Password: cfg.Reader.Password})
	if err != nil {
		return bootstrapError("configure reader verification", err)
	}
	defer client.Close()
	expected := map[string]string{
		"readonly": "2", "max_execution_time": "35", "timeout_overflow_mode": "throw",
		"timeout_before_checking_execution_speed": "0", "read_overflow_mode": "throw",
		"result_overflow_mode": "throw",
		"max_rows_to_read":     fmt.Sprint(cfg.ReaderLimits.MaxRowsToRead),
		"max_bytes_to_read":    fmt.Sprint(cfg.ReaderLimits.MaxBytesToRead),
		"max_memory_usage":     fmt.Sprint(cfg.ReaderLimits.MaxMemoryUsage),
	}
	var settings []struct {
		Name     string  `ch:"name"`
		Value    string  `ch:"value"`
		Min      *string `ch:"min"`
		Max      *string `ch:"max"`
		Readonly uint8   `ch:"readonly"`
	}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.Select(queryCtx, &settings, "SELECT name, value, min, max, readonly FROM system.settings WHERE name IN (?) LIMIT ?", names, len(expected)); err != nil {
		return bootstrapError("verify reader constraints", err)
	}
	if len(settings) != len(expected) {
		return errors.New("ClickHouse reader constraints are incomplete")
	}
	for _, setting := range settings {
		// max_execution_time reflects the driver's deadline setting, not the
		// profile default; its effective upper bound is the relevant fence.
		if setting.Name != "max_execution_time" && setting.Value != expected[setting.Name] {
			return errors.New("ClickHouse reader setting differs from required value")
		}
		bounded := setting.Name == "max_execution_time" || setting.Name == "max_rows_to_read" || setting.Name == "max_bytes_to_read" || setting.Name == "max_memory_usage"
		if bounded {
			if setting.Min == nil || *setting.Min != "1" || setting.Max == nil || *setting.Max != expected[setting.Name] {
				return errors.New("ClickHouse reader limit constraints differ from required bounds")
			}
		} else if setting.Readonly != 1 {
			return errors.New("ClickHouse reader setting is not locked")
		}
	}
	return nil
}

// Database errors may echo SQL literals or connection details. Preserve only
// an operation label and numeric server error code at this credential boundary.
func bootstrapError(operation string, err error) error {
	var exception *ch.Exception
	if errors.As(err, &exception) {
		return fmt.Errorf("ClickHouse bootstrap: %s (server code %d)", operation, exception.Code)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("ClickHouse bootstrap: %s: canceled", operation)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ClickHouse bootstrap: %s: timed out", operation)
	}
	return fmt.Errorf("ClickHouse bootstrap: %s failed", operation)
}
