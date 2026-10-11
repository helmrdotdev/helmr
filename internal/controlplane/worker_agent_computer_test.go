package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func agentComputerBytes(t *testing.T, p proto.Message) []byte {
	t.Helper()
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func beginWorkerAgentCapture(t *testing.T, f agenttest.Fixture, client *workerclient.Client) (workerapi.AgentComputerCaptureRequest, workerapi.AgentComputerCaptureResponse, *agentv1.ComputerSessionCapture) {
	t.Helper()
	request := workerapi.AgentComputerCaptureRequest{EnvironmentID: f.Environment.String(), ComputerID: f.Computer.String(), CheckpointID: uuid.NewV7().String(), LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential}
	response, err := client.BeginAgentComputerCapture(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	capture := new(agentv1.ComputerSessionCapture)
	if err = proto.Unmarshal(response.Capture, capture); err != nil {
		t.Fatal(err)
	}
	return request, response, capture
}
func workerSourceAbort(t *testing.T, f agenttest.Fixture, capture *agentv1.ComputerSessionCapture) *agentv1.ComputerSessionInstallation {
	t.Helper()
	envelope := proto.Clone(capture.Envelope).(*computerv0.ComputerOperationEnvelope)
	envelope.OperationExpiresAtUnixNano = time.Now().Add(20 * time.Second).UnixNano()
	p := &agentv1.ComputerSessionInstallation{Capture: capture, Envelope: envelope, DesiredVersion: capture.DesiredVersion + 1, SourceAbort: true}
	if err := f.Pool.QueryRow(t.Context(), `SELECT COALESCE(l.restored_from_save_id::text,'sha256:'||encode(c.initial_root_digest,'hex')) FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id) WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=1`, f.Environment, f.Computer).Scan(&p.BaseComputerDiskVersionId); err != nil {
		t.Fatal(err)
	}
	for _, identity := range capture.Sessions {
		p.Grants = append(p.Grants, &agentv1.SessionGrant{Identity: identity, ComputerId: envelope.ComputerId, ComputerInstanceId: envelope.ComputerInstanceId, WriterGeneration: 1, ComputerLeaseEpoch: 1, WorkerHostId: f.Worker.String(), AuthorityGeneration: 1, ExpiresAtUnixNano: envelope.OperationExpiresAtUnixNano, ChannelCredential: envelope.ChannelCredential})
	}
	return p
}
func TestWorkerAgentComputerSourceAbortRoundTrip(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	foreign := foreignAgentComputerClient(t, f, server.URL)
	request, result, capture := beginWorkerAgentCapture(t, f, client)
	repeated, err := client.BeginAgentComputerCapture(t.Context(), request)
	if err != nil || !bytes.Equal(result.Capture, repeated.Capture) || result.SaveID != repeated.SaveID {
		t.Fatalf("capture retry changed: %v", err)
	}
	if capture.Envelope.ChannelCredential != agenttest.ChannelCredential {
		t.Fatal("capture credential changed in transport")
	}
	observed := &agentv1.ComputerSessionReceipt{CheckpointId: capture.CheckpointId, DesiredVersion: capture.DesiredVersion, Frozen: true}
	assertForeignAgentComputer(t, f, func() error {
		return foreign.SealAgentComputerCapture(t.Context(), workerapi.AgentComputerReceiptRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Receipt: agentComputerBytes(t, observed)})
	})
	if err := client.SealAgentComputerCapture(t.Context(), workerapi.AgentComputerReceiptRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Receipt: agentComputerBytes(t, observed)}); err != nil {
		t.Fatal(err)
	}
	prepare := workerapi.AgentComputerSourceAbortRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential, Receipt: agentComputerBytes(t, observed)}
	assertForeignAgentComputer(t, f, func() error { _, err := foreign.PrepareAgentComputerSourceAbort(t.Context(), prepare); return err })
	prepared, err := client.PrepareAgentComputerSourceAbort(t.Context(), prepare)
	if err != nil {
		t.Fatal(err)
	}
	p := new(agentv1.ComputerSessionInstallation)
	if err := proto.Unmarshal(prepared.Installation, p); err != nil {
		t.Fatal(err)
	}
	if !p.SourceAbort || !proto.Equal(p.Capture, capture) || p.Grants[0].AuthorityGeneration != 1 {
		t.Fatal("source preparation lost current authority")
	}
	invalidPrepare := prepare
	invalidPrepare.Receipt = []byte{0}
	if _, err := client.PrepareAgentComputerSourceAbort(t.Context(), invalidPrepare); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("malformed source receipt: %v", err)
	}
	install := workerapi.AgentComputerInstallationRequest{EnvironmentID: request.EnvironmentID, Installation: agentComputerBytes(t, p), Receipt: agentComputerBytes(t, observed)}
	assertForeignAgentComputer(t, f, func() error { return foreign.ValidateAgentComputerSourceAbort(t.Context(), install) })
	if err := client.ValidateAgentComputerSourceAbort(t.Context(), install); err != nil {
		t.Fatal(err)
	}
	assertForeignAgentComputer(t, f, func() error {
		return foreign.AgentComputerSaveAbsence(t.Context(), workerapi.AgentComputerSaveAbsenceRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Evidence: "owned disk pipeline joined without a cut"})
	})
	if err := client.AgentComputerSaveAbsence(t.Context(), workerapi.AgentComputerSaveAbsenceRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Evidence: "owned disk pipeline joined without a cut"}); err != nil {
		t.Fatal(err)
	}
	if err := client.CommitAgentComputerSourceAbort(t.Context(), install); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("uninstalled commit: %v", err)
	}
	changed := proto.Clone(p).(*agentv1.ComputerSessionInstallation)
	changed.Envelope.ComputerInstanceId = uuid.NewV7().String()
	invalid := install
	invalid.Installation = agentComputerBytes(t, changed)
	assertAgentComputerErrorCode(t, client.ValidateAgentComputerSourceAbort(t.Context(), invalid), workerapi.AgentComputerAuthorityUnavailable)
	changed = proto.Clone(p).(*agentv1.ComputerSessionInstallation)
	changed.DesiredVersion++
	invalid.Installation = agentComputerBytes(t, changed)
	assertAgentComputerErrorCode(t, client.ValidateAgentComputerSourceAbort(t.Context(), invalid), workerapi.AgentComputerEvidenceConflict)
	observed.Installed = true
	observed.DesiredVersion = p.DesiredVersion
	install.Receipt = agentComputerBytes(t, observed)
	assertWorkerControlsBeforeCommit(t, f, client, foreign, install)
	assertForeignAgentComputer(t, f, func() error { return foreign.CommitAgentComputerSourceAbort(t.Context(), install) })
	if err := client.CommitAgentComputerSourceAbort(t.Context(), install); err != nil {
		t.Fatal(err)
	}
	assertForeignAgentComputer(t, f, func() error { _, err := foreign.PrepareAgentComputerSourceAbort(t.Context(), prepare); return err })
	if _, err := client.PrepareAgentComputerSourceAbort(t.Context(), prepare); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("source prepare after consumption: %v", err)
	}
	assertWorkerCurrentControls(t, f, client, foreign, install, p)
	observed.Frozen = false
	observed.ActivationStarted = true
	observed.Activated = true
	install.Receipt = agentComputerBytes(t, observed)
	complete := install
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.Session)
	if err := client.CompleteAgentComputerSourceAbort(t.Context(), complete); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("stale controls: %v", err)
	} else {
		assertAgentComputerErrorCode(t, err, workerapi.AgentComputerNotReady)
	}
	acknowledgeWorkerComputerMembers(t, client, request.EnvironmentID, p)
	assertForeignAgentComputer(t, f, func() error { return foreign.CompleteAgentComputerSourceAbort(t.Context(), complete) })
	for range 2 {
		if err := client.CompleteAgentComputerSourceAbort(t.Context(), complete); err != nil {
			t.Fatal(err)
		}
	}
	var reconciled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT controls_reconciled_at IS NOT NULL AND capture_request IS NULL FROM computer_checkpoints WHERE id=$1`, request.CheckpointID).Scan(&reconciled); err != nil || !reconciled {
		t.Fatalf("completion not durable: %v", err)
	}
}
func TestWorkerAgentComputerCancellationRequiresOwnedEvidence(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	foreign := foreignAgentComputerClient(t, f, server.URL)
	request, _, _ := beginWorkerAgentCapture(t, f, client)
	cancel := workerapi.AgentComputerUnsealedRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID}
	if err := client.CancelAgentComputerCapture(t.Context(), cancel); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("absent evidence accepted: %v", err)
	}
	cancel.Rejected = true
	assertForeignAgentComputer(t, f, func() error { return foreign.CancelAgentComputerCapture(t.Context(), cancel) })
	if err := client.CancelAgentComputerCapture(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_checkpoints WHERE id=$1`, request.CheckpointID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("state=%s: %v", state, err)
	}
}
func TestWorkerAgentComputerRejectsForeignHostAndMalformedRequests(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	auth := seedHostSecret(t, f.Pool, f.Worker)
	client := auth.client(t, server.URL)
	request, _, capture := beginWorkerAgentCapture(t, f, client)
	foreign := foreignAgentComputerClient(t, f, server.URL)
	assertForeignAgentComputer(t, f, func() error { _, err := foreign.BeginAgentComputerCapture(t.Context(), request); return err })
	p := workerSourceAbort(t, f, capture)
	receipt := agentComputerBytes(t, &agentv1.ComputerSessionReceipt{CheckpointId: capture.CheckpointId, DesiredVersion: capture.DesiredVersion, Frozen: true})
	install := workerapi.AgentComputerInstallationRequest{EnvironmentID: request.EnvironmentID, Installation: agentComputerBytes(t, p), Receipt: receipt}
	calls := []func(context.Context, workerapi.AgentComputerInstallationRequest) error{foreign.ValidateAgentComputerSourceAbort, foreign.CommitAgentComputerSourceAbort, foreign.ValidateAgentComputerRestore, foreign.CommitAgentComputerRestore}
	for index, call := range calls {
		if index >= 2 {
			p.SourceAbort = false
			install.Installation = agentComputerBytes(t, p)
		}
		assertForeignAgentComputer(t, f, func() error { return call(t.Context(), install) })
	}
	p.SourceAbort = true
	install.Installation = agentComputerBytes(t, p)
	completion := install
	assertForeignAgentComputer(t, f, func() error { return foreign.CompleteAgentComputerSourceAbort(t.Context(), completion) })
	assertForeignAgentComputer(t, f, func() error {
		return foreign.AgentComputerSaveAbsence(t.Context(), workerapi.AgentComputerSaveAbsenceRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, Evidence: "joined disk pipeline"})
	})
	p.SourceAbort = false
	install.Installation = agentComputerBytes(t, p)
	completion = install
	assertForeignAgentComputer(t, f, func() error { return foreign.CompleteAgentComputerRestore(t.Context(), completion) })
	assertForeignAgentComputer(t, f, func() error {
		_, err := foreign.PrepareAgentComputerRestore(t.Context(), workerapi.AgentComputerRestoreRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, LeaseEpoch: 2, ChannelCredential: "foreign"})
		return err
	})
	for _, raw := range []string{`{"environment_id":"` + request.EnvironmentID + `","worker_host_id":"` + f.Worker.String() + `"}`, `{"environment_id":"` + request.EnvironmentID + `","installation":"AA==","receipt":"AA=="}`} {
		response := postAgentComputerJSON(t, server, auth, "/worker/v1/agent-computers/restore/validate", []byte(raw))
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed status=%d", response.StatusCode)
		}
		response.Body.Close()
	}
	install.Receipt = bytes.Repeat([]byte{1}, 64*1024+1)
	if err := client.ValidateAgentComputerRestore(t.Context(), install); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("oversized receipt: %v", err)
	}
	// Every new route is beneath worker authentication, including empty bodies.
	for _, path := range []string{"capture/begin", "capture/seal", "capture/cancel", "capture/save-absence", "restore/prepare", "restore/validate", "restore/commit", "restore/complete", "source-abort/validate", "source-abort/commit", "source-abort/complete", "source-abort/prepare", "controls"} {
		response, err := http.Post(server.URL+"/worker/v1/agent-computers/"+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s: %d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}

func postAgentComputerJSON(t *testing.T, server *httptest.Server, auth seededHostSecret, path string, raw []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+auth.issue(t, server.Config.Handler))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestWorkerAgentComputerTargetRoundTrip(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	foreign := foreignAgentComputerClient(t, f, server.URL)
	request, result, _ := beginWorkerAgentCapture(t, f, client)
	prepare, instance := seedWorkerAgentRestoreTarget(t, f, request, result)
	assertForeignAgentComputer(t, f, func() error { _, err := foreign.PrepareAgentComputerRestore(t.Context(), prepare); return err })
	prepared, err := client.PrepareAgentComputerRestore(t.Context(), prepare)
	if err != nil {
		t.Fatal(err)
	}
	p := new(agentv1.ComputerSessionInstallation)
	if err := proto.Unmarshal(prepared.Installation, p); err != nil {
		t.Fatal(err)
	}
	if p.GetEnvelope().GetComputerInstanceId() != instance.String() || p.Grants[0].AuthorityGeneration != 2 {
		t.Fatal("target grant changed")
	}
	observed := &agentv1.ComputerSessionReceipt{CheckpointId: p.Capture.CheckpointId, DesiredVersion: p.Capture.DesiredVersion, Frozen: true}
	install := workerapi.AgentComputerInstallationRequest{EnvironmentID: request.EnvironmentID, Installation: prepared.Installation, Receipt: agentComputerBytes(t, observed)}
	assertForeignAgentComputer(t, f, func() error { return foreign.ValidateAgentComputerRestore(t.Context(), install) })
	if err := client.ValidateAgentComputerRestore(t.Context(), install); err != nil {
		t.Fatal(err)
	}
	observed.Installed = true
	observed.DesiredVersion = p.DesiredVersion
	install.Receipt = agentComputerBytes(t, observed)
	assertWorkerControlsBeforeCommit(t, f, client, foreign, install)
	assertForeignAgentComputer(t, f, func() error { return foreign.CommitAgentComputerRestore(t.Context(), install) })
	for range 2 {
		if err := client.CommitAgentComputerRestore(t.Context(), install); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.PrepareAgentComputerRestore(t.Context(), prepare); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("consumed replay: %v", err)
	}
	assertWorkerCurrentControls(t, f, client, foreign, install, p)
	observed.Frozen = false
	observed.ActivationStarted = true
	observed.Activated = true
	install.Receipt = agentComputerBytes(t, observed)
	assertForeignAgentComputer(t, f, func() error {
		return foreign.CompleteAgentComputerRestore(t.Context(), install)
	})
	acknowledgeWorkerComputerMembers(t, client, request.EnvironmentID, p)
	if err := client.CompleteAgentComputerRestore(t.Context(), install); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='active' FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=2`, f.Environment, f.Computer).Scan(&active); err != nil || !active {
		t.Fatalf("target inactive: %v", err)
	}
}

func TestWorkerAgentComputerCaptureRejectsExpiredAuthority(t *testing.T) {
	for _, change := range []struct {
		name, sql string
		status    int
	}{
		{"lease", "UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'", http.StatusConflict},
		{"host claims", "UPDATE worker_hosts SET claim_version=claim_version+1", http.StatusUnauthorized},
		{"host epoch", "UPDATE worker_hosts SET current_epoch=current_epoch+1", http.StatusUnauthorized},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := agenttest.New(t)
			handler := newPostgresServer(t, f.Pool)
			client := newWorkerHTTPClient(t, handler, f.Pool, f.Worker)
			dbtest.MustExec(t, t.Context(), f.Pool, change.sql)
			client.post(t, "/worker/v1/agent-computers/capture/begin", workerapi.AgentComputerCaptureRequest{EnvironmentID: f.Environment.String(), ComputerID: f.Computer.String(), CheckpointID: uuid.NewV7().String(), LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential}, change.status, nil)
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoints`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unauthorized capture changed state: %d %v", count, err)
			}
		})
	}
}

