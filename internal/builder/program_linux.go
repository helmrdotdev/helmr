//go:build linux

package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

const (
	compilerDocumentLimit      = 16 << 20
	compilerResultChannelLimit = 70 << 20
	compilerOutputLimit        = 128 << 20
)

// ProgramInput is the manager-neutral boundary of the canonical builder. The
// caller has already materialized dependencies in ProjectDirectory inside a
// bounded BuildKit execution container. Package-manager identity, lockfile
// identity, install commands, and source provenance are intentionally absent.
type ProgramInput struct {
	ProjectDirectory string
	WorkDirectory    string
	NodePath         string
	ConfigPath       string
	BundlePath       string
	ProgramCompiler  string
	Compiler         artifact.CompilerInputs
	Runtime          artifact.RuntimeDescriptor
	RuntimeMetadata  artifact.RuntimeMetadata
}

type PreparedProgramInput struct {
	PreparedDirectory string
	WorkDirectory     string
	ProgramObjectPath string
	SquashFSEncoder   string
	Compiler          artifact.CompilerInputs
	Runtime           artifact.RuntimeDescriptor
	RuntimeMetadata   artifact.RuntimeMetadata
	ComputerImages    []bundle.ComputerImage
}

type ProgramResult struct {
	Program      artifact.ProgramOutput
	Config       artifact.BuildConfig
	Verification deployment.VerificationResult
	ObjectPath   string
}

// PrepareProgram executes the trusted compiler while tenant modules are still
// present, then publishes only its closed producer-private output. A later
// BuildKit stage assembles the Program without executing tenant code.
func PrepareProgram(
	ctx context.Context,
	input ProgramInput,
	output string,
) (returnErr error) {
	if ctx == nil {
		return errors.New("program preparation context is nil")
	}
	if err := validateProgramInput(input); err != nil {
		return err
	}
	if output == "" || !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return errors.New("prepared Program output must be an absolute clean path")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("prepared Program output already exists")
		}
		return fmt.Errorf("inspect prepared Program output: %w", err)
	}
	work, err := os.MkdirTemp(input.WorkDirectory, ".helmr-program-")
	if err != nil {
		return fmt.Errorf("create Program preparation directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(work)) }()
	stage, err := os.MkdirTemp(filepath.Dir(output), ".helmr-prepared-program-")
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(stage)) }()
	compilerOutput := filepath.Join(stage, "compiler-output")
	if err := os.Mkdir(compilerOutput, 0o700); err != nil {
		return fmt.Errorf("create compiler output: %w", err)
	}
	config, verification, err := analyzePayload(ctx, input, work, compilerOutput)
	if err != nil {
		return err
	}
	configRaw, err := artifact.CanonicalBuildConfig(config)
	if err != nil {
		return err
	}
	verificationRaw, err := deployment.CanonicalVerificationResult(verification)
	if err != nil {
		return err
	}
	if err := writeExclusiveFile(filepath.Join(stage, "config.json"), configRaw); err != nil {
		return err
	}
	if err := writeExclusiveFile(filepath.Join(stage, "verification.json"), verificationRaw); err != nil {
		return err
	}
	if err := copyPayload(input.ProjectDirectory, filepath.Join(stage, "payload")); err != nil {
		return err
	}
	planRaw := []byte(verification.Succeeded.Files[0].Content)
	if err := writeExclusiveFile(filepath.Join(stage, "build-plan.json"), planRaw); err != nil {
		return err
	}
	if _, _, err := readPreparedProgram(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, output); err != nil {
		return fmt.Errorf("publish prepared Program: %w", err)
	}
	stage = ""
	return nil
}

