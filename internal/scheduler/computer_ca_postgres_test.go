package scheduler

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5/pgxpool"
)

// proxyTrustFunc is a Computer CA issuer backed by a function.
type proxyTrustFunc func(uuid.UUID, uuid.UUID, time.Time) (secret.ProxyTrust, error)

func (f proxyTrustFunc) GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error) {
	return f(environmentID, computerID, createdAt)
}

func testProxyTrustGenerator(t *testing.T, pool *pgxpool.Pool) proxyTrustFunc {
	t.Helper()
	store, err := secret.New(db.New(pool), pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return store.GenerateProxyTrust
}

type caScheduleAuthority struct {
	fixedAuthority
	placements []secretbinding.Placement
}

func (a caScheduleAuthority) ResolveScheduledTask(v int32, id string, manifest, digest, queues []byte) (definition.ScheduledTaskAdmission, error) {
	task, err := a.fixedAuthority.ResolveScheduledTask(v, id, manifest, digest, queues)
	task.SecretPlacements = a.placements
	return task, err
}

func TestScheduledComputerCACreationAndRollback(t *testing.T) {
	for _, mode := range []string{"protected", "mixed", "raw", "none", "generation-failure", "after-generation-failure", "binding-failure"} {
		t.Run(mode, func(t *testing.T) {
			pool := openSchedulePostgres(t)
			candidate, digest := seedScheduleAdmission(t, pool)
			q := db.New(pool)
			var secretID uuid.UUID
			if err := pool.QueryRow(t.Context(), "SELECT secret_id FROM schedule_secrets WHERE schedule_id=$1 LIMIT 1", candidate.ID).Scan(&secretID); err != nil {
				t.Fatal(err)
			}
			placements := []secretbinding.Placement{{Name: "API_TOKEN", Kind: "env", Target: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}
			switch mode {
			case "none":
				placements = nil
			case "raw":
				placements = []secretbinding.Placement{{Name: "API_TOKEN", Kind: "env", Target: "RAW", Mode: "raw"}}
			case "mixed":
				placements = append(placements, secretbinding.Placement{Name: "API_TOKEN", Kind: "env", Target: "RAW", Mode: "raw"}, secretbinding.Placement{Name: "API_TOKEN", Kind: "file", Target: "/run/secrets/key", Mode: "raw"})
			}
			dbtest.MustExec(t, t.Context(), pool, "DELETE FROM schedule_secrets WHERE schedule_id=$1", candidate.ID)
			for _, p := range placements {
				dbtest.MustExec(t, t.Context(), pool, `INSERT INTO schedule_secrets(environment_id,schedule_id,placement_kind,placement_target,secret_id,mode,allowed_origins) VALUES($1,$2,$3,$4,$5,$6,COALESCE($7::text[],'{}'))`, candidate.EnvironmentID, candidate.ID, p.Kind, p.Target, secretID, p.Mode, p.AllowedOrigins)
			}
			generate := testProxyTrustGenerator(t, pool)
			calls := 0
			admission, err := NewDBAdmitter(pool, caScheduleAuthority{fixedAuthority{digest: digest}, placements}, proxyTrustFunc(func(e, w uuid.UUID, c time.Time) (secret.ProxyTrust, error) {
				calls++
				if mode == "generation-failure" {
					return secret.ProxyTrust{}, errors.New("synthetic generation failure")
				}
				return generate(e, w, c)
			}))
			if err != nil {
				t.Fatal(err)
			}
			admission.now = func() time.Time { return candidate.NextFireAt.Time }
			if mode == "after-generation-failure" {
				dbtest.MustExec(t, t.Context(), pool, `CREATE FUNCTION reject_ca_run() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NOT EXISTS(SELECT 1 FROM computers WHERE id=NEW.computer_id AND secret_ca_certificate IS NOT NULL) THEN RAISE EXCEPTION 'CA not generated'; END IF;
    RAISE EXCEPTION 'synthetic post-generation failure'; END $$;
    CREATE TRIGGER reject_ca_run BEFORE INSERT ON runs FOR EACH ROW EXECUTE FUNCTION reject_ca_run();`)
			}
			if mode == "binding-failure" {
				dbtest.MustExec(t, t.Context(), pool, `CREATE FUNCTION reject_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'synthetic binding failure'; END $$;
    CREATE TRIGGER reject_binding BEFORE INSERT ON computer_secrets FOR EACH ROW EXECUTE FUNCTION reject_binding();`)
			}
			var before int
			if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM computers").Scan(&before); err != nil {
				t.Fatal(err)
			}
			err = admission.AdmitSchedule(t.Context(), candidate)
			if strings.Contains(mode, "failure") {
				if err == nil {
					t.Fatal("admission unexpectedly succeeded")
				}
				if mode == "after-generation-failure" && !strings.Contains(err.Error(), "synthetic post-generation failure") {
					t.Fatal(err)
				}
				if mode == "binding-failure" && !strings.Contains(err.Error(), "synthetic binding failure") {
					t.Fatal(err)
				}
				var after int
				if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM computers").Scan(&after); err != nil {
					t.Fatal(err)
				}
				if before != after {
					t.Fatal("failed admission retained Computer/CA")
				}
				assertScheduleAdmissionCounts(t, pool, candidate, 0, 0)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := q.GetScheduledRunReceipt(t.Context(), db.GetScheduledRunReceiptParams{EnvironmentID: candidate.EnvironmentID, ScheduleID: candidate.ID, ScheduledAt: candidate.NextFireAt})
			if err != nil {
				t.Fatal(err)
			}
			ca, err := q.GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: candidate.EnvironmentID, ComputerID: receipt.ComputerID})
			if err != nil {
				t.Fatal(err)
			}
			hasCA := mode == "protected" || mode == "mixed"
			if ca.NotAfter.Valid != hasCA || (len(ca.Certificate) > 0) != hasCA {
				t.Fatal("scheduled CA presence differs")
			}
			if hasCA {
				var created time.Time
				if err := pool.QueryRow(t.Context(), "SELECT created_at FROM computers WHERE id=$1", receipt.ComputerID).Scan(&created); err != nil {
					t.Fatal(err)
				}
				if !ca.NotAfter.Time.Equal(created.AddDate(10, 0, 0).Truncate(time.Second)) {
					t.Fatal("scheduled CA timestamp differs")
				}
			}
			beforeCalls := calls
			if err := admission.AdmitSchedule(t.Context(), candidate); err != nil {
				t.Fatal(err)
			}
			if calls != beforeCalls {
				t.Fatal("scheduled receipt replay regenerated CA")
			}
		})
	}
}
