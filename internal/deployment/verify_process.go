package deployment

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	verifierResultFD           = 3
	verifierArtifactBaseFD     = 4
	verifierHeaderBytes        = 5
	verifierDiagnosticMaxBytes = 65536
	verifierStderrMaxBytes     = 4096
	verifierBootstrapDeadline  = 10 * time.Second
	verifierDeadline           = 5 * time.Minute
)

type verifierBootstrapError struct {
	diagnostic string
}

func (err *verifierBootstrapError) Error() string {
	return "artifact verifier bootstrap failed"
}

func (err *verifierBootstrapError) FatalWorker() bool { return true }

func (err *verifierBootstrapError) LocalDiagnostic() string {
	return err.diagnostic
}

type boundedVerifierStderr struct {
	buffer    bytes.Buffer
	truncated bool
}

func (writer *boundedVerifierStderr) Write(value []byte) (int, error) {
	remaining := verifierStderrMaxBytes - writer.buffer.Len()
	if remaining > 0 {
		copied := len(value)
		if copied > remaining {
			copied = remaining
		}
		_, _ = writer.buffer.Write(value[:copied])
	}
	if len(value) > remaining {
		writer.truncated = true
	}
	return len(value), nil
}

func (writer *boundedVerifierStderr) Diagnostic() string {
	diagnostic := string(bytes.ToValidUTF8(writer.buffer.Bytes(), []byte("?")))
	diagnostic = strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return '?'
		}
		return value
	}, diagnostic)
	if writer.truncated {
		diagnostic += " [truncated]"
	}
	return diagnostic
}

func newVerifierBootstrapError(diagnostic string) error {
	return &verifierBootstrapError{diagnostic: sanitizeVerifierLocalDiagnostic(diagnostic)}
}

func sanitizeVerifierLocalDiagnostic(diagnostic string) string {
	writer := &boundedVerifierStderr{}
	_, _ = writer.Write([]byte(diagnostic))
	return writer.Diagnostic()
}

// VerifierChildLocalDiagnostic bounds and sanitizes a verifier child bootstrap
// cause for the host-local stderr channel. It must not be sent through the
// Worker API.
func VerifierChildLocalDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeVerifierLocalDiagnostic(err.Error())
}

// VerifierLocalDiagnostic returns bounded host-only bootstrap diagnostics.
// Callers must log it locally and must not include it in Worker API payloads.
func VerifierLocalDiagnostic(err error) (string, bool) {
	var diagnostic interface{ LocalDiagnostic() string }
	if !errors.As(err, &diagnostic) {
		return "", false
	}
	return diagnostic.LocalDiagnostic(), true
}

type verifierRecordKind uint8

const (
	verifierReady verifierRecordKind = iota + 1
	verifierVerified
	verifierInvalid
	verifierFailed
)

type verifierProcessResult struct {
	kind       verifierRecordKind
	payload    []byte
	diagnostic string
}

type verifierProcessConfig struct {
	job            verifierJob
	unitCgroupRoot string
	leaseIdentity  string
	artifacts      []*os.File
}

func writeVerifierReady(writer io.Writer) error {
	return writeVerifierRecord(writer, verifierReady, nil)
}

func writeVerifierVerified(writer io.Writer, job verifierJob, payload []byte) error {
	limit := job.verifiedPayloadLimit()
	if len(payload) == 0 || int64(len(payload)) > limit {
		return fmt.Errorf("%s verifier result size is outside [1,%d]", job, limit)
	}
	return writeVerifierRecord(writer, verifierVerified, payload)
}

func writeVerifierInvalid(writer io.Writer, diagnostic string) error {
	if err := validateVerifierDiagnostic(diagnostic); err != nil {
		return fmt.Errorf("artifact invalid diagnostic: %w", err)
	}
	return writeVerifierRecord(writer, verifierInvalid, []byte(diagnostic))
}

