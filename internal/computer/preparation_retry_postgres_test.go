package computer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestInitialPublicationClientReplaysCommittedLostResponse(t *testing.T) {
	f, input := newVersionFixture(t)
	var mu sync.Mutex
	var bodies [][]byte
	var committed Publication
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/credential" {
			json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{Credential: "fixture", ExpiresInSeconds: 3600})
			return
		}
		if r.URL.Path != "/worker/v1/run/computer-instances/initialization/version" {
			w.WriteHeader(404)
			return
		}
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		var request workerapi.InitialComputerVersionRequest
		if e = json.Unmarshal(raw, &request); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		publication, e := f.publisher.PublishInitialVersion(r.Context(), f.principal, f.ref, InitialVersion{Root: request.Root, Config: request.Config})
		if e != nil {
			t.Error(e)
			w.WriteHeader(409)
			return
		}
		mu.Lock()
		bodies = append(bodies, raw)
		n := len(bodies)
		if n == 1 {
			committed = publication
		}
		same := publication == committed
		mu.Unlock()
		if !same {
			t.Error("replay changed committed publication")
		}
		if n == 1 {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(workerapi.InitialComputerVersionResponse{ComputerID: publication.ComputerID.String(), VersionID: publication.VersionID.String()})
	}))
	defer server.Close()
	c, e := workerclient.New(server.URL, workerclient.WithHTTPClient(server.Client()), workerclient.WithAuth(f.principal.HostID.String(), "fixture"), workerclient.WithService(uuid.NewV7().String()))
	if e != nil {
		t.Fatal(e)
	}
	result, e := c.PublishInitialComputerVersion(t.Context(), workerapi.InitialComputerVersionRequest{ComputerInstanceID: pgvalue.MustUUIDValue(f.instance).String(), DesiredVersion: f.ref.DesiredVersion, Root: input.Root, Config: input.Config})
	if e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || result.VersionID != committed.VersionID.String() || result.ComputerID != committed.ComputerID.String() {
		t.Fatalf("replay result=%+v bodies=%q", result, bodies)
	}
	var versions, roots int
	if e = f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_disk_versions WHERE computer_id=$1 AND publication_request_fingerprint IS NOT NULL),(SELECT count(*) FROM computer_disk_version_roots WHERE version_id=$2)`, pgvalue.UUID(committed.ComputerID), pgvalue.UUID(committed.VersionID)).Scan(&versions, &roots); e != nil || versions != 1 || roots != 1 {
		t.Fatalf("publication duplicated: versions=%d roots=%d err=%v", versions, roots, e)
	}
}
