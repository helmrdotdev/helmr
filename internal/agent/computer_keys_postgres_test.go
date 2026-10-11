package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/jackc/pgx/v5"
)

func computerSourceFixture(t *testing.T) (fixture, ComputerLeaseIdentity) {
	t.Helper()
	f, a, computer := freshComputerAllocationFixture(t)
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	id := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if _, err := a.DeliverComputer(t.Context(), *f.host(), id); err != nil {
		t.Fatal(err)
	}
	return f, id
}

func TestComputerDiskSourceConcurrentRetryPinsOnePrivateKey(t *testing.T) {
	f, id := computerSourceFixture(t)
	broker, err := NewComputerKeyBroker(f.pool, preparationWrapper(t))
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make(chan ComputerDiskSource, n)
	errors := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { source, err := broker.Source(t.Context(), *f.host(), id); results <- source; errors <- err })
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first ComputerDiskSource
	for source := range results {
		if first.WriteKeyID == uuid.Nil() {
			first = source
			continue
		}
		if source.Root != first.Root || source.WriteKeyID != first.WriteKeyID || source.Scope != first.Scope || source.BaseVersion != first.BaseVersion || len(source.Keys) != len(first.Keys) {
			t.Fatal("retry changed source or private key")
		}
		for i, k := range source.Keys {
			if k.ID != first.Keys[i].ID || !bytes.Equal(k.Key, first.Keys[i].Key) {
				t.Fatal("retry changed plaintext")
			}
		}
		source.Clear()
	}
	defer first.Clear()
	if len(first.Keys) != 2 {
		t.Fatalf("want image key and Computer key, got %d", len(first.Keys))
	}
	if first.WriteKeyID.String() == first.Root.Page.KeyID {
		t.Fatal("Computer reused preparation write key")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys WHERE environment_id=$1 AND writer_computer_id=$2`, f.env, id.ComputerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("private keys: %d %v", count, err)
	}
	var rootDigest string
	if err := f.pool.QueryRow(t.Context(), `SELECT 'sha256:'||encode(initial_root_digest,'hex') FROM computers WHERE environment_id=$1 AND id=$2`, f.env, id.ComputerID).Scan(&rootDigest); err != nil {
		t.Fatal(err)
	}
	digest, err := first.Root.Digest()
	if err != nil || digest != rootDigest || first.BaseVersion != digest {
		t.Fatal("source changed retained root")
	}
	wrong := id
	wrong.InstanceID = uuid.NewV7()
	if source, err := broker.Source(t.Context(), *f.host(), wrong); err == nil || len(source.Keys) != 0 {
		source.Clear()
		t.Fatal("foreign instance received keys")
	}
}

func TestComputerDiskSourceRechecksAuthorityAfterProviderIO(t *testing.T) {
	for _, phase := range []string{"wrap", "unwrap"} {
		for _, change := range []string{"expire", "delete", "stop"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f, id := computerSourceFixture(t)
				provider := &preparationKeyProvider{DataKeyWrapper: preparationWrapper(t)}
				var once sync.Once
				mutate := func() {
					once.Do(func() {
						switch change {
						case "expire":
							dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, id.ComputerID)
						case "delete":
							dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.env, id.ComputerID)
						case "stop":
							if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), id, uuid.Nil()); err != nil {
								t.Fatal(err)
							}
						}
					})
				}
				if phase == "wrap" {
					provider.beforeWrap = mutate
				} else {
					provider.beforeUnwrap = mutate
				}
				broker, _ := NewComputerKeyBroker(f.pool, provider)
				source, err := broker.Source(t.Context(), *f.host(), id)
				if err == nil || len(source.Keys) != 0 {
					source.Clear()
					t.Fatalf("authority change delivered keys: %v", err)
				}
				if !bytes.Equal(provider.lastPlain, make([]byte, len(provider.lastPlain))) {
					t.Fatal("rejected key material was not cleared")
				}
				if phase == "wrap" {
					var count int
					if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND base_root_id IS NOT NULL`, f.env, id.ComputerID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("rejected binding persisted: %d %v", count, err)
					}
				}
			})
		}
	}
}

