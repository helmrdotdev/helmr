package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestTriggerListCursorRoundTripAndScope(t *testing.T) {
	raw, err := encodeTriggerListCursor(triggerListCursor{View: "current",
		ProjectID:     "project-1",
		EnvironmentID: "environment-1",
		AgentID:       "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
		TriggerID:     "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/triggers?limit=25&cursor="+raw,
		nil,
	)
	limit, cursor, err := parseTriggerListQuery(
		request,
		"project-1",
		"environment-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil ||
		cursor.AgentID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" ||
		cursor.TriggerID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36" {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, err := parseTriggerListQuery(
		request,
		"project-1",
		"environment-2",
	); err == nil {
		t.Fatal("cross-environment cursor succeeded")
	}
}

func TestTriggerListQueryRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, target := range []string{
		"/v1/triggers?task=unexpected",
		"/v1/triggers?agent_id=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
		"/v1/triggers?limit=0",
		"/v1/triggers?limit=101",
		"/v1/triggers?cursor=invalid",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, _, err := parseTriggerListQuery(
			request,
			"project-1",
			"environment-1",
		); err == nil {
			t.Fatalf("parseTriggerListQuery(%q) succeeded", target)
		}
	}
}

func TestTriggerListQueryAcceptsAgentFilter(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/triggers?agent=worker", nil)
	limit, cursor, err := parseTriggerListQuery(request, "project-1", "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 50 || cursor != nil {
		t.Fatalf("limit=%d cursor=%+v ", limit, cursor)
	}
}

func TestTriggerResponseProjectsActivationInterval(t *testing.T) {
	now := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	row := db.ListTriggersRow{Agent: "worker", ID: pgvalue.UUID(uuid.NewV7()), AgentID: pgvalue.UUID(uuid.NewV7()), DeploymentID: pgvalue.UUID(uuid.NewV7()), TriggerKey: "daily", Cron: "0 9 * * *", Timezone: "UTC", Input: []byte(`[{"type":"text","text":"{\"report\":true}"}]`), ActiveFrom: pgvalue.Timestamptz(now), NextFireAt: pgvalue.Timestamptz(now.Add(time.Hour))}
	response, err := triggerResponse(row)
	if err != nil || response.Agent != "worker" || response.TriggerKey != "daily" || response.NextFireAt == nil || response.ActiveUntil != nil || string(response.Input) != string(row.Input) {
		t.Fatalf("response=%+v error=%v", response, err)
	}
	row.ActiveUntil = row.NextFireAt
	response, err = triggerResponse(row)
	if err != nil || response.NextFireAt != nil || response.ActiveUntil == nil {
		t.Fatalf("closed interval=%+v error=%v", response, err)
	}
}

