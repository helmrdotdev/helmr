package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/client"
)

func TestDeploymentHTTPPostgresUploadAuthorityAndManifest(t *testing.T) {
	_, manifest := controlPlaneDeploymentBundle(t)
	body := bytes.Repeat([]byte("a"), 4096)
	manifest.Program.Artifact.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	for i := range manifest.Objects {
		if manifest.Objects[i].MediaType == artifact.ProgramArtifactMediaType {
			manifest.Objects[i].Digest = manifest.Program.Artifact.Digest
			break
		}
	}
	store := newDeploymentHTTPUploadStore(manifest)
	bundleDigest := store.addBundle(t, manifest)
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.CAS, cfg.PlatformStore = store, store
		cfg.BundleAdmission = bundle.Admission{Runtime: deploymentUploadRuntime(manifest)}
	})
	orgID, token := fixture.organizationOwner(t, "upload")
	projectID, environmentID := deploymentHTTPEnvironment(t, fixture, orgID)
	var userID, memberID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id,id FROM org_members WHERE org_id=$1 AND is_owner`, orgID).Scan(&userID, &memberID); err != nil {
		t.Fatal(err)
	}
	fixture.grant(t, orgID, userID, environmentID, "deployments.publish")
	fixture.grant(t, orgID, userID, environmentID, "api_keys.create")
	scope := auth.Scope{OrgID: orgID, ProjectID: projectID.String(), EnvironmentID: environmentID.String()}
	issuer := auth.Principal{OrgID: orgID, UserID: userID, MemberID: memberID, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7(), Grants: []auth.PermissionGrant{{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Permission: auth.PermissionAPIKeysCreate}}}
	issued, err := issueAPIKeyWithRole(t, t.Context(), fixture.pool, issuer, scope, testAPIKeyInput{Name: "upload", RolePermissions: []auth.Permission{auth.PermissionDeploymentsPublish}})
	if err != nil {
		t.Fatal(err)
	}
	viewerID := fixture.user(t, "upload-viewer")
	fixture.member(t, orgID, viewerID, "member")
	viewer := fixture.cliSession(t, viewerID, orgID)
	otherOrg, otherToken := fixture.organizationOwner(t, "other-upload")
	otherProject, otherEnvironment := deploymentHTTPEnvironment(t, fixture, otherOrg)
	var otherUser uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1 AND is_owner`, otherOrg).Scan(&otherUser); err != nil {
		t.Fatal(err)
	}
	fixture.grant(t, otherOrg, otherUser, otherEnvironment, "deployments.publish")
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	base := "/api/projects/deployments/environments/staging"
	objectPath := "/deployment-bundles/" + bundleDigest + "/objects/" + manifest.Program.Artifact.Digest
	request := func(path, credential, media, encoding string, payload []byte) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+path, bytes.NewReader(payload))
		fixture.authenticateRequest(req, credential)
		req.Header.Set("Content-Type", media)
		req.Header.Set("Expect", "100-continue")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	deploymentUploadRawStatus(t, fixture, server.URL, base+objectPath, "invalid", manifest.Program.Artifact.MediaType, 4096, "a", http.StatusUnauthorized)
	deploymentUploadRawStatus(t, fixture, server.URL, base+objectPath, viewer, manifest.Program.Artifact.MediaType, 4096, "a", http.StatusForbidden)
	deploymentUploadRawStatus(t, fixture, server.URL, base+objectPath, token, "application/octet-stream", 4096, "a", http.StatusBadRequest)
	reads := store.reads.Load()
	if got := request(base+objectPath, viewer, manifest.Program.Artifact.MediaType, "", body); got != http.StatusForbidden {
		t.Fatalf("unprivileged upload=%d", got)
	}
	if store.reads.Load() != reads {
		t.Fatal("unprivileged caller read a bundle")
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE projects SET slug='other-deployments' WHERE id=$1`, otherProject); err != nil {
		t.Fatal(err)
	}
	foreignBase := "/api/projects/other-deployments/environments/staging"
	if got := request(foreignBase+objectPath, token, manifest.Program.Artifact.MediaType, "", body); got != 400 && got != 403 && got != 404 {
		t.Fatalf("foreign environment=%d", got)
	}
	badBody := bytes.Clone(body)
	badBody[0] = 'b'
	for name, test := range map[string]struct {
		path, media, encoding string
		body                  []byte
	}{
		"wrong bytes":    {base + objectPath, manifest.Program.Artifact.MediaType, "", badBody},
		"empty body":     {base + objectPath, manifest.Program.Artifact.MediaType, "", nil},
		"wrong size":     {base + objectPath, manifest.Program.Artifact.MediaType, "", body[:len(body)-1]},
		"wrong media":    {base + objectPath, "application/octet-stream", "", body},
		"encoded bytes":  {base + objectPath, manifest.Program.Artifact.MediaType, "gzip", body},
		"extra object":   {base + "/deployment-bundles/" + bundleDigest + "/objects/sha256:" + strings.Repeat("e", 64), manifest.Program.Artifact.MediaType, "", body},
		"missing bundle": {base + "/deployment-bundles/sha256:" + strings.Repeat("e", 64) + "/objects/" + manifest.Program.Artifact.Digest, manifest.Program.Artifact.MediaType, "", body},
	} {
		t.Run(name, func(t *testing.T) {
			before := store.reads.Load()
			if got := request(test.path, token, test.media, test.encoding, test.body); got != http.StatusBadRequest {
				t.Fatalf("status=%d", got)
			}
			if (name == "wrong media" || name == "encoded bytes" || name == "empty body") && store.reads.Load() != before {
				t.Fatal("malformed headers read the bundle root")
			}
		})
	}
	unsupported := manifest
	unsupported.Runtime.Artifact.SizeBytes++
	unsupportedDigest := store.addBundle(t, unsupported)
	if got := request(base+"/deployment-bundles/"+unsupportedDigest+"/objects/"+manifest.Program.Artifact.Digest, token, manifest.Program.Artifact.MediaType, "", body); got != http.StatusBadRequest {
		t.Fatalf("unadmitted bundle=%d", got)
	}
	for _, route := range []struct{ path, token string }{{base + objectPath, token}, {"/v1" + objectPath, issued.Raw}, {foreignBase + objectPath, otherToken}} {
		if got := request(route.path, route.token, manifest.Program.Artifact.MediaType, "", body); got != http.StatusNoContent {
			t.Fatalf("upload %s=%d", route.path, got)
		}
	}
	if !store.has(orgID.String(), manifest.Program.Artifact.Digest) || !store.has(otherOrg.String(), manifest.Program.Artifact.Digest) {
		t.Fatal("quarantine writes were not isolated by organization")
	}
	deploymentUploadRawStatus(t, fixture, server.URL, base+objectPath, token, manifest.Program.Artifact.MediaType, 4096, "a", http.StatusNoContent)
	// A completed quarantine object does not bypass current permission checks.
	if _, err := fixture.pool.Exec(t.Context(), `DELETE FROM role_grants WHERE org_id=$1 AND member_id=$2 AND environment_id=$3`, orgID, memberID, environmentID); err != nil {
		t.Fatal(err)
	}
	if got := request(base+objectPath, token, manifest.Program.Artifact.MediaType, "", body); got != http.StatusForbidden {
		t.Fatalf("revoked upload=%d", got)
	}
	finalize := fixture.request(t, http.MethodPost, base+"/deployment-bundles/finalize", token, fmt.Sprintf(`{"idempotency_key":"revoked","bundle_digest":%q}`, bundleDigest))
	if finalize.Code != http.StatusForbidden {
		t.Fatalf("revoked finalize=%d %s", finalize.Code, finalize.Body.String())
	}
}

func TestDeploymentHTTPPostgresUploadBodyLimits(t *testing.T) {
	for _, session := range []bool{true, false} {
		t.Run(fmt.Sprintf("session=%v", session), func(t *testing.T) {
			_, manifest := controlPlaneDeploymentBundle(t)
			size := int64(129 << 20)
			if !session {
				size = bundle.MaxObjectBytes
				for _, object := range manifest.Objects {
					if object.Digest != manifest.Program.Artifact.Digest {
						size -= object.SizeBytes
					}
				}
			}
			hash := sha256.New()
			_, _ = io.Copy(hash, io.LimitReader(zeroUploadReader{}, size))
			oldDigest := manifest.Program.Artifact.Digest
			manifest.Program.Artifact.Digest = fmt.Sprintf("sha256:%x", hash.Sum(nil))
			manifest.Program.Artifact.SizeBytes = size
			for i := range manifest.Objects {
				if manifest.Objects[i].Digest == oldDigest {
					manifest.Objects[i].Digest = manifest.Program.Artifact.Digest
					manifest.Objects[i].SizeBytes = size
				}
			}
			store := newDeploymentHTTPUploadStore(manifest)
			digest := store.addBundle(t, manifest)
			fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
				cfg.CAS, cfg.PlatformStore = store, store
				cfg.BundleAdmission = bundle.Admission{Runtime: deploymentUploadRuntime(manifest)}
			})
			org, token := fixture.organizationOwner(t, "large-upload")
			project, environment := deploymentHTTPEnvironment(t, fixture, org)
			var user, member uuid.UUID
			if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id,id FROM org_members WHERE org_id=$1 AND is_owner`, org).Scan(&user, &member); err != nil {
				t.Fatal(err)
			}
			fixture.grant(t, org, user, environment, "deployments.publish")
			fixture.grant(t, org, user, environment, "api_keys.create")
			prefix := "/api/projects/deployments/environments/staging"
			if !session {
				scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: environment.String()}
				issuer := auth.Principal{OrgID: org, UserID: user, MemberID: member, Kind: auth.PrincipalKindSession, SessionID: uuid.NewV7(), Grants: []auth.PermissionGrant{{ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Permission: auth.PermissionAPIKeysCreate}}}
				key, err := issueAPIKeyWithRole(t, t.Context(), fixture.pool, issuer, scope, testAPIKeyInput{Name: "large", RolePermissions: []auth.Permission{auth.PermissionDeploymentsPublish}})
				if err != nil {
					t.Fatal(err)
				}
				token = key.Raw
				prefix = "/v1"
			}
			server := httptest.NewServer(fixture.handler)
			defer server.Close()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+prefix+"/deployment-bundles/"+digest+"/objects/"+manifest.Program.Artifact.Digest, io.LimitReader(zeroUploadReader{}, size))
			req.ContentLength = size
			req.Header.Set("Content-Type", manifest.Program.Artifact.MediaType)
			fixture.authenticateRequest(req, token)
			req.Header.Set("Expect", "100-continue")
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			message, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusNoContent || !store.has(org.String(), manifest.Program.Artifact.Digest) {
				t.Fatalf("upload %d bytes=%d %s", size, response.StatusCode, message)
			}
		})
	}
}