func TestComputerDiskSourceRejectsUndeliveredAllocation(t *testing.T) {
	f, a, computer := freshComputerAllocationFixture(t)
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	broker, _ := NewComputerKeyBroker(f.pool, preparationWrapper(t))
	source, err := broker.Source(t.Context(), *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch})
	if !errors.Is(err, ErrDenied) || len(source.Keys) != 0 {
		source.Clear()
		t.Fatalf("undelivered source: %v", err)
	}
}

func TestComputerDiskSourceRestoredClosureOpensMultipleKeys(t *testing.T) {
	f, id := computerSourceFixture(t)
	wrapper := preparationWrapper(t)
	var org string
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id::text FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	scope, err := computerkey.EncryptionScope(org, f.env.String())
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, second, unrelated := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	keys := map[string][]byte{}
	for i, keyID := range []uuid.UUID{first, second, unrelated} {
		key := bytes.Repeat([]byte{byte(i + 4)}, 32)
		defer clear(key)
		keys[keyID.String()] = key
		envelope, err := wrapper.Wrap(t.Context(), scope, keyID.String(), key)
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_data_keys(environment_id,id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,$4,$5)`, f.env, keyID, id.ComputerID, envelope.WrappingKeyID, envelope.Ciphertext)
	}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: scope, ActiveKey: first.String(), Keys: keys, PackLimit: blockformat.MinPackLimit}
	base, err := writer.Empty(t.Context(), 1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	base, err = writer.Capture(t.Context(), base, 1<<20, map[uint64][]byte{100: bytes.Repeat([]byte{21}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	writer.ActiveKey = second.String()
	base, err = writer.Capture(t.Context(), base, 1<<20, map[uint64][]byte{0: bytes.Repeat([]byte{42}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	root, err := disk.NewVersionRoot(base, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(root)
	digest, _ := root.Digest()
	save, rootID := uuid.NewV7(), uuid.NewV7()
	// Certification and restoration selection are fixture state. This test
	// exercises exact restored source/key delivery and actual encrypted reads.
	dbtest.MustExec(t, t.Context(), f.pool, `
 INSERT INTO cas_blobs(digest,size_bytes) VALUES($3,$4);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$3,$4,'application/octet-stream' FROM environments WHERE id=$1;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$3,org_id,project_id,$4,'application/octet-stream','root',$5,'{}',clock_timestamp() FROM environments WHERE id=$1;
 INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($1,$3,$6,false),($1,$3,$7,true);
 INSERT INTO computer_disk_roots(environment_id,id,locator) VALUES($1,$8,$9);
 INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,status,requested_at,flush_acknowledged_at,captured_at,captured_root_digest,capture_evidence,publication_evidence,root_id) VALUES($1,$10,$2,1,1,'published',now(),now(),now(),decode(substring($11::text from 8),'hex'),'fixture capture','fixture publication',$8);
 UPDATE computer_leases SET restored_from_save_id=$10 WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
 `, pgx.QueryExecModeSimpleProtocol, f.env, id.ComputerID, root.Pack.Digest, root.Pack.SizeBytes, root.Pack.Rank, first, second, rootID, string(raw), save, digest)
	broker, _ := NewComputerKeyBroker(f.pool, wrapper)
	source, err := broker.Source(t.Context(), *f.host(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Clear()
	if source.Root != root || source.BaseVersion != save.String() || len(source.Keys) != 3 || source.WriteKeyID == first || source.WriteKeyID == second {
		t.Fatal("restored source lost its root, closure, or private write key")
	}
	delivered := map[string][]byte{}
	for _, key := range source.Keys {
		if key.ID == unrelated {
			t.Fatal("delivered an unrelated Computer key")
		}
		delivered[key.ID.String()] = key.Key
	}
	tree, err := blockformat.OpenTree(t.Context(), store, source.Scope, delivered, base)
	if err != nil {
		t.Fatal(err)
	}
	for block, value := range map[uint64]byte{0: 42, 100: 21} {
		actual, err := tree.ReadBlock(t.Context(), block)
		if err != nil || !bytes.Equal(actual, bytes.Repeat([]byte{value}, 4096)) {
			t.Fatalf("restored block %d: %v", block, err)
		}
	}
}
