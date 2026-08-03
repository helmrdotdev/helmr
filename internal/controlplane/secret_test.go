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
		"/api/secrets?limit=25&cursor="+raw,
		nil,
	)
	limit, cursor, err := parseSecretListQuery(
		request,
		"project-1",
		"environment-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil ||
		cursor.Name != "API_TOKEN" ||
		cursor.ID != "019c8f1e-9b42-7b2c-8a4c-4b3a7f9f6d21" {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, err := parseSecretListQuery(
		request,
		"project-1",
		"environment-2",
	); err == nil {
		t.Fatal("cross-environment cursor succeeded")
	}
}

func TestSecretListQueryRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, target := range []string{
		"/api/secrets?name=legacy",
		"/api/secrets?limit=0",
		"/api/secrets?limit=101",
		"/api/secrets?cursor=invalid",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, _, err := parseSecretListQuery(
			request,
			"project-1",
			"environment-1",
		); err == nil {
			t.Fatalf("parseSecretListQuery(%q) succeeded", target)
		}
	}
}
