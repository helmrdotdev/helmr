package computerhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/brokeredstore"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// testObjectKeys delivers a stable random data key per checkpoint and boot
// seed, as the Control Plane does for an authorized requester.
type testObjectKeys struct {
	mu          sync.Mutex
	keys        map[string][]byte
	err         error
	checkpoints []workerapi.CheckpointKeyRequest
	bootSeeds   []workerapi.BootSeedKeyRequest
}

func newTestObjectKeys() *testObjectKeys { return &testObjectKeys{keys: map[string][]byte{}} }

func (k *testObjectKeys) CheckpointKey(_ context.Context, r workerapi.CheckpointKeyRequest) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.checkpoints = append(k.checkpoints, r)
	if k.err != nil {
		return nil, k.err
	}
	return k.keyLocked("checkpoint/" + r.CheckpointID), nil
}

func (k *testObjectKeys) BootSeedKey(_ context.Context, r workerapi.BootSeedKeyRequest) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.bootSeeds = append(k.bootSeeds, r)
	if k.err != nil {
		return nil, k.err
	}
	return k.keyLocked("boot-seed/" + r.BuildID), nil
}

// keyLocked returns a copy because holders clear delivered keys.
func (k *testObjectKeys) keyLocked(name string) []byte {
	if k.keys[name] == nil {
		k.keys[name] = make([]byte, 32)
		_, _ = rand.Read(k.keys[name])
	}
	return bytes.Clone(k.keys[name])
}

func (k *testObjectKeys) fail(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

// rotateCheckpoint makes later deliveries for id return a different key.
func (k *testObjectKeys) rotateCheckpoint(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.keys, "checkpoint/"+id)
}

func (k *testObjectKeys) rotateBootSeed(build uuid.UUID) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.keys, "boot-seed/"+build.String())
}

func (k *testObjectKeys) checkpointEncryptor(t *testing.T, id string) *CheckpointEncryptor {
	t.Helper()
	k.mu.Lock()
	key := k.keyLocked("checkpoint/" + id)
	k.mu.Unlock()
	c, err := NewCheckpointEncryptor(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func (k *testObjectKeys) bootSeedEncryptor(t *testing.T, build uuid.UUID) *CheckpointEncryptor {
	t.Helper()
	k.mu.Lock()
	key := k.keyLocked("boot-seed/" + build.String())
	k.mu.Unlock()
	c, err := NewCheckpointEncryptor(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCheckpointCipherRequestsOwnerCheckpointKeyAndFailsClosed(t *testing.T) {
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), AllocationID: uuid.NewV7().String(), Epoch: 3}
	checkpoint := uuid.NewV7().String()
	keys := newTestObjectKeys()
	p := &PreparedMachines{ObjectKeys: keys}
	c, err := p.checkpointCipher(t.Context(), identity, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(keys.checkpoints) != 1 || keys.checkpoints[0] != (workerapi.CheckpointKeyRequest{Identity: identity, CheckpointID: checkpoint}) {
		t.Fatalf("key request %+v", keys.checkpoints)
	}
	refused := errors.New("checkpoint key refused")
	keys.fail(refused)
	if c, err := p.checkpointCipher(t.Context(), identity, checkpoint); !errors.Is(err, refused) || c != nil {
		t.Fatalf("refused key produced encryptor: %v", err)
	}
	if c, err := (&PreparedMachines{}).checkpointCipher(t.Context(), identity, checkpoint); err == nil || c != nil {
		t.Fatal("missing key client produced encryptor")
	}
}

func TestCheckpointKeysSeparateCheckpoints(t *testing.T) {
	keys := newTestObjectKeys()
	first, second := uuid.NewV7().String(), uuid.NewV7().String()
	a, b := keys.checkpointEncryptor(t, first), keys.checkpointEncryptor(t, second)
	if a.KeyID() == b.KeyID() || a.KeyID() != keys.checkpointEncryptor(t, first).KeyID() {
		t.Fatal("checkpoint keys are not stable and distinct")
	}
	var encrypted bytes.Buffer
	if err := a.Encrypt(t.Context(), bytes.NewReader([]byte("state")), &encrypted, first+"/vm_state"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := b.Decrypt(t.Context(), bytes.NewReader(encrypted.Bytes()), &out, first+"/vm_state"); !errors.Is(err, errInvalidCheckpoint) || out.Len() != 0 {
		t.Fatalf("other checkpoint key decrypted: %v", err)
	}
}

func TestBootSeedCipherRequiresStorageScope(t *testing.T) {
	keys := newTestObjectKeys()
	p := &PreparedMachines{ObjectKeys: keys}
	build := uuid.NewV7()
	if c, err := p.bootSeedCipher(t.Context(), build); !errors.Is(err, brokeredstore.ErrNoAuthority) || c != nil || len(keys.bootSeeds) != 0 {
		t.Fatalf("unscoped boot seed key: %v", err)
	}
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), AllocationID: uuid.NewV7().String(), Epoch: 1}
	scope := workerapi.StorageScope{Computer: &identity}
	c, err := p.bootSeedCipher(brokeredstore.WithScope(t.Context(), scope), build)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(keys.bootSeeds) != 1 || keys.bootSeeds[0].BuildID != build.String() || keys.bootSeeds[0].Scope.Computer == nil || *keys.bootSeeds[0].Scope.Computer != identity || c.KeyID() != keys.bootSeedEncryptor(t, build).KeyID() {
		t.Fatalf("boot seed key request %+v", keys.bootSeeds)
	}
}
