package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestWorkerComputerAllocationSourceBindsDiskAndBootWithoutProgram(t *testing.T) {
	f := agenttest.New(t)
	wrapper, err := computerkey.NewLocal("allocation-source-test", bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var org string
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id::text FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	scope, err := computerkey.EncryptionScope(org, f.Environment.String())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{9}, 32)
	defer clear(key)
	keyID := uuid.NewV7()
	envelope, err := wrapper.Wrap(t.Context(), scope, keyID.String(), key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: scope, ActiveKey: keyID.String(), Keys: map[string][]byte{keyID.String(): key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), 4096, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(locator, 4096)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := root.Digest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal(definition.ComputerSeedManifest{Profile: definition.ComputerSeedProfile, MediaType: definition.ComputerSeedMediaType, ArtifactDigest: "sha256:" + strings.Repeat("3", 64), Config: oci.RuntimeConfig{WorkingDir: "/project", Env: []string{"STATIC_VALUE=retained"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Storage certification is fixture state. The key broker and authenticated
	// HTTP path are real; OpenVersion below authenticates the returned key/root.
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO computer_data_keys(id,environment_id,wrapping_key_id,wrapped_key) VALUES($3,$1,$4,$5);
 INSERT INTO cas_blobs(digest,size_bytes) VALUES($6,$7);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$6,$7,'application/octet-stream' FROM environments WHERE id=$1;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$6,org_id,project_id,$7,'application/octet-stream','root',$8,'{}',clock_timestamp() FROM environments WHERE id=$1;
 INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($1,$6,$3,true);
 INSERT INTO computer_disk_roots(environment_id,id,locator) VALUES($1,$9,$10);
 UPDATE computers SET initial_root_id=$9,initial_root_digest=decode(substring($11::text from 8),'hex'),preparation_spec_id=$12 WHERE environment_id=$1 AND id=$2;
 UPDATE computer_preparation_specs SET seed=$13 WHERE environment_id=$1 AND id=$12;
 UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE environment_id=$1;
	`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Computer, keyID, envelope.WrappingKeyID, envelope.Ciphertext, root.Pack.Digest, root.Pack.SizeBytes, root.Pack.Rank, uuid.NewV7(), string(raw), digest, f.Deployment, string(seed))
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(cfg *ServerConfig) { cfg.ComputerKeys = wrapper }))
	defer server.Close()
	auth := seedHostSecret(t, f.Pool, f.Worker)
	client := auth.client(t, server.URL)
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: f.Environment.String(), OwnerID: f.Computer.String(), InstanceID: f.Computer.String(), Epoch: 1}
	source, err := client.ComputerAllocationSource(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Disk.Clear()
	if source.Disk.VersionID != digest || source.Disk.Root != root || source.Disk.WriteKeyID == keyID.String() || source.ImageConfig.WorkingDir != "/project" || source.RootfsDigest != "sha256:"+strings.Repeat("1", 64) {
		t.Fatal("source or boot identity changed")
	}
	keys := make(map[string][]byte)
	for _, k := range source.Disk.Keys {
		if k.Scope != scope {
			t.Fatal("key scope changed")
		}
		keys[k.ID] = k.Key
	}
	if _, err := disk.OpenVersion(t.Context(), store, scope, keys, source.Disk.Root, 4096); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(identity)
	response := postAgentComputerJSON(t, server, auth, "/worker/v1/allocations/computer/source", body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("key response permits caching or failed")
	}
	wrong := identity
	wrong.InstanceID = uuid.NewV7().String()
	if source, err := client.ComputerAllocationSource(t.Context(), wrong); err == nil || len(source.Disk.Keys) != 0 {
		source.Disk.Clear()
		t.Fatal("foreign instance received source")
	}
}
