package computer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

// scheduleFor inserts an active schedule of the runtest Task pinned to the
// fixture deployment, at generation 1.
func (f fixture) scheduleFor(t *testing.T) uuid.UUID {
	t.Helper()
	scheduleID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
INSERT INTO schedules (id, environment_id, task_declared_id, deployment_definition_id, deployment_id,
    cron_pattern, timezone, status, effective_from, next_fire_at)
VALUES ($1, $2, 'test-task', $3, $4, '* * * * *', 'UTC', 'active', now(), now() + interval '1 minute')`,
		scheduleID, f.EnvironmentID, f.TaskDefinitionID, f.DeploymentID)
	return scheduleID
}

// schedulePlacement is a placement of the fixture API_TOKEN Secret on a
// schedule.
type schedulePlacement struct {
	kind, target, mode string
	origins            []string
}

var (
	protectedPlacement = schedulePlacement{kind: "env", target: "TOKEN", mode: "protected", origins: []string{"https://example.com"}}
	rawPlacement       = schedulePlacement{kind: "env", target: "RAW", mode: "raw"}
	filePlacement      = schedulePlacement{kind: "file", target: "/run/secrets/key", mode: "raw"}
)

// setScheduleSecrets replaces the schedule's Secret placements.
func (f fixture) setScheduleSecrets(t *testing.T, scheduleID uuid.UUID, placements ...schedulePlacement) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM schedule_secrets WHERE schedule_id=$1`, scheduleID)
	for _, p := range placements {
		dbtest.MustExec(t, t.Context(), f.Pool, `
INSERT INTO schedule_secrets (environment_id, schedule_id, placement_kind, placement_target, secret_id, mode, allowed_origins)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::text[], '{}'))`,
			f.EnvironmentID, scheduleID, p.kind, p.target, f.secretID, p.mode, p.origins)
	}
}

func (f fixture) scheduledRequest(scheduleID uuid.UUID) ScheduledRequest {
	return ScheduledRequest{
		EnvironmentID: f.EnvironmentID, ScheduleID: scheduleID, ScheduleGeneration: 1,
		SandboxDeclaredID: declaredID,
	}
}

// createScheduled locks the request's schedule Secrets and runs
// CreateScheduled in one transaction, committing only when it succeeds.
func (f fixture) createScheduled(t *testing.T, creator Creator, request ScheduledRequest) (ScheduledComputer, error) {
	t.Helper()
	var created ScheduledComputer
	err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		var err error
		request.Secrets, err = LockScheduleSecrets(t.Context(), tx, request.EnvironmentID, request.ScheduleID)
		if err != nil {
			return err
		}
		created, err = creator.CreateScheduled(t.Context(), tx, request)
		return err
	})
	return created, err
}

type issuerFunc func(uuid.UUID, uuid.UUID, time.Time) (secret.ProxyTrust, error)

func (f issuerFunc) GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error) {
	return f(environmentID, computerID, createdAt)
}