func TestDeploymentHTTPPostgresUploadCapacity(t *testing.T) {
	_, manifest := controlPlaneDeploymentBundle(t)
	body := bytes.Repeat([]byte("a"), 4096)
	oldDigest := manifest.Program.Artifact.Digest
	manifest.Program.Artifact.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	for i := range manifest.Objects {
		if manifest.Objects[i].Digest == oldDigest {
			manifest.Objects[i].Digest = manifest.Program.Artifact.Digest
		}
	}
	store := newDeploymentHTTPUploadStore(manifest)
	digest := store.addBundle(t, manifest)
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.CAS, cfg.PlatformStore = store, store
		cfg.BundleAdmission = bundle.Admission{Runtime: deploymentUploadRuntime(manifest)}
	})
	tokens := make([]string, 4)
	releases := make(map[uuid.UUID]chan struct{})
	releaseA, releaseB, releaseD := make(chan struct{}), make(chan struct{}), make(chan struct{})
	for i := range tokens {
		org, _ := fixture.organizationOwner(t, fmt.Sprintf("upload-capacity-%d", i))
		_, environment := deploymentHTTPEnvironment(t, fixture, org)
		var user uuid.UUID
		if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1 AND is_owner`, org).Scan(&user); err != nil {
			t.Fatal(err)
		}
		fixture.grant(t, org, user, environment, "deployments.publish")
		tokens[i] = fixture.cliSession(t, user, org)
		if i == 0 {
			releases[org] = releaseA
		} else if i == 1 {
			releases[org] = releaseB
		} else if i == 3 {
			releases[org] = releaseD
		}
	}
	entered := make(chan struct{}, 4)
	store.beforeStream = func(ctx context.Context) error {
		if release := releases[principalFromContext(ctx).OrgID]; release != nil {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	var onceA, onceB, onceD sync.Once
	defer onceA.Do(func() { close(releaseA) })
	defer onceB.Do(func() { close(releaseB) })
	defer onceD.Do(func() { close(releaseD) })
	clients := make([]*client.Client, 4)
	for i, token := range tokens {
		var err error
		clients[i], err = client.New(server.URL, client.WithBearerToken(token), client.WithSessionScopedRoutes())
		if err != nil {
			t.Fatal(err)
		}
	}
	objectFile := t.TempDir() + "/object"
	if err := os.WriteFile(objectFile, body, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	upload := func(ctx context.Context, index int) error {
		return clients[index].UploadDeploymentBundleObject(ctx, digest, client.EnvironmentScopeOptions{ProjectID: "deployments", EnvironmentID: "staging"}, api.DeploymentBundleUpload{Digest: manifest.Program.Artifact.Digest, SizeBytes: 4096, MediaType: manifest.Program.Artifact.MediaType}, objectFile, nil)
	}
	waitEntered := func(count int) {
		t.Helper()
		for range count {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("uploads did not acquire capacity")
			}
		}
	}
	waitResults := func(results <-chan error, count int) {
		t.Helper()
		for range count {
			select {
			case err := <-results:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("uploads did not finish")
			}
		}
	}
	path := "/api/projects/deployments/environments/staging/deployment-bundles/" + digest + "/objects/" + manifest.Program.Artifact.Digest
	firstResults := make(chan error, 4)
	for range 4 {
		go func() { firstResults <- upload(ctx, 0) }()
	}
	waitEntered(2)
	// One slow organization cannot take the remaining capacity, even though its
	// other CLI attempts keep waiting. Reject a partial body without draining it.
	deploymentUploadRawStatus(t, fixture, server.URL, path, tokens[0], manifest.Program.Artifact.MediaType, 4096, "a", http.StatusTooManyRequests)
	otherResults := make(chan error, 2)
	for range 2 {
		go func() { otherResults <- upload(ctx, 1) }()
	}
	waitEntered(2)
	// Four globally held streams still leave ordinary APIs usable, and the fifth
	// upload is refused regardless of its organization.
	deploymentUploadRawStatus(t, fixture, server.URL, path, tokens[2], manifest.Program.Artifact.MediaType, 4096, "a", http.StatusTooManyRequests)
	ordinary, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/organization", nil)
	fixture.authenticateRequest(ordinary, tokens[0])
	response, err := server.Client().Do(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ordinary API while uploading=%s", response.Status)
	}
	onceB.Do(func() { close(releaseB) })
	waitResults(otherResults, 2)
	if err := upload(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// Organization B and C have completed while A is still held.
	onceA.Do(func() { close(releaseA) })
	waitResults(firstResults, 4)
	// Cancellation returns the organization's permits only after request work
	// joins, so the next ordinary client attempt can progress.
	cancelled, stop := context.WithCancel(ctx)
	defer stop()
	cancelledResults := make(chan error, 2)
	for range 2 {
		go func() { cancelledResults <- upload(cancelled, 3) }()
	}
	waitEntered(2)
	stop()
	for range 2 {
		select {
		case err := <-cancelledResults:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled upload=%v", err)
			}
		case <-ctx.Done():
			t.Fatal("cancelled uploads did not return")
		}
	}
	onceD.Do(func() { close(releaseD) })
	if err := upload(ctx, 3); err != nil {
		t.Fatalf("capacity after cancellation=%v", err)
	}
}

func TestDeploymentHTTPPostgresUploadReleasesOrdinaryAdmission(t *testing.T) {
	_, manifest := controlPlaneDeploymentBundle(t)
	body := bytes.Repeat([]byte("a"), 4096)
	oldDigest := manifest.Program.Artifact.Digest
	manifest.Program.Artifact.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	for i := range manifest.Objects {
		if manifest.Objects[i].Digest == oldDigest {
			manifest.Objects[i].Digest = manifest.Program.Artifact.Digest
		}
	}
	store := newDeploymentHTTPUploadStore(manifest)
	digest := store.addBundle(t, manifest)
	const capacity = 18 // Greater than both ordinary process and principal budgets.
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.CAS, cfg.PlatformStore = store, store
		cfg.BundleAdmission = bundle.Admission{Runtime: deploymentUploadRuntime(manifest)}
		cfg.DeploymentUploadLimits.ProcessConcurrency = capacity
		cfg.DeploymentUploadLimits.OrganizationConcurrency = capacity
	})
	orgID, token := fixture.organizationOwner(t, "upload-handoff")
	_, environmentID := deploymentHTTPEnvironment(t, fixture, orgID)
	var userID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1 AND is_owner`, orgID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	fixture.grant(t, orgID, userID, environmentID, "deployments.publish")
	entered := make(chan struct{}, capacity)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var blocked atomic.Bool
	blocked.Store(true)
	store.beforeStream = func(ctx context.Context) error {
		if blocked.Load() {
			entered <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
		}
		return nil
	}
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	token = fixture.cliSession(t, userID, orgID)
	c, err := client.New(server.URL, client.WithBearerToken(token), client.WithSessionScopedRoutes())
	if err != nil {
		t.Fatal(err)
	}
	objectFile := t.TempDir() + "/object"
	if err := os.WriteFile(objectFile, body, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	transfers, stopTransfers := context.WithCancel(ctx)
	defer stopTransfers()
	upload := func(ctx context.Context) error {
		return c.UploadDeploymentBundleObject(ctx, digest, client.EnvironmentScopeOptions{ProjectID: "deployments", EnvironmentID: "staging"}, api.DeploymentBundleUpload{Digest: manifest.Program.Artifact.Digest, SizeBytes: 4096, MediaType: manifest.Program.Artifact.MediaType}, objectFile, nil)
	}
	results := make(chan error, capacity)
	for range capacity {
		go func() { results <- upload(transfers) }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("configured uploads still occupy ordinary admission")
		}
	}
	path := "/api/projects/deployments/environments/staging/deployment-bundles/" + digest + "/objects/" + manifest.Program.Artifact.Digest
	deploymentUploadRawStatus(t, fixture, server.URL, path, token, manifest.Program.Artifact.MediaType, 4096, "a", http.StatusTooManyRequests)
	ordinary, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/organization", nil)
	fixture.authenticateRequest(ordinary, token)
	response, err := server.Client().Do(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ordinary API with %d uploads: %s", capacity, response.Status)
	}
	stopTransfers()
	for range capacity {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled transfer: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("transfers did not join")
		}
	}
	blocked.Store(false)
	releaseOnce.Do(func() { close(release) })
	if err := upload(ctx); err != nil {
		t.Fatalf("capacity not returned: %v", err)
	}
}

