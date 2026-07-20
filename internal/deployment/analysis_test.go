package deployment

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestAnalysisResultCanonicalRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		result AnalysisResult
	}{
		{name: "workspace only", result: testWorkspaceAnalysisResult(t)},
		{name: "program backed", result: testProgramAnalysisResult(t)},
		{name: "failed", result: testFailedAnalysisResult()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := CanonicalAnalysisResult(test.result)
			if err != nil {
				t.Fatalf("CanonicalAnalysisResult: %v", err)
			}
			parsed, err := ParseAnalysisResult(raw)
			if err != nil {
				t.Fatalf("ParseAnalysisResult: %v", err)
			}
			recoded, err := CanonicalAnalysisResult(parsed)
			if err != nil {
				t.Fatalf("CanonicalAnalysisResult(parsed): %v", err)
			}
			if string(recoded) != string(raw) {
				t.Fatalf("canonical bytes changed:\n%s\n%s", raw, recoded)
			}
		})
	}
}

func TestReadAnalysisResultFrameRequiresExactlyOneFrame(t *testing.T) {
	raw, err := CanonicalAnalysisResult(testProgramAnalysisResult(t))
	if err != nil {
		t.Fatal(err)
	}
	var frame bytes.Buffer
	if err := frameio.WriteMessageFrame(&frame, raw); err != nil {
		t.Fatal(err)
	}
	result, err := ReadAnalysisResultFrame(&frame)
	if err != nil {
		t.Fatalf("ReadAnalysisResultFrame: %v", err)
	}
	if result.Outcome != AnalysisOutcomeSucceeded {
		t.Fatalf("outcome = %q", result.Outcome)
	}

	frame.Reset()
	if err := frameio.WriteMessageFrame(&frame, raw); err != nil {
		t.Fatal(err)
	}
	frame.WriteByte(0)
	if _, err := ReadAnalysisResultFrame(&frame); err == nil ||
		!strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing frame error = %v", err)
	}
}

func TestParseAnalysisResultRejectsOpenOrNoncanonicalShape(t *testing.T) {
	valid, err := CanonicalAnalysisResult(testProgramAnalysisResult(t))
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
				files[1].(map[string]any)["path"] = AnalysisBuildPlanPath
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
			raw := mutateAnalysisResultJSON(t, valid, test.mutate)
			_, err := ParseAnalysisResult(raw)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}

	if _, err := ParseAnalysisResult(append([]byte(" "), valid...)); err == nil ||
		!strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical error = %v", err)
	}
}

func TestAnalysisResultVerifiesGeneratedFilesAgainstPlan(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*AnalysisResult)
		wantErr string
	}{
		{
			name: "noncanonical build plan",
			change: func(result *AnalysisResult) {
				result.Succeeded.Files[0].Content = " " +
					result.Succeeded.Files[0].Content
			},
			wantErr: "build plan",
		},
		{
			name: "noncanonical declaration locator",
			change: func(result *AnalysisResult) {
				result.Succeeded.Files[1].Content = " " +
					result.Succeeded.Files[1].Content
			},
			wantErr: "declaration locator",
		},
		{
			name: "declaration identity mismatch",
			change: func(result *AnalysisResult) {
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
			change: func(result *AnalysisResult) {
				result.Succeeded.Files[2].Content = ProgramEntry + "\n"
			},
			wantErr: "fixed v0 bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testProgramAnalysisResult(t)
			test.change(&result)
			err := ValidateAnalysisResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestAnalysisFailureContract(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*AnalysisResult)
		wantErr string
	}{
		{
			name: "reason",
			change: func(result *AnalysisResult) {
				result.Failed.Error.Reason = "invalid_source"
			},
			wantErr: "unsupported",
		},
		{
			name: "blank message",
			change: func(result *AnalysisResult) {
				result.Failed.Error.Message = " \n "
			},
			wantErr: "nonblank UTF-8",
		},
		{
			name: "message bound",
			change: func(result *AnalysisResult) {
				result.Failed.Error.Message =
					strings.Repeat("x", maxBuildFailureMessageBytes+1)
			},
			wantErr: "nonblank UTF-8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testFailedAnalysisResult()
			test.change(&result)
			err := ValidateAnalysisResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func testWorkspaceAnalysisResult(t *testing.T) AnalysisResult {
	t.Helper()
	plan := testBuildPlan()
	plan.Definitions = []DefinitionInput{plan.Definitions[3]}
	plan.Queues = []QueueInput{}
	raw, err := CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return AnalysisResult{
		FormatVersion: AnalysisResultFormatVersion,
		Outcome:       AnalysisOutcomeSucceeded,
		Succeeded: &AnalysisSucceeded{Files: []AnalysisFile{{
			Path:    AnalysisBuildPlanPath,
			Content: string(raw),
		}}},
	}
}

func testProgramAnalysisResult(t *testing.T) AnalysisResult {
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
	return AnalysisResult{
		FormatVersion: AnalysisResultFormatVersion,
		Outcome:       AnalysisOutcomeSucceeded,
		Succeeded: &AnalysisSucceeded{Files: []AnalysisFile{
			{Path: AnalysisBuildPlanPath, Content: string(planRaw)},
			{Path: AnalysisDeclarationsPath, Content: string(locatorRaw)},
			{Path: AnalysisProgramEntryPath, Content: ProgramEntry},
		}},
	}
}

func testFailedAnalysisResult() AnalysisResult {
	return AnalysisResult{
		FormatVersion: AnalysisResultFormatVersion,
		Outcome:       AnalysisOutcomeFailed,
		Failed: &AnalysisFailed{Error: AnalysisError{
			Reason:  AnalysisFailureReason,
			Message: "declaration analysis failed",
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
			{
				Kind:       DeclarationKindRunStream,
				DeclaredID: "events",
				ModulePath: "src/events.ts",
				ExportName: "events",
			},
		},
	}
}

func mutateAnalysisResultJSON(
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