// BuildPreparedProgram assembles a Program from one installed-tree copy and
// closed preparation output. It never launches tenant config or task modules.
func BuildPreparedProgram(
	ctx context.Context,
	input PreparedProgramInput,
) (_ ProgramResult, returnErr error) {
	if ctx == nil {
		return ProgramResult{}, errors.New("prepared Program build context is nil")
	}
	if err := validatePreparedProgramInput(input); err != nil {
		return ProgramResult{}, err
	}
	work, err := os.MkdirTemp(input.WorkDirectory, ".helmr-program-")
	if err != nil {
		return ProgramResult{}, fmt.Errorf("create Program assembly directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(work)) }()
	config, verification, err := readPreparedProgram(input.PreparedDirectory)
	if err != nil {
		return ProgramResult{}, err
	}
	expectedRaw, err := readBoundedRegularFile(filepath.Join(input.PreparedDirectory, "compiler-output/helmr/compiler-result.json"), compilerDocumentLimit)
	if err != nil {
		return ProgramResult{}, err
	}
	expected, err := artifact.ParseProgramCompilerResult(expectedRaw)
	if err != nil {
		return ProgramResult{}, err
	}
	payload := filepath.Join(work, "payload")
	if err := copyPayload(filepath.Join(input.PreparedDirectory, "payload"), payload); err != nil {
		return ProgramResult{}, err
	}
	actual, err := artifact.ProgramPayloadDigest(ctx, payload)
	if err != nil {
		return ProgramResult{}, err
	}
	if actual != expected.PayloadDigest {
		return ProgramResult{}, errors.New("prepared Program input tree changed before finalization")
	}
	if err := ingestCompilerOutput(payload, filepath.Join(input.PreparedDirectory, "compiler-output")); err != nil {
		return ProgramResult{}, err
	}
	treeArchive, cleanupArchive, err := archive.CreateTarWithOptionsContext(ctx, payload, input.WorkDirectory, archive.TarOptions{
		CanonicalMetadata: true,
		MaxBytes:          artifact.MaxProgramLogicalBytes,
		MaxArchiveBytes:   deployment.MaxBuildTreeStreamBytes,
		MaxNameBytes:      artifact.MaxNameBytes,
		MaxFileBytes:      artifact.MaxFileSize,
		MaxEntries:        artifact.MaxProgramTreeEntries - 1, // SquashFS adds the root.
	})
	if err != nil {
		return ProgramResult{}, fmt.Errorf("freeze installed Program tree: %w", err)
	}
	defer cleanupArchive()
	archiveFile, err := os.Open(treeArchive.Path)
	if err != nil {
		return ProgramResult{}, fmt.Errorf("open installed Program tree: %w", err)
	}
	tree, ingestErr := deployment.IngestBuildTreeArchive(
		ctx,
		work,
		input.SquashFSEncoder,
		treeArchive.Digest,
		treeArchive.SizeBytes,
		archiveFile,
	)
	closeErr := archiveFile.Close()
	if err := errors.Join(ingestErr, closeErr); err != nil {
		return ProgramResult{}, fmt.Errorf("ingest installed Program tree: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, tree.Close()) }()

	configDigest, err := artifact.BuildConfigDigest(config)
	if err != nil {
		return ProgramResult{}, err
	}
	program, err := deployment.EncodeProgram(
		ctx,
		input.WorkDirectory,
		input.SquashFSEncoder,
		tree,
		verification,
		configDigest,
		input.Runtime.Digest,
		input.ComputerImages,
		input.Compiler,
		input.RuntimeMetadata.NodeVersion,
	)
	if err != nil {
		return ProgramResult{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, program.Close()) }()
	if err := deployment.ValidateVerifiedProgram(verification, program.Output.Index); err != nil {
		return ProgramResult{}, err
	}
	if err := program.Materialize(ctx, input.ProgramObjectPath); err != nil {
		return ProgramResult{}, err
	}
	return ProgramResult{
		Program:      program.Output,
		Config:       config,
		Verification: verification,
		ObjectPath:   input.ProgramObjectPath,
	}, nil
}

func analyzePayload(
	ctx context.Context,
	input ProgramInput,
	work string,
	compilerOutput string,
) (artifact.BuildConfig, deployment.VerificationResult, error) {
	inputDigest, err := artifact.ProgramPayloadDigest(ctx, input.ProjectDirectory)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	flags, err := artifact.NodeLanguageFlags(input.RuntimeMetadata.NodeVersion)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	// The config was evaluated once on the invoking host; this phase only reads
	// its resolved discovery settings and never imports helmr.config.ts.
	config, err := readResolvedConfig(input.ConfigPath)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	canonicalConfig, err := artifact.CanonicalBuildConfig(config)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	configPath := filepath.Join(work, "config.json")
	if err := os.WriteFile(configPath, canonicalConfig, 0o600); err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("write canonical Helmr config: %w", err)
	}
	verificationFrame, err := runAnalysisCommand(ctx, analysisCommand{
		NodePath: input.NodePath,
		Arguments: append(append([]string{}, flags...), input.ProgramCompiler,
			"--analyze", input.ProjectDirectory, configPath, input.RuntimeMetadata.NodeVersion, inputDigest, input.BundlePath, compilerOutput),
		Directory: filepath.Dir(input.ProjectDirectory), WorkDir: work,
	})
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("compile Helmr Program: %w", err)
	}
	verification, err := deployment.ReadVerificationResultFrame(bytes.NewReader(verificationFrame))
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	if verification.Outcome != deployment.VerificationOutcomeSucceeded {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf(
			"compile Helmr Program: %s", verification.Failed.Error.Message)
	}
	raw, err := readBoundedRegularFile(filepath.Join(compilerOutput, "helmr/compiler-result.json"), compilerDocumentLimit)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	result, err := artifact.ParseProgramCompilerResult(raw)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	after, err := artifact.ProgramPayloadDigest(ctx, input.ProjectDirectory)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	if after != inputDigest || result.PayloadDigest != inputDigest || result.Bundler != input.Compiler.Bundler || result.NodeVersion != input.RuntimeMetadata.NodeVersion {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, errors.New("compiled Program input or language authority changed during preparation")
	}
	return config, verification, nil
}

func validateProgramInput(input ProgramInput) error {
	for name, value := range map[string]string{
		"project directory": input.ProjectDirectory,
		"work directory":    input.WorkDirectory,
		"Node executable":   input.NodePath,
		"resolved config":   input.ConfigPath,
		"bundle manifest":   input.BundlePath,
		"Program Compiler":  input.ProgramCompiler,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be an absolute clean path", name)
		}
	}
	project, err := os.Stat(input.ProjectDirectory)
	if err != nil || !project.IsDir() {
		return errors.New("project directory is not a directory")
	}
	work, err := os.Stat(input.WorkDirectory)
	if err != nil || !work.IsDir() {
		return errors.New("work directory is not a directory")
	}
	if err := artifact.ValidateCompilerInputs(input.Compiler); err != nil {
		return err
	}
	if err := artifact.ValidateRuntimeDescriptor(input.Runtime); err != nil {
		return err
	}
	if err := artifact.ValidateRuntimeMetadata(input.RuntimeMetadata); err != nil {
		return err
	}
	if input.Runtime.Architecture != input.RuntimeMetadata.Architecture ||
		input.Runtime.RuntimeContract != input.RuntimeMetadata.RuntimeContract {
		return errors.New("runtime descriptor and metadata do not match")
	}
	return nil
}

func validatePreparedProgramInput(input PreparedProgramInput) error {
	for name, value := range map[string]string{
		"prepared Program directory": input.PreparedDirectory,

		"work directory":      input.WorkDirectory,
		"Program object path": input.ProgramObjectPath,
		"SquashFS encoder":    input.SquashFSEncoder,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be an absolute clean path", name)
		}
	}
	prepared, err := os.Stat(input.PreparedDirectory)
	if err != nil || !prepared.IsDir() {
		return errors.New("prepared Program directory is not a directory")
	}
	work, err := os.Stat(input.WorkDirectory)
	if err != nil || !work.IsDir() {
		return errors.New("work directory is not a directory")
	}
	if _, err := os.Lstat(input.ProgramObjectPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("program object path already exists")
		}
		return fmt.Errorf("inspect Program object path: %w", err)
	}
	if err := artifact.ValidateCompilerInputs(input.Compiler); err != nil {
		return err
	}
	if err := artifact.ValidateRuntimeDescriptor(input.Runtime); err != nil {
		return err
	}
	if err := artifact.ValidateRuntimeMetadata(input.RuntimeMetadata); err != nil {
		return err
	}
	if input.Runtime.Architecture != input.RuntimeMetadata.Architecture ||
		input.Runtime.RuntimeContract != input.RuntimeMetadata.RuntimeContract {
		return errors.New("runtime descriptor and metadata do not match")
	}
	return nil
}

