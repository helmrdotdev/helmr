package main

import (
	"context"
	"strings"
	"testing"
)

func TestClickHouseBootstrapConfigurationIsSeparateFromRuntime(t *testing.T) {
	t.Setenv("CLICKHOUSE_URL", "https://clickhouse.example.test")
	t.Setenv("CLICKHOUSE_USER", "shared-runtime-user")
	t.Setenv("CLICKHOUSE_PASSWORD", "shared-runtime-password")
	for _, role := range []string{"BOOTSTRAP", "READER", "INGESTER", "MIGRATION"} {
		t.Setenv("CLICKHOUSE_"+role+"_USER", strings.ToLower(role)+"_test")
		t.Setenv("CLICKHOUSE_"+role+"_PASSWORD", role+"-Unique-Password-1")
	}
	cfg, err := loadClickHouseBootstrapConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.User != "bootstrap_test" || cfg.Reader.User != "reader_test" || cfg.Migration.User != "migration_test" || cfg.Ingester.User != "ingester_test" {
		t.Fatal("bootstrap used shared runtime credentials")
	}
	t.Setenv("CLICKHOUSE_BOOTSTRAP_PASSWORD", "")
	if _, err := loadClickHouseBootstrapConfig(); err == nil {
		t.Fatal("bootstrap accepted missing admin credential")
	}
	if err := runClickHouseBootstrap(context.Background(), []string{"rotate"}); err == nil {
		t.Fatal("unexpected credential mutation command accepted")
	}
}
