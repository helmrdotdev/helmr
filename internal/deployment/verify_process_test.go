package deployment

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifierResultRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name    string
		job     verifierJob
		payload []byte
	}{
		{
			name:    "program",
			job:     programVerifierJob,
			payload: canonicalVerifierProgramVerification(t),
		},
		{
			name:    "runtime",
			job:     runtimeVerifierJob,
			payload: canonicalVerifierRuntimeIndex(t),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeVerifierReady(&output); err != nil {
				t.Fatal(err)
			}
			if err := writeVerifierVerified(&output, test.job, test.payload); err != nil {
				t.Fatal(err)
			}
			result, err := readVerifierResultForTest(bytes.NewReader(output.Bytes()), test.job)
			if err != nil {
				t.Fatal(err)
			}
			if result.kind != verifierVerified || !bytes.Equal(result.payload, test.payload) {
				t.Fatalf("result = %#v", result)
			}
		})
	}

	for _, test := range []struct {
		name       string
		write      func(*bytes.Buffer) error
		kind       verifierRecordKind
		diagnostic string
	}{
		{
			name: "invalid",
			write: func(output *bytes.Buffer) error {
				return writeVerifierInvalid(output, "program index is missing")
			},
			kind:       verifierInvalid,
			diagnostic: "program index is missing",
		},
		{
			name: "failed",
			write: func(output *bytes.Buffer) error {
				return writeVerifierFailed(output, "artifact read failed")
			},
			kind:       verifierFailed,
			diagnostic: "artifact read failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeVerifierReady(&output); err != nil {
				t.Fatal(err)
			}
			if err := test.write(&output); err != nil {
				t.Fatal(err)
			}
			result, err := readVerifierResultForTest(bytes.NewReader(output.Bytes()), programVerifierJob)
			if err != nil {
				t.Fatal(err)
			}
			if result.kind != test.kind || result.diagnostic != test.diagnostic {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestVerifierFramingLeavesVerifiedPayloadOpaque(t *testing.T) {
	payload := []byte(`{"opaque":true}`)
	var output bytes.Buffer
	if err := writeVerifierReady(&output); err != nil {
		t.Fatal(err)
	}
	if err := writeVerifierVerified(&output, runtimeVerifierJob, payload); err != nil {
		t.Fatal(err)
	}
	result, err := readVerifierResultForTest(bytes.NewReader(output.Bytes()), runtimeVerifierJob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.payload, payload) {
		t.Fatalf("payload = %q", result.payload)
	}
}

func TestVerifierResultRejectsMalformedOutput(t *testing.T) {
	canonical := canonicalVerifierProgramVerification(t)
	valid := func() []byte {
		var output bytes.Buffer
		if err := writeVerifierReady(&output); err != nil {
			t.Fatal(err)
		}
		if err := writeVerifierVerified(&output, programVerifierJob, canonical); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}()
	ready := verifierRecordBytes(verifierReady, nil)
	tests := map[string][]byte{
		"empty":             nil,
		"unknown readiness": verifierRecordBytes(0xff, nil),
		"readiness payload": verifierRecordBytes(verifierReady, []byte("x")),
		"missing terminal":  ready,
		"unknown terminal":  append(ready, verifierRecordBytes(0xff, []byte("x"))...),
		"empty terminal":    append(ready, verifierRecordBytes(verifierVerified, nil)...),
		"truncated header":  valid[:verifierHeaderBytes+2],
		"truncated payload": valid[:len(valid)-1],
		"trailing":          append(append([]byte(nil), valid...), 'x'),
		"duplicate terminal": append(
			append([]byte(nil), valid...),
			verifierRecordBytes(verifierVerified, canonical)...,
		),
		"verified before ready": verifierRecordBytes(verifierVerified, canonical),
	}
	var oversized [verifierHeaderBytes]byte
	oversized[0] = byte(verifierVerified)
	binary.BigEndian.PutUint32(oversized[1:], uint32(maxRuntimeDocumentBytes+1))
	tests["runtime verified bound"] = append(
		append([]byte(nil), ready...),
		oversized[:]...,
	)
	var oversizedDiagnostic [verifierHeaderBytes]byte
	oversizedDiagnostic[0] = byte(verifierInvalid)
	binary.BigEndian.PutUint32(
		oversizedDiagnostic[1:],
		uint32(verifierDiagnosticMaxBytes+1),
	)
	tests["oversized diagnostic"] = append(
		append([]byte(nil), ready...),
		oversizedDiagnostic[:]...,
	)
	tests["invalid UTF-8 diagnostic"] = append(
		append([]byte(nil), ready...),
		verifierRecordBytes(verifierInvalid, []byte{0xff})...,
	)
	tests["control character diagnostic"] = append(
		append([]byte(nil), ready...),
		verifierRecordBytes(verifierInvalid, []byte("first\nsecond"))...,
	)
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			job := programVerifierJob
			if name == "runtime verified bound" {
				job = runtimeVerifierJob
			}
			if _, err := readVerifierResultForTest(bytes.NewReader(input), job); err == nil {
				t.Fatal("malformed result was accepted")
			}
		})
	}
}

