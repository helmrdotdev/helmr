package main

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/secure/precis"
)

type databaseBootstrapConfig struct {
	adminURL        string
	application     *pgxpool.Config
	applicationRole string
	migration       *pgxpool.Config
	maintenance     *pgxpool.Config
	databaseName    string
	resetSchema     bool
}

func runDatabaseBootstrap(ctx context.Context, args []string) error {
	if len(args) > 1 || len(args) == 1 && args[0] != "reset" {
		return errors.New("usage: control-plane database-bootstrap [reset]")
	}
	cfg, err := loadDatabaseBootstrapConfig()
	if err != nil {
		return err
	}
	cfg.resetSchema = len(args) == 1
	return bootstrapDatabase(ctx, cfg)
}

func loadDatabaseBootstrapConfig() (databaseBootstrapConfig, error) {
	host := strings.TrimSpace(os.Getenv("DATABASE_ADMIN_HOST"))
	username := strings.TrimSpace(os.Getenv("DATABASE_ADMIN_USERNAME"))
	password := os.Getenv("DATABASE_ADMIN_PASSWORD")
	databaseName := strings.TrimSpace(os.Getenv("DATABASE_NAME"))
	applicationURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if host == "" || username == "" || password == "" || databaseName == "" || applicationURL == "" {
		return databaseBootstrapConfig{}, errors.New("database bootstrap configuration is incomplete")
	}
	port := 5432
	if raw := strings.TrimSpace(os.Getenv("DATABASE_ADMIN_PORT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			return databaseBootstrapConfig{}, errors.New("DATABASE_ADMIN_PORT must be a valid TCP port")
		}
		port = parsed
	}
	application, err := parseApplicationDatabaseConfig(applicationURL)
	if err != nil {
		return databaseBootstrapConfig{}, err
	}
	if application.ConnConfig.Database != databaseName {
		return databaseBootstrapConfig{}, errors.New("DATABASE_URL must target DATABASE_NAME")
	}
	if !strings.EqualFold(application.ConnConfig.Host, host) || application.ConnConfig.Port != uint16(port) {
		return databaseBootstrapConfig{}, errors.New("DATABASE_URL must target the administrative database endpoint")
	}
	if application.ConnConfig.User == username {
		return databaseBootstrapConfig{}, errors.New("application and administrative database roles must be distinct")
	}
	migration, err := parseApplicationDatabaseConfig(strings.TrimSpace(os.Getenv("DATABASE_MIGRATION_URL")))
	if err != nil {
		return databaseBootstrapConfig{}, errors.New("DATABASE_MIGRATION_URL must provide a separate TLS migration credential")
	}
	maintenance, err := parseApplicationDatabaseConfig(strings.TrimSpace(os.Getenv("DATABASE_AUDIT_MAINTENANCE_URL")))
	if err != nil {
		return databaseBootstrapConfig{}, errors.New("DATABASE_AUDIT_MAINTENANCE_URL must provide a separate TLS maintenance credential")
	}
	seen := map[string]bool{username: true, application.ConnConfig.User: true}
	for _, role := range []*pgxpool.Config{migration, maintenance} {
		c := role.ConnConfig
		if c.Database != databaseName || !strings.EqualFold(c.Host, host) || c.Port != uint16(port) || seen[c.User] {
			return databaseBootstrapConfig{}, errors.New("database roles must be distinct and target the same endpoint and database")
		}
		seen[c.User] = true
	}
	adminURL := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(username, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + databaseName,
		RawQuery: "sslmode=require",
	}).String()
	return databaseBootstrapConfig{
		adminURL:  adminURL,
		migration: migration, maintenance: maintenance,
		application:     application,
		applicationRole: application.ConnConfig.User,
		databaseName:    databaseName,
	}, nil
}

func parseApplicationDatabaseConfig(rawURL string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(rawURL)
	if err != nil || config.ConnConfig.Host == "" || config.ConnConfig.Port == 0 ||
		config.ConnConfig.Database == "" || config.ConnConfig.User == "" || config.ConnConfig.Password == "" {
		return nil, errors.New("DATABASE_URL must include one PostgreSQL endpoint, database, username, and password")
	}
	if config.ConnConfig.TLSConfig == nil {
		return nil, errors.New("DATABASE_URL must require TLS")
	}
	if len(config.ConnConfig.Fallbacks) != 0 {
		return nil, errors.New("DATABASE_URL must target one TLS endpoint without fallbacks")
	}
	return config, nil
}

