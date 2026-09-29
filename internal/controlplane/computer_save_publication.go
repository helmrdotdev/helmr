package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func computerSavePublicationKey(r db.ComputerInstance) []byte {
	return computerPublicationKey("save/"+strconv.FormatInt(r.SaveSequence, 10), r.ID, r.SaveDiskVersionID)
}

// recordComputerSaveObject revalidates live authority on both sides of remote
// storage I/O. Only the exact operation may certify or reuse its retained bytes.
func (s *Server) recordComputerSaveObject(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest, inspection blockformat.ObjectInspection, operation string) error {
	descriptor, err := describeComputerObject(inspection)
	if err != nil {
		return err
	}
	if operation != "register" && operation != "certify" && operation != "reuse" {
		return errors.New("invalid save object operation")
	}
	var uploaded *cas.Object
	apply := func(mode string) error {
		_, err := s.withComputerSave(ctx, worker, request, computerSaveWrite, func(tx pgx.Tx, q *db.Queries, r db.ComputerInstance) error {
			key := computerSavePublicationKey(r)
			if mode == "verify" {
				row, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, Digest: descriptor.digest})
				if err != nil {
					return err
				}
				var prior blockformat.ObjectInspection
				if err = json.Unmarshal(row.Inspection, &prior); err != nil {
					return err
				}
				if !reflect.DeepEqual(prior, inspection) {
					return computerObjectConflict("save object differs from registered inspection")
				}
				_, err = q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: r.ID, PublicationKey: key, InstanceDesiredVersion: r.DesiredVersion, Digest: descriptor.digest})
				return err
			}
			keys, err := q.ListInstanceComputerSourceKeys(ctx, r.ID)
			if err != nil {
				return err
			}
			allowed := make(map[string]bool, len(keys)+1)
			for _, k := range keys {
				allowed[pgvalue.UUIDString(k.ID)] = true
			}
			write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID})
			if err != nil {
				return err
			}
			if !r.WriteKeyID.Valid || r.WriteKeyID != write.ID {
				return computerObjectConflict("save write key is not retained")
			}
			allowed[pgvalue.UUIDString(write.ID)] = true
			owner := dispatch.ComputerPreparation{OrgID: r.OrgID, ProjectID: r.ProjectID, EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, LogicalBytes: r.ReservedGuestEphemeralDiskBytes}
			return recordComputerObjectLocked(ctx, tx, owner, r.ID, key, r.DesiredVersion, inspection, uploaded, mode == "reuse", allowed)
		})
		return err
	}
	if operation == "certify" {
		if err = apply("verify"); err != nil {
			return err
		}
		stored, err := s.cas.Stat(ctx, descriptor.digest)
		if err != nil {
			return err
		}
		uploaded = &stored
	}
	return apply(operation)
}

