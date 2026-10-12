package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
)

func seedTrigger(t *testing.T, f fixture, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) VALUES($1,$3,$4,'daily','* * * * *','UTC','[{"text":"scheduled","type":"text"}]') ON CONFLICT DO NOTHING;
 INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,next_fire_at) VALUES($1,$2,$3,$4,'daily',$5,$5)`, pgx.QueryExecModeSimpleProtocol, f.env, id, f.agent, f.deployment, at)
	return id
}
func TestTriggerAdmissionConcurrentRetryAndPinnedDeployment(t *testing.T) {
	f := newAdmissionFixture(t)
	due := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	trigger := seedTrigger(t, f, due)
	// This historical activation remains eligible within grace even after a
	// promotion. Its root must use its original immutable definition.
	current := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) SELECT environment_id,$2,'sha256:'||repeat('e',64) FROM deployments WHERE environment_id=$1 AND id=$3;
 UPDATE environments SET current_deployment_id=$2 WHERE id=$1;
 UPDATE trigger_activations SET active_until=$4 WHERE environment_id=$1 AND id=$5;`, pgx.QueryExecModeSimpleProtocol, f.env, current, f.deployment, due.Add(time.Minute), trigger)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(o.disposition='admitted' AND o.scheduled_at=$3 AND s.deployment_id=$4 AND s.root_session_id=s.id AND s.requester_session_id IS NULL AND s.parent_session_id IS NULL AND s.computer_id<>$5 AND s.status='open' AND t.status='queued' AND t.seq=1 AND t.caller_kind='schedule' AND t.caller_id=$2 AND t.input='[{"text":"scheduled","type":"text"}]'::bytea) FROM trigger_occurrences o JOIN sessions s ON (s.environment_id,s.id)=(o.environment_id,o.session_id) JOIN turns t ON (t.environment_id,t.id)=(o.environment_id,o.turn_id) WHERE o.environment_id=$1 AND o.activation_id=$2`, f.env, trigger, due, f.deployment, f.computer).Scan(&valid); err != nil || !valid {
		t.Fatalf("scheduled admission changed identity: %v %v", valid, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM turns WHERE environment_id=$1)=1 AND (SELECT count(*) FROM computers WHERE environment_id=$1)=2 AND (SELECT next_fire_at FROM trigger_activations WHERE environment_id=$1 AND id=$2)=$3`, f.env, trigger, due.Add(time.Minute)).Scan(&valid); err != nil || !valid {
		t.Fatalf("retry repeated admission: %v %v", valid, err)
	}
}
func TestTriggerRejectionRollsBackAdmissionAndRetainsDisposition(t *testing.T) {
	f := newAdmissionFixture(t)
	trigger := seedTrigger(t, f, time.Now().UTC().Truncate(time.Minute))
	// Force a late admission failure after the token reservation, proving its
	// savepoint preserves a rejected occurrence without leaking that reservation.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE template_definitions SET cpu_millis=1000,memory_bytes=1125899906842624 WHERE environment_id=$1`, f.env)
	var tokens string
	if err := f.pool.QueryRow(t.Context(), `SELECT admission_tokens::text FROM environments WHERE id=$1`, f.env).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT disposition='rejected' AND reason='invalid_definition' AND turn_id IS NULL FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2) AND (SELECT count(*) FROM turns WHERE environment_id=$1)=0 AND (SELECT count(*) FROM computers WHERE environment_id=$1)=1 AND (SELECT admission_tokens::text FROM environments WHERE id=$1)=$3`, f.env, trigger, tokens).Scan(&valid); err != nil || !valid {
		t.Fatalf("rejection leaked work: %v %v", valid, err)
	}
	if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
		t.Fatal(err)
	}
}
func TestTriggerCommitFailureRetriesSameOccurrence(t *testing.T) {
	f := newAdmissionFixture(t)
	trigger := seedTrigger(t, f, time.Now().UTC().Truncate(time.Minute))
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_occurrence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected storage failure' USING ERRCODE='40001'; END $$;
 CREATE TRIGGER reject_occurrence BEFORE INSERT ON trigger_occurrences FOR EACH ROW EXECUTE FUNCTION reject_occurrence();`, pgx.QueryExecModeSimpleProtocol)
	if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err == nil {
		t.Fatal("injected failure accepted")
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM turns WHERE environment_id=$1)=0 AND (SELECT count(*) FROM computers WHERE environment_id=$1)=1`, f.env).Scan(&valid); err != nil || !valid {
		t.Fatalf("failed commit left work: %v %v", valid, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_occurrence ON trigger_occurrences`)
	if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
		t.Fatal(err)
	}
}
func TestTriggerLoopRecoversLatestOnly(t *testing.T) {
	f := newAdmissionFixture(t)
	trigger := seedTrigger(t, f, time.Now().UTC().Add(-24*time.Hour).Truncate(time.Minute))
	if _, _, err := reconcileTriggers(t.Context(), f.pool, nil, triggerPosition{}); err != nil {
		t.Fatal(err)
	}
	var admitted, missed int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE disposition='admitted'),count(*) FILTER(WHERE disposition='missed') FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2`, f.env, trigger).Scan(&admitted, &missed); err != nil || admitted != 1 || missed != 1 {
		t.Fatalf("recovery=%d/%d %v", admitted, missed, err)
	}
	// A real next enqueue keeps the scheduled root open and queued.
	var session uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT session_id FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2 AND disposition='admitted'`, f.env, trigger).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if _, err := Enqueue(context.Background(), f.pool, nil, f.caller(), EnqueueRequest{EnvironmentID: f.env, SessionID: session, Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
}

func TestTriggerTemporaryAdmissionShortageRetriesWithinGrace(t *testing.T) {
	for _, shortage := range []string{"tokens", "outstanding", "storage"} {
		t.Run(shortage, func(t *testing.T) {
			f := newAdmissionFixture(t)
			due := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
			trigger := seedTrigger(t, f, due)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE trigger_activations SET active_until=$3 WHERE environment_id=$1 AND id=$2`, f.env, trigger, due.Add(time.Second))
			switch shortage {
			case "tokens":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_tokens=0,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.env)
			case "outstanding":
				if _, err := Start(t.Context(), f.pool, nil, f.caller(), f.startRequest("occupy")); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_outstanding_admissions=1 WHERE id=$1`, f.env)
			case "storage":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_reserved_storage_bytes=$2 WHERE id=$1`, f.env, disk.SeedCapacity)
			}
			if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); !errors.Is(err, ErrNotReady) {
				t.Fatalf("temporary shortage: %v", err)
			}
			var unchanged bool
			if err := f.pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2) AND (SELECT next_fire_at=$3 FROM trigger_activations WHERE environment_id=$1 AND id=$2)`, f.env, trigger, due).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("shortage committed: %v %v", unchanged, err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_tokens=100,max_outstanding_admissions=100,max_reserved_storage_bytes=1099511627776 WHERE id=$1`, f.env)
			for range 2 {
				if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2 AND scheduled_at=$3 AND disposition='admitted'`, f.env, trigger, due).Scan(&count); err != nil || count != 1 {
				t.Fatalf("recovered admission=%d: %v", count, err)
			}
		})
	}
}

