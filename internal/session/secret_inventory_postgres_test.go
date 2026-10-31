package session_test

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

// Listing Computers by Secret finds every non-deleted Computer that received a
// raw value of that Secret, directly or through its image, and nothing else.
func TestSecretInventoryListsDirectAndImageHolders(t *testing.T) {
	p := preparationResidentImage(t)
	f := p.f.fixture
	var name string
	if err := f.pool.QueryRow(t.Context(), `SELECT name FROM secrets WHERE id=$1`, p.f.secretID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	var waiting uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM computers WHERE environment_id=$1 AND image_build_id=$2 AND initial_root_id IS NULL`, f.env, p.ref.ImageBuildID).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.PinImage(t.Context(), f.pool, f.env, waiting); err != nil {
		t.Fatal(err)
	}
	deleted := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes,deleted_at)
 SELECT environment_id,$3,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes,clock_timestamp() FROM computers WHERE environment_id=$1 AND id=$2`, f.env, waiting, deleted)
	other, err := p.f.secrets.Create(t.Context(), dbtest.SecretManager(t, f.pool, f.env), f.env, "INVENTORY_OTHER", []byte("other"), "")
	if err != nil {
		t.Fatal(err)
	}
	// The resident Computer's process received both raw values directly.
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO secret_exposures(id,environment_id,session_id,process_epoch,secret_id,version_id) SELECT uuidv7(),$1,$2,1,id,current_version_id FROM secrets WHERE environment_id=$1 AND id=ANY($3::uuid[])`, f.env, f.session, []uuid.UUID{p.f.secretID, uuid.UUID(other.ID.Bytes)})
	scope := computer.Scope{EnvironmentID: f.env}
	if err = f.pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.env).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	list := func(secret string, limit int32, after *computer.ListPosition) computer.Listing {
		t.Helper()
		listing, err := computer.List(t.Context(), f.pool, scope, computer.ListPage{Limit: limit, After: after, Secret: secret})
		if err != nil {
			t.Fatal(err)
		}
		return listing
	}
	exposures := func(items []computer.ListItem) map[string]computer.SecretExposure {
		result := map[string]computer.SecretExposure{}
		for _, item := range items {
			result[item.ID] = item.SecretExposure
		}
		return result
	}
	got := exposures(list(name, 100, nil).Items)
	want := map[string]computer.SecretExposure{f.computer.String(): computer.SecretExposureReceived, waiting.String(): computer.SecretExposureImage}
	if len(got) != len(want) || got[f.computer.String()] != want[f.computer.String()] || got[waiting.String()] != want[waiting.String()] {
		t.Fatalf("inventory = %v, want %v", got, want)
	}
	first := list(name, 1, nil)
	if len(first.Items) != 1 || !first.More {
		t.Fatalf("first page = %+v", first)
	}
	position := computer.ListPosition{CreatedAt: first.Items[0].CreatedAt, ID: uuid.MustParse(first.Items[0].ID)}
	second := list(name, 1, &position)
	if len(second.Items) != 1 || second.More || second.Items[0].ID == first.Items[0].ID || second.Items[0].SecretExposure == "" {
		t.Fatalf("second page = %+v", second)
	}
	if got = exposures(list("INVENTORY_OTHER", 100, nil).Items); len(got) != 1 || got[f.computer.String()] != computer.SecretExposureReceived {
		t.Fatalf("other Secret inventory = %v", got)
	}
	if listing := list("UNKNOWN_SECRET", 100, nil); len(listing.Items) != 0 {
		t.Fatalf("unknown Secret inventory = %+v", listing.Items)
	}
	for _, item := range list("", 100, nil).Items {
		if item.SecretExposure != "" {
			t.Fatalf("unfiltered list reported an exposure: %+v", item)
		}
	}
}
