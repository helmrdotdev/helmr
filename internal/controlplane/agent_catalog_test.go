package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func assertRetainedAgentCatalog(t *testing.T, h httpPostgresFixture, org, project, environment, deployment uuid.UUID, owner string) {
	t.Helper()
	base := "/api/projects/project/environments/test/agents"
	viewer := h.user(t, "agent-viewer")
	h.member(t, org, viewer, "member")
	h.grant(t, org, viewer, environment, "runs.view")
	token := h.session(t, viewer, org)
	get := func(path, bearer string, status int) []byte {
		t.Helper()
		response := h.request(t, http.MethodGet, path, bearer, "")
		if response.Code != status {
			t.Fatalf("%s status=%d want=%d body=%s", path, response.Code, status, response.Body.String())
		}
		return response.Body.Bytes()
	}
	status := func(current *string, defined bool) {
		t.Helper()
		var item retainedAgent
		raw := get(base+"/agent/status", token, http.StatusOK)
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		if item.Agent != "agent" || item.DefinedInCurrent != defined || (current == nil) != (item.CurrentDeploymentID == nil) || current != nil && *current != *item.CurrentDeploymentID || strings.Contains(string(raw), "agent_id") {
			t.Fatalf("Agent status=%s", raw)
		}
	}
	id := deployment.String()
	status(&id, true)
	get(base+"/missing/status", token, http.StatusNotFound)
	get(base+"/agent/status?deployment_id="+id, token, http.StatusBadRequest)
	get(base+"/agent/slack", token, http.StatusForbidden)
	// A removed definition must not remove operational identity or its history.
	emptyDeployment := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), h.pool, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,$3); UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, environment, emptyDeployment, "sha256:"+strings.Repeat("e", 64))
	empty := emptyDeployment.String()
	status(&empty, false)
	get(base+"/agent", token, http.StatusNotFound)
	get(base+"/agent?deployment_id="+id, token, http.StatusOK)
	dbtest.MustExec(t, t.Context(), h.pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, environment)
	status(nil, false)
	// Stable name lookup must continue without a current Deployment.
	raw := get(strings.TrimSuffix(base, "/agents")+"/sessions?agent=agent", token, http.StatusOK)
	if strings.Contains(string(raw), "agent_id") || !strings.Contains(string(raw), `"agent":"agent"`) {
		t.Fatalf("Session projection: %s", raw)
	}
	get(strings.TrimSuffix(base, "/agents")+"/sessions?agent_id="+uuid.NewV7().String(), token, http.StatusBadRequest)
	get(strings.TrimSuffix(base, "/agents")+"/triggers?agent_id="+uuid.NewV7().String(), token, http.StatusBadRequest)
	var page struct {
		Agents     []retainedAgent `json:"agents"`
		NextCursor string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(get(base+"?view=all&limit=1", token, http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Agents) != 1 || page.Agents[0].Agent != "agent" || page.Agents[0].DefinedInCurrent || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	var next struct {
		Agents     []retainedAgent `json:"agents"`
		NextCursor string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(get(base+"?view=all&cursor="+url.QueryEscape(page.NextCursor), token, http.StatusOK), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Agents) != 1 || next.Agents[0].Agent != "zebra" || next.NextCursor != "" {
		t.Fatalf("next=%+v", next)
	}
	rawCursor, _ := json.Marshal(retainedAgentCursor{Project: project.String(), Environment: uuid.NewV7().String(), After: "agent"})
	get(base+"?view=all&cursor="+base64.RawURLEncoding.EncodeToString(rawCursor), token, http.StatusBadRequest)
	for _, query := range []string{"view=bad", "view=all&deployment_id=" + id, "view=all&view=all", "view=all&limit=0", "view=all&cursor=bad"} {
		get(base+"?"+query, token, http.StatusBadRequest)
	}
	_, other := h.organizationOwner(t, "agent-outsider")
	get(base+"?view=all", other, http.StatusBadRequest)
	get(base+"/agent/status", other, http.StatusBadRequest)
	// Authored UUID-shaped names are looked up literally, never as internal IDs.
	key := uuid.NewV7().String()
	dbtest.MustExec(t, t.Context(), h.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,$3)`, environment, uuid.NewV7(), key)
	get(base+"/"+key+"/status", token, http.StatusOK)
	dbtest.MustExec(t, t.Context(), h.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, environment, deployment)
	status(&id, true)
	get(base+"/agent/status", owner, http.StatusOK)
}
