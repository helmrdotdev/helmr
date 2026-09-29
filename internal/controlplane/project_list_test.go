package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"
)

func TestProjectListCursorValidatesRequiredFields(t *testing.T) {
	valid := projectListCursor{
		OrgID: uuid.NewV7().String(), IsDefault: true,
		Slug: "project", ID: uuid.NewV7().String(),
	}
	for name, cursor := range map[string]projectListCursor{
		"valid":          valid,
		"invalid org id": {OrgID: "invalid", Slug: valid.Slug, ID: valid.ID},
		"empty slug":     {OrgID: valid.OrgID, ID: valid.ID},
		"invalid id":     {OrgID: valid.OrgID, Slug: valid.Slug, ID: "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := encodeProjectListCursor(cursor)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeProjectListCursor(raw)
			if (err == nil) != (name == "valid") {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestProjectListQueryValidation(t *testing.T) {
	orgID := uuid.NewV7()
	otherOrgCursor, err := encodeProjectListCursor(projectListCursor{
		OrgID: uuid.NewV7().String(), Slug: "project", ID: uuid.NewV7().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rawQuery := range []string{
		"limit=0", "limit=101", "limit=abc", "limit=1&limit=2",
		"cursor=", "cursor=not-base64", "cursor=" + otherOrgCursor, "unsupported=true",
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/projects?"+rawQuery, nil)
		if _, _, err := parseProjectListQuery(request, orgID); err == nil {
			t.Fatalf("query %q succeeded", rawQuery)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	limit, cursor, err := parseProjectListQuery(request, orgID)
	if err != nil || limit != 50 || cursor != nil {
		t.Fatalf("default query = %d/%+v/%v", limit, cursor, err)
	}
}
