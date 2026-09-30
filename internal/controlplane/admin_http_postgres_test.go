package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

const supplyAdminEmail = "admin@example.test"

// newSupplyHTTPFixture serves the control plane with a platform administrator
// address and a capacity token configured.
func newSupplyHTTPFixture(t *testing.T) httpPostgresFixture {
	t.Helper()
	return newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.AdminEmails = []string{supplyAdminEmail}
		cfg.CapacityToken = capacityTestToken()
	})
}

// adminSession signs in the configured administrator address and returns the
// session token.
func (f httpPostgresFixture) adminSession(t *testing.T) string {
	t.Helper()
	token, err := identity.SignIn(t.Context(), f.pool, identity.NewConfig(f.keys, identity.Lifetimes{}, []string{supplyAdminEmail}), identity.ExternalIdentity{
		Provider: "github", Subject: "supply-admin", DisplayName: "Administrator",
		Email: supplyAdminEmail, EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// sealWorkerPool seals a pending pool the way the first activating host does.
func (f httpPostgresFixture) sealWorkerPool(t *testing.T, groupID string, poolID string) {
	t.Helper()
	platformID := dbtest.Digest("supply-http-runtime")
	if _, err := f.queries.UpsertVMPlatform(t.Context(), db.UpsertVMPlatformParams{
		ID: platformID, Arch: "x86_64", Contract: vmplatform.Contract,
		DescriptorDigest:  dbtest.Digest("supply-http-descriptor"),
		FirecrackerDigest: dbtest.Digest("supply-http-firecracker"), FirecrackerVersion: "1.12.0",
		SnapshotFormatVersion: "1.0.0", HostKernelRelease: "6.12.0", CPUTemplateKind: "none",
		KernelDigest: dbtest.Digest("supply-http-kernel"), InitramfsDigest: dbtest.Digest("supply-http-initramfs"),
		RootfsDigest: dbtest.Digest("supply-http-rootfs"),
	}); err != nil {
		t.Fatal(err)
	}
	pool := pgvalue.UUID(uuid.MustParse(poolID))
	if _, err := f.queries.InsertWorkerPoolCPUShape(t.Context(), db.InsertWorkerPoolCPUShapeParams{
		VCPUCount: 1, CPUConfigDigest: dbtest.Digest("supply-http-cpu"), WorkerPoolID: pool,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.queries.SealWorkerPool(t.Context(), db.SealWorkerPoolParams{
		VMPlatformID:                    pgvalue.Text(platformID),
		CapacityCPUMillis:               pgtype.Int8{Int64: 4_000, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: 8 << 30, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: 32 << 30, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: 1_000, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: 1 << 30, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: 4 << 30, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: 4, Valid: true},
		WorkerPoolID:                    pool,
		WorkerGroupID:                   pgvalue.UUID(uuid.MustParse(groupID)),
	}); err != nil {
		t.Fatal(err)
	}
}

// supplyGroup creates a region and a worker group through the admin API and
// returns the group.
func (f httpPostgresFixture) supplyGroup(t *testing.T, admin string, regionID string) api.AdminWorkerGroup {
	t.Helper()
	decodeAdmin[api.AdminRegion](t, f.request(t, http.MethodPost, "/admin/api/v1/regions", admin,
		`{"id":"`+regionID+`","display_name":"Supply"}`), http.StatusCreated)
	created := decodeAdmin[api.CreateAdminWorkerGroupResponse](t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups", admin,
		`{"region_id":"`+regionID+`","name":"default"}`), http.StatusCreated)
	return created.WorkerGroup
}

func decodeAdmin[T any](t *testing.T, response *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var body T
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return body
}

func assertAdminError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	if got := decodeHTTPError(t, response.Body.Bytes()).Code; got != code {
		t.Fatalf("code = %q, want %q: %s", got, code, response.Body.String())
	}
}

func TestAdminHTTPRequiresPlatformAdminSession(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	ordinary := f.session(t, f.user(t, "Ordinary"), uuid.Nil())
	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "unauthenticated", status: http.StatusUnauthorized},
		{name: "ordinary session", token: ordinary, status: http.StatusForbidden},
		{name: "administrator", token: f.adminSession(t), status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, http.MethodGet, "/admin/api/v1/regions", test.token, "")
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestAdminHTTPRegions(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	response := f.request(t, http.MethodPost, "/admin/api/v1/regions", admin,
		`{"id":"us-east","display_name":" US East ","location":"Virginia, USA"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", response.Code, response.Body.String())
	}
	if want := `{"id":"us-east","display_name":"US East","location":"Virginia, USA"}`; strings.TrimSpace(response.Body.String()) != want {
		t.Fatalf("response = %s, want %s", response.Body.String(), want)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/regions", admin, `{"id":"us-east","display_name":"Again"}`), http.StatusConflict, "conflict")
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/regions", admin, `{"id":" us-west","display_name":"West"}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/regions", admin, `{"id":"us-west","display_name":" "}`), http.StatusBadRequest, "bad_request")

	got := decodeAdmin[api.AdminRegion](t, f.request(t, http.MethodGet, "/admin/api/v1/regions/us-east", admin, ""), http.StatusOK)
	if got.DisplayName != "US East" {
		t.Fatalf("region = %+v", got)
	}
	assertAdminError(t, f.request(t, http.MethodGet, "/admin/api/v1/regions/missing", admin, ""), http.StatusNotFound, "not_found")
	updated := decodeAdmin[api.AdminRegion](t, f.request(t, http.MethodPatch, "/admin/api/v1/regions/us-east", admin, `{"location":" Ohio "}`), http.StatusOK)
	if updated.DisplayName != "US East" || updated.Location != "Ohio" {
		t.Fatalf("updated region = %+v", updated)
	}
	assertAdminError(t, f.request(t, http.MethodPatch, "/admin/api/v1/regions/us-east", admin, `{"display_name":""}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.request(t, http.MethodPatch, "/admin/api/v1/regions/missing", admin, `{"location":"x"}`), http.StatusNotFound, "not_found")
	listed := decodeAdmin[api.AdminRegionsResponse](t, f.request(t, http.MethodGet, "/admin/api/v1/regions", admin, ""), http.StatusOK)
	if len(listed.Regions) != 1 || listed.Regions[0] != updated {
		t.Fatalf("regions = %+v", listed.Regions)
	}
}

func TestAdminHTTPWorkerGroups(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups", admin, `{"region_id":"missing","name":"default"}`), http.StatusNotFound, "not_found")

	decodeAdmin[api.AdminRegion](t, f.request(t, http.MethodPost, "/admin/api/v1/regions", admin, `{"id":"us-east","display_name":"US East"}`), http.StatusCreated)
	response := f.request(t, http.MethodPost, "/admin/api/v1/worker-groups", admin, `{"region_id":"us-east","name":"default","description":" runs "}`)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
	created := decodeAdmin[api.CreateAdminWorkerGroupResponse](t, response, http.StatusCreated)
	group := created.WorkerGroup
	if !strings.HasPrefix(created.EnrollmentToken, auth.EnrollmentTokenPrefix) || group.Status != db.WorkerGroupStatusActive || group.Description != "runs" {
		t.Fatalf("created = %+v", created)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups", admin, `{"region_id":"us-east","name":"default"}`), http.StatusConflict, "conflict")
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups", admin, `{"region_id":"us-east","name":"Upper"}`), http.StatusBadRequest, "bad_request")

	listed := decodeAdmin[api.AdminWorkerGroupsResponse](t, f.request(t, http.MethodGet, "/admin/api/v1/worker-groups?region_id=us-east", admin, ""), http.StatusOK)
	if len(listed.WorkerGroups) != 1 || listed.WorkerGroups[0].ID != group.ID {
		t.Fatalf("worker groups = %+v", listed.WorkerGroups)
	}
	assertAdminError(t, f.request(t, http.MethodGet, "/admin/api/v1/worker-groups?region_id=%20us-east", admin, ""), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.request(t, http.MethodGet, "/admin/api/v1/worker-groups/"+uuid.NewV7().String(), admin, ""), http.StatusNotFound, "not_found")
	assertAdminError(t, f.request(t, http.MethodGet, "/admin/api/v1/worker-groups/not-a-group", admin, ""), http.StatusBadRequest, "bad_request")
	described := decodeAdmin[api.AdminWorkerGroup](t, f.request(t, http.MethodPatch, "/admin/api/v1/worker-groups/"+group.ID, admin, `{"description":"changed"}`), http.StatusOK)
	if described.Description != "changed" {
		t.Fatalf("described = %+v", described)
	}

	path := "/admin/api/v1/worker-groups/" + group.ID
	assertAdminError(t, f.request(t, http.MethodPost, path+"/pause", admin, `{"expected_claim_version":0}`), http.StatusBadRequest, "bad_request")
	paused := decodeAdmin[workergroup.GroupStatus](t, f.request(t, http.MethodPost, path+"/pause", admin, `{"expected_claim_version":`+strconv.FormatInt(group.ClaimVersion, 10)+`}`), http.StatusOK)
	if paused.Status != db.WorkerGroupStatusPaused || paused.ClaimVersion != group.ClaimVersion+1 || !paused.TransitionApplied {
		t.Fatalf("paused = %+v", paused)
	}
	assertAdminError(t, f.request(t, http.MethodPost, path+"/activate", admin, `{"expected_claim_version":`+strconv.FormatInt(group.ClaimVersion, 10)+`}`), http.StatusConflict, "conflict")
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+uuid.NewV7().String()+"/disable", admin, `{"expected_claim_version":1}`), http.StatusNotFound, "not_found")

	rotated := f.request(t, http.MethodPost, path+"/token/rotate", admin, "")
	if rotated.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate Cache-Control = %q, want no-store", rotated.Header().Get("Cache-Control"))
	}
	if token := decodeAdmin[api.RotateWorkerGroupTokenResponse](t, rotated, http.StatusOK); !strings.HasPrefix(token.EnrollmentToken, auth.EnrollmentTokenPrefix) {
		t.Fatalf("rotated token = %+v", token)
	}
	assertAdminError(t, f.request(t, http.MethodPost, "/admin/api/v1/worker-groups/"+uuid.NewV7().String()+"/token/rotate", admin, ""), http.StatusNotFound, "not_found")
}

func TestAdminHTTPWorkerPools(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	group := f.supplyGroup(t, admin, "us-east")
	path := "/admin/api/v1/worker-groups/" + group.ID + "/pools"
	claim := strconv.FormatInt(group.ClaimVersion, 10)

	assertAdminError(t, f.request(t, http.MethodPost, path, admin, `{"name":"run-next","expected_group_claim_version":0}`), http.StatusBadRequest, "bad_request")
	pending := decodeAdmin[api.AdminWorkerPool](t, f.request(t, http.MethodPost, path, admin, `{"name":"run-next","expected_group_claim_version":`+claim+`}`), http.StatusCreated)
	if pending.Status != "pending" || pending.Primary {
		t.Fatalf("pending pool = %+v", pending)
	}
	assertAdminError(t, f.request(t, http.MethodPost, path, admin, `{"name":"run-next","expected_group_claim_version":`+claim+`}`), http.StatusConflict, "conflict")
	assertAdminError(t, f.request(t, http.MethodPost, path+"/"+pending.ID+"/primary", admin, `{"expected_group_claim_version":`+claim+`}`), http.StatusConflict, "conflict")

	f.sealWorkerPool(t, group.ID, pending.ID)
	switched := decodeAdmin[api.SwitchAdminWorkerPoolPrimaryResponse](t, f.request(t, http.MethodPost, path+"/"+pending.ID+"/primary", admin, `{"expected_group_claim_version":`+claim+`}`), http.StatusOK)
	if !switched.WorkerPool.Primary || switched.WorkerGroup.PrimaryPoolID != pending.ID || switched.WorkerGroup.ClaimVersion != group.ClaimVersion+1 {
		t.Fatalf("switched = %+v", switched)
	}
	assertAdminError(t, f.request(t, http.MethodPost, path+"/"+uuid.NewV7().String()+"/primary", admin, `{"expected_group_claim_version":`+claim+`}`), http.StatusNotFound, "not_found")

	listed := decodeAdmin[api.AdminWorkerPoolsResponse](t, f.request(t, http.MethodGet, path, admin, ""), http.StatusOK)
	if len(listed.WorkerPools) != 1 || listed.WorkerPools[0].ID != pending.ID || !listed.WorkerPools[0].Primary || listed.WorkerPools[0].Status != "active" {
		t.Fatalf("worker pools = %+v", listed.WorkerPools)
	}
	active := listed.WorkerPools[0]
	assertAdminError(t, f.request(t, http.MethodPost, path+"/"+active.ID+"/drain", admin, `{"expected_pool_claim_version":0}`), http.StatusBadRequest, "bad_request")
	assertAdminError(t, f.request(t, http.MethodPost, path+"/"+active.ID+"/drain", admin, `{"expected_pool_claim_version":`+strconv.FormatInt(active.ClaimVersion, 10)+`}`), http.StatusConflict, "conflict")
	assertAdminError(t, f.request(t, http.MethodPost, path+"/"+active.ID+"/disable", admin, `{"expected_pool_claim_version":`+strconv.FormatInt(active.ClaimVersion, 10)+`}`), http.StatusConflict, "conflict")

	for _, malformed := range []string{
		"/admin/api/v1/worker-groups/not-a-group/pools",
		path + "/" + uuid.New().String() + "/drain",
	} {
		assertAdminError(t, f.request(t, http.MethodPost, malformed, admin, `{}`), http.StatusBadRequest, "bad_request")
	}
}
