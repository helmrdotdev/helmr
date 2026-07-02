package schema

import "embed"

// Migrations contains ClickHouse DDL for the managed-cloud telemetry store.
//
//go:embed migrations/*.sql
var Migrations embed.FS
