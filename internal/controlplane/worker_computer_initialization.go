package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Server) workerRegisterComputerInitialization(w http.ResponseWriter, r *http.Request) {
	s.workerComputerInitialization(w, r, false)
}
func (s *Server) workerPublishComputerInitialization(w http.ResponseWriter, r *http.Request) {
	s.workerComputerInitialization(w, r, true)
}

func (s *Server) workerComputerInitialization(w http.ResponseWriter, r *http.Request, publish bool) {
	var request workerapi.ComputerInitializationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	runtimeID, err := ids.Parse(request.RuntimeInstanceID)
	if err != nil || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("runtime identity and desired version are required")))
		return
	}
	disk := computer.DiskArtifact{Object: cas.Descriptor{Digest: request.Disk.Digest, SizeBytes: request.Disk.SizeBytes, MediaType: request.Disk.MediaType}, LogicalBytes: request.LogicalBytes}
	if err = disk.Validate(request.LogicalBytes); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var config oci.RuntimeConfig
	var configObject map[string]json.RawMessage
	if err = json.Unmarshal(request.InitialConfig, &configObject); err != nil || configObject == nil {
		writeError(w, badRequest(errors.New("initial configuration must be an object")))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(request.InitialConfig))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		writeError(w, badRequest(errors.New("invalid initial configuration")))
		return
	}
	// The trusted host supplies the pinned OCI configuration it verified while
	// seeding. Retain its canonical typed representation in the immutable receipt.
	encoded, err := json.Marshal(config)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	request.InitialConfig = encoded
	fence := dispatch.ComputerPreparationFence{RuntimeID: pgvalue.UUID(runtimeID), WorkerID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch, DesiredVersion: request.DesiredVersion}
	receiptParams := db.GetWorkerComputerInitializationParams{RuntimeInstanceID: fence.RuntimeID, RuntimeDesiredVersion: fence.DesiredVersion, WorkerInstanceID: fence.WorkerID, WorkerGroupID: fence.WorkerGroupID, WorkerEpoch: fence.WorkerEpoch}
	// Receipt readback is deliberately before current-authority checks. It never
	// reopens preparation and cannot register different bytes after a lost reply.
	existing, readErr := s.db.GetWorkerComputerInitialization(r.Context(), receiptParams)
	if readErr == nil {
		if !computerInitializationMatches(existing, request) {
			writeError(w, conflict(errors.New("initial disk differs from its registered candidate")))
			return
		}
		if existing.Status == "consumed" {
			writeJSON(w, http.StatusOK, computerInitializationResponse(existing))
			return
		}
		if existing.Status != "registered" {
			writeError(w, conflict(errors.New("initial disk candidate is abandoned")))
			return
		}
	} else if !errors.Is(readErr, pgx.ErrNoRows) {
		writeError(w, errors.New("read initial disk receipt"))
		return
	}
	if publish {
		if readErr != nil {
			writeError(w, conflict(errors.New("initial disk must be registered before upload")))
			return
		}
		// Immutable host publication verifies the upload bytes. CP checks the
		// stored object's descriptor outside the transaction; restore verifies
		// the digest and authenticated encryption before using disk contents.
		if s.cas == nil {
			writeError(w, errors.New("initial disk object store is unavailable"))
			return
		}
		object, statErr := s.cas.Stat(r.Context(), existing.Digest)
		if statErr != nil {
			writeError(w, unavailable(fmt.Errorf("inspect initial disk storage: %w", statErr)))
			return
		}
		if object.Digest != existing.Digest || object.SizeBytes != existing.SizeBytes || object.MediaType != existing.MediaType {
			writeError(w, conflict(errors.New("registered initial disk is not available in storage")))
			return
		}
	}
	row, err := s.commitComputerInitialization(r.Context(), fence, request, publish)
	if err != nil {
		// Another exact publisher may have committed while this request waited.
		// Resolve only the immutable successful receipt after transaction rollback.
		replay, replayErr := s.db.GetWorkerComputerInitialization(r.Context(), receiptParams)
		if replayErr == nil && replay.Status == "consumed" && computerInitializationMatches(replay, request) {
			writeJSON(w, http.StatusOK, computerInitializationResponse(replay))
			return
		}
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, dispatch.ErrCandidateChanged) {
			writeError(w, conflict(errors.New("initial disk preparation authority is stale")))
			return
		}
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23505" && constraint.ConstraintName == "computer_initializations_digest_key" {
			writeError(w, conflict(errors.New("initial disk ciphertext is already owned by another preparation")))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, computerInitializationResponse(row))
}

