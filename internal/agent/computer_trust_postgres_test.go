package agent

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type countingComputerTrust struct {
	store   *secret.Store
	calls   atomic.Int64
	failure error
}

func (c *countingComputerTrust) GenerateProxyTrust(env, computer uuid.UUID, created time.Time) (secret.ProxyTrust, error) {
	c.calls.Add(1)
	if c.failure != nil {
		return secret.ProxyTrust{}, c.failure
	}
	return c.store.GenerateProxyTrust(env, computer, created)
}

func TestComputerTrustCreationIsAtomicAndRetriesPreserveSigner(t *testing.T) {
	f := preparationFixtureFor(t, newAdmissionFixture(t))
	marker, err := secretbinding.Placeholder("protected")
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_secret_bindings(environment_id,deployment_id,definition_key,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder)
 VALUES($1,$2,'fixture-computer',$3,'env','RUNTIME_TOKEN','protected',ARRAY['https://api.example.com'],$4)`, f.env, f.deployment, f.secretID, marker)
	req := f.startRequest("computer-trust")
	if _, err = Start(t.Context(), f.pool, nil, f.caller(), req); !errors.Is(err, ErrNotReady) {
		t.Fatalf("missing issuer: %v", err)
	}
	failed := &countingComputerTrust{store: f.secrets, failure: errors.New("signing failed")}
	if _, err = Start(t.Context(), f.pool, failed, f.caller(), req); !errors.Is(err, failed.failure) {
		t.Fatalf("failed issuer: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computers WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed creation leaked Computer: %d %v", count, err)
	}
	issuer := &countingComputerTrust{store: f.secrets}
	const n = 8
	receipts := make(chan Admission, n)
	failures := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { r, e := Start(t.Context(), f.pool, issuer, f.caller(), req); receipts <- r; failures <- e })
	}
	wg.Wait()
	close(receipts)
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal(e)
		}
	}
	var session uuid.UUID
	for r := range receipts {
		if session == uuid.Nil() {
			session = r.SessionID
		}
		if r.SessionID != session {
			t.Fatal("retry created another Session")
		}
	}
	if issuer.calls.Load() != 1 {
		t.Fatalf("generated %d signers", issuer.calls.Load())
	}
	var trust secret.ProxyTrust
	trust.EnvironmentID = f.env
	if err = f.pool.QueryRow(t.Context(), `SELECT c.id,c.proxy_ca_certificate,c.proxy_ca_not_after,c.proxy_ca_private_key_nonce,c.proxy_ca_private_key_ciphertext FROM computers c JOIN sessions s ON s.environment_id=c.environment_id AND s.computer_id=c.id WHERE s.environment_id=$1 AND s.id=$2`, f.env, session).Scan(&trust.ComputerID, &trust.Certificate, &trust.NotAfter, &trust.PrivateKeyNonce, &trust.PrivateKeyCiphertext); err != nil {
		t.Fatal(err)
	}
	cert, key, err := f.secrets.ComputerProxyLeaf(trust, []string{"api.example.com"})
	if err != nil || len(cert) == 0 || len(key) == 0 {
		t.Fatalf("Computer trust: %v", err)
	}
	clear(key)
	if _, err = Start(t.Context(), f.pool, nil, f.caller(), req); err != nil {
		t.Fatalf("retry required new signer: %v", err)
	}
	var replay []byte
	if err = f.pool.QueryRow(t.Context(), `SELECT proxy_ca_private_key_ciphertext FROM computers WHERE environment_id=$1 AND id=$2`, f.env, trust.ComputerID).Scan(&replay); err != nil || !bytes.Equal(replay, trust.PrivateKeyCiphertext) {
		t.Fatalf("retry replaced signer: %v", err)
	}
	var copiedMarker string
	if err = f.pool.QueryRow(t.Context(), `SELECT placeholder FROM computer_secret_bindings WHERE environment_id=$1 AND computer_id=$2`, f.env, trust.ComputerID).Scan(&copiedMarker); err != nil || copiedMarker == marker || copiedMarker == "" {
		t.Fatalf("actual Computer selector was not independently allocated: %v", err)
	}
}
