package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestManualRefreshRejectsChangedSecretExposure(t *testing.T) {
	for _, transition := range []string{"unchanged", "rotated", "revoked"} {
		t.Run(transition, func(t *testing.T) {
			p, _ := newPreparationPublicationTest(t)
			if _, err := computer.RecordImageBuildExposure(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, p.f.pool, *p.f.host(), p.ref); err != nil {
				t.Fatal(err)
			}
			switch transition {
			case "rotated":
				if _, err := p.f.secrets.Rotate(t.Context(), audit.System("test-fixture"), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if _, err := p.f.secrets.Revoke(t.Context(), audit.System("test-fixture"), p.f.env, p.f.secretID, "revoke"); err != nil {
					t.Fatal(err)
				}
			}
			var spec uuid.UUID
			if err := p.f.pool.QueryRow(t.Context(), `SELECT image_spec_id FROM image_builds WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID).Scan(&spec); err != nil {
				t.Fatal(err)
			}
			err := db.RunTx(t.Context(), p.f.pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, p.f.env); err != nil {
					return err
				}
				if err := computer.LockImageSpecSecrets(t.Context(), tx, p.f.env, spec); err != nil {
					return err
				}
				if err := computer.LockImageSpec(t.Context(), tx, p.f.env, spec); err != nil {
					return err
				}
				id, err := computer.StartTemplateRefresh(t.Context(), tx, p.f.env, spec, p.f.deployment)
				if err == nil && id != p.ref.ImageBuildID {
					t.Errorf("refresh selected another build: %s", id)
				}
				return err
			})
			switch transition {
			case "unchanged":
				if err != nil {
					t.Fatal(err)
				}
			case "rotated":
				if !errors.Is(err, computer.ErrNotReady) {
					t.Fatalf("rotated exposure: %v", err)
				}
			case "revoked":
				if !errors.Is(err, computer.ErrSecretUnavailable) {
					t.Fatalf("revoked exposure: %v", err)
				}
			}
		})
	}
}

func TestManualRefreshFailurePreservesPreviousImage(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	if err := computer.ObserveImageBuildStopped(t.Context(), p.f.pool, *p.f.host(), preparationIdentity(p.ref)); err != nil {
		t.Fatal(err)
	}
	old, err := computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID, nil)
	if err != nil || !old.Ready {
		t.Fatalf("published image: %+v %v", old, err)
	}
	var fresh uuid.UUID
	err = db.RunTx(t.Context(), p.f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, p.f.env); err != nil {
			return err
		}
		if err := computer.LockImageSpecSecrets(t.Context(), tx, p.f.env, old.Build.SpecID); err != nil {
			return err
		}
		if err := computer.LockImageSpec(t.Context(), tx, p.f.env, old.Build.SpecID); err != nil {
			return err
		}
		var err error
		fresh, err = computer.StartTemplateRefresh(t.Context(), tx, p.f.env, old.Build.SpecID, p.f.deployment)
		return err
	})
	if err != nil || fresh == p.ref.ImageBuildID {
		t.Fatalf("fresh build: %s %v", fresh, err)
	}
	if _, err := p.f.pool.Exec(t.Context(), `UPDATE image_builds SET status='failed',error_code='authored_preparation_failed' WHERE environment_id=$1 AND id=$2`, p.f.env, fresh); err != nil {
		t.Fatal(err)
	}
	failed, err := computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, fresh, nil)
	if err != nil || failed.Ready || failed.Build.Status != "failed" {
		t.Fatalf("failed refresh: %+v %v", failed, err)
	}
	old, err = computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID, nil)
	if err != nil || !old.Ready {
		t.Fatalf("old image discarded: %+v %v", old, err)
	}
	pin, err := computer.PinImage(t.Context(), p.f.pool, p.f.env, p.f.waiter(t))
	if err != nil || pin.ImageBuildID != p.ref.ImageBuildID {
		t.Fatalf("old image cannot serve new Computer: %+v %v", pin, err)
	}
	if _, err := p.f.secrets.Rotate(t.Context(), audit.System("test-fixture"), p.f.env, p.f.secretID, []byte("v2"), "rotate-after-publication"); err != nil {
		t.Fatal(err)
	}
	stale, err := computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID, nil)
	if err != nil || stale.Ready || stale.Build.Status != "succeeded" {
		t.Fatalf("stale published image: %+v %v", stale, err)
	}
}

func TestTemplateBuildObservationUsesDatabaseDeadline(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	// Observation must expose expiry even before the reaper changes stored state.
	if _, err := p.f.pool.Exec(t.Context(), `UPDATE image_builds SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID); err != nil {
		t.Fatal(err)
	}
	expired, err := computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID, nil)
	if err != nil || expired.ErrorCode != "deadline_exceeded" || expired.Build.Status != "running" || expired.Ready {
		t.Fatalf("expired observation: %+v %v", expired, err)
	}
	var status string
	var code *string
	if err := p.f.pool.QueryRow(t.Context(), `SELECT status,error_code FROM image_builds WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID).Scan(&status, &code); err != nil || status != "running" || code != nil {
		t.Fatalf("observation mutated build: %s %v %v", status, code, err)
	}
	if _, err := p.f.pool.Exec(t.Context(), `UPDATE image_builds SET deadline_at=clock_timestamp()+interval '10 minutes' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID); err != nil {
		t.Fatal(err)
	}
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.pool.Exec(t.Context(), `UPDATE image_builds SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, p.ref.ImageBuildID); err != nil {
		t.Fatal(err)
	}
	published, err := computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID, nil)
	if err != nil || !published.Ready || published.ErrorCode != "" || published.Build.Status != "succeeded" {
		t.Fatalf("published eligibility changed at build deadline: %+v %v", published, err)
	}
}

func TestSeedOnlyImagePublicationAndPinning(t *testing.T) {
	for _, bindings := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-bindings", true: "with-bindings"}[bindings], func(t *testing.T) {
			p, key := newPreparationPublicationTest(t)
			var prepare bool
			if err := p.f.pool.QueryRow(t.Context(), `SELECT prepare FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2`, p.f.env, p.f.deployment).Scan(&prepare); err != nil || prepare {
				t.Fatalf("fixture is not seed-only: %v", err)
			}
			if !bindings {
				if _, err := p.f.pool.Exec(t.Context(), `DELETE FROM secret_bindings WHERE environment_id=$1 AND image_spec_id=$2`, p.f.env, p.f.deployment); err != nil {
					t.Fatal(err)
				}
			}
			root := p.capture(t, key)
			if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "isolated clone passed fixed Runtime probe"); err != nil {
				t.Fatal(err)
			}
			if err := computer.ObserveImageBuildStopped(t.Context(), p.f.pool, *p.f.host(), preparationIdentity(p.ref)); err != nil {
				t.Fatal(err)
			}
			state, err := computer.ReadImageBuildState(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID, nil)
			if err != nil || !state.Ready {
				t.Fatalf("seed-only image ineligible: %+v %v", state, err)
			}
			pin, err := computer.PinImage(t.Context(), p.f.pool, p.f.env, p.f.waiter(t))
			if err != nil || pin.ImageBuildID != p.ref.ImageBuildID {
				t.Fatalf("seed-only image not pinned: %+v %v", pin, err)
			}
			var exposures int
			if err := p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_exposures WHERE environment_id=$1 AND image_build_id=$2`, p.f.env, p.ref.ImageBuildID).Scan(&exposures); err != nil || exposures != map[bool]int{false: 0, true: 1}[bindings] {
				t.Fatalf("binding evidence: %d %v", exposures, err)
			}
		})
	}
}