func TestTriggerTemporaryShortageBecomesMissedAfterGrace(t *testing.T) {
	f := newAdmissionFixture(t)
	due := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	trigger := seedTrigger(t, f, due)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET admission_tokens=0,admission_refilled_at=clock_timestamp()+interval '1 day' WHERE id=$1`, f.env)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE trigger_activations SET active_until=$3 WHERE environment_id=$1 AND id=$2`, f.env, trigger, due.Add(time.Second))
	if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); !errors.Is(err, ErrNotReady) {
		t.Fatalf("shortage: %v", err)
	}
	// Age the activation past the lateness window without sleeping.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE trigger_activations SET active_from=active_from-interval '10 minutes',next_fire_at=next_fire_at-interval '10 minutes',active_until=active_until-interval '10 minutes' WHERE environment_id=$1 AND id=$2`, f.env, trigger)
	if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
		t.Fatal(err)
	}
	var missed bool
	if err := f.pool.QueryRow(t.Context(), `SELECT disposition='missed' AND reason='lateness_exceeded' AND session_id IS NULL AND turn_id IS NULL FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2 AND scheduled_at=$3`, f.env, trigger, due.Add(-10*time.Minute)).Scan(&missed); err != nil || !missed {
		t.Fatalf("expiry=%v: %v", missed, err)
	}
}

func TestTriggerSlackGenerationAdmissionAndRevocation(t *testing.T) {
	for _, revocation := range []string{"publication", "disconnected", "authorization_lost", "repaired_before_evaluation"} {
		t.Run(revocation, func(t *testing.T) {
			f := newAdmissionFixture(t)
			installation, channel := slackRouteFixture(t, f)
			due := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_installations SET authorized_at=$2 WHERE id=$1`, installation, due.Add(-time.Minute))
			trigger := seedTrigger(t, f, due)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE trigger_activations SET slack_channel_id=$3,active_until=$4 WHERE environment_id=$1 AND id=$2`, f.env, trigger, channel, due.Add(time.Minute))
			if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
				t.Fatal(err)
			}
			var pinned bool
			if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM slack_threads t JOIN slack_posts p ON p.thread_id=t.id WHERE t.front_session_id=s.id AND t.channel_id=$3 AND t.thread_ts IS NULL AND p.role='opening' AND p.turn_id=o.turn_id AND p.source_thread_id IS NULL) FROM trigger_occurrences o JOIN sessions s ON (s.environment_id,s.id)=(o.environment_id,o.session_id) WHERE o.environment_id=$1 AND o.activation_id=$2 AND o.disposition='admitted'`, f.env, trigger, channel).Scan(&pinned); err != nil || !pinned {
				t.Fatalf("scheduled route missing: %v %v", pinned, err)
			}
			switch revocation {
			case "publication":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_connections SET revoked_at=clock_timestamp() WHERE environment_id=$1`, f.env)
			case "disconnected":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_installations SET disconnected_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, installation)
			case "authorization_lost":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, installation)
			case "repaired_before_evaluation":
				// Same-identity repair has completed before the dispatcher observes
				// this occurrence. Its original due time remains inside the gap.
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE slack_installations SET authorization_lost_at=NULL,authorized_at=clock_timestamp() WHERE id=$1`, installation)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE trigger_activations SET active_until=NULL,next_fire_at=$3 WHERE environment_id=$1 AND id=$2`, f.env, trigger, due.Add(time.Minute))
			if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
				t.Fatal(err)
			}
			var valid bool
			if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions WHERE environment_id=$1)=2 AND (SELECT count(*) FROM computers WHERE environment_id=$1)=2 AND EXISTS(SELECT 1 FROM trigger_occurrences WHERE environment_id=$1 AND activation_id=$2 AND scheduled_at=$3 AND disposition='rejected' AND reason='slack_channel_unavailable' AND session_id IS NULL AND turn_id IS NULL)`, f.env, trigger, due.Add(time.Minute)).Scan(&valid); err != nil || !valid {
				t.Fatalf("revoked fire created work: %v %v", valid, err)
			}
			if err := EvaluateTrigger(t.Context(), f.pool, nil, f.env, trigger); err != nil {
				t.Fatal(err)
			}
		})
	}
}
