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
	"github.com/helmrdotdev/helmr/internal/sessionlock"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runWorkerGroupStateCommand(ctx context.Context, output io.Writer, args []string) error {
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
	return withWorkerStore(ctx, func(pool *pgxpool.Pool, store *db.Queries) error {
		var result workergroup.GroupStatus
		var err error
		switch command {
		case "status":
			result, err = workergroup.ReadGroupStatus(ctx, store, groupID)
		case "pause":
			err = withWorkerGroupStateLease(ctx, pool, groupID, func() error {
				result, err = workergroup.PauseGroup(ctx, store, groupID, expectedClaimVersion)
				return err
			})
		case "activate":
			err = withWorkerGroupStateLease(ctx, pool, groupID, func() error {
				result, err = workergroup.ActivateGroup(ctx, store, groupID, expectedClaimVersion)
				return err
			})
		case "drain":
			err = withWorkerGroupStateLease(ctx, pool, groupID, func() error {
				result, err = workergroup.BeginGroupDrain(ctx, store, groupID, expectedClaimVersion)
				return err
			})
		case "disable":
			err = withWorkerGroupStateLease(ctx, pool, groupID, func() error {
				result, err = workergroup.DisableGroup(ctx, store, groupID, expectedClaimVersion)
				return err
			})
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	})
}

func runWorkerInstanceStateCommand(ctx context.Context, output io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("worker-instance command is required: status or lose")
	}
	command := args[0]
	flags := flag.NewFlagSet("worker-instance "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var groupID string
	var resourceID string
	var expectedClaimVersion int64
	flags.StringVar(&groupID, "group-id", "", "logical Worker group ID")
	flags.StringVar(&resourceID, "resource-id", "", "opaque operator host locator")
	if command == "lose" {
		flags.Int64Var(&expectedClaimVersion, "expected-claim-version", 0, "observed Worker instance claim fence")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("worker-instance command has unexpected positional arguments")
	}
	if command != "status" && command != "lose" {
		return fmt.Errorf("unknown worker-instance command %q", command)
	}
	return withWorkerStore(ctx, func(pool *pgxpool.Pool, store *db.Queries) error {
		var result workergroup.InstanceStatus
		var err error
		switch command {
		case "status":
			result, err = workergroup.ReadInstanceStatus(ctx, store, groupID, resourceID)
		case "lose":
			err = withWorkerGroupStateLease(ctx, pool, groupID, func() error {
				result, err = workergroup.MarkInstanceLost(ctx, store, groupID, resourceID, expectedClaimVersion)
				return err
			})
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	})
}

func withWorkerStore(ctx context.Context, run func(*pgxpool.Pool, *db.Queries) error) error {
	cfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.URL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	return run(pool, db.New(pool))
}

func withWorkerGroupStateLease(ctx context.Context, pool *pgxpool.Pool, groupID string, run func() error) (runErr error) {
	guard, err := sessionlock.Acquire(ctx, pool, []int64{workergroup.StateMutationLockKey(groupID)})
	if err != nil {
		return fmt.Errorf("acquire Worker group lifecycle lease: %w", err)
	}
	defer func() {
		if err := guard.Unlock(); err != nil && runErr == nil {
			runErr = fmt.Errorf("release Worker group lifecycle lease: %w", err)
		}
	}()
	return run()
}
