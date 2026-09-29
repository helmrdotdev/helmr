package verify

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	runtimeNodePath     = "bin/node"
	runtimeEntryPath    = "helmr/entry.mjs"
	runtimeMetadataPath = "helmr/runtime.json"
	runtimeLibcPath     = "lib/libc.so.6"
	runtimeLicensePath  = "share/licenses/node/LICENSE"
)

func Runtime(
	ctx context.Context,
	unitCgroupRoot string,
	leaseIdentity string,
	runtimeSnapshot *snapshot.Runtime,
) (artifact.RuntimeIndex, error) {
	if ctx == nil {
		return artifact.RuntimeIndex{}, fmt.Errorf("runtime verification context is nil")
	}
	source, descriptor, err := runtimeSnapshot.VerifierFile()
	if err != nil {
		return artifact.RuntimeIndex{}, err
	}
	if err := artifact.ValidateRuntimeDescriptor(descriptor); err != nil {
		return artifact.RuntimeIndex{}, err
	}
	if descriptor.SizeBytes > artifact.MaxRuntimePhysicalBytes {
		return artifact.RuntimeIndex{}, fmt.Errorf(
			"runtime artifact size exceeds %d bytes",
			artifact.MaxRuntimePhysicalBytes,
		)
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            runtimeVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{source},
	})
	if err != nil {
		return artifact.RuntimeIndex{}, err
	}
	switch result.kind {
	case verifierVerified:
		return verifiedRuntimeResult(result.payload, descriptor)
	case verifierInvalid:
		return artifact.RuntimeIndex{}, &verifierInvalidError{diagnostic: result.diagnostic}
	case verifierFailed:
		return artifact.RuntimeIndex{}, errors.New("runtime verifier failed")
	default:
		return artifact.RuntimeIndex{}, fmt.Errorf(
			"runtime verifier returned unknown outcome %d",
			result.kind,
		)
	}
}

func verifiedRuntimeResult(
	payload []byte,
	descriptor artifact.RuntimeDescriptor,
) (artifact.RuntimeIndex, error) {
	index, err := artifact.ParseRuntimeIndex(payload)
	if err != nil {
		return artifact.RuntimeIndex{}, fmt.Errorf("parse verified runtime index: %w", err)
	}
	if index.Architecture != descriptor.Architecture {
		return artifact.RuntimeIndex{}, fmt.Errorf(
			"runtime index architecture = %q, descriptor declares %q",
			index.Architecture,
			descriptor.Architecture,
		)
	}
	if index.RuntimeContract != descriptor.RuntimeContract {
		return artifact.RuntimeIndex{}, fmt.Errorf(
			"runtime index runtimeContract = %q, descriptor declares %q",
			index.RuntimeContract,
			descriptor.RuntimeContract,
		)
	}
	return index, nil
}

