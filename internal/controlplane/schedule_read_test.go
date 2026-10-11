package controlplane

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestScheduleListCursorRoundTripAndScope(t *testing.T) {
	raw, err := encodeScheduleListCursor(scheduleListCursor{
		ProjectID:     "project-1",
		EnvironmentID: "environment-1",
		AgentID:       "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37",
		ScheduleID:    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/schedules?limit=25&cursor="+raw,
		nil,
	)
	limit, cursor, taskID, err := parseScheduleListQuery(
		request,
		"project-1",
		"environment-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 25 || cursor == nil || taskID != nil ||
		cursor.AgentID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" ||
		cursor.ScheduleID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36" {
		t.Fatalf("limit=%d cursor=%+v", limit, cursor)
	}
	if _, _, _, err := parseScheduleListQuery(
		request,
		"project-1",
		"environment-2",
	); err == nil {
		t.Fatal("cross-environment cursor succeeded")
	}
}

func TestScheduleListQueryRejectsUnknownAndInvalidValues(t *testing.T) {
	for _, target := range []string{
		"/v1/schedules?task=unexpected",
		"/v1/schedules?limit=0",
		"/v1/schedules?limit=101",
		"/v1/schedules?cursor=invalid",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if _, _, _, err := parseScheduleListQuery(
			request,
			"project-1",
			"environment-1",
		); err == nil {
			t.Fatalf("parseScheduleListQuery(%q) succeeded", target)
		}
	}
}

func TestScheduleListQueryAcceptsAgentFilter(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/schedules?agent_id=019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37", nil)
	limit, cursor, taskID, err := parseScheduleListQuery(request, "project-1", "environment-1")
	if err != nil {
		t.Fatal(err)
	}
	if limit != 50 || cursor != nil || taskID == nil || *taskID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc37" {
		t.Fatalf("limit=%d cursor=%+v taskID=%v", limit, cursor, taskID)
	}
}

func TestScheduleResponseProjectsActivationInterval(t *testing.T) {
	now := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	row := db.AgentSchedule{ID: pgvalue.UUID(uuid.NewV7()), AgentID: pgvalue.UUID(uuid.NewV7()), DeploymentID: pgvalue.UUID(uuid.NewV7()), TriggerKey: "daily", Cron: "0 9 * * *", Timezone: "UTC", Input: []byte(`[{"type":"text","text":"{\"report\":true}"}]`), ActiveFrom: pgvalue.Timestamptz(now), NextFireAt: pgvalue.Timestamptz(now.Add(time.Hour))}
	response, err := scheduleResponse(row)
	if err != nil || response.AgentID != pgvalue.UUIDString(row.AgentID) || response.TriggerKey != "daily" || response.NextFireAt == nil || response.ActiveUntil != nil || string(response.Input) != string(row.Input) {
		t.Fatalf("response=%+v error=%v", response, err)
	}
	row.ActiveUntil = row.NextFireAt
	response, err = scheduleResponse(row)
	if err != nil || response.NextFireAt != nil || response.ActiveUntil == nil {
		t.Fatalf("closed interval=%+v error=%v", response, err)
	}
}

func TestScheduleCursorRejectsFilterChange(t *testing.T) {
	id := uuid.NewV7().String()
	raw, err := encodeScheduleListCursor(scheduleListCursor{ProjectID: "p", EnvironmentID: "e", AgentID: id, ScheduleID: id, FilterAgentID: id})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/schedules?cursor="+raw, nil)
	if _, _, _, err := parseScheduleListQuery(request, "p", "e"); err == nil {
		t.Fatal("changed filter accepted")
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/schedules?agent_id="+id+"&cursor="+raw+"&limit=2", nil)
	if limit, _, filter, err := parseScheduleListQuery(request, "p", "e"); err != nil || limit != 2 || filter == nil || *filter != id {
		t.Fatalf("filter=%v error=%v", filter, err)
	}
}

func TestScheduleHTTPListsMultiplePinnedActivationsWithinScope(t *testing.T) {
	f := agenttest.New(t)
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
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agent_schedules(environment_id,id,agent_id,deployment_id,trigger_key,cron,timezone,input,active_from,active_until,next_fire_at,lateness_tolerance_ms) VALUES($1,$2,$3,$4,$5,'0 * * * *','UTC','{"report":true}',$6,$7,$8,300000)`, f.Environment, id, f.Agent, f.Deployment, trigger, at, until, at.Add(time.Hour))
	}
	q := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB = q
	cfg.TX = f.Pool
	cfg.Auth = identity.NewAPIKeyAuthenticator(q)
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
	base := fmt.Sprintf("/api/projects/%s/environments/%s/schedules", project, f.Environment)
	path := base + "?agent_id=" + f.Agent.String() + "&limit=1"
	seen := map[string]bool{}
	for range 3 {
		response := httpFixture.request(t, http.MethodGet, path, token, "")
		if response.Code != http.StatusOK {
			t.Fatalf("list=%d %s", response.Code, response.Body)
		}
		var page api.ListSchedulesResponse
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Schedules) != 1 || seen[page.Schedules[0].ID] {
			t.Fatalf("page=%+v", page)
		}
		row := page.Schedules[0]
		seen[row.ID] = true
		var input struct {
			Report bool `json:"report"`
		}
		if err := json.Unmarshal(row.Input, &input); err != nil {
			t.Fatal(err)
		}
		if row.AgentID != f.Agent.String() || row.DeploymentID != f.Deployment.String() || !input.Report {
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
		path = base + "?agent_id=" + f.Agent.String() + "&limit=1&cursor=" + page.NextCursor
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
		t.Fatal("cross-Organization schedule read succeeded")
	}
}
