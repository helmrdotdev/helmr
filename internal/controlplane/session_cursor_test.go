package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"uuid"
)

func TestSessionCursorBindsEnvironmentAndFilters(t *testing.T) {
	env, before := uuid.NewV7(), uuid.NewV7()
	request := httptest.NewRequest("GET", "/v1/sessions?agent=agent&status=open&status=closed", nil)
	_, filter, err := parseSessionListCursor(request, env)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(sessionListCursor{EnvironmentID: env.String(), Filter: filter, Before: before.String()})
	cursor := base64.RawURLEncoding.EncodeToString(data)
	for _, query := range []string{"agent=agent&status=closed&status=open&limit=2", "agent=agent&status=open&status=closed"} {
		id, _, err := parseSessionListCursor(httptest.NewRequest("GET", "/v1/sessions?"+query+"&cursor="+cursor, nil), env)
		if err != nil || id != before {
			t.Fatalf("equivalent filters: %v %v", id, err)
		}
	}
	for _, query := range []string{"agent=other", "agent=agent&status=open", "agent_id=" + uuid.NewV7().String(), "agent=agent&agent_id=" + uuid.NewV7().String(), "unknown=x", "agent="} {
		if _, _, err := parseSessionListCursor(httptest.NewRequest("GET", "/v1/sessions?"+query+"&cursor="+cursor, nil), env); err == nil {
			t.Fatalf("changed filters accepted: %s", query)
		}
	}
	if _, _, err := parseSessionListCursor(httptest.NewRequest("GET", "/v1/sessions?agent=agent&status=open&status=closed&cursor="+cursor, nil), uuid.NewV7()); err == nil {
		t.Fatal("cross-environment cursor accepted")
	}
}