func verifyRuntimeTopology(
	ctx context.Context,
	tree *artifact.Tree,
) (artifact.RuntimeIndex, error) {
	requiredDirectories := []string{
		".",
		"bin",
		"helmr",
		"lib",
		"share",
		"share/licenses",
		"share/licenses/node",
		"share/licenses/debian",
	}
	for _, required := range requiredDirectories {
		if _, err := tree.Require(required, artifact.EntryDirectory); err != nil {
			return artifact.RuntimeIndex{}, fmt.Errorf("runtime layout: %w", err)
		}
	}
	requiredFiles := map[string]uint32{
		runtimeNodePath:                    0755,
		runtimeEntryPath:                   0644,
		runtimeMetadataPath:                0644,
		runtimeLibcPath:                    0644,
		runtimeLicensePath:                 0644,
		"helmr/module-preload.mjs":         0644,
		"share/licenses/debian/libc6":      0644,
		"share/licenses/debian/libgcc-s1":  0644,
		"share/licenses/debian/libstdc++6": 0644,
	}
	for required, mode := range requiredFiles {
		entry, err := tree.Require(required, artifact.EntryRegular)
		if err != nil {
			return artifact.RuntimeIndex{}, fmt.Errorf("runtime layout: %w", err)
		}
		if entry.Mode != mode {
			return artifact.RuntimeIndex{}, fmt.Errorf(
				"runtime path %q mode = %#o, want %#o",
				required,
				entry.Mode,
				mode,
			)
		}
	}
	for _, entry := range tree.Entries() {
		if err := validateRuntimePath(entry, requiredFiles); err != nil {
			return artifact.RuntimeIndex{}, err
		}
	}
	metadataRaw, err := tree.Read(
		ctx,
		runtimeMetadataPath,
		artifact.MaxRuntimeDocumentBytes,
	)
	if err != nil {
		return artifact.RuntimeIndex{}, err
	}
	metadata, err := artifact.ParseRuntimeMetadata(metadataRaw)
	if err != nil {
		return artifact.RuntimeIndex{}, err
	}
	for name, digest := range map[string]string{"helmr/module-preload.mjs": metadata.ModulePolicyDigest} {
		if err := artifact.VerifyProgramPathDigest(ctx, tree, artifact.ProgramPathDigest{Path: name, Digest: digest}); err != nil {
			return artifact.RuntimeIndex{}, err
		}
	}
	return artifact.RuntimeIndex{
		Architecture:    metadata.Architecture,
		RuntimeContract: metadata.RuntimeContract,
	}, nil
}

func validateRuntimePath(entry artifact.Entry, required map[string]uint32) error {
	switch entry.Path {
	case ".", "bin", "helmr", "lib", "share", "share/licenses", "share/licenses/node", "share/licenses/debian":
		return nil
	}
	if _, exists := required[entry.Path]; exists {
		return nil
	}
	if entry.Path == "bin" || strings.HasPrefix(entry.Path, "bin/") {
		return fmt.Errorf("runtime contains unlisted bin path %q", entry.Path)
	}
	if entry.Path == "helmr" || strings.HasPrefix(entry.Path, "helmr/") {
		return fmt.Errorf("runtime contains unlisted Helmr path %q", entry.Path)
	}
	if entry.Path == "lib" || strings.HasPrefix(entry.Path, "lib/") {
		return nil
	}
	return fmt.Errorf("runtime contains unlisted top-level path %q", entry.Path)
}

func verifyRuntimeExecutables(
	ctx context.Context,
	tree *artifact.Tree,
	architecture definition.RuntimeArchitecture,
) error {
	machine, loader, err := runtimeELFTarget(architecture)
	if err != nil {
		return err
	}
	if err := verifyRuntimeExecutable(
		ctx,
		tree,
		runtimeNodePath,
		machine,
		loader,
		loader,
		true,
	); err != nil {
		return fmt.Errorf("runtime Node.js: %w", err)
	}
	loaderPath := strings.TrimPrefix(loader, artifact.RuntimeMountPath+"/")
	loaderEntry, err := tree.Require(loaderPath, artifact.EntryRegular)
	if err != nil {
		return fmt.Errorf("runtime loader: %w", err)
	}
	if loaderEntry.Mode != 0755 {
		return fmt.Errorf("runtime loader mode = %#o, want %#o", loaderEntry.Mode, 0755)
	}
	if err := verifyRuntimeLoader(
		ctx,
		tree,
		loaderPath,
		machine,
		loader,
	); err != nil {
		return fmt.Errorf("runtime loader: %w", err)
	}
	for _, entry := range tree.Entries() {
		if entry.Kind == artifact.EntrySymlink && strings.HasPrefix(entry.Path, "lib/") {
			if _, _, err := resolveRuntimeLibrary(tree, entry.Path); err != nil {
				return err
			}
		}
		if entry.Kind != artifact.EntryRegular || !strings.HasPrefix(entry.Path, "lib/") {
			continue
		}
		wantMode := uint32(0644)
		if entry.Path == loaderPath {
			wantMode = 0755
		}
		if entry.Mode != wantMode {
			return fmt.Errorf(
				"runtime library %q mode = %#o, want %#o",
				entry.Path,
				entry.Mode,
				wantMode,
			)
		}
		if entry.Path == loaderPath {
			continue
		}
		isELF, err := runtimeFileHasELFMagic(ctx, tree, entry.Path)
		if err != nil {
			return err
		}
		if !isELF {
			continue
		}
		if err := verifyRuntimeSharedObject(
			ctx,
			tree,
			entry.Path,
			machine,
			loader,
		); err != nil {
			return fmt.Errorf("runtime library %q: %w", entry.Path, err)
		}
	}
	return nil
}

