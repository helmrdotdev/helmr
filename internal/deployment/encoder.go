package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const maxEncoderDiagnosticBytes = 1 << 20

var encoderArguments = []string{
	"-",
	"/proc/self/fd/3",
	"-tar",
	"-noappend",
	"-all-root",
	"-no-xattrs",
	"-no-exports",
	"-no-fragments",
	"-no-tailends",
	"-no-duplicates",
	"-no-hardlinks",
	"-no-progress",
	"-exit-on-error",
	"-processors",
	"2",
	"-mem",
	"1024M",
	"-comp",
	"zstd",
	"-b",
	"131072",
	"-root-mode",
	"0755",
	"-mkfs-time",
	"0",
	"-all-time",
	"0",
}

func encodeProgramArchive(
	ctx context.Context,
	executable string,
	source io.Reader,
	destination *os.File,
) error {
	if ctx == nil {
		return errors.New("program encoder context is nil")
	}
	if source == nil {
		return errors.New("program encoder source is nil")
	}
	if destination == nil {
		return errors.New("program encoder destination is nil")
	}
	if err := validateProgramEncoder(executable); err != nil {
		return err
	}
	if err := verifyProgramEncoder(ctx, executable); err != nil {
		return err
	}
	info, err := destination.Stat()
	if err != nil {
		return fmt.Errorf("inspect program encoder destination: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
		return fmt.Errorf("program encoder destination is not an empty regular mode 0600 file")
	}

	stdout := &limitedEncoderOutput{remaining: maxEncoderDiagnosticBytes}
	stderr := &limitedEncoderOutput{remaining: maxEncoderDiagnosticBytes}
	command := exec.CommandContext(ctx, executable, encoderArguments...)
	command.Env = []string{"LC_ALL=C", "TZ=UTC"}
	command.Stdin = source
	command.Stdout = stdout
	command.Stderr = stderr
	command.ExtraFiles = []*os.File{destination}
	if err := command.Run(); err != nil {
		return fmt.Errorf(
			"encode Program SquashFS: %w%s",
			err,
			encoderDiagnostic(stdout, stderr),
		)
	}
	if stdout.exceeded || stderr.exceeded {
		return errors.New("program encoder diagnostic exceeds 1048576 bytes")
	}
	return nil
}

func validateProgramEncoder(executable string) error {
	if executable == "" || !filepath.IsAbs(executable) ||
		filepath.Clean(executable) != executable {
		return errors.New("program encoder executable must be an absolute clean path")
	}
	info, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("inspect program encoder executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("program encoder executable is not an executable regular file")
	}
	return nil
}

func verifyProgramEncoder(ctx context.Context, executable string) error {
	stdout := &limitedEncoderOutput{remaining: maxEncoderDiagnosticBytes}
	stderr := &limitedEncoderOutput{remaining: maxEncoderDiagnosticBytes}
	command := exec.CommandContext(ctx, executable, "-version")
	command.Env = []string{"LC_ALL=C", "TZ=UTC"}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf(
			"probe program encoder: %w%s",
			err,
			encoderDiagnostic(stdout, stderr),
		)
	}
	if stdout.exceeded || stderr.exceeded {
		return errors.New("program encoder version output exceeds 1048576 bytes")
	}
	output := stdout.String() + stderr.String()
	first, _, _ := strings.Cut(output, "\n")
	if !strings.HasPrefix(first, "mksquashfs version 4.6.1 ") {
		return fmt.Errorf("program encoder version output = %q", first)
	}
	return nil
}

type limitedEncoderOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (output *limitedEncoderOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(value) > output.remaining {
		value = value[:output.remaining]
		output.exceeded = true
	}
	count, err := output.buffer.Write(value)
	output.remaining -= count
	if err != nil {
		return count, err
	}
	if output.exceeded {
		return count, errors.New("program encoder diagnostic limit exceeded")
	}
	return count, nil
}

func (output *limitedEncoderOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

func encoderDiagnostic(stdout, stderr *limitedEncoderOutput) string {
	value := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if value == "" {
		return ""
	}
	return ": " + value
}
