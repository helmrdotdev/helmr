package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestCommandCompletionRequiresCanonicalUUIDv7(t *testing.T) {
	valid := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	exitCode := int32(0)
	base := workerapi.ComputerCommandCompleteRequest{EnvironmentID: valid, CommandID: valid, ComputerInstanceID: valid, WriterGeneration: 1, Outcome: "exited", ExitCode: &exitCode, Stdout: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}, Stderr: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}}
	if report, err := completionReport(base); err != nil || report.Validate() != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"environment", "command", "instance"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []string{"8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d", "019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC31", " " + valid} {
				request := base
				switch field {
				case "environment":
					request.EnvironmentID = value
				case "command":
					request.CommandID = value
				case "instance":
					request.ComputerInstanceID = value
				}
				if _, err := completionReport(request); err == nil {
					t.Fatalf("accepted %s ID %q", field, value)
				}
			}
		})
	}
}

func TestWorkerCompleteComputerCommandRejectsNonCanonicalUUIDv7(t *testing.T) {
	body, err := json.Marshal(workerapi.ComputerCommandCompleteRequest{
		EnvironmentID: " 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/worker/computers/commands/complete", bytes.NewReader(body))
	response := httptest.NewRecorder()

	(&Server{}).workerCompleteComputerCommand(response, request)

	if response.Code != 400 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}
