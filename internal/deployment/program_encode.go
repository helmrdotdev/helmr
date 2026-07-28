package deployment

import (
	"bytes"
	"context"
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
) (_ *EncodedProgram, returnErr error) {
	if ctx == nil {
		return nil, errors.New("Program encoding context is nil")
	}
	if tree == nil || tree.content == nil || tree.inspected == nil {
		return nil, errors.New("build tree is closed")
	}
	if err := validateBuildProvenance("Program encoding provenance", provenance); err != nil {
		return nil, err
	}
	if err := ValidateVerificationResult(verification); err != nil {
		return nil, err
	}
	if verification.Outcome != VerificationOutcomeSucceeded {
		return nil, errors.New("Program encoding requires successful verification")
	}
	plan, err := ParseBuildPlan(
		[]byte(verification.Succeeded.Files[0].Content),
	)
	if err != nil {
		return nil, err
	}
	declarations := buildPlanProgramDeclarations(plan)
	if len(declarations) == 0 {
		return nil, errors.New("Program encoding requires a Program-backed verification")
	}

	index := ProgramIndex{
		Architecture:         provenance.Architecture,
		BuildContractVersion: provenance.BuildContractVersion,
		Declarations:         declarations,
		FormatVersion:        ProgramIndexFormatVersion,
		Manager:              provenance.Manager,
		RuntimeAPIVersion:    RuntimeAPIVersion,
		RuntimeDigest:        provenance.RuntimeDigest,
		ToolchainDigest:      provenance.ToolchainDigest,
		Submitted:            provenance.Submitted,
	}
	indexRaw, err := CanonicalProgramIndex(index)
	if err != nil {
		return nil, err
	}
	generated := map[string][]byte{
		VerificationDeclarationsPath: []byte(verification.Succeeded.Files[1].Content),
		VerificationProgramEntryPath: []byte(verification.Succeeded.Files[2].Content),
		"helmr/program.json":         indexRaw,
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
		return nil, fmt.Errorf("encode Program: %w", err)
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
		return fmt.Errorf("open encoded Program: %w", err)
	}
	verified, err := verifyProgramArtifact(ctx, artifactInput{
		Digest:    output.Artifact.Digest,
		SizeBytes: output.Artifact.SizeBytes,
		MediaType: output.Artifact.MediaType,
		Reader:    reader,
	})
	if err != nil {
		return fmt.Errorf("verify encoded Program: %w", err)
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
		return errors.New("encoded Program index changed during verification")
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
		if entry.Path == "." {
			continue
		}
		sourcePath := entry.Path
		sources = append(sources, programTreeSource{
			entry:      entry,
			sourcePath: sourcePath,
		})
	}
	sources = append(sources, programTreeSource{entry: artifactEntry{
		Path: "helmr",
		Kind: artifactEntryDirectory,
		Mode: 0755,
	}})
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
					"open frozen Program path %q: %w",
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
					"close frozen Program path %q: %w",
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
		return ProgramOutput{}, errors.New("encoded Program is closed")
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
		return ProgramOutput{}, fmt.Errorf("publish Program: %w", err)
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
		return errors.New("published Program object does not match its descriptor")
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