func bootstrapDatabase(ctx context.Context, cfg databaseBootstrapConfig) error {
	// Administrative connection for role and database DDL.
	adminPool, err := pgxpool.New(ctx, cfg.adminURL)
	if err != nil {
		return fmt.Errorf("configure administrative database connection: %w", err)
	}
	defer adminPool.Close()
	if err := adminPool.Ping(ctx); err != nil {
		return fmt.Errorf("connect to database as administrator: %w", err)
	}

	if cfg.migration == nil || cfg.maintenance == nil {
		return errors.New("separate migration and maintenance roles are required")
	}
	tx, err := adminPool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin database bootstrap: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if cfg.resetSchema {
		if _, err := tx.Exec(ctx, "DROP SCHEMA IF EXISTS public CASCADE"); err != nil {
			return fmt.Errorf("drop application database schema: %w", err)
		}
		if _, err := tx.Exec(ctx, "CREATE SCHEMA public"); err != nil {
			return fmt.Errorf("create application database schema: %w", err)
		}
	}
	for _, role := range []*pgxpool.Config{cfg.application, cfg.migration, cfg.maintenance} {
		name := role.ConnConfig.User
		var exists, unsafe bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1), EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1 AND (NOT rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=$1))`, name).Scan(&exists, &unsafe); err != nil {
			return err
		}
		if unsafe {
			return errors.New("database role has administrative privileges or memberships")
		}
		if exists {
			if err := verifyApplicationDatabase(ctx, role); err != nil {
				return errors.New("stored database credential does not authenticate the existing role")
			}
		} else {
			verifier, err := scramPasswordVerifier(role.ConnConfig.Password)
			if err != nil {
				return err
			}
			statement, err := formattedDatabaseStatement(ctx, tx, "CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS", name, verifier)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
		}
	}
	for _, spec := range []struct {
		format string
		args   []any
	}{
		{"REVOKE CREATE ON SCHEMA public FROM PUBLIC", nil},
		{"REVOKE CREATE ON DATABASE %I FROM PUBLIC", []any{cfg.databaseName}},
		{"GRANT %I TO CURRENT_USER", []any{cfg.migration.ConnConfig.User}},
		{"GRANT CREATE ON DATABASE %I TO %I", []any{cfg.databaseName, cfg.migration.ConnConfig.User}},
		{"ALTER SCHEMA public OWNER TO %I", []any{cfg.migration.ConnConfig.User}},
		{"REVOKE ALL ON DATABASE %I FROM %I, %I", []any{cfg.databaseName, cfg.applicationRole, cfg.maintenance.ConnConfig.User}},
		{"GRANT CONNECT ON DATABASE %I TO %I, %I, %I", []any{cfg.databaseName, cfg.applicationRole, cfg.migration.ConnConfig.User, cfg.maintenance.ConnConfig.User}},
		{"REVOKE ALL ON SCHEMA public FROM %I, %I", []any{cfg.applicationRole, cfg.maintenance.ConnConfig.User}},
		{"GRANT USAGE ON SCHEMA public TO %I, %I", []any{cfg.applicationRole, cfg.maintenance.ConnConfig.User}},
	} {
		statement, err := formattedDatabaseStatement(ctx, tx, spec.format, spec.args...)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("configure database role boundary: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit database bootstrap: %w", err)
	}
	if err := verifyApplicationDatabase(ctx, cfg.application); err != nil {
		return fmt.Errorf("verify application database credential: %w", err)
	}
	return nil
}

func scramPasswordVerifier(password string) (string, error) {
	const iterations = 4096
	normalizedPassword, err := precis.OpaqueString.String(password)
	if err != nil {
		// PostgreSQL and pgx retain passwords that PRECIS rejects.
		normalizedPassword = password
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("generate application database credential salt")
	}
	saltedPassword, err := pbkdf2.Key(sha256.New, normalizedPassword, salt, iterations, sha256.Size)
	if err != nil {
		return "", errors.New("derive application database credential verifier")
	}
	clientMAC := hmac.New(sha256.New, saltedPassword)
	_, _ = clientMAC.Write([]byte("Client Key"))
	storedKey := sha256.Sum256(clientMAC.Sum(nil))
	serverMAC := hmac.New(sha256.New, saltedPassword)
	_, _ = serverMAC.Write([]byte("Server Key"))
	return fmt.Sprintf(
		"SCRAM-SHA-256$%d:%s$%s:%s",
		iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(storedKey[:]),
		base64.StdEncoding.EncodeToString(serverMAC.Sum(nil)),
	), nil
}

func formattedDatabaseStatement(ctx context.Context, tx pgx.Tx, format string, arguments ...any) (string, error) {
	values := make([]string, len(arguments))
	for index, value := range arguments {
		values[index] = fmt.Sprint(value)
	}
	var statement string
	if err := tx.QueryRow(ctx, "SELECT format($1, VARIADIC $2::text[])", format, values).Scan(&statement); err != nil {
		return "", fmt.Errorf("format database bootstrap statement: %w", err)
	}
	return statement, nil
}

func verifyApplicationDatabase(ctx context.Context, config *pgxpool.Config) error {
	pool, err := dbpool.New(ctx, config)
	if err != nil {
		return errors.New("configure application database connection")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.New("connect with application database credential")
	}
	return nil
}