func deploymentUploadRawStatus(t *testing.T, fixture httpPostgresFixture, origin, path, token, media string, length int64, prefix string, status int) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(origin, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	request := httptest.NewRequest("PUT", path, nil)
	fixture.authenticateRequest(request, token)
	var headers strings.Builder
	if err := request.Header.Write(&headers); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: test\r\n%sContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", path, headers.String(), media, length, prefix)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status || !response.Close {
		t.Fatalf("response=%s close=%v", response.Status, response.Close)
	}
	if status == http.StatusTooManyRequests && response.Header.Get("Retry-After") != "1" {
		t.Fatal("missing capacity retry delay")
	}
}

func TestDeploymentHTTPPostgresUploadStorageFailure(t *testing.T) {
	_, manifest := controlPlaneDeploymentBundle(t)
	base := newDeploymentHTTPUploadStore(manifest)
	digest := base.addBundle(t, manifest)
	store := &deploymentUploadFaultStore{UploadStore: base, runtimeDigest: manifest.Runtime.Artifact.Digest}
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.CAS, cfg.PlatformStore = store, store
		cfg.BundleAdmission = bundle.Admission{Runtime: deploymentUploadRuntime(manifest)}
	})
	org, token := fixture.organizationOwner(t, "upload-storage-failure")
	_, environment := deploymentHTTPEnvironment(t, fixture, org)
	var user uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1 AND is_owner`, org).Scan(&user); err != nil {
		t.Fatal(err)
	}
	fixture.grant(t, org, user, environment, "deployments.publish")
	path := "/api/projects/deployments/environments/staging/deployment-bundles/" + digest + "/objects/" + manifest.Program.Artifact.Digest
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	for _, phase := range []string{"stat", "get", "read", "close", "runtime"} {
		t.Run(phase, func(t *testing.T) {
			store.phase = phase
			for range 5 { // Failed requests must return both capacity permits.
				request, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+path, strings.NewReader(strings.Repeat("a", 4096)))
				fixture.authenticateRequest(request, token)
				request.Header.Set("Content-Type", manifest.Program.Artifact.MediaType)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				message, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusInternalServerError {
					t.Fatalf("failure=%d %s", response.StatusCode, message)
				}
				if strings.Contains(string(message), "private-storage-detail") {
					t.Fatal("storage error leaked")
				}
			}
		})
	}
}

func TestDeploymentHTTPPostgresUploadPreflightDeadline(t *testing.T) {
	_, manifest := controlPlaneDeploymentBundle(t)
	base := newDeploymentHTTPUploadStore(manifest)
	digest := base.addBundle(t, manifest)
	store := &deploymentUploadFaultStore{UploadStore: base, phase: "blocked", deadline: make(chan time.Time, 1)}
	fixture := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.CAS, cfg.PlatformStore = store, store
		cfg.BundleAdmission = bundle.Admission{Runtime: deploymentUploadRuntime(manifest)}
		cfg.DeploymentUploadLimits.IdleTimeout = time.Hour
	})
	orgID, token := fixture.organizationOwner(t, "upload-preflight")
	_, environmentID := deploymentHTTPEnvironment(t, fixture, orgID)
	var userID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1 AND is_owner`, orgID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	fixture.grant(t, orgID, userID, environmentID, "deployments.publish")
	path := "/api/projects/deployments/environments/staging/deployment-bundles/" + digest + "/objects/" + manifest.Program.Artifact.Digest
	for range 5 { // More cancellations than the configured organization capacity.
		ctx, cancel := context.WithCancel(t.Context())
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(strings.Repeat("a", 4096))).WithContext(ctx)
		fixture.authenticateRequest(request, token)
		request.Header.Set("Content-Type", manifest.Program.Artifact.MediaType)
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); fixture.handler.ServeHTTP(response, request) }()
		select {
		case deadline := <-store.deadline:
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > deploymentUploadPreflightTimeout {
				cancel()
				t.Fatalf("preflight deadline=%s", remaining)
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("preflight did not reach bounded storage lookup")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("preflight cancellation did not join")
		}
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("preflight cancellation=%d", response.Code)
		}
	}
}