func TestTriggerCursorRejectsFilterChange(t *testing.T) {
	id := uuid.NewV7().String()
	raw, err := encodeTriggerListCursor(triggerListCursor{View: "current", ProjectID: "p", EnvironmentID: "e", AgentID: id, TriggerID: id, FilterAgent: id})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/triggers?cursor="+raw, nil)
	if _, _, err := parseTriggerListQuery(request, "p", "e"); err == nil {
		t.Fatal("changed filter accepted")
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/triggers?agent="+id+"&cursor="+raw+"&limit=2", nil)
	if limit, _, err := parseTriggerListQuery(request, "p", "e"); err != nil || limit != 2 {
		t.Fatalf("error=%v", err)
	}
}

func TestTriggerHTTPListsMultiplePinnedActivationsWithinScope(t *testing.T) {
	f := sessiontest.New(t)
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	oldID, currentID, otherID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	for i, id := range []uuid.UUID{oldID, currentID, otherID} {
		trigger := "daily"
		if i == 2 {
			trigger = "weekly"
		}
		var until *time.Time
		if i == 0 {
			v := at.Add(time.Hour)
			until = &v
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input) VALUES($1,$3,$4,$5,'0 * * * *','UTC','{"report":true}') ON CONFLICT DO NOTHING;
 INSERT INTO trigger_activations(environment_id,id,agent_id,deployment_id,trigger_key,active_from,active_until,next_fire_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, pgx.QueryExecModeSimpleProtocol, f.Environment, id, f.Agent, f.Deployment, trigger, at, until, at.Add(time.Hour))
	}
	q := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB = q
	cfg.TX = f.Pool
	cfg.Auth = identity.NewAPIKeyAuthenticator(f.Pool)
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	httpFixture := httpPostgresFixture{pool: f.Pool, queries: q, handler: handler, keys: keys}
	token := httpFixture.session(t, f.User, org)
	base := "/api/projects/project/environments/test/triggers"
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=NULL WHERE id=$1`, f.Environment)

	for _, query := range []struct {
		suffix string
		count  int
	}{{"", 2}, {"?key=daily", 1}, {"?view=all&key=daily", 2}} {
		response := httpFixture.request(t, http.MethodGet, base+query.suffix, token, "")
		var page api.ListTriggersResponse
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Triggers) != query.count {
			t.Fatalf("current/filter %s: %d %s", query.suffix, response.Code, response.Body)
		}
	}
	for i := 1; i <= 3; i++ {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO trigger_occurrences(environment_id,agent_id,trigger_key,scheduled_at,activation_id,disposition,reason,evaluated_at) VALUES($1,$2,'daily',$3,$4,'missed','agent_deleted',$3)`, f.Environment, f.Agent, at.Add(time.Duration(i)*time.Minute), oldID)
	}
	occurrencePath := base + "/" + oldID.String() + "/occurrences?limit=2"
	response := httpFixture.request(t, http.MethodGet, occurrencePath, token, "")
	var occurrences api.TriggerOccurrencesPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &occurrences) != nil || len(occurrences.Occurrences) != 2 || occurrences.NextCursor == "" {
		t.Fatalf("occurrences: %d %s", response.Code, response.Body)
	}
	cursor := occurrences.NextCursor
	response = httpFixture.request(t, http.MethodGet, occurrencePath+"&cursor="+cursor, token, "")
	occurrences = api.TriggerOccurrencesPage{}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &occurrences) != nil || len(occurrences.Occurrences) != 1 || occurrences.NextCursor != "" {
		t.Fatalf("occurrence pagination: %d %s", response.Code, response.Body)
	}
	response = httpFixture.request(t, http.MethodGet, base+"/"+currentID.String()+"/occurrences?cursor="+cursor, token, "")
	if response.Code != 400 {
		t.Fatalf("cross-activation cursor: %d %s", response.Code, response.Body)
	}
	path := base + "?view=all&agent=agent&limit=1"
	seen := map[string]bool{}
	for range 3 {
		response := httpFixture.request(t, http.MethodGet, path, token, "")
		if response.Code != http.StatusOK {
			t.Fatalf("list=%d %s", response.Code, response.Body)
		}
		var page api.ListTriggersResponse
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Triggers) != 1 || seen[page.Triggers[0].ID] {
			t.Fatalf("page=%+v", page)
		}
		row := page.Triggers[0]
		seen[row.ID] = true
		var input struct {
			Report bool `json:"report"`
		}
		if err := json.Unmarshal(row.Input, &input); err != nil {
			t.Fatal(err)
		}
		if row.Agent != "agent" || row.DeploymentID != f.Deployment.String() || !input.Report {
			t.Fatalf("projection=%+v", row)
		}
		if row.ID == oldID.String() && (row.ActiveUntil == nil || row.NextFireAt != nil) {
			t.Fatalf("ended activation=%+v", row)
		}
		if len(seen) < 3 && page.NextCursor == "" {
			t.Fatal("lost remaining triggers")
		}
		if len(seen) == 3 && page.NextCursor != "" {
			t.Fatal("unexpected cursor")
		}
		path = base + "?view=all&agent=agent&limit=1&cursor=" + page.NextCursor
	}
	item := httpFixture.request(t, http.MethodGet, base+"/"+oldID.String(), token, "")
	if item.Code != http.StatusOK {
		t.Fatalf("historical activation=%d %s", item.Code, item.Body)
	}
	foreignOrg := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Other','other')`, foreignOrg)
	otherToken := httpFixture.session(t, f.User, foreignOrg)
	denied := httpFixture.request(t, http.MethodGet, base, otherToken, "")
	if denied.Code == http.StatusOK {
		t.Fatal("cross-Organization trigger read succeeded")
	}
}
