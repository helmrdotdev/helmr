package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"sort"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type EncodedProgram struct {
	Output   ProgramOutput
	artifact *artifactSnapshot
}

func EncodeProgram(
	ctx context.Context,
	directory string,
	encoder string,
	tree *BuildTree,
	verification VerificationResult,
	provenance BuildProvenance,
	workspaceImages []WorkspaceImage,
	compiler CompilerInputs,
	nodeVersion string,
) (_ *EncodedProgram, returnErr error) {
	if ctx == nil {
		return nil, errors.New("program encoding context is nil")
	}
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return nil, errors.New("build tree is closed")
	}
	if err := validateBuildProvenance("program encoding provenance", provenance); err != nil {
		return nil, err
	}
	if err := ValidateVerificationResult(verification); err != nil {
		return nil, err
	}
	if verification.Outcome != VerificationOutcomeSucceeded {
		return nil, errors.New("program encoding requires successful verification")
	}
	plan, err := ParseBuildPlan(
		[]byte(verification.Succeeded.Files[0].Content),
	)
	if err != nil {
		return nil, err
	}
	if len(buildPlanProgramDeclarations(plan)) == 0 {
		return nil, errors.New("program encoding requires a program-backed verification")
	}
	compilerResultRaw, err := tree.inspected.read(
		ctx,
		"helmr/compiler-result.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return nil, err
	}
	compilerResult, err := ParseProgramCompilerResult(compilerResultRaw)
	if err != nil {
		return nil, err
	}
	if err := validateProgramBuildAuthority(
		compilerResult,
		compiler,
		nodeVersion,
	); err != nil {
		return nil, err
	}
	if err := validateProgramAggregateResult(compilerResult, plan); err != nil {
		return nil, err
	}
	if compilerResult.Config.Digest != provenance.Config.ResultDigest {
		return nil, errors.New(
			"program build manifest config digest does not match build provenance",
		)
	}
	if err := verifyProgramBuildFiles(ctx, tree.inspected, compilerResult); err != nil {
		return nil, err
	}
	locator, err := ParseDeclarationLocator(
		[]byte(verification.Succeeded.Files[1].Content),
	)
	if err != nil {
		return nil, err
	}
	if err := validateProgramBuildLocators(compilerResult, locator); err != nil {
		return nil, err
	}
	index, err := buildProgramIndex(
		plan,
		locator,
		workspaceImages,
		provenance.Config.ResultDigest,
		provenance.RuntimeDigest,
	)
	if err != nil {
		return nil, err
	}
	indexRaw, err := CanonicalProgramIndex(index)
	if err != nil {
		return nil, err
	}
	indexHash := sha256.Sum256(indexRaw)
	manifest := buildManifestFromCompilerResult(
		compilerResult,
		"sha256:"+hex.EncodeToString(indexHash[:]),
		ProgramBuildFile{
			Digest: provenance.Config.SourceDigest,
			Path:   "helmr.config.ts",
		},
		ProgramBuildFile{
			Digest: provenance.Submitted.LockfileDigest,
			Path:   provenance.Submitted.LockfileName,
		},
	)
	if err := verifyProgramBuildFile(
		ctx,
		tree.inspected,
		manifest.ConfigSource,
	); err != nil {
		return nil, err
	}
	if err := verifyProgramBuildFile(ctx, tree.inspected, manifest.Lockfile); err != nil {
		return nil, err
	}
	manifestRaw, err := canonicalProgramBuildManifest(manifest)
	if err != nil {
		return nil, err
	}
	generated := map[string][]byte{
		"helmr/build-manifest.json": manifestRaw,
		"helmr/declarations.json":   indexRaw,
		"helmr/entry.mjs":           []byte(ProgramEntry),
	}
	artifact, err := encodeProgramTree(
		ctx,
		directory,
		encoder,
		programArtifact,
		programTreeEntries(ctx, tree.inspected, generated),
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("encode program: %w", err)
	}
	defer func() {
		if artifact != nil {
			returnErr = errors.Join(returnErr, artifact.Close())
		}
	}()

	output := ProgramOutput{
		Artifact: ProgramDescriptor(artifact.descriptor),
		Index:    index,
	}
	if err := ValidateProgramOutput(output); err != nil {
		return nil, err
	}
	if err := verifyEncodedProgram(ctx, artifact, output); err != nil {
		return nil, err
	}

	program := &EncodedProgram{
		Output:   output,
		artifact: artifact,
	}
	artifact = nil
	return program, nil
}

