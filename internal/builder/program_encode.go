package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type encodedProgram struct {
	Output   artifact.ProgramOutput
	artifact *snapshot.Artifact
}

func encodeProgram(
	ctx context.Context,
	directory string,
	encoder string,
	tree *buildTree,
	verification VerificationResult,
	configResultDigest string,
	runtimeDigest string,
	computerImages []bundle.ComputerImage,
	compiler artifact.CompilerInputs,
	nodeVersion string,
) (_ *encodedProgram, returnErr error) {
	if ctx == nil {
		return nil, errors.New("program encoding context is nil")
	}
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return nil, errors.New("build tree is closed")
	}
	if !sha256sum.ValidDigest(configResultDigest) {
		return nil, errors.New("program encoding config result digest is invalid")
	}
	if !sha256sum.ValidDigest(runtimeDigest) {
		return nil, errors.New("program encoding runtime digest is invalid")
	}
	if err := validateVerificationResult(verification); err != nil {
		return nil, err
	}
	if verification.Outcome != VerificationOutcomeSucceeded {
		return nil, errors.New("program encoding requires successful verification")
	}
	plan, err := definition.ParseBuildPlan(verification.BuildPlan())
	if err != nil {
		return nil, err
	}
	if len(artifact.BuildPlanProgramDeclarations(plan)) == 0 {
		return nil, errors.New("program encoding requires a program-backed verification")
	}
	compilerResultRaw, err := tree.inspected.Read(
		ctx,
		"helmr/compiler-result.json",
		artifact.MaxProgramFileSizeBytes,
	)
	if err != nil {
		return nil, err
	}
	compilerResult, err := artifact.ParseProgramCompilerResult(compilerResultRaw)
	if err != nil {
		return nil, err
	}
	if err := artifact.ValidateProgramCompilerAuthority(
		compilerResult,
		compiler,
		nodeVersion,
	); err != nil {
		return nil, err
	}
	if compilerResult.Config.Digest != configResultDigest {
		return nil, errors.New(
			"program compiler result config digest does not match evaluated config",
		)
	}
	if err := artifact.VerifyProgramCompilerFiles(ctx, tree.inspected, compilerResult); err != nil {
		return nil, err
	}
	locator, err := artifact.ParseDeclarationLocator(verification.Declarations())
	if err != nil {
		return nil, err
	}
	if err := artifact.ValidateProgramCompilerLocators(compilerResult, locator); err != nil {
		return nil, err
	}
	images := make(map[string]definition.ComputerImage, len(computerImages))
	for _, image := range computerImages {
		images[image.DeclaredID] = image.Artifact.ComputerImage()
	}
	index, err := artifact.BuildProgramIndex(
		plan,
		locator,
		images,
		configResultDigest,
		runtimeDigest,
	)
	if err != nil {
		return nil, err
	}
	indexRaw, err := artifact.CanonicalProgramIndex(index)
	if err != nil {
		return nil, err
	}
	manifest := artifact.ProgramManifestFromCompilerResult(
		compilerResult,
		artifact.ProgramIndexDigest(indexRaw),
	)
	manifestRaw, err := artifact.CanonicalProgramManifest(manifest)
	if err != nil {
		return nil, err
	}
	generated := map[string][]byte{
		"helmr/program-manifest.json": manifestRaw,
		"helmr/declarations.json":     indexRaw,
	}
	content, err := encodeProgramTree(
		ctx,
		directory,
		encoder,
		artifact.RoleProgram,
		programTreeEntries(ctx, tree.inspected, generated),
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("encode program: %w", err)
	}
	defer func() {
		if content != nil {
			returnErr = errors.Join(returnErr, content.Close())
		}
	}()

	descriptor := content.Descriptor()
	output := artifact.ProgramOutput{
		Artifact: artifact.ProgramDescriptor{
			Digest:    descriptor.Digest,
			SizeBytes: descriptor.SizeBytes,
			MediaType: descriptor.MediaType,
		},
		Index: index,
	}
	if err := artifact.ValidateProgramOutput(output); err != nil {
		return nil, err
	}
	if err := verifyEncodedProgram(ctx, content, output); err != nil {
		return nil, err
	}

	program := &encodedProgram{
		Output:   output,
		artifact: content,
	}
	content = nil
	return program, nil
}

func verifyEncodedProgram(
	ctx context.Context,
	content *snapshot.Artifact,
	output artifact.ProgramOutput,
) error {
	file, err := content.VerifierFile()
	if err != nil {
		return err
	}
	if err := verify.ProgramOutputFile(ctx, file, output); err != nil {
		return fmt.Errorf("verify encoded program: %w", err)
	}
	return nil
}

type programTreeSource struct {
	entry      artifact.Entry
	sourcePath string
	content    []byte
}

