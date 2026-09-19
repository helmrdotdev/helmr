package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/clickhouse"
	clickhouseschema "github.com/helmrdotdev/helmr/internal/clickhouse/schema"
)

func loadClickHouseBootstrapConfig() (clickhouse.BootstrapConfig, error) {
	credential := func(role string) clickhouse.BootstrapCredential {
		return clickhouse.BootstrapCredential{
			User:     strings.TrimSpace(os.Getenv("CLICKHOUSE_" + role + "_USER")),
			Password: os.Getenv("CLICKHOUSE_" + role + "_PASSWORD"),
		}
	}
	cfg := clickhouse.BootstrapConfig{
		URL:   strings.TrimSpace(os.Getenv("CLICKHOUSE_URL")),
		Admin: credential("BOOTSTRAP"), Reader: credential("READER"),
		Ingester: credential("INGESTER"), Migration: credential("MIGRATION"),
		ReaderLimits: clickhouse.DefaultReaderLimits(),
	}
	return cfg, cfg.Validate()
}

func runClickHouseBootstrap(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: control-plane clickhouse-bootstrap")
	}
	cfg, err := loadClickHouseBootstrapConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client, err := clickhouse.New(clickhouse.Config{URL: cfg.URL, User: cfg.Admin.User, Password: cfg.Admin.Password})
	if err != nil {
		return errors.New("configure ClickHouse bootstrap administrator")
	}
	defer client.Close()
	if err := clickhouseschema.WaitReady(ctx, client); err != nil {
		return errors.New("ClickHouse bootstrap service readiness check failed")
	}
	return clickhouse.Bootstrap(ctx, client, cfg)
}
