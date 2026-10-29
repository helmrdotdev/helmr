package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"os"
	"path/filepath"
	"strings"
	"uuid"
)

// Operator capabilities are issued by deployment authority, never an HTTP
// administrator or remote CLI identity. They leave this command only in a new
// owner-readable file, so terminal recording and process arguments contain none.
func runOperatorCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "enroll" && args[0] != "repair" && args[0] != "repair-account") {
		return errors.New("usage: control-plane operator enroll|repair|repair-account --label LABEL --output-file ABSOLUTE_PATH [--operator-id ID]")
	}
	flags := flag.NewFlagSet("operator", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	label := flags.String("label", "", "Operator display label")
	output := flags.String("output-file", "", "New protected capability file")
	userTarget := flags.String("user-id", "", "Ordinary account to repair")
	target := flags.String("operator-id", "", "Operator to repair")
	if err := flags.Parse(args[1:]); err != nil {
		return errors.New("invalid operator command arguments")
	}
	if flags.NArg() != 0 || (args[0] != "repair-account" && strings.TrimSpace(*label) == "") || !filepath.IsAbs(*output) {
		return errors.New("an absolute output-file and (for operator enrollment) label are required")
	}
	id := uuid.Nil()
	if args[0] != "enroll" {
		var err error
		id, err = uuid.Parse(*target)
		if err != nil || id == uuid.Nil() {
			return errors.New("repair requires operator-id")
		}
	} else if *target != "" {
		return errors.New("operator-id is only valid for repair")
	}
	userID := uuid.Nil()
	if args[0] == "repair-account" {
		if os.Getenv("DEPLOYMENT_MODE") != "self-hosted" {
			return errors.New("ordinary account repair requires explicit self-hosted deployment mode")
		}
		var err error
		userID, err = uuid.Parse(*userTarget)
		if err != nil || userID == uuid.Nil() {
			return errors.New("repair-account requires user-id")
		}
	} else if *userTarget != "" {
		return errors.New("user-id is only valid for repair-account")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create protected output file; it must not already exist")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(*output)
		}
	}()
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	pool, err := dbpool.New(ctx, cfg)
	if err != nil {
		return errors.New("cannot connect to the configured database")
	}
	defer pool.Close()
	var enrollment identity.OperatorEnrollment
	if args[0] == "repair-account" {
		enrollment, err = identity.IssueAccountRepair(ctx, pool, userID, id)
	} else {
		enrollment, err = identity.IssueOperatorEnrollment(ctx, pool, strings.TrimSpace(*label), id)
	}
	if err != nil {
		return fmt.Errorf("issue operator enrollment: %w", err)
	}
	if err = json.NewEncoder(file).Encode(enrollment); err != nil {
		return errors.New("cannot write protected enrollment file")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot persist protected enrollment file")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close protected enrollment file")
	}
	complete = true
	return nil
}
