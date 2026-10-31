package controlplane

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"
)

func TestComputerListQueryFiltersBySecret(t *testing.T) {
	parse := func(query string) (int32, *computerListCursor, *string, string, error) {
		return parseComputerListQuery(httptest.NewRequest(http.MethodGet, "/v1/computers?"+query, nil), "project", "environment")
	}
	limit, cursor, key, secretName, err := parse("secret=GITHUB_TOKEN&limit=10")
	if err != nil || limit != 10 || cursor != nil || key != nil || secretName != "GITHUB_TOKEN" {
		t.Fatalf("secret filter = %d %v %v %q %v", limit, cursor, key, secretName, err)
	}
	for _, query := range []string{"secret=GITHUB_TOKEN&key=repository", "secret=bad%20name", "secret=GITHUB_TOKEN&secret=OTHER"} {
		if _, _, _, _, err := parse(query); err == nil {
			t.Fatalf("%q accepted", query)
		}
	}
	raw, err := encodeComputerListCursor(computerListCursor{ProjectID: "project", EnvironmentID: "environment", CreatedAt: time.Now().UTC(), ID: uuid.NewV7().String(), Secret: "GITHUB_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if _, cursor, _, _, err = parse("secret=GITHUB_TOKEN&cursor=" + url.QueryEscape(raw)); err != nil || cursor == nil || cursor.Secret != "GITHUB_TOKEN" {
		t.Fatalf("matching cursor = %+v %v", cursor, err)
	}
	for _, query := range []string{"secret=OTHER&cursor=" + url.QueryEscape(raw), "cursor=" + url.QueryEscape(raw)} {
		if _, _, _, _, err := parse(query); err == nil {
			t.Fatalf("cursor reused across Secret filters: %q", query)
		}
	}
}