type deploymentUploadFaultStore struct {
	cas.UploadStore
	phase, runtimeDigest string
	deadline             chan time.Time
}

func (s *deploymentUploadFaultStore) Stat(ctx context.Context, digest string) (cas.Object, error) {
	if s.phase == "blocked" {
		deadline, _ := ctx.Deadline()
		s.deadline <- deadline
		<-ctx.Done()
		return cas.Object{}, ctx.Err()
	}
	if s.phase == "stat" || (s.phase == "runtime" && digest == s.runtimeDigest) {
		return cas.Object{}, errors.New("private-storage-detail")
	}
	return s.UploadStore.Stat(ctx, digest)
}
func (s *deploymentUploadFaultStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	if s.phase == "get" {
		return nil, errors.New("private-storage-detail")
	}
	r, err := s.UploadStore.Get(ctx, digest)
	if err != nil {
		return nil, err
	}
	return deploymentUploadFaultReader{ReadCloser: r, phase: s.phase}, nil
}

type deploymentUploadFaultReader struct {
	io.ReadCloser
	phase string
}

func (r deploymentUploadFaultReader) Read(p []byte) (int, error) {
	if r.phase == "read" {
		return 0, errors.New("private-storage-detail")
	}
	return r.ReadCloser.Read(p)
}
func (r deploymentUploadFaultReader) Close() error {
	err := r.ReadCloser.Close()
	if r.phase == "close" {
		return errors.New("private-storage-detail")
	}
	return err
}