func foreignAgentComputerClient(t *testing.T, f agenttest.Fixture, baseURL string) *workerclient.Client {
	t.Helper()
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id',$2::text,'current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.Worker, other)
	return seedHostSecret(t, f.Pool, other).client(t, baseURL)
}
func assertAgentComputerErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var response *httpclient.Error
	if !errors.As(err, &response) || response.StatusCode != http.StatusConflict || response.Code != code {
		t.Fatalf("expected 409 %s: %v", code, err)
	}
}
func assertForeignAgentComputer(t *testing.T, f agenttest.Fixture, call func() error) {
	t.Helper()
	snapshot := func() string {
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('checkpoints',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM computer_checkpoints c WHERE environment_id=$1),'saves',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM computer_saves s WHERE environment_id=$1),'leases',(SELECT jsonb_agg(to_jsonb(l) ORDER BY epoch) FROM computer_leases l WHERE environment_id=$1),'sessions',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM sessions s WHERE environment_id=$1),'computers',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM computers c WHERE environment_id=$1),'processes',(SELECT jsonb_agg(to_jsonb(p) ORDER BY session_id,epoch) FROM session_processes p WHERE environment_id=$1))::text`, f.Environment).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	assertAgentComputerErrorCode(t, call(), workerapi.AgentComputerAuthorityUnavailable)
	if after := snapshot(); before != after {
		t.Fatal("foreign request changed durable Computer state")
	}
}
func TestWorkerAgentComputerOversizedChunkedBody(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool)
	credential := seedHostSecret(t, f.Pool, f.Worker).issue(t, handler)
	// An unknown Content-Length must still preserve the decoder's 413 result.
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/agent-computers/capture/begin", strings.NewReader(`{"channel_credential":"`+strings.Repeat("x", 32768)+`"}`))
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunked body: %d", response.Code)
	}
}

func assertWorkerControlsBeforeCommit(t *testing.T, f agenttest.Fixture, client, foreign *workerclient.Client, request workerapi.AgentComputerInstallationRequest) {
	t.Helper()
	assertForeignAgentComputer(t, f, func() error { _, err := foreign.ReadAgentComputerControls(t.Context(), request); return err })
	if _, err := client.ReadAgentComputerControls(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("controls before commit: %v", err)
	}
}

func assertWorkerCurrentControls(t *testing.T, f agenttest.Fixture, client, foreign *workerclient.Client, request workerapi.AgentComputerInstallationRequest, installed *agentv1.ComputerSessionInstallation) {
	t.Helper()
	assertForeignAgentComputer(t, f, func() error { _, err := foreign.ReadAgentComputerControls(t.Context(), request); return err })
	result, err := client.ReadAgentComputerControls(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	current := new(agentv1.ComputerSessionControls)
	if err := proto.Unmarshal(result.Controls, current); err != nil {
		t.Fatal(err)
	}
	if current.CheckpointId != installed.Capture.CheckpointId || current.DesiredVersion != installed.DesiredVersion || len(current.Sessions) != len(installed.Grants) {
		t.Fatalf("wrong controls: %v", current)
	}
	for i, control := range current.Sessions {
		if !proto.Equal(control.Identity, installed.Grants[i].Identity) || control.AuthorityGeneration != installed.Grants[i].AuthorityGeneration || control.Held || control.Stopped {
			t.Fatalf("wrong member: %v", control)
		}
	}
	bad := request
	bad.Installation = []byte{0}
	if _, err := client.ReadAgentComputerControls(t.Context(), bad); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("malformed controls: %v", err)
	}
	receipt := new(agentv1.ComputerSessionReceipt)
	if err := proto.Unmarshal(request.Receipt, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Activated = true
	receipt.ActivationStarted = true
	receipt.Frozen = false
	bad = request
	bad.Receipt = agentComputerBytes(t, receipt)
	if _, err := client.ReadAgentComputerControls(t.Context(), bad); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("activated controls: %v", err)
	}
	var acknowledged bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT controls_reconciled_at IS NOT NULL FROM computer_checkpoints WHERE id=$1`, current.CheckpointId).Scan(&acknowledged); err != nil || acknowledged {
		t.Fatalf("control read acknowledged physical application: %v %v", acknowledged, err)
	}
}

