package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

type saveHTTPPublication struct {
	client *workerclient.Client
	save   workerapi.AgentSave
	store  testUploadStore
}

func (p saveHTTPPublication) Register(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.RegisterAgentSaveObject(ctx, workerapi.AgentSaveObject{Save: p.save, Inspection: i})
}
func (p saveHTTPPublication) Certify(ctx context.Context, i blockformat.ObjectInspection) error {
	return p.client.CertifyAgentSaveObject(ctx, workerapi.AgentSaveObject{Save: p.save, Inspection: i})
}
func (p saveHTTPPublication) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	return p.store.Put(ctx, d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes))
}

func TestWorkerAgentSavePublicationThroughHTTP(t *testing.T) {
	f := agenttest.New(t)
	store := newTestUploadStore(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.CAS = store }))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	a, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "save-http", Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Dispatch(t.Context(), f.Pool, execution); err != nil {
		t.Fatal(err)
	}
	if err := agent.CloseProcessing(t.Context(), f.Pool, execution, a.TurnID); err != nil {
		t.Fatal(err)
	}
	save, err := agent.RecordResult(t.Context(), f.Pool, execution, a.TurnID, json.RawMessage(`{"saved":true}`), "native work drained")
	if err != nil {
		t.Fatal(err)
	}
	allocation := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: f.Environment.String(), OwnerID: f.Computer.String(), InstanceID: f.Computer.String(), Epoch: 1}
	next, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation})
	if err != nil || next.Save == nil || next.Save.SaveID != save.ID.String() || next.Sequence != save.Sequence {
		t.Fatalf("durable request: %+v %v", next, err)
	}
	wrongAllocation := allocation
	wrongAllocation.InstanceID = uuid.NewV7().String()
	if _, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: wrongAllocation}); err == nil {
		t.Fatal("foreign physical allocation discovered save")
	}
	keyID := uuid.NewV7()
	key := bytes.Repeat([]byte{21}, 32)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'test',decode('01','hex'))`, keyID, f.Environment, f.Computer)
	if _, err := agent.BindComputerLeaseDisk(t.Context(), f.Pool, host, f.Environment, f.Computer, 1, keyID); err != nil {
		t.Fatal(err)
	}
	// The file is a deterministic quiescent capture fixture; this test verifies
	// real encrypted bytes, HTTP authentication and PostgreSQL publication.
	file, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte{31}, 4096)
	if _, err := file.WriteAt(content, 4096); err != nil {
		t.Fatal(err)
	}
	candidate, err := disk.CaptureInitialVersion(t.Context(), disk.VersionCapture{Disk: file, Capacity: 1 << 20, StagingParent: t.TempDir(), Scope: f.Environment.String(), KeyID: keyID.String(), Key: key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 4 << 20, MaxObjects: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	locator, err := candidate.Publish(t.Context(), saveHTTPPublication{client: client, save: *next.Save, store: store})
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentSavePublication{Save: *next.Save, Root: root, Evidence: "operation-bound capture"}
	if err := client.PublishAgentSave(t.Context(), request); err == nil {
		t.Fatal("published before capture receipt")
	}
	wrong := request
	wrong.Save.LeaseEpoch++
	if err := client.CaptureAgentSave(t.Context(), wrong); err == nil {
		t.Fatal("wrong lease recorded capture")
	}
	if err := client.CaptureAgentSave(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.PublishAgentSave(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	background, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation, BackgroundDue: true})
	if err != nil || background.Save == nil || background.Save.SaveID == next.Save.SaveID || background.Sequence != next.Sequence+1 {
		t.Fatalf("background HTTP admission: %+v %v", background, err)
	}
	if _, err := candidate.Publish(t.Context(), saveHTTPPublication{client: client, save: *background.Save, store: store}); err != nil {
		t.Fatal(err)
	}
	backgroundRequest := workerapi.AgentSavePublication{Save: *background.Save, Root: root, Evidence: "optional operation-bound cut"}
	if err := client.CaptureAgentSave(t.Context(), backgroundRequest); err != nil {
		t.Fatal(err)
	}
	if err := client.PublishAgentSave(t.Context(), backgroundRequest); err != nil {
		t.Fatal(err)
	}
	var backgroundHead bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.recovery_save_id=$2 AND s.turn_id IS NULL AND t.status='finalizing' AND t.completion_save_id IS NULL FROM computers c JOIN computer_saves s ON s.environment_id=c.environment_id AND s.id=$2 JOIN turns t ON t.environment_id=c.environment_id AND t.id=$3 WHERE c.environment_id=$1 AND c.id=$4`, f.Environment, background.Save.SaveID, a.TurnID, f.Computer).Scan(&backgroundHead); err != nil || !backgroundHead {
		t.Fatalf("optional publication completed Turn: %v %v", backgroundHead, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.Environment)
	if err := client.PublishAgentSave(t.Context(), request); err != nil {
		t.Fatalf("committed receipt after expiry: %v", err)
	}
	if err := agent.Complete(t.Context(), f.Pool, f.Environment, f.Session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	tree, err := blockformat.OpenTree(t.Context(), store, f.Environment.String(), map[string][]byte{keyID.String(): key}, locator)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := tree.ReadBlock(t.Context(), 1)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("published bytes differ: %v", err)
	}
}

func TestWorkerSaveDiscoveryContentionIsTemporary(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	// Resolve worker credentials before holding a conflicting supply lock.
	if _, err := client.RenewAgentAuthority(t.Context(), runtimeTestSession(f)); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM worker_hosts WHERE id=$1 FOR UPDATE`, f.Worker); err != nil {
		t.Fatal(err)
	}
	allocation := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: f.Environment.String(), OwnerID: f.Computer.String(), InstanceID: f.Computer.String(), Epoch: 1}
	if _, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation}); !httpclient.IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("contention must be temporary: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if result, err := client.NextAgentSave(t.Context(), workerapi.AgentSaveDiscovery{AllocationIdentity: allocation}); err != nil || result.Save != nil {
		t.Fatalf("unblocked discovery: %+v %v", result, err)
	}
}

func TestWorkerSaveTerminalErrorIsNotRetryable(t *testing.T) {
	server := &Server{log: discardTestLogger()}
	for _, err := range []error{agent.ErrTerminal, fmt.Errorf("save disposition: %w", agent.ErrTerminal)} {
		response := httptest.NewRecorder()
		server.writeAgentSaveError(response, err)
		if response.Code != http.StatusConflict {
			t.Fatalf("terminal save response: %d", response.Code)
		}
	}
	response := httptest.NewRecorder()
	server.writeAgentSaveError(response, fmt.Errorf("database unavailable"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown save outcome response: %d", response.Code)
	}
}
