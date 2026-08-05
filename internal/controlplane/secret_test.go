package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecretListCursorRoundTripAndScope(t *testing.T) {
	raw, err := encodeSecretListCursor(secretListCursor{
		ProjectID:     "project-1",
		EnvironmentID: "environment-1",
		Name:          "API_TOKEN",
		ID:            "019c8f1e-9b42-7b2c-8a4c-4b3a7f9f6d21",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/secrets?limit=25&cursor="+raw,
		nil,
	)
	limit, cursor, name, err := parseSecretListQuery(
		request,
		"project-1",
		"environment-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil || name != "" ||
		cursor.Name != "API_TOKEN" ||
		cursor.ID != "019c8f1e-9b42-7b2c-8a4c-4b3a7f9f6d21" {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, _, err := parseSecretListQuery(
		request,
		"project-1",
		"environment-2",
	); err == nil {
		t.Fatal("cross-environment cursor succeeded")
	}
}

func TestSecretListQueryRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, target := range []string{
		"/v1/secrets?name=API_TOKEN&limit=1",
		"/v1/secrets?name=bad%20name",
		"/v1/secrets?limit=0",
		"/v1/secrets?limit=101",
		"/v1/secrets?cursor=invalid",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, _, _, err := parseSecretListQuery(
			request,
			"project-1",
			"environment-1",
		); err == nil {
			t.Fatalf("parseSecretListQuery(%q) succeeded", target)
		}
	}
}

func TestSecretListQueryAcceptsExactName(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/secrets?name=API_TOKEN", nil)
	limit, cursor, name, err := parseSecretListQuery(request, "project-1", "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 0 || cursor != nil || name != "API_TOKEN" {
		t.Fatalf("limit=%d cursor=%+v name=%q", limit, cursor, name)
	}
}
