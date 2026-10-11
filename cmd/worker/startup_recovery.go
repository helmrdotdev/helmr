package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
)

func validateStartupRecovery(evidence worker.RecoveryEvidence) error {
	if len(evidence.Quarantined) != len(evidence.QuarantinedOwners) {
		return errors.New("startup recovery found VM residue without exact ownership")
	}
	for _, owner := range evidence.QuarantinedOwners {
		if owner.Kind != vm.OwnerInstance {
			return errors.New("startup recovery found non-instance VM residue")
		}
	}
	return nil
}

// qualifyRecoveredRuntime reserves retained owners and a full lifecycle slot for
// the real probe. A failed qualification exits startup with those claims intact;
// only successful probe cleanup makes its capacity available for workloads.
func qualifyRecoveredRuntime(ctx context.Context, evidence worker.RecoveryEvidence, ledger *reservation.Ledger, perVM reservation.Vector, qualify func(context.Context) (*firecracker.QualifiedRuntime, error)) (*firecracker.QualifiedRuntime, error) {
	for _, owner := range evidence.QuarantinedOwners {
		created, err := ledger.Reserve(reservation.Key{Kind: "quarantine", Epoch: 1, ID: owner.ID}, perVM)
		if err != nil {
			return nil, fmt.Errorf("reserve quarantined instance capacity: %w", err)
		}
		if !created {
			return nil, errors.New("quarantined instance is already reserved")
		}
	}
	probe := reservation.Key{Kind: "qualification", Epoch: 1, ID: "startup"}
	if _, err := ledger.Reserve(probe, perVM); err != nil {
		return nil, fmt.Errorf("reserve runtime qualification capacity: %w", err)
	}
	runtime, err := qualify(ctx)
	if err != nil {
		return nil, err
	}
	if err := ledger.Release(probe); err != nil {
		return nil, err
	}
	return runtime, nil
}

// This startup boundary runs under the process singleton and authenticated epoch.
// Qualification can boot a VM, so it must follow owned-state recovery and the
// attachment-custody barrier. Return the original evidence for Supervisor reporting.
func recoverAndQualifyWorkerRuntime(ctx context.Context, log *slog.Logger, recover func(context.Context) (worker.RecoveryEvidence, error), attachments func() error, qualify func(context.Context, worker.RecoveryEvidence) (*firecracker.QualifiedRuntime, error)) (worker.RecoveryEvidence, *firecracker.QualifiedRuntime, error) {
	evidence, err := recover(ctx)
	// Keep the original recovery diagnosis even if a later startup step fails
	// before Supervisor receives this evidence.
	if len(evidence.Quarantined) != 0 || len(evidence.QuarantineErrors) != 0 {
		log.WarnContext(ctx, "worker startup retained quarantined state", "owners", evidence.Quarantined, "recovery_errors", evidence.QuarantineErrors)
	}
	if err != nil {
		return evidence, nil, fmt.Errorf("recover local worker state: %w", err)
	}
	if err := validateStartupRecovery(evidence); err != nil {
		return evidence, nil, err
	}
	if err := attachments(); err != nil {
		return evidence, nil, fmt.Errorf("recover local worker state: %w", err)
	}
	runtime, err := qualify(ctx, evidence)
	if err != nil {
		return evidence, nil, fmt.Errorf("qualify Firecracker worker runtime: %w", err)
	}
	return evidence, runtime, nil
}
