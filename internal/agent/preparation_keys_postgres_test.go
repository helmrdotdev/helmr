package agent

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

type preparationKeyProvider struct {
	DataKeyWrapper
	beforeWrap   func()
	beforeUnwrap func()
	lastPlain    []byte
}

func (p *preparationKeyProvider) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	if p.beforeWrap != nil {
		p.beforeWrap()
	}
	return p.DataKeyWrapper.Wrap(ctx, scope, id, key)
}
func (p *preparationKeyProvider) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	if p.beforeUnwrap != nil {
		p.beforeUnwrap()
	}
	plain, err := p.DataKeyWrapper.Unwrap(ctx, scope, id, e)
	p.lastPlain = plain
	return plain, err
}
func preparationWrapper(t *testing.T) *computerkey.Local {
	t.Helper()
	p, err := computerkey.NewLocal("preparation-test", bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPreparationKeyConcurrentRetryAndOwnership(t *testing.T) {
	f := newPreparationFixture(t)
	ref := f.claim(t, f.attach(t, f.waiter(t)))
	broker, err := NewPreparationKeyBroker(f.pool, preparationWrapper(t))
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make(chan PreparationKey, n)
	failures := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { k, err := broker.WriteKey(t.Context(), *f.host(), ref); results <- k; failures <- err })
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first PreparationKey
	for k := range results {
		if first.Key == nil {
			first = k
			continue
		}
		if k.ID != first.ID || k.Scope != first.Scope || !bytes.Equal(k.Key, first.Key) {
			t.Fatal("replay changed key")
		}
		clear(k.Key)
	}
	defer clear(first.Key)
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys WHERE environment_id=$1 AND writer_preparation_id=$2`, f.env, ref.PreparationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("keys %d: %v", count, err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE computer_data_keys SET writer_computer_id=$3 WHERE environment_id=$1 AND id=$2`, f.env, first.ID, f.computer); err == nil {
		t.Fatal("dual ownership accepted")
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE environment_id=$1 AND id=$2`, f.env, first.ID); err == nil {
		t.Fatal("retired retained key")
	}
	wrong := ref
	wrong.ChannelCredential = bytes.Repeat([]byte{9}, 32)
	if k, err := broker.WriteKey(t.Context(), *f.host(), wrong); !errors.Is(err, ErrDenied) || k.Key != nil {
		t.Fatalf("wrong channel: %v", err)
	}
}

func TestPreparationKeyReauthorizesProviderIO(t *testing.T) {
	for _, phase := range []string{"wrap", "unwrap"} {
		for _, change := range []string{"expire", "revoke", "stop"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f := newPreparationFixture(t)
				ref := f.claim(t, f.attach(t, f.waiter(t)))
				provider := &preparationKeyProvider{DataKeyWrapper: preparationWrapper(t)}
				mutate := func() {
					switch change {
					case "expire":
						dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, ref.PreparationID)
					case "revoke":
						if _, err := f.secrets.Revoke(t.Context(), f.env, f.secretID, "revoke"); err != nil {
							t.Fatal(err)
						}
					case "stop":
						if err := ObservePreparationStopped(t.Context(), f.pool, *f.host(), ref.Identity()); err != nil {
							t.Fatal(err)
						}
					}
				}
				if phase == "wrap" {
					provider.beforeWrap = mutate
				} else {
					provider.beforeUnwrap = mutate
				}
				broker, _ := NewPreparationKeyBroker(f.pool, provider)
				key, err := broker.WriteKey(t.Context(), *f.host(), ref)
				if !errors.Is(err, ErrDenied) || key.Key != nil {
					t.Fatalf("key escaped: %v", err)
				}
				if phase == "unwrap" && (len(provider.lastPlain) != 32 || !bytes.Equal(provider.lastPlain, make([]byte, 32))) {
					t.Fatal("rejected plaintext not cleared")
				}
				if phase == "wrap" {
					var count int
					if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys WHERE environment_id=$1 AND writer_preparation_id=$2`, f.env, ref.PreparationID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("unauthorized insert %d: %v", count, err)
					}
				}
			})
		}
	}
}