func readVerifierResultForTest(reader *bytes.Reader, job verifierJob) (verifierProcessResult, error) {
	if err := readVerifierReady(reader); err != nil {
		return verifierProcessResult{}, err
	}
	return readVerifierTerminal(reader, job)
}

func TestVerifierResultValidatesPayloadBoundsAndDiagnostics(t *testing.T) {
	var output bytes.Buffer
	if err := writeVerifierVerified(&output, runtimeVerifierJob, nil); err == nil {
		t.Fatal("empty verified payload was accepted")
	}
	if err := writeVerifierVerified(
		&output,
		runtimeVerifierJob,
		make([]byte, maxRuntimeDocumentBytes+1),
	); err == nil {
		t.Fatal("oversized Runtime payload was accepted")
	}
	for name, diagnostic := range map[string]string{
		"empty":       "",
		"control":     "first\nsecond",
		"invalidUTF8": string([]byte{0xff}),
		"oversized":   strings.Repeat("x", verifierDiagnosticMaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeVerifierInvalid(&output, diagnostic); err == nil {
				t.Fatal("invalid diagnostic was accepted")
			}
		})
	}
}

func verifierRecordBytes(kind verifierRecordKind, payload []byte) []byte {
	var output bytes.Buffer
	var header [verifierHeaderBytes]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	output.Write(header[:])
	output.Write(payload)
	return output.Bytes()
}

func canonicalVerifierProgramIndex(t *testing.T) []byte {
	t.Helper()
	canonical, err := CanonicalProgramIndex(ProgramIndex{
		Architecture:       ArchitectureX8664,
		ConfigResultDigest: "sha256:" + strings.Repeat("8", 64),
		Declarations: []ProgramIndexDeclaration{{
			Kind:       DefinitionKindTask,
			DeclaredID: "verify",
			Task: &TaskManifest{
				Payload: SchemaManifest{Kind: SchemaKindNone},
				Run: RunManifest{
					Queue:         "task/verify",
					MaxDurationMs: 900000,
					Retry:         RetryManifest{Enabled: false},
				},
			},
			Locator: &ProgramLocator{
				ExportName: "verify",
				ModulePath: generatedTestModule("a"),
				Slot:       DeclarationSlotHandler,
			},
		}},
		Queues: []QueueInput{{
			Name: "task/verify",
		}},
		RuntimeContract: RuntimeContract,
		RuntimeDigest:   "sha256:" + strings.Repeat("f", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func canonicalVerifierProgramVerification(t *testing.T) []byte {
	t.Helper()
	var index ProgramIndex
	if err := json.Unmarshal(canonicalVerifierProgramIndex(t), &index); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalProgramVerification(programVerification{
		FormatVersion: programVerificationVersion,
		Index:         index,
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func canonicalVerifierRuntimeIndex(t *testing.T) []byte {
	t.Helper()
	canonical, err := CanonicalRuntimeIndex(RuntimeIndex{
		Architecture:    ArchitectureX8664,
		RuntimeContract: RuntimeContract,
	})
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
