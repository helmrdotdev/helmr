package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
)

// GET /v1/computers?secret= reports how each listed Computer holds the raw value.
func TestComputerListBySecretHTTP(t *testing.T) {
	f := sessiontest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
	store, err := secret.New(queries, f.Pool, bytes.Repeat([]byte{17}, 32), auditpolicy.Policy{RetentionSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Secrets, cfg.SecretDelivery, cfg.SecretProxy = store, store, store
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	h := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
	created, err := store.Create(t.Context(), dbtest.SecretManager(t, f.Pool, f.Environment), f.Environment, "INVENTORY_TOKEN", []byte("raw"), "")
	if err != nil {
		t.Fatal(err)
	}
	secretID := pgvalue.MustUUIDValue(created.ID)
	build, imaged := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO secret_exposures(id,environment_id,session_id,process_epoch,secret_id,version_id) SELECT uuidv7(),$1,$2,1,id,current_version_id FROM secrets WHERE id=$3;
 INSERT INTO secret_bindings(environment_id,image_spec_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$4,$3,'env','INVENTORY_TOKEN','raw');
 INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at) VALUES($1,$5,$4,'inventory',clock_timestamp()+interval '10 minutes');
 INSERT INTO secret_exposures(id,environment_id,image_build_id,secret_id,version_id) SELECT uuidv7(),$1,$5,id,current_version_id FROM secrets WHERE id=$3;
 INSERT INTO images(environment_id,id,image_spec_id,image_build_id,seq,root_id,publication_evidence) SELECT environment_id,$5,$4,$5,1,initial_root_id,'fixture' FROM computers WHERE environment_id=$1 AND id=$6;
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,image_spec_id,image_id,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes)
 SELECT environment_id,$7,initial_root_id,initial_root_digest,$4,$5,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,storage_reservation_bytes FROM computers WHERE environment_id=$1 AND id=$6;
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Session, secretID, f.Deployment, build, f.Computer, imaged)
	key, err := issueAPIKeyWithRole(t, t.Context(), f.Pool, f.Principal(t), auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: f.Environment.String()}, testAPIKeyInput{Name: "inventory", RolePermissions: []auth.Permission{auth.PermissionComputersRead}})
	if err != nil {
		t.Fatal(err)
	}
	list := func(path string) map[string]api.ComputerSecretExposure {
		t.Helper()
		response := h.request(t, http.MethodGet, path, key.Raw, "")
		var page api.ListComputersResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
		}
		result := map[string]api.ComputerSecretExposure{}
		for _, item := range page.Computers {
			result[item.ID] = item.SecretExposure
		}
		return result
	}
	got := list("/v1/computers?secret=INVENTORY_TOKEN")
	if len(got) != 2 || got[f.Computer.String()] != api.ComputerSecretExposureReceived || got[imaged.String()] != api.ComputerSecretExposureImage {
		t.Fatalf("inventory = %v", got)
	}
	raw := h.request(t, http.MethodGet, "/v1/computers?secret=INVENTORY_TOKEN", key.Raw, "").Body.Bytes()
	if !bytes.Contains(raw, []byte(`"secret_exposure":"received"`)) || !bytes.Contains(raw, []byte(`"secret_exposure":"image"`)) {
		t.Fatalf("secret_exposure JSON field missing: %s", raw)
	}
	unfiltered := h.request(t, http.MethodGet, "/v1/computers", key.Raw, "").Body.Bytes()
	if bytes.Contains(unfiltered, []byte(`secret_exposure`)) {
		t.Fatalf("unfiltered list reported exposures: %s", unfiltered)
	}
	if bad := h.request(t, http.MethodGet, "/v1/computers?secret=INVENTORY_TOKEN&key=fixture", key.Raw, ""); bad.Code != http.StatusBadRequest {
		t.Fatalf("secret with key: %d", bad.Code)
	}
}
