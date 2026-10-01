package controlplane

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestHashCapacityTokenRequiresCanonicalHighEntropyValue(t *testing.T) {
	valid := capacityTestToken()
	if hash, err := hashCapacityToken(valid); err != nil || len(hash) == 0 {
		t.Fatalf("hash valid capacity token: hash=%x err=%v", hash, err)
	}
	for _, invalid := range []string{"short", valid + "=", " " + valid, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31))} {
		if _, err := hashCapacityToken(invalid); err == nil {
			t.Fatalf("hashCapacityToken(%q) succeeded", invalid)
		}
	}
	if hash, err := hashCapacityToken(""); err != nil || hash != nil {
		t.Fatalf("empty optional token = %x, %v", hash, err)
	}
}

func TestCapacityWorkerHostFilterIsBounded(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/?worker_group_id="+controlplaneTestWorkerGroup+"&resource_id=host-1&resource_id=host-2&status=active&status=draining&has_unreclaimed_runtime=true&limit=50", nil)
	filter, err := capacityWorkerHostFilter(request)
	if err != nil {
		t.Fatal(err)
	}
	want := workergroup.HostFilter{
		GroupID:                controlplaneTestWorkerGroupID,
		ResourceIDs:            []string{"host-1", "host-2"},
		Statuses:               []workergroup.WorkerHostStatus{workergroup.WorkerHostStatusActive, workergroup.WorkerHostStatusDraining},
		HasUnreclaimedInstance: true,
		Limit:                  50,
	}
	if !reflect.DeepEqual(filter, want) {
		t.Fatalf("filter = %+v, want %+v", filter, want)
	}
	if filter, err := capacityWorkerHostFilter(httptest.NewRequest(http.MethodGet, "/", nil)); err != nil || filter.Limit != defaultCapacityInstanceLimit {
		t.Fatalf("default filter = %+v, %v", filter, err)
	}
	for _, raw := range []string{"/?unsupported=active", "/?worker_group_id=", "/?worker_group_id=%20", "/?worker_group_id=%20" + controlplaneTestWorkerGroup + "%20", "/?worker_group_id=run-workers", "/?status=unknown", "/?resource_id=", "/?resource_id=host-1&resource_id=host-1", "/?has_unreclaimed_runtime=false", "/?has_unreclaimed_runtime=true&has_unreclaimed_runtime=true", "/?limit=0", "/?limit=501"} {
		if _, err := capacityWorkerHostFilter(httptest.NewRequest(http.MethodGet, raw, nil)); err == nil {
			t.Fatalf("filter for %q succeeded", raw)
		}
	}
}

func capacityTestToken() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, capacityTokenDecodedByteCount))
}

func capacityJSON(t *testing.T, response *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response JSON: %v: %s", err, response.Body.String())
	}
	return result
}

func capacityJSONObject(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("JSON value = %#v, want object", value)
	}
	return result
}

func assertCapacityJSONKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}