func readPreparedProgram(directory string) (artifact.BuildConfig, deployment.VerificationResult, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("read prepared Program: %w", err)
	}
	want := map[string]bool{"compiler-output": false, "config.json": false, "verification.json": false, "payload": false, "build-plan.json": false}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("prepared Program contains unexpected path %q", entry.Name())
		}
		want[entry.Name()] = true
	}
	for name, present := range want {
		if !present {
			return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("prepared Program is missing %q", name)
		}
	}
	compilerOutput, err := os.Lstat(filepath.Join(directory, "compiler-output"))
	if err != nil || compilerOutput.Mode()&os.ModeSymlink != 0 || !compilerOutput.IsDir() {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, errors.New("prepared compiler output is not a directory")
	}
	configRaw, err := readBoundedRegularFile(filepath.Join(directory, "config.json"), compilerDocumentLimit)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("read prepared config: %w", err)
	}
	config, err := artifact.ParseBuildConfig(configRaw)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	verificationRaw, err := readBoundedRegularFile(filepath.Join(directory, "verification.json"), compilerResultChannelLimit)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, fmt.Errorf("read prepared verification: %w", err)
	}
	verification, err := deployment.ParseVerificationResult(verificationRaw)
	if err != nil {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, err
	}
	if verification.Outcome != deployment.VerificationOutcomeSucceeded || verification.Succeeded == nil || len(verification.Succeeded.Files) == 0 {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, errors.New("prepared verification did not succeed")
	}
	payload, err := os.Lstat(filepath.Join(directory, "payload"))
	if err != nil || !payload.IsDir() || payload.Mode()&os.ModeSymlink != 0 {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, errors.New("prepared payload is not a directory")
	}
	plan, err := readBoundedRegularFile(filepath.Join(directory, "build-plan.json"), compilerDocumentLimit)
	if err != nil || !bytes.Equal(plan, []byte(verification.Succeeded.Files[0].Content)) {
		return artifact.BuildConfig{}, deployment.VerificationResult{}, errors.New("prepared plan does not match analysis")
	}

	return config, verification, nil
}

