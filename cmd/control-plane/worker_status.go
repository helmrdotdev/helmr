package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runWorkerGroupStatusCommand(ctx context.Context, output io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("worker-group command is required: status, pause, activate, drain, or disable")
	}
	command := args[0]
	flags := flag.NewFlagSet("worker-group "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var groupID string
	var expectedClaimVersion int64
	flags.StringVar(&groupID, "group-id", "", "logical Worker group ID")
	if command != "status" {
		flags.Int64Var(&expectedClaimVersion, "expected-claim-version", 0, "observed Worker group claim fence")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("worker-group command has unexpected positional arguments")
	}
	if command != "status" && command != "pause" && command != "activate" && command != "drain" && command != "disable" {
		return fmt.Errorf("unknown worker-group command %q", command)
	}
	parsedGroupID, err := ids.Parse(groupID)
	if err != nil {
		return errors.New("worker group id must be a canonical UUIDv7")
	}
	return withWorkerDatabase(ctx, func(pool *pgxpool.Pool) error {
		var result workergroup.GroupStatus
		var err error
		switch command {
		case "status":
			result, err = workergroup.ReadGroupStatus(ctx, db.New(pool), parsedGroupID)
		case "pause":
			result, err = workergroup.PauseGroup(ctx, pool, parsedGroupID, expectedClaimVersion)
		case "activate":
			result, err = workergroup.ActivateGroup(ctx, pool, parsedGroupID, expectedClaimVersion)
		case "drain":
			result, err = workergroup.BeginGroupDrain(ctx, pool, parsedGroupID, expectedClaimVersion)
		case "disable":
			result, err = workergroup.DisableGroup(ctx, pool, parsedGroupID, expectedClaimVersion)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	})
}

func runWorkerHostStatusCommand(ctx context.Context, output io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("worker-host command is required: status or lose")
	}
	command := args[0]
	flags := flag.NewFlagSet("worker-host "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var groupID string
	var resourceID string
	var expectedClaimVersion int64
	flags.StringVar(&groupID, "group-id", "", "logical Worker group ID")
	flags.StringVar(&resourceID, "resource-id", "", "opaque operator host locator")
	if command == "lose" {
		flags.Int64Var(&expectedClaimVersion, "expected-claim-version", 0, "observed worker host claim fence")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("worker-host command has unexpected positional arguments")
	}
	if command != "status" && command != "lose" {
		return fmt.Errorf("unknown worker-host command %q", command)
	}
	parsedGroupID, err := ids.Parse(groupID)
	if err != nil {
		return errors.New("worker group id must be a canonical UUIDv7")
	}
	return withWorkerDatabase(ctx, func(pool *pgxpool.Pool) error {
		var result workergroup.HostStatus
		var err error
		switch command {
		case "status":
			result, err = workergroup.ReadHostStatus(ctx, db.New(pool), parsedGroupID, resourceID)
		case "lose":
			result, err = workergroup.MarkHostLost(ctx, pool, parsedGroupID, resourceID, expectedClaimVersion)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	})
}

func withWorkerDatabase(ctx context.Context, run func(*pgxpool.Pool) error) error {
	cfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.URL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	return run(pool)
}
