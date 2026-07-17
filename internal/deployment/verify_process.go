package deployment

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	programVerifierCodeFD       = 3
	programVerifierDependencyFD = 4
	programVerifierResultFD     = 5
	programVerifierFrameBytes   = 4
	programVerifierDeadline     = 5 * time.Minute
)

type programVerifierProcessConfig struct {
	executable     string
	arguments      []string
	unitCgroupRoot string
	leaseIdentity  string
	code           *os.File
	dependencies   *os.File
}

func writeProgramVerifierResult(writer io.Writer, result []byte) error {
	if _, err := ParseProgramIndex(result); err != nil {
		return fmt.Errorf("program verifier result: %w", err)
	}
	return writeProgramVerifierFrame(writer, result)
}

func readProgramVerifierResult(reader io.Reader) ([]byte, error) {
	result, err := readProgramVerifierFrame(reader)
	if err != nil {
		return nil, err
	}
	if _, err := ParseProgramIndex(result); err != nil {
		return nil, fmt.Errorf("program verifier result: %w", err)
	}
	return result, nil
}

func writeProgramVerifierFrame(writer io.Writer, result []byte) error {
	if len(result) == 0 || int64(len(result)) > maxProgramFileSizeBytes {
		return fmt.Errorf("program verifier result size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	var prefix [programVerifierFrameBytes]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(result)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return fmt.Errorf("write program verifier result length: %w", err)
	}
	if err := writeAll(writer, result); err != nil {
		return fmt.Errorf("write program verifier result: %w", err)
	}
	return nil
}

func readProgramVerifierFrame(reader io.Reader) ([]byte, error) {
	var prefix [programVerifierFrameBytes]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, fmt.Errorf("read program verifier result length: %w", err)
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size == 0 || int64(size) > maxProgramFileSizeBytes {
		return nil, fmt.Errorf("program verifier result size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	result := make([]byte, int(size))
	if _, err := io.ReadFull(reader, result); err != nil {
		return nil, fmt.Errorf("read program verifier result: %w", err)
	}
	var trailing [1]byte
	if count, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("close program verifier result: %w", err)
		}
		if count != 0 {
			return nil, errors.New("program verifier result has trailing output")
		}
		return nil, errors.New("program verifier result did not close")
	}
	return result, nil
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