func writeExclusiveFile(path string, body []byte) (returnErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	written, err := file.Write(body)
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

// readResolvedConfig reads the bounded canonical discovery config the CLI
// resolved on the host.
func readResolvedConfig(path string) (artifact.BuildConfig, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<20 {
		return artifact.BuildConfig{}, errors.New("resolved config is not a bounded regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return artifact.BuildConfig{}, fmt.Errorf("read resolved config: %w", err)
	}
	return artifact.ParseBuildConfig(raw)
}

type analysisCommand struct {
	NodePath  string
	Arguments []string
	Directory string
	WorkDir   string
}

func runAnalysisCommand(
	ctx context.Context,
	input analysisCommand,
) (_ []byte, returnErr error) {
	result, err := os.CreateTemp(input.WorkDir, ".helmr-result-")
	if err != nil {
		return nil, err
	}
	open := true
	defer func() {
		if open {
			returnErr = errors.Join(returnErr, result.Close())
		}
		returnErr = errors.Join(returnErr, os.Remove(result.Name()))
	}()
	command := exec.CommandContext(ctx, input.NodePath, input.Arguments...)
	command.Dir = input.Directory
	command.ExtraFiles = []*os.File{result}
	command.Env = []string{
		"HELMR_SUPERVISOR_FD=3",
		"HOME=" + input.WorkDir,
		"LANG=C.UTF-8",
		"PATH=" + filepath.Dir(input.NodePath),
		"TMPDIR=" + input.WorkDir,
	}
	logs := &boundedBuffer{remaining: compilerDocumentLimit}
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("analysis process failed: %w: %s", err, logs.String())
	}
	if logs.exceeded {
		return nil, errors.New("analysis process output exceeds the v0 bound")
	}
	if _, err := result.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	frame, err := io.ReadAll(io.LimitReader(result, compilerResultChannelLimit+1))
	if err != nil {
		return nil, err
	}
	if len(frame) > compilerResultChannelLimit {
		return nil, errors.New("analysis result exceeds the v0 bound")
	}
	if err := result.Close(); err != nil {
		return nil, err
	}
	open = false
	return frame, nil
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (buffer *boundedBuffer) Write(body []byte) (int, error) {
	accepted := len(body)
	if accepted > buffer.remaining {
		accepted = buffer.remaining
		buffer.exceeded = true
	}
	if accepted > 0 {
		_, _ = buffer.buffer.Write(body[:accepted])
		buffer.remaining -= accepted
	}
	return len(body), nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }

func ingestCompilerOutput(project, output string) error {
	resultPath := filepath.Join(output, "helmr/compiler-result.json")
	resultRaw, err := readBoundedRegularFile(resultPath, compilerDocumentLimit)
	if err != nil {
		return fmt.Errorf("read compiler result: %w", err)
	}
	_, err = artifact.ParseProgramCompilerResult(resultRaw)
	if err != nil {
		return fmt.Errorf("parse compiler result: %w", err)
	}
	filesByPath := map[string]struct{}{
		"helmr/config.json":          {},
		"helmr/compiler-result.json": {},
	}
	directories := map[string]struct{}{".": {}}
	for name := range filesByPath {
		for directory := filepath.ToSlash(filepath.Dir(name)); directory != "."; {
			directories[directory] = struct{}{}
			directory = filepath.ToSlash(filepath.Dir(directory))
		}
	}
	var files int
	var total int64
	if err := filepath.WalkDir(output, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(output, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if _, ok := directories[relative]; !ok {
				return fmt.Errorf("compiler output directory %q is not allowed", relative)
			}
			return nil
		}
		if info.Mode()&os.ModeType != 0 {
			return fmt.Errorf("compiler output %q is not a regular file", relative)
		}
		if _, ok := filesByPath[relative]; !ok {
			return fmt.Errorf("compiler output path %q is not allowed", relative)
		}
		files++
		total += info.Size()
		if info.Size() <= 0 ||
			info.Size() > compilerDocumentLimit || total > compilerOutputLimit {
			return errors.New("compiler output exceeds the document bounds")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("validate compiler output: %w", err)
	}
	if files != len(filesByPath) {
		return errors.New("compiler output is incomplete")
	}

	metadataTarget := filepath.Join(project, "helmr")
	info, err := os.Lstat(metadataTarget)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Program metadata target is not a directory")
	}
	for name := range filesByPath {
		if err := copyCompilerFile(filepath.Join(output, name), filepath.Join(project, name)); err != nil {
			return err
		}
	}

	return nil
}

// copyPayload preserves package executable modes and contained relative links.
// The payload is admitted and hashed before and after customer analysis.
func copyPayload(source, target string) error {
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Mkdir(destination, 0755)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		case info.Mode().IsRegular():
			if err := copyCompilerFile(name, destination); err != nil {
				return err
			}
			mode := os.FileMode(0644)
			if info.Mode().Perm()&0111 != 0 {
				mode = 0755
			}
			return os.Chmod(destination, mode)
		default:
			return fmt.Errorf("unsupported payload file %q", relative)
		}
	})
}

func copyCompilerFile(source, target string) (returnErr error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	open := true
	defer func() {
		if open {
			returnErr = errors.Join(returnErr, output.Close())
		}
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(target))
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	open = false
	return nil
}

func readBoundedRegularFile(path string, limit int64) (_ []byte, returnErr error) {
	entry, err := os.Lstat(path)
	if err != nil || !entry.Mode().IsRegular() || entry.Size() < 1 || entry.Size() > limit {
		return nil, errors.New("compiler output file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(entry, info) || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("compiler output file is not a bounded regular file")
	}
	return io.ReadAll(file)
}
