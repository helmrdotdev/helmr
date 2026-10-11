package builder

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestVerificationResultCanonicalRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		result VerificationResult
	}{
		{name: "computer only", result: testComputerVerificationResult(t)},
		{name: "program backed", result: testProgramVerificationResult(t)},
		{name: "failed", result: testFailedVerificationResult()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := canonicalVerificationResult(test.result)
			if err != nil {
				t.Fatalf("canonicalVerificationResult: %v", err)
			}
			parsed, err := parseVerificationResult(raw)
			if err != nil {
				t.Fatalf("parseVerificationResult: %v", err)
			}
			recoded, err := canonicalVerificationResult(parsed)
			if err != nil {
				t.Fatalf("canonicalVerificationResult(parsed): %v", err)
			}
			if string(recoded) != string(raw) {
				t.Fatalf("canonical bytes changed:\n%s\n%s", raw, recoded)
			}
		})
	}
}

func TestReadVerificationResultFrameRequiresExactlyOneFrame(t *testing.T) {
	raw, err := canonicalVerificationResult(testProgramVerificationResult(t))
	if err != nil {
		t.Fatal(err)
	}
	var frame bytes.Buffer
	if err := frameio.WriteMessageFrame(&frame, raw); err != nil {
		t.Fatal(err)
	}
	result, err := readVerificationResultFrame(&frame)
	if err != nil {
		t.Fatalf("readVerificationResultFrame: %v", err)
	}
	if result.Outcome != VerificationOutcomeSucceeded {
		t.Fatalf("outcome = %q", result.Outcome)
	}

	frame.Reset()
	if err := frameio.WriteMessageFrame(&frame, raw); err != nil {
		t.Fatal(err)
	}
	frame.WriteByte(0)
	if _, err := readVerificationResultFrame(&frame); err == nil ||
		!strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing frame error = %v", err)
	}
}

func TestParseVerificationResultRejectsOpenOrNoncanonicalShape(t *testing.T) {
	valid, err := canonicalVerificationResult(testProgramVerificationResult(t))
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
				files[1].(map[string]any)["path"] = verificationBuildPlanPath
			},
			wantErr: "requires exactly",
		},
		{
			name: "out of order file",
			mutate: func(root map[string]any) {
				files := root["files"].([]any)
				files[0], files[1] = files[1], files[0]
			},
			wantErr: "requires exactly",
		},
		{
			name: "partial program result",
			mutate: func(root map[string]any) {
				root["files"] = root["files"].([]any)[:1]
			},
			wantErr: "requires exactly",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := mutateVerificationResultJSON(t, valid, test.mutate)
			_, err := parseVerificationResult(raw)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}

	if _, err := parseVerificationResult(append([]byte(" "), valid...)); err == nil ||
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
			wantErr: "Definition index",
		},
		{
			name: "declaration identity mismatch",
			change: func(result *VerificationResult) {
				locator := artifacttest.DefinitionIndex()
				locator.Agents[0].ID = "different"
				raw, err := artifact.CanonicalDefinitionIndex(locator)
				if err != nil {
					panic(err)
				}
				result.Succeeded.Files[1].Content = string(raw)
			},
			wantErr: "does not match program definition",
		},
		{
			name: "program entry mismatch",
			change: func(result *VerificationResult) {
				result.Succeeded.Files = append(result.Succeeded.Files, VerificationFile{Path: "helmr/entry.mjs", Content: "obsolete"})
			},
			wantErr: "requires exactly",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testProgramVerificationResult(t)
			test.change(&result)
			err := validateVerificationResult(result)
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
					strings.Repeat("x", maxVerificationFailureMessageBytes+1)
			},
			wantErr: "nonblank UTF-8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testFailedVerificationResult()
			test.change(&result)
			err := validateVerificationResult(result)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func testComputerVerificationResult(t *testing.T) VerificationResult {
	t.Helper()
	plan := artifacttest.BuildPlan()
	plan.Definitions = []definition.Input{plan.Definitions[2]}
	planRaw, err := definition.CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	index := artifacttest.DefinitionIndex()
	index.Agents = []artifact.AgentBundleEntry{}
	index.Computers[0].ThroughAgent = false
	index.Computers[0].ExportName = "repo"
	indexRaw, err := artifact.CanonicalDefinitionIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	return VerificationResult{FormatVersion: verificationResultFormatVersion, Outcome: VerificationOutcomeSucceeded, Succeeded: &VerificationSucceeded{
		Files: []VerificationFile{{Path: verificationBuildPlanPath, Content: string(planRaw)}, {Path: verificationDefinitionIndexPath, Content: string(indexRaw)}},
	}}
}

func testProgramVerificationResult(t *testing.T) VerificationResult {
	t.Helper()
	plan := artifacttest.BuildPlan()
	planRaw, err := definition.CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	locatorRaw, err := artifact.CanonicalDefinitionIndex(
		artifacttest.DefinitionIndex(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return VerificationResult{
		FormatVersion: verificationResultFormatVersion,
		Outcome:       VerificationOutcomeSucceeded,
		Succeeded: &VerificationSucceeded{
			Files: []VerificationFile{
				{Path: verificationBuildPlanPath, Content: string(planRaw)},
				{Path: verificationDefinitionIndexPath, Content: string(locatorRaw)},
			},
		},
	}
}

func testFailedVerificationResult() VerificationResult {
	return VerificationResult{
		FormatVersion: verificationResultFormatVersion,
		Outcome:       VerificationOutcomeFailed,
		Failed: &VerificationFailed{Error: VerificationError{
			Reason:  verificationFailureReason,
			Message: "declaration verification failed",
		}},
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

func TestCompilerVerificationFrameCrossContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/verification.frame")
	if err != nil {
		t.Fatal(err)
	}
	result, err := readVerificationResultFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := definition.ParseBuildPlan(result.BuildPlan())
	if err != nil {
		t.Fatal(err)
	}
	index, err := artifact.ParseDefinitionIndex(result.DefinitionIndex())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Definitions) != 2 || len(index.Agents) != 1 || index.Agents[0].ID != "coder" || len(index.Computers) != 1 || !index.Computers[0].ThroughAgent {
		t.Fatal("compiler contract differs")
	}
	var regenerated bytes.Buffer
	canonical, err := canonicalVerificationResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := frameio.WriteMessageFrame(&regenerated, canonical); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, regenerated.Bytes()) {
		t.Fatal("compiler frame changed during Go round trip")
	}
}
