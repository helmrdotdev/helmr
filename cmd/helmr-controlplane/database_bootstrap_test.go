package main

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbschema "github.com/helmrdotdev/helmr/internal/db/schema"
)

func TestLoadDatabaseBootstrapConfig(t *testing.T) {
	t.Setenv("DATABASE_ADMIN_HOST", "db.example.test")
	t.Setenv("DATABASE_ADMIN_PORT", "5433")
	t.Setenv("DATABASE_ADMIN_USERNAME", "helmr")
	t.Setenv("DATABASE_ADMIN_PASSWORD", "admin password")
	t.Setenv("DATABASE_NAME", "helmr")
	t.Setenv("DATABASE_URL", "postgres://helmr_app:application%20password@db.example.test:5433/helmr?sslmode=require")

	cfg, err := loadDatabaseBootstrapConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.applicationRole != "helmr_app" || cfg.databaseName != "helmr" {
		t.Fatalf("config = %+v", cfg)
	}
	if strings.Contains(cfg.adminURL, "admin password") {
		t.Fatal("administrative URL contains an unescaped password")
	}
}

func TestLoadDatabaseBootstrapConfigRejectsSplitTargets(t *testing.T) {
	for name, applicationURL := range map[string]string{
		"host":     "postgres://helmr_app:secret@other.example.test/helmr?sslmode=require",
		"database": "postgres://helmr_app:secret@db.example.test/other?sslmode=require",
		"admin":    "postgres://helmr:secret@db.example.test/helmr?sslmode=require",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_ADMIN_HOST", "db.example.test")
			t.Setenv("DATABASE_ADMIN_USERNAME", "helmr")
			t.Setenv("DATABASE_ADMIN_PASSWORD", "admin-secret")
			t.Setenv("DATABASE_NAME", "helmr")
			t.Setenv("DATABASE_URL", applicationURL)
			if _, err := loadDatabaseBootstrapConfig(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseApplicationDatabaseConfigRejectsInvalidURLs(t *testing.T) {
	for _, rawURL := range []string{
		"",
		"https://helmr:secret@db.example.test/helmr",
		"postgres://db.example.test/helmr?sslmode=require",
		"postgres://helmr_app@db.example.test/helmr?sslmode=require",
		"postgres://helmr_app:secret@db.example.test?sslmode=require",
		"postgres://helmr_app:secret@db.example.test/helmr?sslmode=disable",
		"postgres://helmr_app:secret@db.example.test/helmr",
		"postgres://helmr_app:secret@db.example.test/helmr?host=db.example.test,other.example.test&sslmode=require",
	} {
		if _, err := parseApplicationDatabaseConfig(rawURL); err == nil {
			t.Fatalf("parseApplicationDatabaseConfig(%q) succeeded", rawURL)
		}
	}
}

func TestLoadDatabaseBootstrapConfigRejectsEffectiveOverrides(t *testing.T) {
	for name, applicationURL := range map[string]string{
		"host":     "postgres://helmr_app:secret@db.example.test/helmr?host=other.example.test&sslmode=require",
		"database": "postgres://helmr_app:secret@db.example.test/helmr?dbname=other&sslmode=require",
		"admin":    "postgres://helmr_app:secret@db.example.test/helmr?user=helmr&sslmode=require",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_ADMIN_HOST", "db.example.test")
			t.Setenv("DATABASE_ADMIN_USERNAME", "helmr")
			t.Setenv("DATABASE_ADMIN_PASSWORD", "admin-secret")
			t.Setenv("DATABASE_NAME", "helmr")
			t.Setenv("DATABASE_URL", applicationURL)
			if _, err := loadDatabaseBootstrapConfig(); err == nil {
				t.Fatal("expected effective configuration validation error")
			}
		})
	}
}

func TestSCRAMPasswordVerifierDoesNotContainPassword(t *testing.T) {
	const password = "application-password-must-not-reach-ddl"
	verifier, err := scramPasswordVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$4096:") || strings.Contains(verifier, password) {
		t.Fatalf("invalid SCRAM verifier %q", verifier)
	}
}

func TestBootstrapDatabasePostgres(t *testing.T) {
	adminBaseURL := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if adminBaseURL == "" {
		t.Skip("HELMR_TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, adminBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	databaseName := "helmr_bootstrap_" + suffix
	adminRole := "helmr_admin_" + suffix
	applicationRole := "helmr_app_" + suffix
	// U+212B normalizes to U+00C5 under the PRECIS profile used by PostgreSQL
	// SCRAM clients. Successful verification proves the stored verifier and
	// pgx authenticate the same password representation.
	applicationPassword := "application \u212B password " + suffix
	if _, err := adminPool.Exec(ctx, "CREATE ROLE "+pgx.Identifier{adminRole}.Sanitize()+" LOGIN CREATEDB CREATEROLE NOSUPERUSER"); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" OWNER "+pgx.Identifier{adminRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		if _, err := adminPool.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{applicationRole}.Sanitize()); err != nil {
			t.Errorf("drop role: %v", err)
		}
		if _, err := adminPool.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{adminRole}.Sanitize()); err != nil {
			t.Errorf("drop admin role: %v", err)
		}
	}()

	adminURL := databaseTestURL(t, adminBaseURL, databaseName, adminRole, "unused")
	applicationURL := databaseTestURL(t, adminBaseURL, databaseName, applicationRole, applicationPassword)
	applicationConfig, err := pgxpool.ParseConfig(applicationURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := databaseBootstrapConfig{
		adminURL:        adminURL,
		application:     applicationConfig,
		applicationRole: applicationRole,
		databaseName:    databaseName,
	}
	if err := bootstrapDatabase(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapDatabase(ctx, cfg); err != nil {
		t.Fatalf("idempotent bootstrap: %v", err)
	}
	if err := dbschema.Up(ctx, applicationURL); err != nil {
		t.Fatalf("application role cannot run the complete schema migration: %v", err)
	}

	applicationPool, err := pgxpool.New(ctx, applicationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer applicationPool.Close()
	if _, err := applicationPool.Exec(ctx, "CREATE TABLE bootstrap_probe (id bigint PRIMARY KEY)"); err != nil {
		t.Fatalf("application role cannot create schema objects: %v", err)
	}
	if _, err := applicationPool.Exec(ctx, "CREATE DATABASE forbidden"); err == nil {
		t.Fatal("application role created a database")
	}
	if _, err := applicationPool.Exec(ctx, "CREATE ROLE forbidden"); err == nil {
		t.Fatal("application role created a role")
	}
	var superuser, createDatabase, createRole, replication, bypassRLS, hasMembership bool
	if err := adminPool.QueryRow(ctx, `
		SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls,
		       EXISTS(SELECT 1 FROM pg_auth_members WHERE member = pg_roles.oid)
		  FROM pg_roles
		 WHERE rolname = $1
	`, applicationRole).Scan(&superuser, &createDatabase, &createRole, &replication, &bypassRLS, &hasMembership); err != nil {
		t.Fatal(err)
	}
	if superuser || createDatabase || createRole || replication || bypassRLS || hasMembership {
		t.Fatal("application role retained administrative privileges")
	}
}

func databaseTestURL(t *testing.T, base, databaseName, username, password string) string {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + databaseName
	if username != "" {
		parsed.User = url.UserPassword(username, password)
	}
	return parsed.String()
}