func verifyRuntimeLoader(
	ctx context.Context,
	tree *artifact.Tree,
	filePath string,
	machine elf.Machine,
	loader string,
) error {
	file, err := openRuntimeSharedObject(ctx, tree, filePath, machine, loader)
	if err != nil {
		return err
	}
	defer file.Close()
	needed, err := runtimeDynamicStrings(file, elf.DT_NEEDED)
	if err != nil {
		return err
	}
	if len(needed) != 0 {
		return fmt.Errorf("managed loader declares dynamic libraries")
	}
	searchPaths, err := runtimeELFSearchPaths(file)
	if err != nil {
		return err
	}
	if len(searchPaths) != 0 {
		return fmt.Errorf("managed loader declares a runtime search path")
	}
	return nil
}

func verifyRuntimeExecutable(
	ctx context.Context,
	tree *artifact.Tree,
	filePath string,
	machine elf.Machine,
	interpreter string,
	loader string,
	requireDynamic bool,
) error {
	file, err := openRuntimeELF(ctx, tree, filePath, machine)
	if err != nil {
		return err
	}
	defer file.Close()
	if file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
		return fmt.Errorf("ELF type %s is not executable", file.Type)
	}
	gotInterpreter, hasInterpreter, err := runtimeELFInterpreter(file)
	if err != nil {
		return err
	}
	if hasInterpreter != (interpreter != "") || gotInterpreter != interpreter {
		return fmt.Errorf("ELF interpreter = %q, want %q", gotInterpreter, interpreter)
	}
	needed, err := verifyRuntimeDynamicClosure(
		ctx,
		tree,
		file,
		machine,
		loader,
		requireDynamic,
	)
	if err != nil {
		return err
	}
	if !requireDynamic && len(needed) != 0 {
		return fmt.Errorf("static executable declares dynamic libraries")
	}
	if requireDynamic && len(needed) == 0 {
		return fmt.Errorf("dynamic executable has no dynamic-library closure")
	}
	return nil
}

func verifyRuntimeSharedObject(
	ctx context.Context,
	tree *artifact.Tree,
	filePath string,
	machine elf.Machine,
	loader string,
) error {
	file, err := openRuntimeSharedObject(ctx, tree, filePath, machine, loader)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = verifyRuntimeDynamicClosure(ctx, tree, file, machine, loader, true)
	return err
}

func openRuntimeSharedObject(
	ctx context.Context,
	tree *artifact.Tree,
	filePath string,
	machine elf.Machine,
	loader string,
) (*elf.File, error) {
	file, err := openRuntimeELF(ctx, tree, filePath, machine)
	if err != nil {
		return nil, err
	}
	if file.Type != elf.ET_DYN {
		file.Close()
		return nil, fmt.Errorf("ELF type %s is not a shared object", file.Type)
	}
	interpreter, hasInterpreter, err := runtimeELFInterpreter(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	if filePath == runtimeLibcPath {
		if !hasInterpreter || interpreter != loader {
			file.Close()
			return nil, fmt.Errorf("libc interpreter = %q, want %q", interpreter, loader)
		}
		sonames, err := runtimeDynamicStrings(file, elf.DT_SONAME)
		if err != nil {
			file.Close()
			return nil, err
		}
		if len(sonames) != 1 || sonames[0] != "libc.so.6" {
			file.Close()
			return nil, fmt.Errorf("libc SONAME = %q, want %q", sonames, "libc.so.6")
		}
		return file, nil
	}
	if hasInterpreter {
		file.Close()
		return nil, fmt.Errorf("shared object declares ELF interpreter %q", interpreter)
	}
	return file, nil
}

func openRuntimeELF(
	ctx context.Context,
	tree *artifact.Tree,
	filePath string,
	machine elf.Machine,
) (*elf.File, error) {
	raw, err := tree.Read(ctx, filePath, artifact.MaxFileSize)
	if err != nil {
		return nil, err
	}
	file, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse ELF: %w", err)
	}
	if file.Class != elf.ELFCLASS64 ||
		file.Data != elf.ELFDATA2LSB ||
		file.Machine != machine {
		file.Close()
		return nil, fmt.Errorf("ELF identity is outside the runtime contract")
	}
	return file, nil
}

