package control

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerWorkspaceExecFailureDoesNotClassifyUnknownInfrastructureError(t *testing.T) {
	failure, ok := workerWorkspaceExecFailure(errors.New("database connection lost"))
	if ok || failure.Code != "" {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}

func TestWorkerWorkspaceFileReadCommitsBeforeCAS(t *testing.T) {
	_, claimStore, worker, receipt := validRunLeaseRenewalFixture(t)
	claimStore.authority.attempt.EntrypointEnteredAt = pgtype.Timestamptz{Valid: true}

	workspaceID := claimStore.authority.workspace.ID
	versionID := claimStore.authority.workspace.HeadVersionID
	artifactID := pgvalue.UUID(uuid.New())
	record := claimStore.authority.workspace
	record.EnvironmentID = claimStore.renewal.EnvironmentID
	record.PublicID = "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	store := &workerWorkspaceFileStore{
		runLeaseClaimStore: claimStore,
		workspace:          record,
		version: db.WorkspaceVersion{
			ID:            versionID,
			PublicID:      "wsv_aaaaaaaaaaaaaaaaaaaaaaaaaa",
			EnvironmentID: record.EnvironmentID,
			WorkspaceID:   workspaceID,
			ArtifactID:    artifactID,
		},
		artifact: db.Artifact{
			ID:        artifactID,
			Kind:      db.ArtifactKindWorkspaceVersion,
			Digest:    "sha256:resolved",
			MediaType: workspace.ArtifactMediaType,
		},
	}
	var artifact bytes.Buffer
	writer := tar.NewWriter(&artifact)
	content := []byte("hello")
	if err := writer.WriteHeader(&tar.Header{
		Name: "src/main.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db:  store,
		cas: &workerWorkspaceFileCAS{store: store, body: artifact.Bytes()},
	}
	requestBody, err := json.Marshal(api.WorkerReadWorkspaceFileRequest{
		WorkerRetrieveWorkspaceRequest: api.WorkerRetrieveWorkspaceRequest{
			Lease:         receipt,
			CorrelationID: uuid.Must(uuid.NewV7()).String(),
			Workspace:     api.WorkerWorkspaceAddress{WorkspaceID: record.PublicID},
		},
		Path: "src/main.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/worker/workspaces/files/read", bytes.NewReader(requestBody))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, worker))
	response := httptest.NewRecorder()

	server.workerReadWorkspaceFile(response, request)

	if response.Code != 200 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var result api.WorkerReadWorkspaceFileResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Completed == nil || result.Completed.DataBase64 != "aGVsbG8=" {
		t.Fatalf("response = %+v", result)
	}
	if !store.committed {
		t.Fatal("transaction was not committed")
	}
}

type workerWorkspaceFileStore struct {
	*runLeaseClaimStore
	workspace db.Workspace
	version   db.WorkspaceVersion
	artifact  db.Artifact
	committed bool
}

func (store *workerWorkspaceFileStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	store.committed = false
	return store, workerWorkspaceFileTransaction{store: store}, nil
}

func (store *workerWorkspaceFileStore) ResolveWorkspaceTarget(
	context.Context,
	db.ResolveWorkspaceTargetParams,
) (pgtype.UUID, error) {
	return store.workspace.ID, nil
}

func (store *workerWorkspaceFileStore) GetWorkspace(
	context.Context,
	db.GetWorkspaceParams,
) (db.Workspace, error) {
	return store.workspace, nil
}

func (store *workerWorkspaceFileStore) GetWorkspaceVersion(
	context.Context,
	db.GetWorkspaceVersionParams,
) (db.WorkspaceVersion, error) {
	return store.version, nil
}

func (store *workerWorkspaceFileStore) GetWorkspaceVersionArtifact(
	context.Context,
	db.GetWorkspaceVersionArtifactParams,
) (db.Artifact, error) {
	return store.artifact, nil
}

type workerWorkspaceFileTransaction struct {
	store *workerWorkspaceFileStore
}

func (tx workerWorkspaceFileTransaction) Commit(context.Context) error {
	tx.store.committed = true
	return nil
}

func (tx workerWorkspaceFileTransaction) Rollback(context.Context) error {
	return nil
}

type workerWorkspaceFileCAS struct {
	store *workerWorkspaceFileStore
	body  []byte
}

func (*workerWorkspaceFileCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS stat")
}

func (store *workerWorkspaceFileCAS) Get(context.Context, string) (io.ReadCloser, error) {
	if !store.store.committed {
		return nil, errors.New("CAS read started before transaction commit")
	}
	return io.NopCloser(bytes.NewReader(store.body)), nil
}

func (*workerWorkspaceFileCAS) Put(context.Context, string, io.Reader) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected CAS put")
}

func (*workerWorkspaceFileCAS) Stage(context.Context, string) (cas.Stage, error) {
	return nil, errors.New("unexpected CAS stage")
}

func (*workerWorkspaceFileCAS) Delete(context.Context, string) error {
	return errors.New("unexpected CAS delete")
}
