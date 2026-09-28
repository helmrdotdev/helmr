package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func applyInstanceChannelClaim(t *testing.T, f runtest.Fixture, w workerActor, r workerapi.ComputerInstanceRenewRequest, token string) (db.ComputerInstance, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return db.ComputerInstance{}, err
	}
	defer tx.Rollback(context.Background())
	i, err := claimComputerInstanceChannel(t.Context(), tx, w, pgvalue.UUID(uuid.MustParse(r.ComputerInstanceID)), pgvalue.UUID(f.EnvironmentID), token)
	if err == nil {
		err = tx.Commit(t.Context())
	}
	return i, err
}
func TestComputerInstanceChannelHasOneOwner(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	w, r := instanceRenewalFixture(t, f, work)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, token := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			i, err := applyInstanceChannelClaim(t, f, w, r, token)
			if err == nil {
				hash := sha256.Sum256([]byte(token))
				if !bytes.Equal(i.GuestChannelTokenHash, hash[:]) || i.WriterGeneration != r.WriterGeneration || !i.GuestChannelTokenExpiresAt.Time.Equal(i.WriterExpiresAt.Time) {
					results <- errors.New("channel changed writer authority")
					return
				}
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("channel owners=%d", accepted)
	}
	if _, err := applyInstanceChannelClaim(t, f, w, r, "replacement"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("live channel rotated: %v", err)
	}
}
func TestComputerInstanceChannelRejectsStaleAdmission(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second'`},
		{"unobserved desired state", `UPDATE computer_instances SET desired_version=desired_version+1`},
		{"stale Worker observation", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour'`},
		{"Worker claim changed", `UPDATE worker_hosts SET claim_version=claim_version+1`},
		{"Worker epoch changed", `UPDATE worker_hosts SET current_epoch=current_epoch+1`},
		{"new writer", `UPDATE computers SET writer_generation=writer_generation+1`},
		{"closed admission", `UPDATE computer_instances SET admission_state='closed'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now())
			w, r := instanceRenewalFixture(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			_, err := applyInstanceChannelClaim(t, f, w, r, "token")
			if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, errStaleWorkerClaims) {
				t.Fatalf("claim=%v", err)
			}
		})
	}
}

func TestComputerInstanceClaimHandlerProjectsFreshAuthority(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	w, r := instanceRenewalFixture(t, f, work)
	server := &Server{db: db.New(f.Pool), tx: f.Pool}
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
		req = req.WithContext(context.WithValue(t.Context(), workerContextKey{}, w))
		out := httptest.NewRecorder()
		server.workerClaimComputerInstance(out, req)
		return out
	}
	if out := call(`{"computer_mount_id":"obsolete"}`); out.Code != 400 {
		t.Fatalf("unknown authority accepted: %d", out.Code)
	}
	out := call(`{}`)
	if out.Code != 200 {
		t.Fatalf("claim=%d %s", out.Code, out.Body.String())
	}
	var response workerapi.ComputerInstanceClaimResponse
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	i := response.Assignment
	if i == nil || i.ComputerInstanceID != r.ComputerInstanceID || i.WriterGeneration != r.WriterGeneration || i.DesiredVersion != 1 || i.ObservedVersion != 1 || i.Target.BaseComputerDiskVersionID == "" || i.RestoreCheckpointID != "" || i.GuestdChannelToken == "" {
		t.Fatal("fresh Instance authority incorrectly projected")
	}
	if out := call(`{}`); out.Code != 200 || out.Body.String() != "{}\n" {
		t.Fatalf("second claim=%d %s", out.Code, out.Body.String())
	}
}
