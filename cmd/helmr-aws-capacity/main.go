package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/helmrdotdev/helmr/internal/awscapacity"
	"github.com/helmrdotdev/helmr/internal/operatorclient"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("capacity reconciliation failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	controlURL := strings.TrimSpace(os.Getenv("HELMR_CONTROL_URL"))
	operatorToken := strings.TrimSpace(os.Getenv("HELMR_OPERATOR_TOKEN"))
	groupsJSON := strings.TrimSpace(os.Getenv("HELMR_CAPACITY_GROUPS"))
	if controlURL == "" || operatorToken == "" || groupsJSON == "" {
		return errors.New("HELMR_CONTROL_URL, HELMR_OPERATOR_TOKEN, and HELMR_CAPACITY_GROUPS are required")
	}
	observationMaxAge := 2 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("HELMR_CAPACITY_OBSERVATION_MAX_AGE")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("HELMR_CAPACITY_OBSERVATION_MAX_AGE must be a positive duration")
		}
		observationMaxAge = parsed
	}
	reconcileTimeout := 50 * time.Second
	if raw := strings.TrimSpace(os.Getenv("HELMR_CAPACITY_RECONCILE_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("HELMR_CAPACITY_RECONCILE_TIMEOUT must be a positive duration")
		}
		reconcileTimeout = parsed
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	config, err := awscapacity.DecodeConfig(groupsJSON, observationMaxAge)
	if err != nil {
		return err
	}
	control, err := operatorclient.New(controlURL, operatorToken)
	if err != nil {
		return err
	}
	aws, err := awsconfig.LoadDefaultConfig(reconcileCtx)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	reconciler, err := awscapacity.NewReconciler(log, control, autoscaling.NewFromConfig(aws), config)
	if err != nil {
		return err
	}
	return reconciler.Reconcile(reconcileCtx)
}