func verifyRuntimeDynamicClosure(
	ctx context.Context,
	tree *artifact.Tree,
	file *elf.File,
	machine elf.Machine,
	loader string,
	requireSearchPath bool,
) ([]string, error) {
	needed, err := runtimeDynamicStrings(file, elf.DT_NEEDED)
	if err != nil {
		return nil, err
	}
	searchPaths, err := runtimeELFSearchPaths(file)
	if err != nil {
		return nil, err
	}
	if requireSearchPath && len(searchPaths) == 0 {
		return nil, fmt.Errorf("dynamic ELF has no runtime-confined search path")
	}
	for _, library := range needed {
		if library == "" || path.Base(library) != library {
			return nil, fmt.Errorf("dynamic library name %q is not confined", library)
		}
		resolved, err := resolveRuntimeDependency(tree, searchPaths, library)
		if err != nil {
			return nil, fmt.Errorf("resolve dynamic library %q: %w", library, err)
		}
		dependency, err := openRuntimeSharedObject(ctx, tree, resolved, machine, loader)
		if err != nil {
			return nil, fmt.Errorf("dynamic library %q: %w", library, err)
		}
		dependency.Close()
	}
	return needed, nil
}

func runtimeELFSearchPaths(file *elf.File) ([]string, error) {
	rpaths, err := runtimeDynamicStrings(file, elf.DT_RPATH)
	if err != nil {
		return nil, err
	}
	if len(rpaths) != 0 {
		return nil, fmt.Errorf("dynamic ELF declares DT_RPATH")
	}
	runpaths, err := runtimeDynamicStrings(file, elf.DT_RUNPATH)
	if err != nil {
		return nil, err
	}
	if len(runpaths) > 1 {
		return nil, fmt.Errorf("dynamic ELF declares multiple DT_RUNPATH values")
	}
	if len(runpaths) == 0 {
		return nil, nil
	}
	if runpaths[0] != artifact.RuntimeMountPath+"/lib" {
		return nil, fmt.Errorf(
			"dynamic RUNPATH = %q, want %q",
			runpaths[0],
			artifact.RuntimeMountPath+"/lib",
		)
	}
	return runpaths, nil
}

