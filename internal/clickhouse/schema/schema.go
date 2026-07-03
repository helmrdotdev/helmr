package schema

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/helmrdotdev/helmr/internal/clickhouse"
)

// Migrations contains ClickHouse DDL for the managed-cloud telemetry store.
//
//go:embed migrations/*.sql
var Migrations embed.FS

func Up(ctx context.Context, cfg clickhouse.Config) error {
	client, err := clickhouse.New(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := Migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		for _, statement := range splitStatements(string(content)) {
			if err := client.Exec(ctx, statement); err != nil {
				return fmt.Errorf("apply clickhouse migration %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func splitStatements(content string) []string {
	parts := strings.Split(content, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}