func programTreeEntries(
	ctx context.Context,
	tree *artifact.Tree,
	generated map[string][]byte,
) iter.Seq2[treeEntry, error] {
	entries := tree.Entries()
	sources := make([]programTreeSource, 0, len(entries)+len(generated)+2)
	for _, entry := range entries {
		if entry.Path == "." || entry.Path == "helmr/compiler-result.json" {
			continue
		}
		if _, replaced := generated[entry.Path]; replaced {
			continue
		}
		sourcePath := entry.Path
		sources = append(sources, programTreeSource{
			entry:      entry,
			sourcePath: sourcePath,
		})
	}
	if _, exists := tree.Lookup("helmr"); !exists {
		sources = append(sources, programTreeSource{entry: artifact.Entry{
			Path: "helmr",
			Kind: artifact.EntryDirectory,
			Mode: 0755,
		}})
	}
	for name, content := range generated {
		sources = append(sources, programTreeSource{
			entry: artifact.Entry{
				Path:      name,
				Kind:      artifact.EntryRegular,
				Mode:      0644,
				SizeBytes: int64(len(content)),
			},
			content: append([]byte(nil), content...),
		})
	}
	sort.Slice(sources, func(left, right int) bool {
		return bytes.Compare(
			[]byte(sources[left].entry.Path),
			[]byte(sources[right].entry.Path),
		) < 0
	})
	return func(yield func(treeEntry, error) bool) {
		for _, source := range sources {
			if err := ctx.Err(); err != nil {
				yield(treeEntry{}, err)
				return
			}
			entry := treeEntry{
				Path:       source.entry.Path,
				Kind:       source.entry.Kind,
				Mode:       source.entry.Mode,
				LinkTarget: source.entry.LinkTarget,
			}
			if entry.Kind != artifact.EntryRegular {
				if !yield(entry, nil) {
					return
				}
				continue
			}
			entry.SizeBytes = source.entry.SizeBytes
			if source.content != nil {
				entry.Content = bytes.NewReader(source.content)
				if !yield(entry, nil) {
					return
				}
				continue
			}
			reader, err := tree.Open(ctx, source.sourcePath)
			if err != nil {
				yield(treeEntry{}, fmt.Errorf(
					"open frozen program path %q: %w",
					source.sourcePath,
					err,
				))
				return
			}
			entry.Content = reader
			continued := yield(entry, nil)
			closeErr := reader.Close()
			if closeErr != nil {
				yield(treeEntry{}, fmt.Errorf(
					"close frozen program path %q: %w",
					source.sourcePath,
					closeErr,
				))
				return
			}
			if !continued {
				return
			}
		}
	}
}

func (program *encodedProgram) Publish(
	ctx context.Context,
	store cas.Store,
) (artifact.ProgramOutput, error) {
	if program == nil || program.artifact == nil {
		return artifact.ProgramOutput{}, errors.New("encoded program is closed")
	}
	if store == nil {
		return artifact.ProgramOutput{}, errors.New("program store is required")
	}
	if err := publishProgramArtifact(
		ctx,
		store,
		program.artifact,
		program.Output.Artifact,
	); err != nil {
		return artifact.ProgramOutput{}, fmt.Errorf("publish program: %w", err)
	}
	output := program.Output
	output.Index = output.Index.Clone()
	return output, nil
}

// Materialize writes the exact verified Program object to a new local file.
// It is the producer-side counterpart to Publish: local/CI builders retain the
// object for bundle finalization instead of publishing it to a service CAS.
func (program *encodedProgram) Materialize(
	ctx context.Context,
	path string,
) (returnErr error) {
	if program == nil || program.artifact == nil {
		return errors.New("encoded program is closed")
	}
	if ctx == nil {
		return errors.New("program materialization context is nil")
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("program materialization path must be an absolute clean path")
	}
	reader, err := program.artifact.UploadReader(ctx)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create Program object: %w", err)
	}
	open := true
	defer func() {
		if open {
			returnErr = errors.Join(returnErr, file.Close())
		}
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(path))
		}
	}()
	written, err := io.Copy(file, reader)
	if err != nil {
		return fmt.Errorf("write Program object: %w", err)
	}
	if written != program.Output.Artifact.SizeBytes {
		return fmt.Errorf(
			"materialized Program size = %d, want %d",
			written,
			program.Output.Artifact.SizeBytes,
		)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync Program object: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Program object: %w", err)
	}
	open = false

	verified, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reopen Program object: %w", err)
	}
	verifyErr := verify.ProgramOutputFile(ctx, verified, program.Output)
	closeErr := verified.Close()
	if err := errors.Join(verifyErr, closeErr); err != nil {
		return fmt.Errorf("verify materialized Program object: %w", err)
	}
	return nil
}

func publishProgramArtifact(
	ctx context.Context,
	store cas.Store,
	content *snapshot.Artifact,
	expected artifact.ProgramDescriptor,
) error {
	reader, err := content.UploadReader(ctx)
	if err != nil {
		return err
	}
	object, err := store.Put(ctx, expected.MediaType, reader)
	if err != nil {
		return err
	}
	if object.Digest != expected.Digest ||
		object.SizeBytes != expected.SizeBytes ||
		object.MediaType != expected.MediaType {
		return errors.New("published program object does not match its descriptor")
	}
	return nil
}

func (program *encodedProgram) Close() error {
	if program == nil {
		return nil
	}
	var err error
	if program.artifact != nil {
		err = errors.Join(err, program.artifact.Close())
		program.artifact = nil
	}
	return err
}
