package command

import (
	"context"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// WriterGeneration is the Computer lease epoch on the guest's physical wire.
type ClaimRequest struct {
	EnvironmentID         uuid.UUID
	InstanceID            uuid.UUID
	WriterGeneration      int64
	ActiveCommandIDs      []uuid.UUID
	ActiveCancellationIDs []uuid.UUID
}
type ClaimResult struct {
	Start        *Start
	Cancellation *Cancellation
	Release      *Release
}

// Secret pins commit before Start can leave the owner. Replaying a lost response
// keeps the same Command, lease, fingerprint, versions and protected selectors.
type Start struct {
	TailOnly           bool
	Command            db.ComputerCommand
	Lease              Lease
	Secrets            []secret.DeliveryEnvelope
	ProtectedEnv       map[string]string
	ProxyCA            []byte
	RequestFingerprint []byte
}
type Cancellation struct {
	CommandID          uuid.UUID
	ComputerID         uuid.UUID
	InstanceID         uuid.UUID
	WriterGeneration   int64
	RequestFingerprint []byte
	ExpiresAt          time.Time
}
type Release struct {
	ComputerID         uuid.UUID
	RequestFingerprint []byte
	Completion         CompletionReport
}

func Claim(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, request ClaimRequest) (ClaimResult, error) {
	var result ClaimResult
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error { var err error; result, err = claimNext(ctx, tx, worker, request); return err })
	if err != nil {
		return ClaimResult{}, changed(err)
	}
	return result, nil
}

func claimNext(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request ClaimRequest) (ClaimResult, error) {
	if err := lockHost(ctx, tx, worker); err != nil {
		return ClaimResult{}, err
	}
	var computer uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT l.computer_id FROM computer_leases l
 WHERE l.environment_id=$1 AND l.computer_instance_id=$2 AND l.epoch=$3 AND l.worker_host_id=$4 AND l.worker_epoch=$5`, request.EnvironmentID, request.InstanceID, request.WriterGeneration, worker.HostID, worker.Epoch).Scan(&computer); err != nil {
		return ClaimResult{}, err
	}
	bindings, err := lockBindings(ctx, tx, request.EnvironmentID, computer)
	if err != nil {
		return ClaimResult{}, err
	}
	if err = lockComputer(ctx, tx, request.EnvironmentID, computer); err != nil {
		return ClaimResult{}, err
	}
	lease, err := readLease(tx.QueryRow(ctx, `SELECT `+leaseColumns+` FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 FOR NO KEY UPDATE`, request.EnvironmentID, computer, request.WriterGeneration))
	if err != nil {
		return ClaimResult{}, err
	}
	if !lease.matches(worker, request.InstanceID, request.WriterGeneration) {
		return ClaimResult{}, pgx.ErrNoRows
	}
	if err = producerStillAuthorized(ctx, tx, worker, lease, false); err != nil {
		return ClaimResult{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM computer_commands WHERE environment_id=$1 AND computer_id=$2
 AND ((computer_lease_epoch=$3 AND (terminal_at IS NULL OR process_reconciled_at IS NULL)) OR (computer_lease_epoch IS NULL AND status='pending'))
 ORDER BY CASE WHEN status='stopping' THEN 0 WHEN terminal_at IS NOT NULL THEN 1 WHEN computer_lease_epoch IS NOT NULL THEN 2 ELSE 3 END,created_at,id`, request.EnvironmentID, computer, lease.Epoch)
	if err != nil {
		return ClaimResult{}, err
	}
	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (uuid.UUID, error) {
		var id uuid.UUID
		err := row.Scan(&id)
		return id, err
	})
	if err != nil {
		return ClaimResult{}, err
	}
	for _, id := range ids {
		c, err := lockCommand(ctx, tx, request.EnvironmentID, computer, id)
		if err != nil {
			return ClaimResult{}, err
		}
		if c.ComputerLeaseEpoch.Valid && c.ComputerLeaseEpoch.Int64 != lease.Epoch {
			continue
		}
		var fingerprint []byte
		if err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM platform_retry_keys WHERE environment_id=$1 AND command_id=$2 AND operation='computer.exec'`, request.EnvironmentID, id).Scan(&fingerprint); err != nil {
			return ClaimResult{}, err
		}
		if c.Status == "stopping" && !slices.Contains(request.ActiveCancellationIDs, id) {
			if err = producerStillAuthorized(ctx, tx, worker, lease, false); err != nil {
				return ClaimResult{}, err
			}
			return ClaimResult{Cancellation: &Cancellation{CommandID: id, ComputerID: computer, InstanceID: lease.InstanceID, WriterGeneration: lease.Epoch, RequestFingerprint: fingerprint, ExpiresAt: *lease.ExpiresAt}}, nil
		}
		if slices.Contains(request.ActiveCommandIDs, id) {
			continue
		}
		if c.TerminalAt.Valid {
			report, err := release(c, request.EnvironmentID, lease)
			if err != nil {
				return ClaimResult{}, err
			}
			if report != nil {
				if err = producerStillAuthorized(ctx, tx, worker, lease, false); err != nil {
					return ClaimResult{}, err
				}
				return ClaimResult{Release: &Release{ComputerID: computer, RequestFingerprint: fingerprint, Completion: *report}}, nil
			}
			if c.StdoutFinalThrough.Valid && c.StderrFinalThrough.Valid && !c.OutputFenced && !c.ProcessReconciledAt.Valid && c.Status != "lost" && c.FailureReason.String != "scope_termination_failed" {
				if err = producerStillAuthorized(ctx, tx, worker, lease, false); err != nil {
					return ClaimResult{}, err
				}
				return ClaimResult{Start: &Start{TailOnly: true, Command: c, Lease: lease, RequestFingerprint: fingerprint}}, nil
			}
			continue
		}
		if c.Status != "pending" && c.Status != "starting" && c.Status != "running" {
			continue
		}
		if err = launchAvailable(ctx, tx, worker, lease, c.Status != "running"); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return ClaimResult{}, err
		}
		for _, b := range bindings {
			if b.status != "active" {
				return ClaimResult{}, secret.ErrDeliveryRevoked
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_commands SET computer_lease_epoch=$3,status='running',started_at=COALESCE(started_at,clock_timestamp()),
 revision=revision+CASE WHEN status='running' THEN 0 ELSE 1 END,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2 AND status IN ('pending','starting','running')`, request.EnvironmentID, id, lease.Epoch); err != nil {
			return ClaimResult{}, err
		}
		c, err = lockCommand(ctx, tx, request.EnvironmentID, computer, id)
		if err != nil {
			return ClaimResult{}, err
		}
		envelopes, protected, err := recordExposure(ctx, tx, c, bindings)
		if err != nil {
			return ClaimResult{}, err
		}
		if err = launchAvailable(ctx, tx, worker, lease, false); err != nil {
			return ClaimResult{}, err
		}
		var proxyCA []byte
		if len(protected) > 0 {
			var expiry time.Time
			if err = tx.QueryRow(ctx, `SELECT proxy_ca_certificate,proxy_ca_not_after FROM computers WHERE environment_id=$1 AND id=$2`, request.EnvironmentID, computer).Scan(&proxyCA, &expiry); err != nil {
				return ClaimResult{}, err
			}
			if err = secret.ValidateProxyTrust(proxyCA, expiry, time.Now()); err != nil {
				return ClaimResult{}, err
			}
		}
		return ClaimResult{Start: &Start{Command: c, Lease: lease, Secrets: envelopes, ProtectedEnv: protected, ProxyCA: proxyCA, RequestFingerprint: fingerprint}}, nil
	}
	return ClaimResult{}, nil
}

