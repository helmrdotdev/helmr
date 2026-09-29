package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type initialComputerPublication struct {
	Root   disk.GenerationRoot `json:"root"`
	Config oci.RuntimeConfig   `json:"config"`
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
		v, err := s.db.GetWorkerInitialComputerDiskVersion(ctx, db.GetWorkerInitialComputerDiskVersionParams{ComputerInstanceID: fence.RuntimeID, DesiredVersion: pgtype.Int8{Int64: fence.DesiredVersion, Valid: true}, WorkerHostID: fence.WorkerID, WorkerGroupID: fence.WorkerGroupID, WorkerEpoch: fence.WorkerEpoch})
		if err != nil {
			return empty, err
		}
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return empty, errors.New("initial Computer publication differs from committed request")
		}
		return computerPublicationResult{ComputerID: v.ComputerID, VersionID: v.ID}, nil
	}
	if result, err := replay(); !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	var published computerPublicationResult
	preparationLockFailed := false
	err = s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		owner, err := dispatch.LockComputerPreparation(ctx, tx, fence.ComputerPreparationFence)
		if err != nil {
			preparationLockFailed = true
			return err
		}
		if input.Root.LogicalBytes != owner.LogicalBytes {
			return errors.New("initial root capacity differs from preparation")
		}
		if err = fence.checkLockedClaims(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, Digest: input.Root.Pack.Digest})
		if err != nil {
			return err
		}
		if !object.Certified.Bool {
			return errors.New("initial root is not certified")
		}
		var evidence blockformat.ObjectInspection
		if err = json.Unmarshal(object.Inspection, &evidence); err != nil {
			return err
		}
		if evidence.Pack == nil {
			return errors.New("initial root is not an inspected pack")
		}
		if err = evidence.Pack.CheckRoot(locator, owner.LogicalBytes); err != nil {
			return err
		}
		var retained bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_object_pins WHERE computer_instance_id=$1 AND publication_key=$4 AND instance_desired_version=$2 AND digest=$3)`, fence.RuntimeID, fence.DesiredVersion, input.Root.Pack.Digest, computerPublicationKey("initial", fence.RuntimeID, fence.RuntimeID)).Scan(&retained); err != nil {
			return err
		}
		if !retained {
			return errors.New("initial root is not retained by publisher")
		}
		rawRoot, err := json.Marshal(input.Root)
		if err != nil {
			return err
		}
		rawConfig, err := json.Marshal(input.Config)
		if err != nil {
			return err
		}
		version, err := q.PublishInitialComputerDiskVersion(ctx, db.PublishInitialComputerDiskVersionParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, VersionID: owner.VersionID, ComputerInstanceID: fence.RuntimeID, DesiredVersion: pgtype.Int8{Int64: fence.DesiredVersion, Valid: true}, Fingerprint: fingerprint[:], RootPackDigest: pgvalue.Text(input.Root.Pack.Digest), LogicalBytes: owner.LogicalBytes, Locator: rawRoot, InitialConfig: rawConfig})
		if err != nil {
			return err
		}
		// Publication and the preparing Runtime's source retention are one commit.
		// Otherwise the next source/key request has no retained root, and the
		// version could be reclaimed between publication and preparation.
		pinned, err := q.PinInstanceComputerSource(ctx, db.PinInstanceComputerSourceParams{
			ComputerInstanceID: fence.RuntimeID, EnvironmentID: owner.EnvironmentID,
			ComputerID: owner.ComputerID, VersionID: version.ID,
		})
		if err != nil {
			return err
		}
		if pinned != 1 {
			return errors.New("initial generation has no matching Runtime source reservation")
		}
		if err = owner.CheckDeadlines(ctx, tx); err != nil {
			return err
		}
		published = computerPublicationResult{ComputerID: version.ComputerID, VersionID: version.ID}
		return nil
	})
	if err != nil {
		if preparationLockFailed {
			// A competing exact publisher may have committed while the locks waited.
			if result, replayErr := replay(); !errors.Is(replayErr, pgx.ErrNoRows) {
				return result, replayErr
			}
		}
		return empty, err
	}
	return published, nil
}

func (s *Server) writeComputerPublicationError(w http.ResponseWriter, err error) {
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleRunFinalization) || isDeterministicWorkerAdmission(err) {
		writeError(w, conflict(errors.New("computer publication authority changed")))
		return
	}
	if errorStatus(err) < 500 {
		writeError(w, err)
		return
	}
	s.log.Error("Computer object publication failed", "error", err)
	writeError(w, errors.New("computer object publication failed"))
}
