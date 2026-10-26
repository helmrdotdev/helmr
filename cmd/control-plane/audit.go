package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runAuditCommand(ctx context.Context, output io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "purge" && args[0] != "record-restore" {
		return errors.New("audit requires purge or record-restore")
	}
	operation := args[0]
	flags := flag.NewFlagSet("audit "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var reference, from, to string
	if operation == "record-restore" {
		flags.StringVar(&reference, "backup-reference", "", "Nonsecret backup reference")
		flags.StringVar(&from, "unknown-from", "", "Start of unknown interval, RFC3339")
		flags.StringVar(&to, "unknown-to", "", "End of unknown interval, RFC3339")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected audit argument")
	}
	policy, err := auditpolicy.Parse(os.Getenv("AUDIT_RETENTION_SECONDS"))
	if err != nil {
		return err
	}
	seconds, err := strconv.Atoi(os.Getenv("AUDIT_MAINTENANCE_TIMEOUT_SECONDS"))
	if err != nil || seconds < 1 || seconds > 3600 {
		return errors.New("AUDIT_MAINTENANCE_TIMEOUT_SECONDS must be explicitly configured between 1 and 3600")
	}
	batches := 0
	detail := audit.RestoreDetails{}
	if operation == "purge" {
		batches, err = strconv.Atoi(os.Getenv("AUDIT_PURGE_MAX_BATCHES"))
		if err != nil || batches < 1 || batches > 1000 {
			return errors.New("AUDIT_PURGE_MAX_BATCHES must be explicitly configured between 1 and 1000")
		}
		if batches*5+15 > seconds {
			return errors.New("AUDIT_MAINTENANCE_TIMEOUT_SECONDS must cover all five-second purge batch deadlines plus 15 seconds of setup overhead")
		}
	} else {
		detail.BackupReference = reference
		detail.UnknownFrom, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return errors.New("--unknown-from requires RFC3339 time")
		}
		detail.UnknownTo, err = time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return errors.New("--unknown-to requires RFC3339 time")
		}
	}
	dsn := os.Getenv("DATABASE_AUDIT_MAINTENANCE_URL")
	if dsn == "" {
		return errors.New("DATABASE_AUDIT_MAINTENANCE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid audit maintenance database configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	pool, err := dbpool.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = audit.ValidatePrivileges(ctx, pool, true); err != nil {
		return err
	}
	action := "audit.purge_completed"
	var result any
	if operation == "purge" {
		result, err = audit.Purge(ctx, pool, policy, batches)
	} else {
		action = "audit.restore_recorded"
		err = audit.RecordRestore(ctx, pool, policy, detail)
		result = map[string]bool{"recorded": err == nil}
	}
	if err != nil {
		if db.CommitOutcomeUnknown(err) {
			return err
		}
		failureCtx, failureCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer failureCancel()
		captureErr := db.RunTx(failureCtx, pool, func(tx pgx.Tx) error {
			return audit.Append(failureCtx, tx, policy, audit.Event{Evidence: audit.Internal(audit.AuditMaintenance), Action: action, Outcome: "failed", TargetKind: "audit", Details: audit.FailureDetails{Reason: "operation_failed"}})
		})
		if captureErr != nil {
			return fmt.Errorf("audit maintenance failed: %w; failure capture also failed: %v", err, captureErr)
		}
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