func TestCreateScheduledInstallsComputerCAAndPlacements(t *testing.T) {
	for _, mode := range []string{"protected", "mixed", "raw", "none"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			scheduleID := f.scheduleFor(t)
			var secrets []schedulePlacement
			switch mode {
			case "protected":
				secrets = []schedulePlacement{protectedPlacement}
			case "mixed":
				secrets = []schedulePlacement{protectedPlacement, rawPlacement, filePlacement}
			case "raw":
				secrets = []schedulePlacement{rawPlacement}
			}
			f.setScheduleSecrets(t, scheduleID, secrets...)
			calls := 0
			creator := NewCreator(issuerFunc(func(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error) {
				calls++
				return f.store.GenerateProxyTrust(environmentID, computerID, createdAt)
			}))
			created, err := f.createScheduled(t, creator, f.scheduledRequest(scheduleID))
			if err != nil {
				t.Fatal(err)
			}

			var (
				headID, creationDeployment uuid.UUID
				revision                   int64
				createdAt                  time.Time
				versionStatus              string
			)
			if err := f.Pool.QueryRow(t.Context(), `
SELECT computers.head_disk_version_id, computers.creation_deployment_id, computers.revision, computers.created_at, versions.status
  FROM computers
  JOIN computer_disk_versions AS versions ON versions.id = computers.head_disk_version_id AND versions.computer_id = computers.id
 WHERE computers.id = $1 AND computers.environment_id = $2 AND computers.key IS NULL`,
				created.ID, f.EnvironmentID).Scan(&headID, &creationDeployment, &revision, &createdAt, &versionStatus); err != nil {
				t.Fatal(err)
			}
			if headID != created.HeadDiskVersionID || revision != created.Revision || creationDeployment != f.DeploymentID || versionStatus != "initializing" {
				t.Fatalf("scheduled Computer differs: head %s/%s revision %d/%d deployment %s status %s",
					headID, created.HeadDiskVersionID, revision, created.Revision, creationDeployment, versionStatus)
			}

			ca, err := db.New(f.Pool).GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{
				EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(created.ID),
			})
			if err != nil {
				t.Fatal(err)
			}
			hasCA := mode == "protected" || mode == "mixed"
			if (len(ca.Certificate) > 0) != hasCA || ca.NotAfter.Valid != hasCA {
				t.Fatal("CA presence differs from placements")
			}
			wantCalls := 0
			if hasCA {
				wantCalls = 1
				if !ca.NotAfter.Time.Equal(createdAt.AddDate(10, 0, 0).Truncate(time.Second)) {
					t.Fatal("CA expiry not anchored to the inserted Computer")
				}
			}
			if calls != wantCalls {
				t.Fatalf("CA generated %d times, want %d", calls, wantCalls)
			}

			rows, err := db.New(f.Pool).ListComputerSecrets(t.Context(), pgvalue.UUID(created.ID))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(secrets) {
				t.Fatalf("placements = %d, want %d", len(rows), len(secrets))
			}
			for _, row := range rows {
				if row.SecretID != pgvalue.UUID(f.secretID) {
					t.Fatal("placement changed Secret identity")
				}
				if row.Mode == "protected" {
					if row.PlacementTarget != "TOKEN" || len(row.AllowedOrigins) != 1 || row.AllowedOrigins[0] != "https://example.com" || !strings.HasPrefix(row.Placeholder, "hlmr_protected_") {
						t.Fatal("protected placement metadata incorrect")
					}
				} else if row.Placeholder != "" || len(row.AllowedOrigins) != 0 {
					t.Fatal("raw placement acquired protected state")
				}
			}
		})
	}
}