// publishComputerSave atomically records the exact receipt, retained generation
// and saved head. It deliberately leaves the pending slot and object pins intact:
// upload success does not prove that the host has adopted its durable source.
func (s *Server) publishComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest, root disk.GenerationRoot) (computerPublicationResult, error) {
	var zero computerPublicationResult
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		return zero, err
	}
	fingerprint, err := computerSaveFingerprint(request, root)
	if err != nil {
		return zero, err
	}
	id, err := parseCanonicalUUID("save_id", request.SaveID)
	if err != nil {
		return zero, err
	}
	receipt, err := computerSaveReceiptParams(worker, request)
	if err != nil {
		return zero, err
	}
	replay := func() (computerPublicationResult, error) {
		v, err := s.db.GetWorkerComputerSave(ctx, receipt)
		if err != nil {
			return zero, err
		}
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return zero, computerObjectConflict("save publication differs from committed request")
		}
		return computerPublicationResult{ComputerID: v.ComputerID, VersionID: v.ID}, nil
	}
	if result, err := replay(); !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	var result computerPublicationResult
	_, err = s.withComputerSave(ctx, worker, request, computerSaveWrite, func(tx pgx.Tx, q *db.Queries, r db.ComputerInstance) error {
		if root.LogicalBytes != r.ReservedGuestEphemeralDiskBytes {
			return computerObjectConflict("save capacity differs from Computer")
		}
		object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, Digest: root.Pack.Digest})
		if err != nil {
			return err
		}
		if !object.Certified.Bool {
			return computerObjectConflict("save root is not certified")
		}
		var inspected blockformat.ObjectInspection
		if err = json.Unmarshal(object.Inspection, &inspected); err != nil {
			return err
		}
		if inspected.Pack == nil {
			return computerObjectConflict("save root is not a pack")
		}
		if err = inspected.Pack.CheckRoot(locator, root.LogicalBytes); err != nil {
			return err
		}
		if _, err = q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: r.ID, PublicationKey: computerSavePublicationKey(r), InstanceDesiredVersion: r.DesiredVersion, Digest: root.Pack.Digest}); err != nil {
			return err
		}
		encoded, err := json.Marshal(root)
		if err != nil {
			return err
		}
		v, err := q.PublishComputerInstanceSave(ctx, db.PublishComputerInstanceSaveParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, WorkerHostID: r.WorkerHostID, WorkerEpoch: r.WorkerEpoch, WriterGeneration: r.WriterGeneration, WriterTokenHash: r.WriterTokenHash, DesiredVersion: r.DesiredVersion, SaveID: pgvalue.UUID(id), Sequence: request.Sequence, RootPackDigest: pgvalue.Text(root.Pack.Digest), LogicalBytes: root.LogicalBytes, Fingerprint: fingerprint[:], Locator: encoded})
		if err != nil {
			return err
		}
		result = computerPublicationResult{ComputerID: v.ComputerID, VersionID: v.ID}
		return nil
	})
	if err != nil {
		if historical, e := replay(); !errors.Is(e, pgx.ErrNoRows) {
			return historical, e
		}
		return zero, err
	}
	return result, nil
}

// abandonComputerSave is called only after the host has cancelled and joined
// every producer/upload for this operation. Published saves require source
// adoption instead; they cannot be abandoned. Lost execution authority leaves
// retention to the existing physical Runtime reclamation path. Abandonment keeps
// object pins until that reclamation: the live disk can still reuse the same
// ciphertext in a later save or checkpoint, even after this producer has joined.
func (s *Server) abandonComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest) error {
	receipt, err := computerSaveReceiptParams(worker, request)
	if err != nil {
		return badRequest(err)
	}
	params := db.IsComputerSaveAbandonedParams{Sequence: request.Sequence, ComputerInstanceID: receipt.ComputerInstanceID, EnvironmentID: receipt.EnvironmentID, WorkerHostID: receipt.WorkerHostID, WorkerGroupID: receipt.WorkerGroupID, WorkerEpoch: receipt.WorkerEpoch, WriterGeneration: receipt.WriterGeneration}
	absent := func() bool {
		result, err := s.db.IsComputerSaveAbandoned(ctx, params)
		return err == nil && result.Valid && result.Bool
	}
	// This acknowledges the desired absence, not that an arbitrary SaveID was
	// once admitted. No mutation is permitted at an already retired sequence.
	if absent() {
		return nil
	}
	_, err = s.withComputerSave(ctx, worker, request, computerSaveAbandon, func(tx pgx.Tx, q *db.Queries, r db.ComputerInstance) error {
		cleared, err := q.AbandonComputerInstanceSave(ctx, db.AbandonComputerInstanceSaveParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, WriterGeneration: r.WriterGeneration, WriterTokenHash: r.WriterTokenHash, WorkerHostID: r.WorkerHostID, WorkerEpoch: r.WorkerEpoch, Sequence: r.SaveSequence, SaveID: r.SaveDiskVersionID})
		if err != nil {
			return err
		}
		if cleared != 1 {
			return computerObjectConflict("save operation changed during abandonment")
		}
		return nil
	})
	if err != nil && absent() {
		return nil
	}
	return err
}

func computerSaveFingerprint(request workerapi.ComputerSaveBeginRequest, root disk.GenerationRoot) ([32]byte, error) {
	raw, err := json.Marshal(struct {
		Request workerapi.ComputerSaveBeginRequest
		Root    disk.GenerationRoot
	}{request, root})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
