//go:build linux && computerproof

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

type nativePostCutProof struct {
	ctx      context.Context
	cancel   context.CancelFunc
	pool     *pgxpool.Pool
	client   *client.Client
	env      uuid.UUID
	evidence string
	selected atomic.Pointer[uuid.UUID]
	done     chan struct{}
	err      error
}

func newNativePostCutProof(ctx context.Context, pool *pgxpool.Pool, c *client.Client, env uuid.UUID, evidence string) *nativePostCutProof {
	ctx, cancel := context.WithCancel(ctx)
	return &nativePostCutProof{ctx: ctx, cancel: cancel, pool: pool, client: c, env: env, evidence: evidence, done: make(chan struct{})}
}
func (p *nativePostCutProof) intercept(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/worker/v1/agent-computers/checkpoint/register" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))
	var publication workerapi.AgentCheckpointPublication
	if err == nil {
		err = json.Unmarshal(body, &publication)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	id := publication.Manifest.CheckpointID
	if p.selected.CompareAndSwap(nil, &id) {
		go func() {
			defer close(p.done)
			p.err = p.run(publication)
		}()
	}
	if *p.selected.Load() != id {
		return false
	}
	// Registration remains unacknowledged until the production source owner
	// reserves continuation and cancels its uploader. No fixture release can make
	// this checkpoint publish successfully.
	select {
	case <-p.done:
	case <-r.Context().Done():
	case <-p.ctx.Done():
	}
	http.Error(w, "post-cut registration withheld", http.StatusServiceUnavailable)
	return true
}
func (p *nativePostCutProof) join(t *testing.T) {
	p.cancel()
	if p.selected.Load() != nil {
		<-p.done
	}
}
func (p *nativePostCutProof) verify(t *testing.T) {
	t.Helper()
	if p.selected.Load() == nil {
		t.Fatal("post-cut publication gate was never reached")
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatal(p.err)
		}
	case <-p.ctx.Done():
		t.Fatal(p.ctx.Err())
	}
}
func (p *nativePostCutProof) run(publication workerapi.AgentCheckpointPublication) error {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Minute)
	defer cancel()
	m := publication.Manifest
	if publication.EnvironmentID != p.env.String() {
		return errors.New("post-cut environment differs")
	}
	execute := func(key, script string) (string, error) {
		receipt, err := p.client.ExecuteComputer(ctx, m.ComputerID.String(), api.ExecuteComputerRequest{Command: []string{"/bin/sh", "-ceu", script}, Timeout: "1m", IdempotencyKey: key}, client.ComputerScopeOptions{})
		if err != nil {
			return "", err
		}
		for {
			info, err := p.client.RetrieveCommand(ctx, receipt.CommandID, client.ComputerScopeOptions{})
			if err != nil {
				return "", err
			}
			if info.Outcome != nil {
				if info.Outcome.Kind != "exited" || info.Outcome.ExitCode == nil || *info.Outcome.ExitCode != 0 {
					return "", fmt.Errorf("post-cut command: %+v", info.Outcome)
				}
				return receipt.CommandID, nil
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	command, err := execute("post-cut-write", `test ! -e /workspace/post-cut-proof; printf '%s' retained-source > /workspace/post-cut-proof`)
	if err != nil {
		return err
	}
	var valid bool
	var source []byte
	err = p.pool.QueryRow(ctx, `SELECT cp.status='consumed' AND cp.abort_identity IS NOT NULL AND cp.controls_reconciled_at IS NOT NULL AND cp.target_lease_epoch IS NULL AND cmd.computer_lease_epoch=cp.source_lease_epoch,
 jsonb_build_object('checkpoint_id',cp.id,'source_epoch',cp.source_lease_epoch,'command_id',cmd.id,'command_epoch',cmd.computer_lease_epoch,'status',cp.status,'controls_reconciled_at',cp.controls_reconciled_at)
 FROM checkpoints cp JOIN commands cmd ON cmd.environment_id=cp.environment_id AND cmd.id=$3
 WHERE cp.environment_id=$1 AND cp.id=$2`, p.env, m.CheckpointID, command).Scan(&valid, &source)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("post-cut work did not use reconciled source: %s", source)
	}
	if err = os.WriteFile(filepath.Join(p.evidence, "post-cut-source.json"), source, 0600); err != nil {
		return err
	}
	for {
		var restored bool
		err = p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch>$3 AND initialized_at IS NOT NULL AND fenced_at IS NULL)`, p.env, m.ComputerID, m.LeaseEpoch).Scan(&restored)
		if err != nil {
			return err
		}
		if restored {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	read, err := execute("post-cut-restored-read", `test "$(cat /workspace/post-cut-proof)" = retained-source`)
	if err != nil {
		return err
	}
	var raw []byte
	err = p.pool.QueryRow(ctx, `SELECT computer_lease_epoch>$3,jsonb_build_object('command_id',id,'restored_epoch',computer_lease_epoch,'source_epoch',$3::bigint) FROM commands WHERE environment_id=$1 AND id=$2`, p.env, read, m.LeaseEpoch).Scan(&valid, &raw)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("post-cut file read did not execute after restore")
	}
	return os.WriteFile(filepath.Join(p.evidence, "post-cut-restored-read.json"), raw, 0600)
}

func TestNativePostCutGateWithholdsEverySelectedCheckpointAttempt(t *testing.T) {
	p := newNativePostCutProof(t.Context(), nil, nil, uuid.NewV7(), t.TempDir())
	defer p.cancel()
	selected := uuid.NewV7()
	p.selected.Store(&selected)
	close(p.done)
	for _, id := range []uuid.UUID{selected, selected, uuid.NewV7()} {
		publication := workerapi.AgentCheckpointPublication{}
		publication.Manifest.CheckpointID = id
		body, err := json.Marshal(publication)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/worker/v1/agent-computers/checkpoint/register", bytes.NewReader(body))
		w := httptest.NewRecorder()
		intercepted := p.intercept(w, r)
		if intercepted != (id == selected) {
			t.Fatalf("checkpoint %s intercepted=%v", id, intercepted)
		}
		if intercepted && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d", w.Code)
		}
		if !intercepted {
			forwarded, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(forwarded, body) {
				t.Fatalf("forwarded body lost: %v", err)
			}
		}
	}
}