func deploymentUploadRuntime(manifest bundle.Manifest) artifact.RuntimeDescriptor {
	return artifact.RuntimeDescriptor{FormatVersion: artifact.RuntimeDescriptorFormatVersion, Architecture: manifest.Platform.Architecture, RuntimeContract: manifest.Runtime.Contract, Digest: manifest.Runtime.Artifact.Digest, SizeBytes: manifest.Runtime.Artifact.SizeBytes, MediaType: manifest.Runtime.Artifact.MediaType}
}

type deploymentHTTPUploadStore struct {
	cas.Store
	mu           sync.Mutex
	roots        map[string][]byte
	quarantine   map[string]cas.Descriptor
	runtime      cas.Object
	reads        atomic.Int32
	beforeStream func(context.Context) error
}

func newDeploymentHTTPUploadStore(manifest bundle.Manifest) *deploymentHTTPUploadStore {
	return &deploymentHTTPUploadStore{roots: map[string][]byte{}, quarantine: map[string]cas.Descriptor{}, runtime: bundleCASObject(manifest.Runtime.Artifact)}
}
func (s *deploymentHTTPUploadStore) addBundle(t *testing.T, manifest bundle.Manifest) string {
	t.Helper()
	sort.Slice(manifest.Objects, func(i, j int) bool { return manifest.Objects[i].Digest < manifest.Objects[j].Digest })
	raw, err := bundle.Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := bundle.Digest(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.roots[digest] = raw
	s.mu.Unlock()
	return digest
}
func (s *deploymentHTTPUploadStore) Stat(_ context.Context, digest string) (cas.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if digest == s.runtime.Digest {
		return s.runtime, nil
	}
	raw, ok := s.roots[digest]
	if !ok {
		return cas.Object{}, os.ErrNotExist
	}
	return cas.Object{Digest: digest, SizeBytes: int64(len(raw)), MediaType: bundle.MediaType}, nil
}
func (s *deploymentHTTPUploadStore) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.roots[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}
func (s *deploymentHTTPUploadStore) PutQuarantine(ctx context.Context, owner string, expected cas.Descriptor, body io.Reader) error {
	return s.StreamQuarantine(ctx, owner, expected, body)
}
func (s *deploymentHTTPUploadStore) StreamQuarantine(ctx context.Context, owner string, expected cas.Descriptor, body io.Reader) error {
	if s.beforeStream != nil {
		if err := s.beforeStream(ctx); err != nil {
			return err
		}
	}
	hash := sha256.New()
	size, err := io.Copy(hash, body)
	if err != nil {
		return err
	}
	if size != expected.SizeBytes || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != expected.Digest {
		return cas.ErrDigestMismatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantine[owner+"/"+expected.Digest] = expected
	return nil
}
func (s *deploymentHTTPUploadStore) HasExactQuarantine(_ context.Context, owner string, expected cas.Descriptor) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actual, ok := s.quarantine[owner+"/"+expected.Digest]
	if ok && actual != expected {
		return false, cas.ErrDigestMismatch
	}
	return ok, nil
}
func (s *deploymentHTTPUploadStore) PromoteQuarantine(context.Context, string, cas.Descriptor) (cas.Object, error) {
	return cas.Object{}, fmt.Errorf("test does not publish objects")
}
func (s *deploymentHTTPUploadStore) has(owner, digest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.quarantine[owner+"/"+digest]
	return ok
}

var _ cas.UploadStore = (*deploymentHTTPUploadStore)(nil)