func writeVerifierFailed(writer io.Writer, diagnostic string) error {
	if err := validateVerifierDiagnostic(diagnostic); err != nil {
		return fmt.Errorf("artifact verifier failure diagnostic: %w", err)
	}
	return writeVerifierRecord(writer, verifierFailed, []byte(diagnostic))
}

func writeVerifierRecord(
	writer io.Writer,
	kind verifierRecordKind,
	payload []byte,
) error {
	switch kind {
	case verifierReady:
		if len(payload) != 0 {
			return errors.New("artifact verifier READY payload is not empty")
		}
	case verifierVerified, verifierInvalid, verifierFailed:
		if len(payload) == 0 {
			return errors.New("artifact verifier terminal payload is empty")
		}
	default:
		return fmt.Errorf("artifact verifier record kind = %#x", kind)
	}
	var header [verifierHeaderBytes]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeAll(writer, header[:]); err != nil {
		return fmt.Errorf("write artifact verifier record header: %w", err)
	}
	if err := writeAll(writer, payload); err != nil {
		return fmt.Errorf("write artifact verifier record payload: %w", err)
	}
	return nil
}

func readVerifierReady(reader io.Reader) error {
	kind, size, err := readVerifierRecordHeader(reader)
	if err != nil {
		return err
	}
	if kind != verifierReady || size != 0 {
		return errors.New("artifact verifier result does not begin with empty READY")
	}
	return nil
}

func readVerifierTerminal(reader io.Reader, job verifierJob) (verifierProcessResult, error) {
	kind, size, err := readVerifierRecordHeader(reader)
	if err != nil {
		return verifierProcessResult{}, err
	}
	var limit int64
	switch kind {
	case verifierVerified:
		limit = job.verifiedPayloadLimit()
	case verifierInvalid, verifierFailed:
		limit = verifierDiagnosticMaxBytes
	default:
		return verifierProcessResult{}, fmt.Errorf("artifact verifier terminal kind = %#x", kind)
	}
	if limit < 1 {
		return verifierProcessResult{}, fmt.Errorf("artifact verifier job = %q", job)
	}
	if size == 0 || int64(size) > limit {
		return verifierProcessResult{}, fmt.Errorf(
			"artifact verifier terminal size is outside [1,%d]",
			limit,
		)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return verifierProcessResult{}, fmt.Errorf("read artifact verifier terminal payload: %w", err)
	}
	var trailing [1]byte
	if count, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
		if err != nil {
			return verifierProcessResult{}, fmt.Errorf("close artifact verifier result: %w", err)
		}
		if count != 0 {
			return verifierProcessResult{}, errors.New("artifact verifier result has trailing output")
		}
		return verifierProcessResult{}, errors.New("artifact verifier result did not close")
	}
	result := verifierProcessResult{kind: kind}
	switch kind {
	case verifierVerified:
		result.payload = payload
	case verifierInvalid, verifierFailed:
		result.diagnostic = string(payload)
		if err := validateVerifierDiagnostic(result.diagnostic); err != nil {
			return verifierProcessResult{}, fmt.Errorf("artifact verifier diagnostic: %w", err)
		}
	}
	return result, nil
}

func readVerifierRecordHeader(reader io.Reader) (verifierRecordKind, uint32, error) {
	var header [verifierHeaderBytes]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, 0, fmt.Errorf("read artifact verifier record header: %w", err)
	}
	return verifierRecordKind(header[0]), binary.BigEndian.Uint32(header[1:]), nil
}

func validateVerifierDiagnostic(diagnostic string) error {
	if len(diagnostic) == 0 || len(diagnostic) > verifierDiagnosticMaxBytes {
		return fmt.Errorf("size is outside [1,%d]", verifierDiagnosticMaxBytes)
	}
	if !utf8.ValidString(diagnostic) {
		return errors.New("value is not valid UTF-8")
	}
	for _, value := range diagnostic {
		if unicode.IsControl(value) {
			return errors.New("value contains a control character")
		}
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
