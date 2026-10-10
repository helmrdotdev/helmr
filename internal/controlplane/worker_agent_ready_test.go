package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentReadyUsesCurrentAttachment(t *testing.T) {
	f := agenttest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='starting' WHERE session_id=$1`, f.Session)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	session := runtimeTestSession(f)
	first, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.AgentControlRequest{Session: session, AttachmentSequence: first.AttachmentSequence + 1}
	if err = client.ObserveAgentReady(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("wrong attachment: %v", err)
	}
	second, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	request.AttachmentSequence = first.AttachmentSequence
	if err = client.ObserveAgentReady(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("stale attachment: %v", err)
	}
	request.AttachmentSequence = second.AttachmentSequence
	for range 2 {
		if err = client.ObserveAgentReady(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	var ready bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='ready' FROM session_processes WHERE session_id=$1`, f.Session).Scan(&ready); err != nil || !ready {
		t.Fatalf("setup receipt: %v %v", ready, err)
	}
	if _, err = agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "stop"}); err != nil {
		t.Fatal(err)
	}
	if err = client.ObserveAgentReady(t.Context(), request); err != nil {
		t.Fatalf("late ready cannot be retired: %v", err)
	}
	control, err := client.PrepareAgentControl(t.Context(), request)
	if err != nil || control.Kind != "shutdown" {
		t.Fatalf("late Ready resurrected Session: %+v %v", control, err)
	}
	foreign := request
	foreign.Session.ComputerLeaseEpoch++
	if err = client.ObserveAgentReady(t.Context(), foreign); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("wrong physical lease: %v", err)
	}
	request.AttachmentSequence = 0
	if err = client.ObserveAgentReady(t.Context(), request); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("invalid attachment: %v", err)
	}
}
