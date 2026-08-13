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
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

const (
	compilerDocumentLimit      = 16 << 20
	compilerResultChannelLimit = 70 << 20
	compilerOutputLimit        = 128 << 20
	compilerFileLimit          = 8194
)

// ProgramInput is the manager-neutral boundary of the canonical builder. The
// caller has already materialized dependencies in ProjectDirectory inside a
// bounded BuildKit execution container. Package-manager identity, lockfile
// identity, install commands, and source provenance are intentionally absent.
type ProgramInput struct {
	ProjectDirectory  string
	WorkDirectory     string
	ProgramObjectPath string
	NodePath          string
	NodeLoader        string
	NodeLibraryPath   string
	ConfigEvaluator   string
	ProgramCompiler   string
	SquashFSEncoder   string
	Compiler          deployment.CompilerInputs
	Runtime           deployment.RuntimeDescriptor
	RuntimeMetadata   deployment.RuntimeMetadata
}

type ProgramResult struct {
	Program      deployment.ProgramOutput
	Config       deployment.BuildConfig
	Verification deployment.VerificationResult
	ObjectPath   string
}

// BuildProgram performs only Helmr-owned finalization of an installed project
// tree. BuildKit, not this function, owns dependency-install isolation and
// networking. This function never invokes a package manager or a user command.
func BuildProgram(
	ctx context.Context,
	input ProgramInput,
) (_ ProgramResult, returnErr error) {
	if ctx == nil {
		return ProgramResult{}, errors.New("Program build context is nil")
	}
	if err := validateProgramInput(input); err != nil {
		return ProgramResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProgramResult{}, err
	}
	work, err := os.MkdirTemp(input.WorkDirectory, ".helmr-program-")
	if err != nil {
		return ProgramResult{}, fmt.Errorf("create Program work directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(work)) }()
	compilerOutput := filepath.Join(work, "compiler-output")
	if err := os.Mkdir(compilerOutput, 0o700); err != nil {
		return ProgramResult{}, fmt.Errorf("create compiler output: %w", err)
	}

	flags := append([]string(nil), input.RuntimeMetadata.ProgramNodeFlags...)
	configFrame, err := runFinalizerCommand(ctx, finalizerCommand{
		NodePath:        input.NodePath,
		NodeLoader:      input.NodeLoader,
		NodeLibraryPath: input.NodeLibraryPath,
		Arguments: append(append([]string{}, flags...),
			input.ConfigEvaluator,
			input.ProjectDirectory,
			input.RuntimeMetadata.NodeVersion,
			work,
		),
		Directory: input.ProjectDirectory,
		WorkDir:   work,
	})
	if err != nil {
		return ProgramResult{}, fmt.Errorf("evaluate Helmr config: %w", err)
	}
	config, err := deployment.ReadBuildConfigFrame(bytes.NewReader(configFrame))
	if err != nil {
		return ProgramResult{}, err
	}
	canonicalConfig, err := deployment.CanonicalBuildConfig(config)
	if err != nil {
		return ProgramResult{}, err
	}
	configPath := filepath.Join(work, "config.json")
	if err := os.WriteFile(configPath, canonicalConfig, 0o600); err != nil {
		return ProgramResult{}, fmt.Errorf("write canonical Helmr config: %w", err)
	}

	verificationFrame, err := runFinalizerCommand(ctx, finalizerCommand{
		NodePath:        input.NodePath,
		NodeLoader:      input.NodeLoader,
		NodeLibraryPath: input.NodeLibraryPath,
		Arguments: append(append([]string{}, flags...),
			input.ProgramCompiler,
			input.ProjectDirectory,
			configPath,
			input.RuntimeMetadata.NodeVersion,
			compilerOutput,
		),
		Directory: input.ProjectDirectory,
		WorkDir:   work,
	})
	if err != nil {
		return ProgramResult{}, fmt.Errorf("compile Helmr Program: %w", err)
	}
	verification, err := deployment.ReadVerificationResultFrame(
		bytes.NewReader(verificationFrame),
	)
	if err != nil {
		return ProgramResult{}, err
	}
	if verification.Outcome != deployment.VerificationOutcomeSucceeded {
		return ProgramResult{}, fmt.Errorf(
			"compile Helmr Program: %s",
			verification.Failed.Error.Message,
		)
	}
	if err := ingestCompilerOutput(input.ProjectDirectory, compilerOutput); err != nil {
		return ProgramResult{}, err
	}

	treeArchive, cleanupArchive, err := archive.CreateTarWithOptionsContext(
		ctx,
		input.ProjectDirectory,
		work,
		archive.TarOptions{},
	)
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

	configDigest, err := deployment.BuildConfigDigest(config)
	if err != nil {
		return ProgramResult{}, err
	}
	program, err := deployment.EncodeProgram(
		ctx,
		work,
		input.SquashFSEncoder,
		tree,
		verification,
		configDigest,
		input.Runtime.Digest,
		nil,
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

func validateProgramInput(input ProgramInput) error {
	for name, value := range map[string]string{
		"project directory":   input.ProjectDirectory,
		"work directory":      input.WorkDirectory,
		"Program object path": input.ProgramObjectPath,
		"Node executable":     input.NodePath,
		"Node loader":         input.NodeLoader,
		"Node library path":   input.NodeLibraryPath,
		"Config Evaluator":    input.ConfigEvaluator,
		"Program Compiler":    input.ProgramCompiler,
		"SquashFS encoder":    input.SquashFSEncoder,
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
	if _, err := os.Lstat(input.ProgramObjectPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("Program object path already exists")
		}
		return fmt.Errorf("inspect Program object path: %w", err)
	}
	if err := deployment.ValidateCompilerInputs(input.Compiler); err != nil {
		return err
	}
	if err := deployment.ValidateRuntimeDescriptor(input.Runtime); err != nil {
		return err
	}
	if err := deployment.ValidateRuntimeMetadata(input.RuntimeMetadata); err != nil {
		return err
	}
	if input.Runtime.Architecture != input.RuntimeMetadata.Architecture ||
		input.Runtime.RuntimeContract != input.RuntimeMetadata.RuntimeContract {
		return errors.New("Runtime descriptor and metadata do not match")
	}
	return nil
}

type finalizerCommand struct {
	NodePath        string
	NodeLoader      string
	NodeLibraryPath string
	Arguments       []string
	Directory       string
	WorkDir         string
}

func runFinalizerCommand(
	ctx context.Context,
	input finalizerCommand,
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
	arguments := []string{"--library-path", input.NodeLibraryPath, input.NodePath}
	arguments = append(arguments, input.Arguments...)
	command := exec.CommandContext(ctx, input.NodeLoader, arguments...)
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
		return nil, fmt.Errorf("finalizer process failed: %w: %s", err, logs.String())
	}
	if logs.exceeded {
		return nil, errors.New("finalizer process output exceeds the v0 bound")
	}
	if _, err := result.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	frame, err := io.ReadAll(io.LimitReader(result, compilerResultChannelLimit+1))
	if err != nil {
		return nil, err
	}
	if len(frame) > compilerResultChannelLimit {
		return nil, errors.New("finalizer result exceeds the v0 bound")
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
	result, err := deployment.ParseProgramCompilerResult(resultRaw)
	if err != nil {
		return fmt.Errorf("parse compiler result: %w", err)
	}
	filesByPath := map[string]struct{}{
		"helmr/config.json":          {},
		"helmr/compiler-result.json": {},
	}
	for _, generated := range result.Outputs {
		filesByPath[generated.ModulePath] = struct{}{}
		filesByPath[generated.SourceMapPath] = struct{}{}
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
		if files > compilerFileLimit || info.Size() <= 0 ||
			info.Size() > compilerDocumentLimit || total > compilerOutputLimit {
			return errors.New("compiler output exceeds the v0 bounds")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("validate compiler output: %w", err)
	}
	if files != len(filesByPath) {
		return errors.New("compiler output is incomplete")
	}

	metadataTarget := filepath.Join(project, "helmr")
	if _, err := os.Lstat(metadataTarget); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("installed tree contains reserved path \"helmr\"")
		}
		return fmt.Errorf("inspect Program metadata target: %w", err)
	}
	if err := copyCompilerTree(filepath.Join(output, "helmr"), metadataTarget); err != nil {
		return fmt.Errorf("ingest compiler metadata: %w", err)
	}

	generatedDirectories := make(map[string]struct{}, len(result.Outputs))
	for _, generated := range result.Outputs {
		directory, ok := generatedOutputDirectory(generated.ModulePath)
		if !ok {
			return fmt.Errorf("compiler output path %q has no reserved output directory", generated.ModulePath)
		}
		generatedDirectories[directory] = struct{}{}
	}
	orderedDirectories := make([]string, 0, len(generatedDirectories))
	for directory := range generatedDirectories {
		orderedDirectories = append(orderedDirectories, directory)
	}
	sort.Strings(orderedDirectories)
	for _, directory := range orderedDirectories {
		target := filepath.Join(project, filepath.FromSlash(directory))
		if err := ensureSafeTargetParents(project, directory); err != nil {
			return err
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("installed tree contains reserved path %q", directory)
			}
			return err
		}
		if err := os.MkdirAll(filepath.Join(target, "modules"), 0o755); err != nil {
			return err
		}
	}
	orderedFiles := make([]string, 0, len(filesByPath)-2)
	for name := range filesByPath {
		if !strings.HasPrefix(name, "helmr/") {
			orderedFiles = append(orderedFiles, name)
		}
	}
	sort.Strings(orderedFiles)
	for _, name := range orderedFiles {
		source := filepath.Join(output, filepath.FromSlash(name))
		target := filepath.Join(project, filepath.FromSlash(name))
		if err := copyCompilerFile(source, target); err != nil {
			return fmt.Errorf("ingest compiler output %q: %w", name, err)
		}
	}
	return nil
}

func generatedOutputDirectory(name string) (string, bool) {
	const root = ".helmr/modules/"
	if strings.HasPrefix(name, root) {
		return ".helmr", true
	}
	const nested = "/.helmr/modules/"
	index := strings.LastIndex(name, nested)
	if index <= 0 {
		return "", false
	}
	return name[:index] + "/.helmr", true
}

func ensureSafeTargetParents(root, relative string) error {
	current := root
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect compiler output parent %q: %w", current, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("compiler output parent %q is not a directory", current)
		}
	}
	return nil
}

func copyCompilerTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.Mkdir(destination, 0o755)
		}
		return copyCompilerFile(path, destination)
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
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("compiler output file is not a bounded regular file")
	}
	return io.ReadAll(file)
}
