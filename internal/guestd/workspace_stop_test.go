package guestd

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

func TestWorkspaceStopFencesAdmissionBeforeHostCapture(t *testing.T) {
	entry := &workspaceMountEntry{}
	registry := testWorkspaceBasicExecRegistry(entry)
	request := testWorkspaceBasicExecRequest("process", "token")
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.StopWorkspaceRequest{Envelope: request.Envelope}); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceStop(&stream, registry); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.StopWorkspaceResponse
	if err := frameio.ReadProtoFrame(&stream, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "stopped" || !entry.stopping || entry.authorityState != workspaceAuthorityFinalizing {
		t.Fatal("capture acknowledged without admission fence")
	}
	result := registry.runWorkspaceBasicExec(t.Context(), entry, request)
	if result.Outcome == "exited" {
		t.Fatal("new exec admitted after capture preparation")
	}
}
func TestWorkspaceStopRejectsActiveExec(t *testing.T) {
	entry := &workspaceMountEntry{processAdmissions: 1}
	registry := testWorkspaceBasicExecRegistry(entry)
	request := testWorkspaceBasicExecRequest("process", "token")
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.StopWorkspaceRequest{Envelope: request.Envelope}); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceStop(&stream, registry); err == nil {
		t.Fatal("capture preparation accepted active exec")
	}
	if entry.stopping {
		t.Fatal("rejected stop fenced active execution")
	}
}
