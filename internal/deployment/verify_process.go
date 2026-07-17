package deployment

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	programVerifierCodeFD             = 3
	programVerifierDependencyFD       = 4
	programVerifierResultFD           = 5
	programVerifierHeaderBytes        = 5
	programVerifierDiagnosticMaxBytes = 65536
	programVerifierBootstrapDeadline  = 10 * time.Second
	programVerifierDeadline           = 5 * time.Minute
)

type programVerifierRecordKind uint8

const (
	programVerifierReady programVerifierRecordKind = iota + 1
	programVerifierVerified
	programVerifierInvalid
	programVerifierFailed
)

type programVerifierProcessResult struct {
	kind       programVerifierRecordKind
	index      []byte
	diagnostic string
}

type programVerifierProcessConfig struct {
	executable     string
	arguments      []string
	unitCgroupRoot string
	leaseIdentity  string
	code           *os.File
	dependencies   *os.File
}

func writeProgramVerifierReady(writer io.Writer) error {
	return writeProgramVerifierRecord(writer, programVerifierReady, nil)
}

func writeProgramVerifierVerified(writer io.Writer, index []byte) error {
	if _, err := ParseProgramIndex(index); err != nil {
		return fmt.Errorf("program verifier result: %w", err)
	}
	return writeProgramVerifierRecord(writer, programVerifierVerified, index)
}

func writeProgramVerifierInvalid(writer io.Writer, diagnostic string) error {
	if err := validateProgramVerifierDiagnostic(diagnostic); err != nil {
		return fmt.Errorf("program invalid diagnostic: %w", err)
	}
	return writeProgramVerifierRecord(writer, programVerifierInvalid, []byte(diagnostic))
}

func writeProgramVerifierFailed(writer io.Writer, diagnostic string) error {
	if err := validateProgramVerifierDiagnostic(diagnostic); err != nil {
		return fmt.Errorf("program verifier failure diagnostic: %w", err)
	}
	return writeProgramVerifierRecord(writer, programVerifierFailed, []byte(diagnostic))
}

func writeProgramVerifierRecord(
	writer io.Writer,
	kind programVerifierRecordKind,
	payload []byte,
) error {
	switch kind {
	case programVerifierReady:
		if len(payload) != 0 {
			return errors.New("program verifier READY payload is not empty")
		}
	case programVerifierVerified, programVerifierInvalid, programVerifierFailed:
		if len(payload) == 0 {
			return errors.New("program verifier terminal payload is empty")
		}
	default:
		return fmt.Errorf("program verifier record kind = %#x", kind)
	}
	var header [programVerifierHeaderBytes]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeAll(writer, header[:]); err != nil {
		return fmt.Errorf("write program verifier record header: %w", err)
	}
	if err := writeAll(writer, payload); err != nil {
		return fmt.Errorf("write program verifier record payload: %w", err)
	}
	return nil
}

func readProgramVerifierReady(reader io.Reader) error {
	kind, size, err := readProgramVerifierRecordHeader(reader)
	if err != nil {
		return err
	}
	if kind != programVerifierReady || size != 0 {
		return errors.New("program verifier result does not begin with empty READY")
	}
	return nil
}

func readProgramVerifierTerminal(reader io.Reader) (programVerifierProcessResult, error) {
	kind, size, err := readProgramVerifierRecordHeader(reader)
	if err != nil {
		return programVerifierProcessResult{}, err
	}
	var limit int64
	switch kind {
	case programVerifierVerified:
		limit = maxProgramFileSizeBytes
	case programVerifierInvalid, programVerifierFailed:
		limit = programVerifierDiagnosticMaxBytes
	default:
		return programVerifierProcessResult{}, fmt.Errorf("program verifier terminal kind = %#x", kind)
	}
	if size == 0 || int64(size) > limit {
		return programVerifierProcessResult{}, fmt.Errorf(
			"program verifier terminal size is outside [1,%d]",
			limit,
		)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return programVerifierProcessResult{}, fmt.Errorf("read program verifier terminal payload: %w", err)
	}
	var trailing [1]byte
	if count, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
		if err != nil {
			return programVerifierProcessResult{}, fmt.Errorf("close program verifier result: %w", err)
		}
		if count != 0 {
			return programVerifierProcessResult{}, errors.New("program verifier result has trailing output")
		}
		return programVerifierProcessResult{}, errors.New("program verifier result did not close")
	}
	result := programVerifierProcessResult{kind: kind}
	switch kind {
	case programVerifierVerified:
		if _, err := ParseProgramIndex(payload); err != nil {
			return programVerifierProcessResult{}, fmt.Errorf("program verifier result: %w", err)
		}
		result.index = payload
	case programVerifierInvalid, programVerifierFailed:
		result.diagnostic = string(payload)
		if err := validateProgramVerifierDiagnostic(result.diagnostic); err != nil {
			return programVerifierProcessResult{}, fmt.Errorf("program verifier diagnostic: %w", err)
		}
	}
	return result, nil
}

func readProgramVerifierResult(reader io.Reader) (programVerifierProcessResult, error) {
	if err := readProgramVerifierReady(reader); err != nil {
		return programVerifierProcessResult{}, err
	}
	return readProgramVerifierTerminal(reader)
}

func readProgramVerifierRecordHeader(reader io.Reader) (programVerifierRecordKind, uint32, error) {
	var header [programVerifierHeaderBytes]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, 0, fmt.Errorf("read program verifier record header: %w", err)
	}
	return programVerifierRecordKind(header[0]), binary.BigEndian.Uint32(header[1:]), nil
}

func validateProgramVerifierDiagnostic(diagnostic string) error {
	if len(diagnostic) == 0 || len(diagnostic) > programVerifierDiagnosticMaxBytes {
		return fmt.Errorf("size is outside [1,%d]", programVerifierDiagnosticMaxBytes)
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