func computerInitializationMatches(row db.ComputerInitialization, request workerapi.ComputerInitializationRequest) bool {
	var recorded, requested oci.RuntimeConfig
	return row.Digest == request.Disk.Digest && row.SizeBytes == request.Disk.SizeBytes && row.MediaType == request.Disk.MediaType && row.LogicalBytes == request.LogicalBytes &&
		json.Unmarshal(row.InitialConfig, &recorded) == nil && json.Unmarshal(request.InitialConfig, &requested) == nil && reflect.DeepEqual(recorded, requested)
}
func computerInitializationResponse(row db.ComputerInitialization) workerapi.ComputerInitializationResponse {
	return workerapi.ComputerInitializationResponse{ID: pgvalue.UUIDString(row.ID), ComputerID: pgvalue.UUIDString(row.ComputerID), VersionID: pgvalue.UUIDString(row.VersionID), Status: row.Status, ArtifactID: pgvalue.UUIDString(row.ArtifactID)}
}

func (s *Server) commitComputerInitialization(ctx context.Context, fence dispatch.ComputerPreparationFence, request workerapi.ComputerInitializationRequest, publish bool) (db.ComputerInitialization, error) {
	if s.tx == nil {
		return db.ComputerInitialization{}, errors.New("initial disk transaction authority unavailable")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.ComputerInitialization{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	authority, err := dispatch.LockComputerPreparation(ctx, tx, fence)
	if err != nil {
		return db.ComputerInitialization{}, err
	}
	if request.LogicalBytes != authority.LogicalBytes {
		return db.ComputerInitialization{}, conflict(errors.New("initial disk capacity differs from its reservation"))
	}
	q := db.New(tx)
	row, err := q.RegisterComputerInitialization(ctx, db.RegisterComputerInitializationParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: authority.EnvironmentID, ComputerID: authority.ComputerID, VersionID: authority.VersionID, RuntimeInstanceID: fence.RuntimeID,
		RuntimeDesiredVersion: fence.DesiredVersion, OwnershipGeneration: authority.OwnershipGeneration, WriterGeneration: authority.WriterGeneration,
		Digest: request.Disk.Digest, SizeBytes: request.Disk.SizeBytes, MediaType: request.Disk.MediaType, LogicalBytes: request.LogicalBytes, InitialConfig: request.InitialConfig,
	})
	if err != nil {
		return db.ComputerInitialization{}, err
	}
	if publish {
		if _, err = q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: authority.OrgID, Digest: row.Digest, SizeBytes: row.SizeBytes, MediaType: row.MediaType}); err != nil {
			return db.ComputerInitialization{}, err
		}
		artifact, err := q.CreateArtifact(ctx, db.CreateArtifactParams{ID: pgvalue.UUID(uuid.NewV7()), OrgID: authority.OrgID, ProjectID: authority.ProjectID, EnvironmentID: authority.EnvironmentID,
			Digest: row.Digest, SizeBytes: row.SizeBytes, MediaType: row.MediaType, Kind: "workspace_version", CreatedByWorkerInstanceID: fence.WorkerID})
		if err != nil {
			return db.ComputerInitialization{}, err
		}
		row, err = q.PublishComputerInitialization(ctx, db.PublishComputerInitializationParams{ID: row.ID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID, ArtifactID: artifact.ID})
		if err != nil {
			return db.ComputerInitialization{}, err
		}
	}
	if err = authority.CheckDeadlines(ctx, tx); err != nil {
		return db.ComputerInitialization{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return db.ComputerInitialization{}, fmt.Errorf("commit initial disk: %w", err)
	}
	return row, nil
}