func resolveRuntimeDependency(
	tree *artifact.Tree,
	searchPaths []string,
	library string,
) (string, error) {
	var failures []error
	for _, directory := range searchPaths {
		relativeDirectory := strings.TrimPrefix(directory, artifact.RuntimeMountPath+"/")
		directoryEntry, _, err := resolveRuntimeLibrary(tree, relativeDirectory)
		if err != nil {
			return "", fmt.Errorf("search directory %q: %w", directory, err)
		}
		if directoryEntry.Kind != artifact.EntryDirectory {
			return "", fmt.Errorf("search path %q is not a directory", directory)
		}
		_, resolvedPath, err := resolveRuntimeLibrary(
			tree,
			path.Join(relativeDirectory, library),
		)
		if err == nil {
			return resolvedPath, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", directory, err))
	}
	return "", errors.Join(failures...)
}

func runtimeELFTarget(architecture definition.RuntimeArchitecture) (elf.Machine, string, error) {
	if architecture == definition.ArchitectureX8664 {
		return elf.EM_X86_64, artifact.RuntimeMountPath + "/lib/ld-linux-x86-64.so.2", nil
	}
	return elf.EM_NONE, "", fmt.Errorf("runtime architecture %q is unsupported", architecture)
}

func runtimeELFInterpreter(file *elf.File) (string, bool, error) {
	var interpreter string
	count := 0
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		count++
		if count > 1 {
			return "", false, fmt.Errorf("ELF declares multiple interpreters")
		}
		raw, err := io.ReadAll(io.LimitReader(program.Open(), safepath.TreePathBytes+1))
		if err != nil {
			return "", false, fmt.Errorf("read ELF interpreter: %w", err)
		}
		if len(raw) == 0 || len(raw) > safepath.TreePathBytes || raw[len(raw)-1] != 0 {
			return "", false, fmt.Errorf("ELF interpreter is not a bounded NUL-terminated path")
		}
		value := string(raw[:len(raw)-1])
		if strings.IndexByte(value, 0) >= 0 {
			return "", false, fmt.Errorf("ELF interpreter contains an embedded NUL")
		}
		interpreter = value
	}
	return interpreter, count == 1, nil
}

func runtimeDynamicStrings(file *elf.File, tag elf.DynTag) ([]string, error) {
	values, err := file.DynString(tag)
	if errors.Is(err, elf.ErrNoSymbols) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ELF dynamic tag %s: %w", tag, err)
	}
	return values, nil
}

func resolveRuntimeLibrary(
	tree *artifact.Tree,
	value string,
) (artifact.Entry, string, error) {
	if value != "lib" && !strings.HasPrefix(value, "lib/") {
		return artifact.Entry{}, "", fmt.Errorf("runtime library path %q escapes lib", value)
	}
	pending := strings.Split(value, "/")
	resolved := make([]string, 0, len(pending))
	visited := make(map[string]struct{})
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case ".":
			continue
		case "..":
			if len(resolved) <= 1 {
				return artifact.Entry{}, "", fmt.Errorf("runtime library path %q escapes lib", value)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		if candidate != "lib" && !strings.HasPrefix(candidate, "lib/") {
			return artifact.Entry{}, "", fmt.Errorf("runtime library path %q escapes lib", value)
		}
		entry, exists := tree.Lookup(candidate)
		if !exists {
			return artifact.Entry{}, "", fmt.Errorf("runtime library path %q is missing", candidate)
		}
		if entry.Kind == artifact.EntrySymlink {
			hops++
			if hops > safepath.TreeLinkHops {
				return artifact.Entry{}, "", fmt.Errorf(
					"runtime library path %q exceeds %d link hops",
					value,
					safepath.TreeLinkHops,
				)
			}
			if path.IsAbs(entry.LinkTarget) {
				return artifact.Entry{}, "", fmt.Errorf(
					"runtime library link %q has an absolute target",
					candidate,
				)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return artifact.Entry{}, "", fmt.Errorf(
					"runtime library path %q contains a link cycle",
					value,
				)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.Kind != artifact.EntryDirectory {
			return artifact.Entry{}, "", fmt.Errorf(
				"runtime library path %q traverses non-directory %q",
				value,
				candidate,
			)
		}
		if len(pending) == 0 {
			return entry, candidate, nil
		}
	}
	return artifact.Entry{}, "", fmt.Errorf("runtime library path %q is empty", value)
}

func runtimeFileHasELFMagic(
	ctx context.Context,
	tree *artifact.Tree,
	filePath string,
) (bool, error) {
	reader, err := tree.Open(ctx, filePath)
	if err != nil {
		return false, fmt.Errorf("open %q: %w", filePath, err)
	}
	defer reader.Close()
	var magic [4]byte
	count, err := io.ReadFull(reader, magic[:])
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %q ELF magic: %w", filePath, err)
	}
	return count == len(magic) && string(magic[:]) == "\x7fELF", nil
}