func launchAvailable(ctx context.Context, tx db.DBTX, host workergroup.HostPrincipal, l Lease, newLaunch bool) error {
	if err := producerStillAuthorized(ctx, tx, host, l, true); err != nil {
		return err
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id
 WHERE c.environment_id=$1 AND c.id=$2 AND e.retired_at IS NULL AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL
 AND c.initial_root_id IS NOT NULL AND c.preparation_failed_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations r WHERE r.environment_id=c.environment_id AND r.computer_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND (cp.status IN ('capturing','sealed','ready','restoring','aborting') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.computer_lease_epoch<>$3 AND s.status IN ('requested','captured'))
 AND (NOT $4 OR EXISTS(SELECT 1 FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id JOIN worker_pools p ON p.id=h.worker_pool_id
 WHERE h.id=$5 AND h.status='active' AND g.status='active' AND p.status='active' AND h.run_paused_reason IS NULL AND h.vm_paused_reason IS NULL)))`, l.EnvironmentID, l.ComputerID, l.Epoch, newLaunch, host.HostID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return pgx.ErrNoRows
	}
	return nil
}

// CheckStart is the final database capture after opening envelopes and immediately
// before delivery. It neither changes the selected versions nor grants new work.
func CheckStart(ctx context.Context, txb db.TxBeginner, host workergroup.HostPrincipal, start Start) error {
	return changed(db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, host); err != nil {
			return err
		}
		if start.TailOnly {
			target, err := db.New(tx).GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{EnvironmentID: start.Command.EnvironmentID, CommandID: start.Command.ID})
			if err != nil {
				return err
			}
			lease, err := lockCommandLease(ctx, tx, target, uuid.UUID(start.Command.ID.Bytes))
			if err != nil {
				return err
			}
			if !lease.matches(host, start.Lease.InstanceID, start.Lease.Epoch) {
				return pgx.ErrNoRows
			}
			c, err := lockCommand(ctx, tx, lease.EnvironmentID, lease.ComputerID, uuid.UUID(start.Command.ID.Bytes))
			if err != nil {
				return err
			}
			if !c.TerminalAt.Valid || c.OutputFenced || c.ProcessReconciledAt.Valid || !c.ComputerLeaseEpoch.Valid || c.ComputerLeaseEpoch.Int64 != lease.Epoch {
				return pgx.ErrNoRows
			}
			return producerStillAuthorized(ctx, tx, host, lease, false)
		}
		bindings, err := lockBindings(ctx, tx, start.Lease.EnvironmentID, start.Lease.ComputerID)
		if err != nil {
			return err
		}
		for _, b := range bindings {
			if b.status != "active" {
				return secret.ErrDeliveryRevoked
			}
		}
		if err = lockComputer(ctx, tx, start.Lease.EnvironmentID, start.Lease.ComputerID); err != nil {
			return err
		}
		c, err := lockCommand(ctx, tx, start.Lease.EnvironmentID, start.Lease.ComputerID, uuid.UUID(start.Command.ID.Bytes))
		if err != nil {
			return err
		}
		if c.Status != "running" || c.CancelRequestedAt.Valid || !c.ComputerLeaseEpoch.Valid || c.ComputerLeaseEpoch.Int64 != start.Lease.Epoch {
			return pgx.ErrNoRows
		}
		return launchAvailable(ctx, tx, host, start.Lease, false)
	}))
}

type runtimeBinding struct {
	id                                      uuid.UUID
	kind, target, mode, placeholder, status string
	generation                              int64
}

func lockBindings(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) ([]runtimeBinding, error) {
	rows, err := tx.Query(ctx, `SELECT b.secret_id,b.placement_kind,b.placement_target,b.mode,COALESCE(b.placeholder,''),s.status,s.revocation_generation
 FROM computer_secret_bindings b JOIN secrets s ON (s.environment_id,s.id)=(b.environment_id,b.secret_id)
 WHERE b.environment_id=$1 AND b.computer_id=$2 ORDER BY s.id,b.placement_kind,b.placement_target FOR SHARE OF s`, env, computer)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (runtimeBinding, error) {
		var b runtimeBinding
		err := row.Scan(&b.id, &b.kind, &b.target, &b.mode, &b.placeholder, &b.status, &b.generation)
		return b, err
	})
}
func recordExposure(ctx context.Context, tx pgx.Tx, c db.ComputerCommand, bindings []runtimeBinding) ([]secret.DeliveryEnvelope, map[string]string, error) {
	env, id := uuid.UUID(c.EnvironmentID.Bytes), uuid.UUID(c.ID.Bytes)
	ids := []uuid.UUID{}
	for _, b := range bindings {
		ids = append(ids, b.id)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secret_exposures(environment_id,command_id,secret_id,version_id,revocation_generation)
 SELECT $1,$2,s.id,s.current_version_id,s.revocation_generation FROM secrets s WHERE s.environment_id=$1 AND s.id=ANY($3::uuid[]) AND s.status='active'
 ON CONFLICT(environment_id,command_id,secret_id) WHERE command_id IS NOT NULL DO NOTHING`, env, id, ids); err != nil {
		return nil, nil, err
	}
	result := make([]secret.DeliveryEnvelope, 0, len(bindings))
	protected := map[string]string{}
	for _, b := range bindings {
		var version uuid.UUID
		var generation int64
		if err := tx.QueryRow(ctx, `SELECT version_id,revocation_generation FROM secret_exposures WHERE environment_id=$1 AND command_id=$2 AND secret_id=$3`, env, id, b.id).Scan(&version, &generation); err != nil {
			return nil, nil, err
		}
		if generation != b.generation || b.status != "active" {
			return nil, nil, secret.ErrDeliveryRevoked
		}
		v, err := db.New(tx).GetSecretVersion(ctx, db.GetSecretVersionParams{EnvironmentID: c.EnvironmentID, SecretID: pgvalue.UUID(b.id), VersionID: pgvalue.UUID(version)})
		if err != nil {
			return nil, nil, err
		}
		result = append(result, secret.DeliveryEnvelope{Mode: b.mode, PlacementKind: b.kind, PlacementTarget: b.target, Secret: db.Secret{ID: pgvalue.UUID(b.id), EnvironmentID: c.EnvironmentID, Status: b.status}, Version: v})
		if b.mode == "protected" {
			protected[b.target] = secret.RuntimeSelector(env, "command", id, 0, b.placeholder)
		}
	}
	if len(protected) > 0 {
		var certificate []byte
		var expires time.Time
		if err := tx.QueryRow(ctx, `SELECT proxy_ca_certificate,proxy_ca_not_after FROM computers WHERE environment_id=$1 AND id=$2`, env, c.ComputerID).Scan(&certificate, &expires); err != nil {
			return nil, nil, err
		}
		if err := secret.ValidateProxyTrust(certificate, expires, time.Now()); err != nil {
			return nil, nil, err
		}
	}
	return result, protected, nil
}
