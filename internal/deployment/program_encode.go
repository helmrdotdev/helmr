package deployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type EncodedProgram struct {
	Receipt      ProgramReceipt
	code         *artifactSnapshot
	dependencies *artifactSnapshot
}

func EncodeProgram(
	ctx context.Context,
	directory string,
	encoder string,
	tree *BuildTree,
	analysis AnalysisResult,
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
	if err := ValidateAnalysisResult(analysis); err != nil {
		return nil, err
	}
	if analysis.Outcome != AnalysisOutcomeSucceeded {
		return nil, errors.New("Program encoding requires successful analysis")
	}
	plan, err := ParseBuildPlan(
		[]byte(analysis.Succeeded.Files[0].Content),
	)
	if err != nil {
		return nil, err
	}
	declarations := buildPlanProgramDeclarations(plan)
	if len(declarations) == 0 {
		return nil, errors.New("Program encoding requires Program-backed analysis")
	}

	dependencies, err := encodeProgramTree(
		ctx,
		directory,
		encoder,
		dependencyArtifact,
		programTreeEntries(ctx, tree.inspected, dependencyArtifact, nil),
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("encode Program dependencies: %w", err)
	}
	defer func() {
		if dependencies != nil {
			returnErr = errors.Join(returnErr, dependencies.Close())
		}
	}()

	dependencyDescriptor := ProgramDescriptor(dependencies.descriptor)
	index := ProgramIndex{
		Architecture:            provenance.Architecture,
		BuildContractVersion:    provenance.BuildContractVersion,
		Declarations:            declarations,
		DependenciesDigest:      dependencyDescriptor.Digest,
		FormatVersion:           ProgramIndexFormatVersion,
		Manager:                 provenance.Manager,
		RuntimeAPIVersion:       RuntimeAPIVersion,
		RuntimeDigest:           provenance.RuntimeDigest,
		StandardToolchainDigest: provenance.StandardToolchainDigest,
		Submitted:               provenance.Submitted,
	}
	indexRaw, err := CanonicalProgramIndex(index)
	if err != nil {
		return nil, err
	}
	generated := map[string][]byte{
		AnalysisDeclarationsPath: []byte(analysis.Succeeded.Files[1].Content),
		AnalysisProgramEntryPath: []byte(analysis.Succeeded.Files[2].Content),
		"helmr/program.json":     indexRaw,
	}
	code, err := encodeProgramTree(
		ctx,
		directory,
		encoder,
		codeArtifact,
		programTreeEntries(ctx, tree.inspected, codeArtifact, generated),
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("encode Program code: %w", err)
	}
	defer func() {
		if code != nil {
			returnErr = errors.Join(returnErr, code.Close())
		}
	}()

	receipt := ProgramReceipt{
		FormatVersion: ProgramReceiptFormatVersion,
		Code:          ProgramDescriptor(code.descriptor),
		Dependencies:  dependencyDescriptor,
		Index:         index,
	}
	if err := ValidateProgramReceipt(receipt); err != nil {
		return nil, err
	}
	if err := verifyEncodedProgram(ctx, code, dependencies, receipt); err != nil {
		return nil, err
	}

	program := &EncodedProgram{
		Receipt:      receipt,
		code:         code,
		dependencies: dependencies,
	}
	code = nil
	dependencies = nil
	return program, nil
}

