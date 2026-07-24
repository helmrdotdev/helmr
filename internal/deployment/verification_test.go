package deployment

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestVerificationResultCanonicalRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		result VerificationResult
	}{
		{name: "workspace only", result: testWorkspaceVerificationResult(t)},
		{name: "program backed", result: testProgramVerificationResult(t)},
		{name: "failed", result: testFailedVerificationResult()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := CanonicalVerificationResult(test.result)
			if err != nil {
				t.Fatalf("CanonicalVerificationResult: %v", err)
			}
			parsed, err := ParseVerificationResult(raw)
			if err != nil {
				t.Fatalf("ParseVerificationResult: %v", err)
			}
			recoded, err := CanonicalVerificationResult(parsed)
			if err != nil {
				t.Fatalf("CanonicalVerificationResult(parsed): %v", err)
			}
			if string(recoded) != string(raw) {
				t.Fatalf("canonical bytes changed:\n%s\n%s", raw, recoded)
			}
		})
	}
}

func TestReadVerificationResultFrameRequiresExactlyOneFrame(t *testing.T) {
	raw, err := CanonicalVerificationResult(testProgramVerificationResult(t))
	if err != nil {
		t.Fatal(err)
	}
	var frame bytes.Buffer
	if err := frameio.WriteMessageFrame(&frame, raw); err != nil {
		t.Fatal(err)
	}
	result, err := ReadVerificationResultFrame(&frame)
	if err != nil {
		t.Fatalf("ReadVerificationResultFrame: %v", err)
	}
	if result.Outcome != VerificationOutcomeSucceeded {
		t.Fatalf("outcome = %q", result.Outcome)
	}

	frame.Reset()
	if err := frameio.WriteMessageFrame(&frame, raw); err != nil {
		t.Fatal(err)
	}
	frame.WriteByte(0)
	if _, err := ReadVerificationResultFrame(&frame); err == nil ||
		!strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing frame error = %v", err)
	}
}

func TestParseVerificationResultRejectsOpenOrNoncanonicalShape(t *testing.T) {
	valid, err := CanonicalVerificationResult(testProgramVerificationResult(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "unknown root member",
			mutate: func(root map[string]any) {
				root["extra"] = true
			},
			wantErr: "unknown field",
		},
		{
			name: "missing format version",
			mutate: func(root map[string]any) {
				delete(root, "formatVersion")
			},
			wantErr: "complete canonical v0 shape",
		},
		{
			name: "duplicate file",
			mutate: func(root map[string]any) {
				files := root["files"].([]any)
				files[1].(map[string]any)["path"] = VerificationBuildPlanPath
			},
			wantErr: "files[1].path",
		},
		{
			name: "out of order file",
			mutate: func(root map[string]any) {
				files := root["files"].([]any)
				files[1], files[2] = files[2], files[1]
			},
			wantErr: "files[1].path",
		},
		{
			name: "partial program result",
			mutate: func(root map[string]any) {
				root["files"] = root["files"].([]any)[:1]
			},
			wantErr: "program-backed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := mutateVerificationResultJSON(t, valid, test.mutate)
			_, err := ParseVerificationResult(raw)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}

	if _, err := ParseVerificationResult(append([]byte(" "), valid...)); err == nil ||
		!strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical error = %v", err)
	}
}

func TestVerificationResultVerifiesGeneratedFilesAgainstPlan(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*VerificationResult)
		wantErr string
	}{
		{
			name: "noncanonical build plan",
			change: func(result *VerificationResult) {
				result.Succeeded.Files[0].Content = " " +
					result.Succeeded.Files[0].Content
			},
			wantErr: "build plan",
		},
		{
			name: "noncanonical declaration locator",
			change: func(result *VerificationResult) {
				result.Succeeded.Files[1].Content = " " +
					result.Succeeded.Files[1].Content
			},
			wantErr: "declaration locator",
		},
		{
			name: "declaration identity mismatch",
			change: func(result *VerificationResult) {
				locator := testAnalysisDeclarationLocator()
				locator.Declarations[0].DeclaredID = "different"
				raw, err := CanonicalDeclarationLocator(locator)
				if err != nil {
					panic(err)
				}
				result.Succeeded.Files[1].Content = string(raw)
			},
			wantErr: "does not match build plan at position 0",
		},
		{
			name: "program entry mismatch",
			change: func(result *VerificationResult) {
				result.Succeeded.Files[2].Content = ProgramEntry + "\n"
			},
			wantErr: "fixed v0 bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testProgramVerificationResult(t)
			test.change(&result)
			err := ValidateVerificationResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestVerificationFailureContract(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*VerificationResult)
		wantErr string
	}{
		{
			name: "reason",
			change: func(result *VerificationResult) {
				result.Failed.Error.Reason = "invalid_source"
			},
			wantErr: "unsupported",
		},
		{
			name: "blank message",
			change: func(result *VerificationResult) {
				result.Failed.Error.Message = " \n "
			},
			wantErr: "nonblank UTF-8",
		},
		{
			name: "message bound",
			change: func(result *VerificationResult) {
				result.Failed.Error.Message =
					strings.Repeat("x", maxBuildFailureMessageBytes+1)
			},
			wantErr: "nonblank UTF-8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testFailedVerificationResult()
			test.change(&result)
			err := ValidateVerificationResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func testWorkspaceVerificationResult(t *testing.T) VerificationResult {
	t.Helper()
	plan := testBuildPlan()
	plan.Definitions = []DefinitionInput{plan.Definitions[2]}
	plan.Queues = []QueueInput{}
	raw, err := CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return VerificationResult{
		FormatVersion: VerificationResultFormatVersion,
		Outcome:       VerificationOutcomeSucceeded,
		Succeeded: &VerificationSucceeded{
			Declarations: []ProgramDeclaration{},
			Files: []VerificationFile{{
				Path:    VerificationBuildPlanPath,
				Content: string(raw),
			}},
		},
	}
}

func testProgramVerificationResult(t *testing.T) VerificationResult {
	t.Helper()
	plan := testBuildPlan()
	planRaw, err := CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	locatorRaw, err := CanonicalDeclarationLocator(
		testAnalysisDeclarationLocator(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return VerificationResult{
		FormatVersion: VerificationResultFormatVersion,
		Outcome:       VerificationOutcomeSucceeded,
		Succeeded: &VerificationSucceeded{
			Declarations: buildPlanProgramDeclarations(plan),
			Files: []VerificationFile{
				{Path: VerificationBuildPlanPath, Content: string(planRaw)},
				{Path: VerificationDeclarationsPath, Content: string(locatorRaw)},
				{Path: VerificationProgramEntryPath, Content: ProgramEntry},
			},
		},
	}
}

func testFailedVerificationResult() VerificationResult {
	return VerificationResult{
		FormatVersion: VerificationResultFormatVersion,
		Outcome:       VerificationOutcomeFailed,
		Failed: &VerificationFailed{Error: VerificationError{
			Reason:  VerificationFailureReason,
			Message: "declaration verification failed",
		}},
	}
}

func testAnalysisDeclarationLocator() DeclarationLocator {
	return DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations: []LocatedDeclaration{
			{
				Kind:       DeclarationKindTask,
				DeclaredID: "build",
				ModulePath: "src/build.ts",
				ExportName: "build",
			},
			{
				Kind:       DeclarationKindActor,
				DeclaredID: "chat",
				ModulePath: "src/chat.ts",
				ExportName: "chat",
			},
		},
	}
}

func mutateVerificationResultJSON(
	t *testing.T,
	raw []byte,
	mutate func(map[string]any),
) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