func acknowledgeWorkerComputerMembers(t *testing.T, client *workerclient.Client, environment string, p *agentv1.ComputerSessionInstallation) {
	t.Helper()
	for _, g := range p.Grants {
		session := workerapi.RuntimeSession{EnvironmentID: environment, SessionID: g.Identity.SessionId, ProcessEpoch: g.Identity.ProcessEpoch, ComputerLeaseEpoch: g.ComputerLeaseEpoch}
		a, err := client.AcquireAgentAttachment(t.Context(), session)
		if err != nil {
			t.Fatal(err)
		}
		request := workerapi.AgentControlRequest{Session: session, AttachmentSequence: a.AttachmentSequence}
		control, err := client.PrepareAgentControl(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.AcknowledgeAgentControl(t.Context(), workerapi.AgentControlReceipt{Session: session, AttachmentSequence: a.AttachmentSequence, Sequence: control.Sequence, AuthorityGeneration: control.AuthorityGeneration, Kind: control.Kind}); err != nil {
			t.Fatal(err)
		}
	}
}

func seedWorkerAgentRestoreTarget(t *testing.T, f agenttest.Fixture, request workerapi.AgentComputerCaptureRequest, result workerapi.AgentComputerCaptureResponse) (workerapi.AgentComputerRestoreRequest, uuid.UUID) {
	t.Helper()
	// Readiness, exact root certification and physical admission are fixture state
	// here. Their storage/VM boundaries are not proved by this HTTP transport test.
	platform := "sha256:" + strings.Repeat("1", 64)
	pack := "sha256:" + strings.Repeat("2", 64)
	key, root, instance := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	targetCredential := "new-target-channel"
	credentialDigest := sha256.Sum256([]byte(targetCredential))
	locator := fmt.Sprintf(`{"format_version":1,"logical_bytes":4096,"pack":{"digest":%q,"size_bytes":64,"rank":1},"page":{"key_id":%q},"offset":8}`, pack, key.String())
	manifest, err := json.Marshal(computercheckpoint.Manifest{Runtime: vm.CheckpointIdentity{RuntimeBackend: "firecracker", RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0", RuntimeID: platform, KernelDigest: platform, InitramfsDigest: platform, RootfsDigest: platform, VMConfigDigest: platform, VMVCPUCount: 1, CPUConfigDigest: platform}})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($4,$1,$2,'test',decode('01','hex'));
 INSERT INTO cas_blobs(digest,size_bytes) VALUES($6,64);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$6,64,'application/octet-stream' FROM environments WHERE id=$1;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$6,org_id,project_id,64,'application/octet-stream','root',1,'{}',clock_timestamp() FROM environments WHERE id=$1;
 INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($1,$6,$4,true);
 INSERT INTO computer_disk_roots(environment_id,id,locator) VALUES($1,$5,$7);
 UPDATE computer_saves SET status='published',root_id=$5,captured_root_digest=decode(substring($8::text from 8),'hex'),flush_acknowledged_at=statement_timestamp(),captured_at=statement_timestamp(),capture_evidence='fixture coherent cut',publication_evidence='fixture certified root' WHERE id=$3;
 UPDATE computers SET recovery_save_id=$3 WHERE environment_id=$1 AND id=$2;
 UPDATE computer_checkpoints SET status='ready',manifest=$9,vm_platform_id=$8,ready_at=clock_timestamp() WHERE id=$10;
 UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture physical closure' WHERE environment_id=$1 AND computer_id=$2;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,restored_from_save_id,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) VALUES($1,$2,2,$11,1,clock_timestamp()+interval '1 hour',$12,$13,$3,'acquiring',1000,536870912,1073741824,'sha256:1111111111111111111111111111111111111111111111111111111111111111',1,'sha256:1111111111111111111111111111111111111111111111111111111111111111',clock_timestamp(),NULL);
 UPDATE computer_leases SET base_root_id=$5,write_key_id=$4 WHERE environment_id=$1 AND computer_id=$2 AND epoch=2;
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Computer, result.SaveID, key, root, pack, locator, platform, manifest, request.CheckpointID, f.Worker, instance, credentialDigest[:])
	prepare := workerapi.AgentComputerRestoreRequest{EnvironmentID: request.EnvironmentID, CheckpointID: request.CheckpointID, LeaseEpoch: 2, ChannelCredential: targetCredential}
	return prepare, instance
}
