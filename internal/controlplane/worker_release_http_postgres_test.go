package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/release"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func releaseObject(data []byte) workergroup.ReleaseObject {
	sum := sha256.Sum256(data)
	return workergroup.ReleaseObject{Digest: "sha256:" + hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
}

// A host learns the Control Plane's own release and reads only that
// release's objects. A Control Plane built from source names none.
func TestWorkerReleaseNamesTheControlPlaneRelease(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	admin := f.adminSession(t)
	group := f.createSupplyGroup(t, admin, "us-east")
	public, private := newMachineKey(t)
	var enrolled workerapi.EnrollmentResponse
	if err := json.Unmarshal(f.machineEnrollment(t, group.EnrollmentToken, "machine", public).Body.Bytes(), &enrolled); err != nil {
		t.Fatal(err)
	}
	code, credential := f.machineCredential(t, enrolled.WorkerHostID, uuid.NewV7().String(), private, nil)
	if code != http.StatusOK {
		t.Fatalf("credential = %d", code)
	}
	get := func() *workerapi.Release {
		t.Helper()
		return decodeAdmin[workerapi.ReleaseResponse](t, f.request(t, http.MethodGet, "/worker/v1/release", credential.Credential, ""), http.StatusOK).Release
	}
	if got := get(); got != nil {
		t.Fatalf("a source build named release %+v", got)
	}

	built := version.Version
	version.Version = "v0.2.9"
	t.Cleanup(func() { version.Version = built })
	assertAdminError(t, f.request(t, http.MethodGet, "/worker/v1/release", credential.Credential, ""), http.StatusServiceUnavailable, "release_not_published")

	index, err := os.ReadFile("../release/testdata/v0.2.9-release-index.json")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := os.ReadFile("../release/testdata/v0.2.9-release-index.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := release.Verify(index, signature, "v0.2.9")
	if err != nil {
		t.Fatal(err)
	}
	if err := workergroup.RecordRelease(t.Context(), f.pool, verified, releaseObject(index), releaseObject(signature)); err != nil {
		t.Fatal(err)
	}
	if err := workergroup.RecordRelease(t.Context(), f.pool, verified, releaseObject(index), releaseObject(signature)); err != nil {
		t.Fatalf("recording the same release again: %v", err)
	}
	got := get()
	if got == nil || got.Version != "v0.2.9" || got.Index.Digest != releaseObject(index).Digest || got.Signature.Digest != releaseObject(signature).Digest {
		t.Fatalf("release = %+v", got)
	}

	// A digest that is no release object is refused without reaching storage.
	body, _ := json.Marshal(workerapi.ReleaseReadRequest{Digest: "sha256:" + hex.EncodeToString(make([]byte, 32))})
	if response := f.request(t, http.MethodPost, "/worker/v1/release/reads", credential.Credential, string(body)); response.Code == http.StatusOK {
		t.Fatalf("read of a foreign digest granted: %s", response.Body.String())
	}
}