func verifyEncodedProgram(
	ctx context.Context,
	artifact *artifactSnapshot,
	output ProgramOutput,
) error {
	file, err := artifact.verifierFile()
	if err != nil {
		return err
	}
	reader, err := newSquashFSArtifactReader(
		ctx,
		file,
		output.Artifact.SizeBytes,
		programArtifact,
	)
	if err != nil {
		return fmt.Errorf("open encoded program: %w", err)
	}
	verified, err := verifyProgramArtifact(ctx, artifactInput{
		Digest:    output.Artifact.Digest,
		SizeBytes: output.Artifact.SizeBytes,
		MediaType: output.Artifact.MediaType,
		Reader:    reader,
	})
	if err != nil {
		return fmt.Errorf("verify encoded program: %w", err)
	}
	verifiedIndex, err := CanonicalProgramIndex(verified.Index())
	if err != nil {
		return err
	}
	expectedIndex, err := CanonicalProgramIndex(output.Index)
	if err != nil {
		return err
	}
	if !bytes.Equal(verifiedIndex, expectedIndex) {
		return errors.New("encoded program index changed during verification")
	}
	return nil
}

type programTreeSource struct {
	entry      artifactEntry
	sourcePath string
	content    []byte
}

func programTreeEntries(
	ctx context.Context,
	tree *inspectedArtifact,
	generated map[string][]byte,
) iter.Seq2[treeEntry, error] {
	sources := make([]programTreeSource, 0, len(tree.ordered)+len(generated)+2)
	for _, entry := range tree.ordered {
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
	if _, exists := tree.entries["helmr"]; !exists {
		sources = append(sources, programTreeSource{entry: artifactEntry{
			Path: "helmr",
			Kind: artifactEntryDirectory,
			Mode: 0755,
		}})
	}
	if _, exists := tree.entries["node_modules"]; !exists {
		sources = append(sources, programTreeSource{entry: artifactEntry{
			Path: "node_modules",
			Kind: artifactEntryDirectory,
			Mode: 0755,
		}})
	}
	for name, content := range generated {
		sources = append(sources, programTreeSource{
			entry: artifactEntry{
				Path:      name,
				Kind:      artifactEntryRegular,
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
				SizeBytes:  source.entry.SizeBytes,
				LinkTarget: source.entry.LinkTarget,
			}
			if entry.Kind != artifactEntryRegular {
				if !yield(entry, nil) {
					return
				}
				continue
			}
			if source.content != nil {
				entry.Content = bytes.NewReader(source.content)
				if !yield(entry, nil) {
					return
				}
				continue
			}
			reader, err := tree.reader.Open(ctx, source.sourcePath)
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

func (program *EncodedProgram) Publish(
	ctx context.Context,
	store cas.Store,
) (ProgramOutput, error) {
	if program == nil || program.artifact == nil {
		return ProgramOutput{}, errors.New("encoded program is closed")
	}
	if store == nil {
		return ProgramOutput{}, errors.New("program store is required")
	}
	if err := publishProgramArtifact(
		ctx,
		store,
		program.artifact,
		program.Output.Artifact,
	); err != nil {
		return ProgramOutput{}, fmt.Errorf("publish program: %w", err)
	}
	output := program.Output
	output.Index = cloneProgramIndex(output.Index)
	return output, nil
}

func publishProgramArtifact(
	ctx context.Context,
	store cas.Store,
	snapshot *artifactSnapshot,
	expected ProgramDescriptor,
) error {
	reader, err := snapshot.uploadReader(ctx)
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

func (program *EncodedProgram) Close() error {
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
