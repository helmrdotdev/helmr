package controlplane

import (
	"bytes"
	"crypto/sha256"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerCheckpointPublicationThroughHTTP(t *testing.T) {
	f := agenttest.New(t)
	store := newTestUploadStore(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.CAS = store }))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	foreign := foreignAgentComputerClient(t, f, server.URL)
	request, result, capture := beginWorkerAgentCapture(t, f, client)
	if err := client.SealAgentComputerCapture(t.Context(), workerapi.AgentComputerReceiptRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Receipt: agentComputerBytes(t, &agentv1.ComputerSessionReceipt{CheckpointId: capture.CheckpointId, DesiredVersion: capture.DesiredVersion, Frozen: true})}); err != nil {
		t.Fatal(err)
	}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	keyID := uuid.NewV7()
	key := bytes.Repeat([]byte{21}, 32)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'test',decode('01','hex'))`, keyID, f.Environment, f.Computer)
	if _, err := agent.BindComputerLeaseDisk(t.Context(), f.Pool, host, f.Environment, f.Computer, 1, keyID); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	candidate, err := disk.CaptureInitialVersion(t.Context(), disk.VersionCapture{Disk: file, Capacity: 1 << 20, StagingParent: t.TempDir(), Scope: f.Environment.String(), KeyID: keyID.String(), Key: key, Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 4 << 20, MaxObjects: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	save := workerapi.AgentSave{EnvironmentID: request.EnvironmentID, SaveID: result.SaveID, LeaseEpoch: 1}
	locator, err := candidate.Publish(t.Context(), saveHTTPPublication{client: client, save: save, store: store})
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	publication := workerapi.AgentSavePublication{Save: save, Root: root, Evidence: "coherent checkpoint cut"}
	if err := client.CaptureAgentSave(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"checkpoint":"runtime"}`)
	digest := sha256.Sum256(result.Capture)
	platform := "sha256:" + strings.Repeat("1", 64)
	manifest := computercheckpoint.Manifest{CheckpointID: uuid.MustParse(request.CheckpointID), ComputerID: f.Computer, InstanceID: f.Computer, LeaseEpoch: 1, ControlVersion: capture.DesiredVersion, CaptureDigest: digest[:], Config: config, Disk: root, Runtime: vm.CheckpointIdentity{RuntimeBackend: "firecracker", RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0", RuntimeID: platform, KernelDigest: platform, InitramfsDigest: platform, RootfsDigest: platform, VMConfigDigest: sha256sum.DigestBytes(config), VMVCPUCount: 1, CPUConfigDigest: platform}}
	for _, m := range capture.Sessions {
		manifest.Members = append(manifest.Members, computercheckpoint.Session{SessionID: uuid.MustParse(m.SessionId), ProcessEpoch: m.ProcessEpoch})
	}
	// These opaque objects test the CP's descriptor/authentication boundary. Actual
	// encryption and VM serialization are owned by separate host/runtime tests.
	descriptors := []*computercheckpoint.Object{&manifest.VMConfig, &manifest.VMState, &manifest.Memory, &manifest.ScratchDisk}
	media := []string{cas.CheckpointVMConfigMediaType, cas.CheckpointVMStateMediaType, cas.CheckpointMemoryMediaType, cas.CheckpointScratchDiskMediaType}
	for i, d := range descriptors {
		raw := []byte("opaque runtime object: " + media[i])
		*d = computercheckpoint.Object{Digest: sha256sum.DigestBytes(raw), SizeBytes: int64(len(raw)), MediaType: media[i]}
	}
	checkpoint := workerapi.AgentCheckpointPublication{EnvironmentID: request.EnvironmentID, Manifest: manifest}
	if err := foreign.RegisterAgentCheckpoint(t.Context(), checkpoint); err == nil {
		t.Fatal("foreign host registered checkpoint")
	}
	if err := client.RegisterAgentCheckpoint(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := client.CompleteAgentCheckpoint(t.Context(), checkpoint); err == nil {
		t.Fatal("missing objects made checkpoint ready")
	}
	for _, d := range descriptors {
		if _, err := store.Put(t.Context(), d.MediaType, strings.NewReader("opaque runtime object: "+d.MediaType)); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.CompleteAgentCheckpoint(t.Context(), checkpoint); err == nil {
		t.Fatal("unpublished disk made checkpoint ready")
	}
	if err := client.PublishAgentSave(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if err := foreign.CompleteAgentCheckpoint(t.Context(), checkpoint); err == nil {
		t.Fatal("foreign host completed checkpoint")
	}
	for range 2 {
		if err := client.CompleteAgentCheckpoint(t.Context(), checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	changed := checkpoint
	changed.Manifest.Memory.SizeBytes++
	if err := client.CompleteAgentCheckpoint(t.Context(), changed); err == nil {
		t.Fatal("changed committed manifest accepted")
	}
	id := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: f.Environment.String(), OwnerID: f.Computer.String(), InstanceID: f.Computer.String(), Epoch: 1}
	if _, err := client.ReadAgentCheckpoint(t.Context(), id); err == nil {
		t.Fatal("source allocation read restore image")
	}
	if _, err := foreign.ReadAgentCheckpoint(t.Context(), id); err == nil {
		t.Fatal("foreign host read image")
	}
	if err := client.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: workerapi.AgentComputerLeaseRequest{EnvironmentID: id.EnvironmentID, ComputerID: id.OwnerID, InstanceID: id.InstanceID, LeaseEpoch: id.Epoch}}); err != nil {
		t.Fatal(err)
	}
	// Seed only the target allocation for this transport test. Source fencing,
	// publication and encrypted disk certification above use their real owners.
	target := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,restored_from_save_id,delivered_at,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,status,base_root_id,write_key_id)
      SELECT l.environment_id,l.computer_id,2,l.worker_host_id,l.worker_epoch,clock_timestamp()+interval '1 minute',$3,decode(repeat('42',32),'hex'),s.id,clock_timestamp(),l.reserved_cpu_millis,l.reserved_memory_bytes,l.reserved_scratch_bytes,l.vm_platform_id,l.vm_vcpu_count,l.cpu_config_digest,'acquiring',s.root_id,l.write_key_id
      FROM computer_leases l JOIN computer_saves s ON s.environment_id=l.environment_id AND s.id=$4 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=1`, f.Environment, f.Computer, target, result.SaveID)
	id.InstanceID = target.String()
	id.Epoch = 2
	got, err := client.ReadAgentCheckpoint(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := manifest.Encode()
	actual, _ := got.Encode()
	if !bytes.Equal(want, actual) {
		t.Fatal("restore transport changed manifest")
	}
	if _, err := foreign.ReadAgentCheckpoint(t.Context(), id); err == nil {
		t.Fatal("foreign host read target image")
	}

}
