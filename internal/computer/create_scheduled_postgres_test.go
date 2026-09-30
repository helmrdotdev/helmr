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

func (f fixture) scheduledRequest(scheduleID uuid.UUID, secrets ...LockedSecret) ScheduledRequest {
	return ScheduledRequest{
		EnvironmentID: f.EnvironmentID, ScheduleID: scheduleID, ScheduleGeneration: 1,
		SandboxDeclaredID: declaredID, Secrets: secrets,
	}
}

// createScheduled runs CreateScheduled in its own transaction and commits
// only when it succeeds.
func (f fixture) createScheduled(t *testing.T, creator Creator, request ScheduledRequest) (ScheduledComputer, error) {
	t.Helper()
	var created ScheduledComputer
	err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		var err error
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
			protected := LockedSecret{SecretID: f.secretID, Kind: "env", Target: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}
			raw := LockedSecret{SecretID: f.secretID, Kind: "env", Target: "RAW", Mode: "raw"}
			file := LockedSecret{SecretID: f.secretID, Kind: "file", Target: "/run/secrets/key", Mode: "raw"}
			var secrets []LockedSecret
			switch mode {
			case "protected":
				secrets = []LockedSecret{protected}
			case "mixed":
				secrets = []LockedSecret{protected, raw, file}
			case "raw":
				secrets = []LockedSecret{raw}
			}
			calls := 0
			creator := NewCreator(issuerFunc(func(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error) {
				calls++
				return f.store.GenerateProxyTrust(environmentID, computerID, createdAt)
			}))
			created, err := f.createScheduled(t, creator, f.scheduledRequest(scheduleID, secrets...))
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
	raw := LockedSecret{SecretID: f.secretID, Kind: "env", Target: "RAW", Mode: "raw"}
	protected := LockedSecret{SecretID: f.secretID, Kind: "env", Target: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}
	for name, test := range map[string]struct {
		request ScheduledRequest
		creator Creator
		setup   func(t *testing.T) func()
		want    func(error) bool
	}{
		"stale generation": {
			request: ScheduledRequest{EnvironmentID: f.EnvironmentID, ScheduleID: scheduleID, ScheduleGeneration: 2, SandboxDeclaredID: declaredID, Secrets: []LockedSecret{raw}},
			want:    func(err error) bool { return errors.Is(err, ErrNotDeployed) },
		},
		"absent schedule": {
			request: f.scheduledRequest(uuid.NewV7(), raw),
			want:    func(err error) bool { return errors.Is(err, ErrNotDeployed) },
		},
		"absent Sandbox": {
			request: ScheduledRequest{EnvironmentID: f.EnvironmentID, ScheduleID: scheduleID, ScheduleGeneration: 1, SandboxDeclaredID: "absent-computer"},
			want:    func(err error) bool { return errors.Is(err, ErrNotDeployed) },
		},
		"CA generation failure": {
			request: f.scheduledRequest(scheduleID, protected),
			creator: NewCreator(issuerFunc(func(uuid.UUID, uuid.UUID, time.Time) (secret.ProxyTrust, error) {
				return secret.ProxyTrust{}, errors.New("synthetic generation failure")
			})),
			want: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "synthetic generation failure")
			},
		},
		"placement failure after CA": {
			request: f.scheduledRequest(scheduleID, protected, raw),
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
			if test.setup != nil {
				defer test.setup(t)()
			}
			creator := test.creator
			if creator == (Creator{}) {
				creator = f.creator()
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
	if _, err := f.createScheduled(t, f.creator(), f.scheduledRequest(scheduleID, protected, raw)); err != nil {
		t.Fatalf("creation after rejected attempts: %v", err)
	}
}