func verifyEncodedProgram(
	ctx context.Context,
	code *artifactSnapshot,
	dependencies *artifactSnapshot,
	receipt ProgramReceipt,
) error {
	codeFile, err := code.verifierFile()
	if err != nil {
		return err
	}
	codeReader, err := newSquashFSArtifactReader(
		ctx,
		codeFile,
		receipt.Code.SizeBytes,
		codeArtifact,
	)
	if err != nil {
		return fmt.Errorf("open encoded Program code: %w", err)
	}
	dependencyFile, err := dependencies.verifierFile()
	if err != nil {
		return err
	}
	dependencyReader, err := newSquashFSArtifactReader(
		ctx,
		dependencyFile,
		receipt.Dependencies.SizeBytes,
		dependencyArtifact,
	)
	if err != nil {
		return fmt.Errorf("open encoded Program dependencies: %w", err)
	}
	verified, err := verifyProgramArtifacts(ctx, programArtifacts{
		Code: programArtifact{
			Digest:    receipt.Code.Digest,
			SizeBytes: receipt.Code.SizeBytes,
			MediaType: receipt.Code.MediaType,
			Reader:    codeReader,
		},
		Dependencies: programArtifact{
			Digest:    receipt.Dependencies.Digest,
			SizeBytes: receipt.Dependencies.SizeBytes,
			MediaType: receipt.Dependencies.MediaType,
			Reader:    dependencyReader,
		},
	})
	if err != nil {
		return fmt.Errorf("verify encoded Program: %w", err)
	}
	verifiedIndex, err := CanonicalProgramIndex(verified.Index())
	if err != nil {
		return err
	}
	expectedIndex, err := CanonicalProgramIndex(receipt.Index)
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
	role artifactRole,
	generated map[string][]byte,
) iter.Seq2[treeEntry, error] {
	sources := make([]programTreeSource, 0, len(tree.ordered)+len(generated)+2)
	for _, entry := range tree.ordered {
		if entry.Path == "." {
			continue
		}
		sourcePath := entry.Path
		switch role {
		case codeArtifact:
			if entry.Path == "node_modules" ||
				strings.HasPrefix(entry.Path, "node_modules/") {
				continue
			}
		case dependencyArtifact:
			if !strings.HasPrefix(entry.Path, "node_modules/") {
				continue
			}
			entry.Path = strings.TrimPrefix(entry.Path, "node_modules/")
		default:
			return errorTreeEntries(
				fmt.Errorf("Program tree role = %d", role),
			)
		}
		sources = append(sources, programTreeSource{
			entry:      entry,
			sourcePath: sourcePath,
		})
	}
	if role == codeArtifact {
		sources = append(
			sources,
			programTreeSource{entry: artifactEntry{
				Path: "helmr",
				Kind: artifactEntryDirectory,
				Mode: 0755,
			}},
			programTreeSource{entry: artifactEntry{
				Path: "node_modules",
				Kind: artifactEntryDirectory,
				Mode: 0755,
			}},
		)
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

func errorTreeEntries(err error) iter.Seq2[treeEntry, error] {
	return func(yield func(treeEntry, error) bool) {
		yield(treeEntry{}, err)
	}
}

func (program *EncodedProgram) LinkInto(
	directory string,
	codeName string,
	dependencyName string,
	uid int,
	gid int,
) error {
	if program == nil || program.code == nil || program.dependencies == nil {
		return errors.New("encoded Program is closed")
	}
	if err := program.code.LinkInto(directory, codeName, uid, gid); err != nil {
		return fmt.Errorf("link Program code: %w", err)
	}
	if err := program.dependencies.LinkInto(
		directory,
		dependencyName,
		uid,
		gid,
	); err != nil {
		return fmt.Errorf("link Program dependencies: %w", err)
	}
	return nil
}

func (program *EncodedProgram) Publish(
	ctx context.Context,
	store cas.Store,
) (ProgramReceipt, error) {
	if program == nil || program.code == nil || program.dependencies == nil {
		return ProgramReceipt{}, errors.New("encoded Program is closed")
	}
	if store == nil {
		return ProgramReceipt{}, errors.New("Program store is required")
	}
	if err := publishProgramArtifact(
		ctx,
		store,
		program.code,
		program.Receipt.Code,
	); err != nil {
		return ProgramReceipt{}, fmt.Errorf("publish Program code: %w", err)
	}
	if err := publishProgramArtifact(
		ctx,
		store,
		program.dependencies,
		program.Receipt.Dependencies,
	); err != nil {
		return ProgramReceipt{}, fmt.Errorf("publish Program dependencies: %w", err)
	}
	return cloneProgramReceipt(program.Receipt), nil
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
	if program.code != nil {
		err = errors.Join(err, program.code.Close())
		program.code = nil
	}
	if program.dependencies != nil {
		err = errors.Join(err, program.dependencies.Close())
		program.dependencies = nil
	}
	return err
}
