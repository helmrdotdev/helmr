package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type initialComputerPublication struct {
	Root   computer.GenerationRoot `json:"root"`
	Config oci.RuntimeConfig       `json:"config"`
}

type computerPublicationResult struct {
	ComputerID, VersionID pgtype.UUID
}

// publishInitialComputerGeneration is the generation publication owner. Its
// caller authenticates the Worker; the fence is not a caller-selected identity.
// It does not upload, release pins or authorize guest execution. The VM storage
// cutover must call this only after all referenced objects have been certified.
func (s *Server) publishInitialComputerGeneration(ctx context.Context, fence computerKeyFence, input initialComputerPublication) (computerPublicationResult, error) {
	empty := computerPublicationResult{}
	locator, err := input.Root.Locator(input.Root.LogicalBytes)
	if err != nil {
		return empty, err
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return empty, err
	}
	fingerprint := sha256.Sum256(canonical)
	replay := func() (computerPublicationResult, error) {
		v, err := s.db.GetWorkerInitialComputerVersion(ctx, db.GetWorkerInitialComputerVersionParams{RuntimeInstanceID: fence.RuntimeID, DesiredVersion: pgtype.Int8{Int64: fence.DesiredVersion, Valid: true}, WorkerInstanceID: fence.WorkerID, WorkerGroupID: fence.WorkerGroupID, WorkerEpoch: fence.WorkerEpoch})
		if err != nil {
			return empty, err
		}
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return empty, errors.New("initial Computer publication differs from committed request")
		}
		return computerPublicationResult{ComputerID: v.WorkspaceID, VersionID: v.ID}, nil
	}
	if result, err := replay(); !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	owner, err := dispatch.LockComputerPreparation(ctx, tx, fence.ComputerPreparationFence)
	if err != nil {
		// A competing exact publisher may have committed while the locks waited.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if result, replayErr := replay(); !errors.Is(replayErr, pgx.ErrNoRows) {
			return result, replayErr
		}
		return empty, err
	}
	if input.Root.LogicalBytes != owner.LogicalBytes {
		return empty, errors.New("initial root capacity differs from preparation")
	}
	var claims bool
	if err = tx.QueryRow(ctx, `SELECT w.claim_version=$3 AND g.claim_version=$4 FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, fence.WorkerID, fence.WorkerGroupID, fence.ClaimVersion, fence.GroupClaimVersion).Scan(&claims); err != nil {
		return empty, err
	}
	if !claims {
		return empty, errors.New("publication worker claims changed")
	}
	q := db.New(tx)
	object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, Digest: input.Root.Pack.Digest})
	if err != nil {
		return empty, err
	}
	if !object.Certified.Bool {
		return empty, errors.New("initial root is not certified")
	}
	var evidence blockformat.ObjectInspection
	if err = json.Unmarshal(object.Inspection, &evidence); err != nil {
		return empty, err
	}
	if evidence.Pack == nil {
		return empty, errors.New("initial root is not an inspected pack")
	}
	if err = evidence.Pack.CheckRoot(locator, owner.LogicalBytes); err != nil {
		return empty, err
	}
	var retained bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_computer_object_pins WHERE runtime_instance_id=$1 AND publication_key=$4 AND runtime_desired_version=$2 AND digest=$3)`, fence.RuntimeID, fence.DesiredVersion, input.Root.Pack.Digest, computerPublicationKey("initial", fence.RuntimeID, fence.RuntimeID)).Scan(&retained); err != nil {
		return empty, err
	}
	if !retained {
		return empty, errors.New("initial root is not retained by publisher")
	}
	rawRoot, err := json.Marshal(input.Root)
	if err != nil {
		return empty, err
	}
	rawConfig, err := json.Marshal(input.Config)
	if err != nil {
		return empty, err
	}
	version, err := q.PublishInitialComputerVersion(ctx, db.PublishInitialComputerVersionParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, VersionID: owner.VersionID, RuntimeInstanceID: fence.RuntimeID, DesiredVersion: pgtype.Int8{Int64: fence.DesiredVersion, Valid: true}, Fingerprint: fingerprint[:], ContentDigest: pgvalue.Text(input.Root.Pack.Digest), LogicalBytes: owner.LogicalBytes, Locator: rawRoot, InitialConfig: rawConfig})
	if err != nil {
		return empty, err
	}
	// Publication and the preparing Runtime's source retention are one commit.
	// Otherwise the next source/key request has no retained root, and the
	// version could be reclaimed between publication and preparation.
	pinned, err := q.PinRuntimeComputerSource(ctx, db.PinRuntimeComputerSourceParams{
		RuntimeInstanceID: fence.RuntimeID, EnvironmentID: owner.EnvironmentID,
		ComputerID: owner.ComputerID, VersionID: version.ID,
	})
	if err != nil {
		return empty, err
	}
	if pinned != 1 {
		return empty, errors.New("initial generation has no matching Runtime source reservation")
	}
	if err = owner.CheckDeadlines(ctx, tx); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return computerPublicationResult{ComputerID: version.WorkspaceID, VersionID: version.ID}, nil
}
