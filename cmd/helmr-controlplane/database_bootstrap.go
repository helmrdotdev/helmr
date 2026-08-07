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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/secure/precis"
)

type databaseBootstrapConfig struct {
	adminURL        string
	application     *pgxpool.Config
	applicationRole string
	databaseName    string
	resetSchema     bool
}

func runDatabaseBootstrap(ctx context.Context, args []string) error {
	if len(args) > 1 || len(args) == 1 && args[0] != "reset" {
		return errors.New("usage: helmr-controlplane database-bootstrap [reset]")
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
	adminURL := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(username, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + databaseName,
		RawQuery: "sslmode=require",
	}).String()
	return databaseBootstrapConfig{
		adminURL:        adminURL,
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
	adminPool, err := pgxpool.New(ctx, cfg.adminURL)
	if err != nil {
		return fmt.Errorf("configure administrative database connection: %w", err)
	}
	defer adminPool.Close()
	if err := adminPool.Ping(ctx); err != nil {
		return fmt.Errorf("connect to database as administrator: %w", err)
	}

	var exists, login, superuser, createDatabase, createRole, replication, bypassRLS, hasMembership bool
	err = adminPool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1),
		       COALESCE((SELECT rolcanlogin FROM pg_roles WHERE rolname = $1), false),
		       COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = $1), false),
		       COALESCE((SELECT rolcreatedb FROM pg_roles WHERE rolname = $1), false),
		       COALESCE((SELECT rolcreaterole FROM pg_roles WHERE rolname = $1), false),
		       COALESCE((SELECT rolreplication FROM pg_roles WHERE rolname = $1), false),
		       COALESCE((SELECT rolbypassrls FROM pg_roles WHERE rolname = $1), false),
		       EXISTS(
		           SELECT 1 FROM pg_auth_members
		            WHERE member = (SELECT oid FROM pg_roles WHERE rolname = $1)
		       )
	`, cfg.applicationRole).Scan(
		&exists,
		&login,
		&superuser,
		&createDatabase,
		&createRole,
		&replication,
		&bypassRLS,
		&hasMembership,
	)
	if err != nil {
		return fmt.Errorf("inspect application database role: %w", err)
	}
	if exists {
		if !login || superuser || createDatabase || createRole || replication || bypassRLS || hasMembership {
			return errors.New("application database role has administrative privileges")
		}
		if err := verifyApplicationDatabase(ctx, cfg.application); err != nil {
			return errors.New("stored application database credential does not authenticate the existing role")
		}
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
	if !exists {
		passwordVerifier, err := scramPasswordVerifier(cfg.application.ConnConfig.Password)
		if err != nil {
			return err
		}
		statement, err := formattedDatabaseStatement(
			ctx,
			tx,
			"CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
			cfg.applicationRole,
			passwordVerifier,
		)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create application database role: %w", err)
		}
	}
	for _, statementSpec := range []struct {
		format    string
		arguments []any
	}{
		{"GRANT CONNECT, CREATE, TEMPORARY ON DATABASE %I TO %I", []any{cfg.databaseName, cfg.applicationRole}},
		{"GRANT USAGE, CREATE ON SCHEMA public TO %I", []any{cfg.applicationRole}},
	} {
		statement, err := formattedDatabaseStatement(ctx, tx, statementSpec.format, statementSpec.arguments...)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant application database privileges: %w", err)
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
	pool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		return errors.New("configure application database connection")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.New("connect with application database credential")
	}
	return nil
}
