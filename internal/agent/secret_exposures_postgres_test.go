package agent

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSecretExposureTypedOwnersAndPinnedVersions(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	values, err := RecordPreparationExposure(t.Context(), f.pool, *f.host(), ref)
	if err != nil || len(values) == 0 {
		t.Fatalf("exposure: %v", err)
	}
	version := values[0].VersionID
	command := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_commands(environment_id,id,computer_id,computer_lease_epoch,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id)
 VALUES($1,$2,$3,1,ARRAY['true'],'{}','',1000,'user',$4)`, f.env, command, f.computer, f.user.String())
	// The same version may be exposed independently to all three typed owners.
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation) VALUES($1,$2,1,$3,$4,0)`, f.env, f.session, f.secretID, version)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO secret_exposures(environment_id,command_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,0)`, f.env, command, f.secretID, version)
	reject := func(name, code, query string, args ...any) {
		t.Helper()
		_, err := f.pool.Exec(t.Context(), query, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s: expected %s, got %v", name, code, err)
		}
	}
	reject("no owner", "23514", `INSERT INTO secret_exposures(environment_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,0)`, f.env, f.secretID, version)
	reject("multiple owners", "23514", `INSERT INTO secret_exposures(environment_id,preparation_id,command_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,$5,0)`, f.env, p.ID, command, f.secretID, version)
	reject("missing process epoch", "23514", `INSERT INTO secret_exposures(environment_id,session_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,0)`, f.env, f.session, f.secretID, version)
	reject("wrong process epoch", "23503", `INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation) VALUES($1,$2,9,$3,$4,0)`, f.env, f.session, f.secretID, version)
	reject("unknown command", "23503", `INSERT INTO secret_exposures(environment_id,command_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,0)`, f.env, uuid.NewV7(), f.secretID, version)
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) SELECT 'until_environment_deletion',$2,org_id,project_id,'other-exposure','Other','#123456' FROM environments WHERE id=$1`, f.env, other)
	if _, err = f.secrets.Create(t.Context(), other, "OTHER", []byte("other"), "other-exposure"); err != nil {
		t.Fatal(err)
	}
	var otherSecret, otherVersion uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT id,current_version_id FROM secrets WHERE environment_id=$1 AND name='OTHER'`, other).Scan(&otherSecret, &otherVersion); err != nil {
		t.Fatal(err)
	}
	reject("cross environment preparation", "23503", `INSERT INTO secret_exposures(environment_id,preparation_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,0)`, other, p.ID, otherSecret, otherVersion)
	reject("cross environment process", "23503", `INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation) VALUES($1,$2,1,$3,$4,0)`, other, f.session, otherSecret, otherVersion)
	reject("cross environment command", "23503", `INSERT INTO secret_exposures(environment_id,command_id,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,0)`, other, command, otherSecret, otherVersion)
	_, err = f.secrets.Rotate(t.Context(), f.env, f.secretID, []byte("v2"), "rotate-exposure")
	if err != nil {
		t.Fatal(err)
	}
	var next uuid.UUID
	if err = f.pool.QueryRow(t.Context(), `SELECT current_version_id FROM secrets WHERE environment_id=$1 AND id=$2`, f.env, f.secretID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"preparation_id", "command_id"} {
		id := p.ID
		if owner == "command_id" {
			id = command
		}
		reject("duplicate "+owner, "23505", `INSERT INTO secret_exposures(environment_id,`+owner+`,secret_id,version_id,revocation_generation) VALUES($1,$2,$3,$4,0)`, f.env, id, f.secretID, next)
	}
	reject("duplicate process", "23505", `INSERT INTO secret_exposures(environment_id,session_id,process_epoch,secret_id,version_id,revocation_generation) VALUES($1,$2,1,$3,$4,0)`, f.env, f.session, f.secretID, next)
	var pinned bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=3 AND bool_and(version_id=$2 AND revocation_generation=0) FROM secret_exposures WHERE environment_id=$1`, f.env, version).Scan(&pinned); err != nil || !pinned {
		t.Fatalf("pinned versions: %v %v", pinned, err)
	}
	if _, err = f.secrets.Revoke(t.Context(), f.env, f.secretID, "revoke-exposure"); err != nil {
		t.Fatal(err)
	}
	var older bool
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*)=3 AND bool_and(x.revocation_generation<s.revocation_generation) FROM secret_exposures x JOIN secrets s ON s.environment_id=x.environment_id AND s.id=x.secret_id WHERE x.environment_id=$1`, f.env).Scan(&older); err != nil || !older {
		t.Fatalf("exposure generation lost after revocation: %v %v", older, err)
	}

}