func TestCreateScheduledRejectsWithoutWrites(t *testing.T) {
	f := newFixture(t)
	scheduleID := f.scheduleFor(t)
	stale := f.scheduledRequest(scheduleID)
	stale.ScheduleGeneration = 2
	absentSandbox := f.scheduledRequest(scheduleID)
	absentSandbox.SandboxDeclaredID = "absent-computer"
	for name, test := range map[string]struct {
		request    ScheduledRequest
		placements []schedulePlacement
		issuer     CAIssuer
		setup      func(t *testing.T) func()
		want       func(error) bool
	}{
		"stale generation": {
			request: stale, placements: []schedulePlacement{rawPlacement},
			want: func(err error) bool { return errors.Is(err, ErrNotDeployed) },
		},
		"absent schedule": {
			request: f.scheduledRequest(uuid.NewV7()),
			want:    func(err error) bool { return errors.Is(err, ErrNotDeployed) },
		},
		"absent Sandbox": {
			request: absentSandbox,
			want:    func(err error) bool { return errors.Is(err, ErrNotDeployed) },
		},
		"CA generation failure": {
			request: f.scheduledRequest(scheduleID), placements: []schedulePlacement{protectedPlacement},
			issuer: issuerFunc(func(uuid.UUID, uuid.UUID, time.Time) (secret.ProxyTrust, error) {
				return secret.ProxyTrust{}, errors.New("synthetic generation failure")
			}),
			want: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "synthetic generation failure")
			},
		},
		"placement failure after CA": {
			request: f.scheduledRequest(scheduleID), placements: []schedulePlacement{protectedPlacement, rawPlacement},
			setup: func(t *testing.T) func() {
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_scheduled_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NOT EXISTS (SELECT 1 FROM computers WHERE id=NEW.computer_id AND secret_ca_certificate IS NOT NULL) THEN RAISE EXCEPTION 'CA not generated'; END IF;
        RAISE EXCEPTION 'synthetic binding failure'; END $$;
        CREATE TRIGGER reject_scheduled_binding BEFORE INSERT ON computer_secrets FOR EACH ROW EXECUTE FUNCTION reject_scheduled_binding();`)
				return func() {
					dbtest.MustExec(t, context.Background(), f.Pool, `DROP TRIGGER reject_scheduled_binding ON computer_secrets; DROP FUNCTION reject_scheduled_binding()`)
				}
			},
			want: func(err error) bool { return err != nil && strings.Contains(err.Error(), "synthetic binding failure") },
		},
	} {
		t.Run(name, func(t *testing.T) {
			f.setScheduleSecrets(t, scheduleID, test.placements...)
			if test.setup != nil {
				defer test.setup(t)()
			}
			creator := f.creator()
			if test.issuer != nil {
				creator = NewCreator(test.issuer)
			}
			before := f.count(t, "SELECT count(*) FROM computers")
			beforeVersions := f.count(t, "SELECT count(*) FROM computer_disk_versions")
			if _, err := f.createScheduled(t, creator, test.request); !test.want(err) {
				t.Fatalf("error = %v", err)
			}
			if f.count(t, "SELECT count(*) FROM computers") != before || f.count(t, "SELECT count(*) FROM computer_disk_versions") != beforeVersions {
				t.Fatal("rejected scheduled creation retained a Computer")
			}
		})
	}
	f.setScheduleSecrets(t, scheduleID, protectedPlacement, rawPlacement)
	if _, err := f.createScheduled(t, f.creator(), f.scheduledRequest(scheduleID)); err != nil {
		t.Fatalf("creation after rejected attempts: %v", err)
	}
}

func TestCreateScheduledRequiresSecretsLockedForItsScheduleAndTransaction(t *testing.T) {
	f := newFixture(t)
	scheduleID := f.scheduleFor(t)
	f.setScheduleSecrets(t, scheduleID, protectedPlacement)
	lockIn := func(tx pgx.Tx, environmentID, scheduleID uuid.UUID) ScheduleSecrets {
		t.Helper()
		held, err := LockScheduleSecrets(t.Context(), tx, environmentID, scheduleID)
		if err != nil {
			t.Fatal(err)
		}
		return held
	}
	for name, secrets := range map[string]func(tx pgx.Tx) ScheduleSecrets{
		"never locked":        func(pgx.Tx) ScheduleSecrets { return ScheduleSecrets{} },
		"another schedule":    func(tx pgx.Tx) ScheduleSecrets { return lockIn(tx, f.EnvironmentID, uuid.NewV7()) },
		"another environment": func(tx pgx.Tx) ScheduleSecrets { return lockIn(tx, uuid.NewV7(), scheduleID) },
		"another transaction": func(pgx.Tx) ScheduleSecrets {
			other, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			held := lockIn(other, f.EnvironmentID, scheduleID)
			if err := other.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			return held
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			creator := NewCreator(issuerFunc(func(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error) {
				calls++
				return f.store.GenerateProxyTrust(environmentID, computerID, createdAt)
			}))
			before := f.count(t, "SELECT count(*) FROM computers")
			err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				request := f.scheduledRequest(scheduleID)
				request.Secrets = secrets(tx)
				_, err := creator.CreateScheduled(t.Context(), tx, request)
				return err
			})
			if !errors.Is(err, errScheduleSecretsNotHeld) {
				t.Fatalf("error = %v", err)
			}
			if calls != 0 || f.count(t, "SELECT count(*) FROM computers") != before {
				t.Fatal("rejected scheduled creation wrote a Computer or CA")
			}
		})
	}
}
