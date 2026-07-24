package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const (
	runtimeNodePath    = "bin/node"
	runtimeEntryPath   = "helmr/entry.mjs"
	runtimeIndexPath   = "helmr/runtime.json"
	runtimePreloadPath = "helmr/preload.mjs"
	runtimeLibcPath    = "lib/libc.so.6"
)

func VerifyRuntimeArtifact(
	ctx context.Context,
	unitCgroupRoot string,
	leaseIdentity string,
	snapshot *RuntimeArtifactSnapshot,
) (RuntimeIndex, error) {
	if ctx == nil {
		return RuntimeIndex{}, fmt.Errorf("runtime verification context is nil")
	}
	source, descriptor, err := snapshot.verifier()
	if err != nil {
		return RuntimeIndex{}, err
	}
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return RuntimeIndex{}, err
	}
	if descriptor.SizeBytes > maxRuntimePhysicalBytes {
		return RuntimeIndex{}, fmt.Errorf(
			"runtime Artifact size exceeds %d bytes",
			maxRuntimePhysicalBytes,
		)
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            runtimeVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{source},
	})
	if err != nil {
		return RuntimeIndex{}, err
	}
	switch result.kind {
	case verifierVerified:
		return verifiedRuntimeResult(result.payload, descriptor)
	case verifierInvalid:
		return RuntimeIndex{}, &verifierInvalidError{diagnostic: result.diagnostic}
	case verifierFailed:
		return RuntimeIndex{}, errors.New("runtime verifier failed")
	default:
		return RuntimeIndex{}, fmt.Errorf(
			"runtime verifier returned unknown outcome %d",
			result.kind,
		)
	}
}

func verifiedRuntimeResult(
	payload []byte,
	descriptor RuntimeDescriptor,
) (RuntimeIndex, error) {
	index, err := ParseRuntimeIndex(payload)
	if err != nil {
		return RuntimeIndex{}, fmt.Errorf("parse verified Runtime index: %w", err)
	}
	if index.Architecture != descriptor.Architecture {
		return RuntimeIndex{}, fmt.Errorf(
			"runtime index architecture = %q, descriptor declares %q",
			index.Architecture,
			descriptor.Architecture,
		)
	}
	if index.RuntimeAPIVersion != descriptor.RuntimeAPIVersion {
		return RuntimeIndex{}, fmt.Errorf(
			"runtime index runtimeApiVersion = %q, descriptor declares %q",
			index.RuntimeAPIVersion,
			descriptor.RuntimeAPIVersion,
		)
	}
	return index, nil
}

func verifyRuntimeArtifactReader(
	ctx context.Context,
	source io.ReaderAt,
	size int64,
) (RuntimeIndex, error) {
	if source == nil {
		return RuntimeIndex{}, fmt.Errorf("runtime Artifact reader is nil")
	}
	if size < 1 || size > maxRuntimePhysicalBytes {
		return RuntimeIndex{}, fmt.Errorf(
			"runtime Artifact size is outside [1,%d]",
			maxRuntimePhysicalBytes,
		)
	}
	digest, err := digestRuntimeArtifact(ctx, source, size)
	if err != nil {
		return RuntimeIndex{}, err
	}
	reader, err := newSquashFSArtifactReader(
		ctx,
		source,
		size,
		runtimeArtifact,
	)
	if err != nil {
		return RuntimeIndex{}, err
	}
	return verifyRuntimeArtifact(ctx, artifactInput{
		Digest:    digest,
		SizeBytes: size,
		MediaType: RuntimeArtifactMediaType,
		Reader:    reader,
	})
}

func verifyRuntimeArtifact(
	ctx context.Context,
	artifact artifactInput,
) (RuntimeIndex, error) {
	if err := validateArtifactDescriptor(artifact, runtimeArtifact); err != nil {
		return RuntimeIndex{}, err
	}
	inspected, err := inspectArtifact(
		ctx,
		artifact.Reader,
		runtimeArtifact,
		maxRuntimeLogicalBytes,
		artifact.SizeBytes,
	)
	if err != nil {
		return RuntimeIndex{}, fmt.Errorf("runtime Artifact: %w", err)
	}
	return verifyRuntimeLayout(ctx, inspected)
}

func verifyRuntimeLayout(
	ctx context.Context,
	artifact *inspectedArtifact,
) (RuntimeIndex, error) {
	index, err := verifyRuntimeTopology(ctx, artifact)
	if err != nil {
		return RuntimeIndex{}, err
	}
	if err := verifyRuntimeExecutables(ctx, artifact, index.Architecture); err != nil {
		return RuntimeIndex{}, err
	}
	return index, nil
}

func verifyRuntimeTopology(
	ctx context.Context,
	artifact *inspectedArtifact,
) (RuntimeIndex, error) {
	requiredDirectories := []string{".", "bin", "helmr", "lib"}
	for _, required := range requiredDirectories {
		if _, err := artifact.require(required, artifactEntryDirectory); err != nil {
			return RuntimeIndex{}, fmt.Errorf("runtime layout: %w", err)
		}
	}
	requiredFiles := map[string]uint32{
		runtimeNodePath:    0755,
		runtimeEntryPath:   0644,
		runtimeIndexPath:   0644,
		runtimePreloadPath: 0644,
		runtimeLibcPath:    0644,
	}
	for required, mode := range requiredFiles {
		entry, err := artifact.require(required, artifactEntryRegular)
		if err != nil {
			return RuntimeIndex{}, fmt.Errorf("runtime layout: %w", err)
		}
		if entry.Mode != mode {
			return RuntimeIndex{}, fmt.Errorf(
				"runtime path %q mode = %#o, want %#o",
				required,
				entry.Mode,
				mode,
			)
		}
	}
	for _, entry := range artifact.ordered {
		if err := validateRuntimePath(entry, requiredFiles); err != nil {
			return RuntimeIndex{}, err
		}
	}
	indexRaw, err := artifact.read(ctx, runtimeIndexPath, maxRuntimeDocumentBytes)
	if err != nil {
		return RuntimeIndex{}, err
	}
	index, err := ParseRuntimeIndex(indexRaw)
	if err != nil {
		return RuntimeIndex{}, err
	}
	return index, nil
}

func validateRuntimePath(entry artifactEntry, required map[string]uint32) error {
	switch entry.Path {
	case ".", "bin", "helmr", "lib":
		return nil
	}
	if _, exists := required[entry.Path]; exists {
		return nil
	}
	if entry.Path == "bin" || strings.HasPrefix(entry.Path, "bin/") {
		return fmt.Errorf("runtime contains unlisted bin path %q", entry.Path)
	}
	if entry.Path == "helmr" || strings.HasPrefix(entry.Path, "helmr/") {
		return fmt.Errorf("runtime contains unlisted helmr path %q", entry.Path)
	}
	if entry.Path == "lib" || strings.HasPrefix(entry.Path, "lib/") {
		return nil
	}
	return fmt.Errorf("runtime contains unlisted top-level path %q", entry.Path)
}

func verifyRuntimeExecutables(
	ctx context.Context,
	artifact *inspectedArtifact,
	architecture RuntimeArchitecture,
) error {
	machine, loader, err := runtimeELFTarget(architecture)
	if err != nil {
		return err
	}
	if err := verifyRuntimeExecutable(
		ctx,
		artifact,
		runtimeNodePath,
		machine,
		loader,
		loader,
		true,
	); err != nil {
		return fmt.Errorf("runtime Node: %w", err)
	}
	loaderPath := strings.TrimPrefix(loader, runtimeMountPath+"/")
	loaderEntry, err := artifact.require(loaderPath, artifactEntryRegular)
	if err != nil {
		return fmt.Errorf("runtime loader: %w", err)
	}
	if loaderEntry.Mode != 0755 {
		return fmt.Errorf("runtime loader mode = %#o, want %#o", loaderEntry.Mode, 0755)
	}
	if err := verifyRuntimeLoader(
		ctx,
		artifact,
		loaderPath,
		machine,
		loader,
	); err != nil {
		return fmt.Errorf("runtime loader: %w", err)
	}
	for _, entry := range artifact.ordered {
		if entry.Kind == artifactEntrySymlink && strings.HasPrefix(entry.Path, "lib/") {
			if _, _, err := resolveRuntimeLibrary(artifact, entry.Path); err != nil {
				return err
			}
		}
		if entry.Kind != artifactEntryRegular || !strings.HasPrefix(entry.Path, "lib/") {
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
		isELF, err := runtimeFileHasELFMagic(ctx, artifact, entry.Path)
		if err != nil {
			return err
		}
		if !isELF {
			continue
		}
		if err := verifyRuntimeSharedObject(
			ctx,
			artifact,
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
	artifact *inspectedArtifact,
	filePath string,
	machine elf.Machine,
	loader string,
) error {
	file, err := openRuntimeSharedObject(ctx, artifact, filePath, machine, loader)
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
	artifact *inspectedArtifact,
	filePath string,
	machine elf.Machine,
	interpreter string,
	loader string,
	requireDynamic bool,
) error {
	file, err := openRuntimeELF(ctx, artifact, filePath, machine)
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
		artifact,
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
	artifact *inspectedArtifact,
	filePath string,
	machine elf.Machine,
	loader string,
) error {
	file, err := openRuntimeSharedObject(ctx, artifact, filePath, machine, loader)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = verifyRuntimeDynamicClosure(ctx, artifact, file, machine, loader, true)
	return err
}

func openRuntimeSharedObject(
	ctx context.Context,
	artifact *inspectedArtifact,
	filePath string,
	machine elf.Machine,
	loader string,
) (*elf.File, error) {
	file, err := openRuntimeELF(ctx, artifact, filePath, machine)
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
	artifact *inspectedArtifact,
	filePath string,
	machine elf.Machine,
) (*elf.File, error) {
	raw, err := artifact.read(ctx, filePath, maxArtifactFileSize)
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
	artifact *inspectedArtifact,
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
		resolved, err := resolveRuntimeDependency(artifact, searchPaths, library)
		if err != nil {
			return nil, fmt.Errorf("resolve dynamic library %q: %w", library, err)
		}
		dependency, err := openRuntimeSharedObject(ctx, artifact, resolved, machine, loader)
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
	if runpaths[0] != runtimeMountPath+"/lib" {
		return nil, fmt.Errorf(
			"dynamic RUNPATH = %q, want %q",
			runpaths[0],
			runtimeMountPath+"/lib",
		)
	}
	return runpaths, nil
}

func resolveRuntimeDependency(
	artifact *inspectedArtifact,
	searchPaths []string,
	library string,
) (string, error) {
	var failures []error
	for _, directory := range searchPaths {
		relativeDirectory := strings.TrimPrefix(directory, runtimeMountPath+"/")
		directoryEntry, _, err := resolveRuntimeLibrary(artifact, relativeDirectory)
		if err != nil {
			return "", fmt.Errorf("search directory %q: %w", directory, err)
		}
		if directoryEntry.Kind != artifactEntryDirectory {
			return "", fmt.Errorf("search path %q is not a directory", directory)
		}
		_, resolvedPath, err := resolveRuntimeLibrary(
			artifact,
			path.Join(relativeDirectory, library),
		)
		if err == nil {
			return resolvedPath, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", directory, err))
	}
	return "", errors.Join(failures...)
}

func runtimeELFTarget(architecture RuntimeArchitecture) (elf.Machine, string, error) {
	switch architecture {
	case ArchitectureX8664:
		return elf.EM_X86_64, runtimeMountPath + "/lib/ld-linux-x86-64.so.2", nil
	case ArchitectureAArch64:
		return elf.EM_AARCH64, runtimeMountPath + "/lib/ld-linux-aarch64.so.1", nil
	default:
		return elf.EM_NONE, "", fmt.Errorf("runtime architecture %q is unsupported", architecture)
	}
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
		raw, err := io.ReadAll(io.LimitReader(program.Open(), maxMountedArtifactPathBytes+1))
		if err != nil {
			return "", false, fmt.Errorf("read ELF interpreter: %w", err)
		}
		if len(raw) == 0 || len(raw) > maxMountedArtifactPathBytes || raw[len(raw)-1] != 0 {
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
	artifact *inspectedArtifact,
	value string,
) (artifactEntry, string, error) {
	if value != "lib" && !strings.HasPrefix(value, "lib/") {
		return artifactEntry{}, "", fmt.Errorf("runtime library path %q escapes lib", value)
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
				return artifactEntry{}, "", fmt.Errorf("runtime library path %q escapes lib", value)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		if candidate != "lib" && !strings.HasPrefix(candidate, "lib/") {
			return artifactEntry{}, "", fmt.Errorf("runtime library path %q escapes lib", value)
		}
		entry, exists := artifact.entries[candidate]
		if !exists {
			return artifactEntry{}, "", fmt.Errorf("runtime library path %q is missing", candidate)
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return artifactEntry{}, "", fmt.Errorf(
					"runtime library path %q exceeds %d link hops",
					value,
					maxSymlinkHops,
				)
			}
			if path.IsAbs(entry.LinkTarget) {
				return artifactEntry{}, "", fmt.Errorf(
					"runtime library link %q has an absolute target",
					candidate,
				)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return artifactEntry{}, "", fmt.Errorf(
					"runtime library path %q contains a link cycle",
					value,
				)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.Kind != artifactEntryDirectory {
			return artifactEntry{}, "", fmt.Errorf(
				"runtime library path %q traverses non-directory %q",
				value,
				candidate,
			)
		}
		if len(pending) == 0 {
			return entry, candidate, nil
		}
	}
	return artifactEntry{}, "", fmt.Errorf("runtime library path %q is empty", value)
}

func runtimeFileHasELFMagic(
	ctx context.Context,
	artifact *inspectedArtifact,
	filePath string,
) (bool, error) {
	reader, err := artifact.reader.Open(ctx, filePath)
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

func digestRuntimeArtifact(
	ctx context.Context,
	source io.ReaderAt,
	size int64,
) (string, error) {
	hash := sha256.New()
	reader := io.NewSectionReader(source, 0, size)
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("hash runtime Artifact: %w", err)
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			if _, writeErr := hash.Write(buffer[:count]); writeErr != nil {
				return "", fmt.Errorf("hash runtime Artifact: %w", writeErr)
			}
			total += int64(count)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("hash runtime Artifact: %w", err)
		}
	}
	if total != size {
		return "", fmt.Errorf("runtime Artifact size = %d, descriptor declares %d", total, size)
	}
	var trailing [1]byte
	if count, err := source.ReadAt(trailing[:], size); count != 0 || err == nil {
		return "", fmt.Errorf("runtime Artifact exceeds descriptor size %d", size)
	} else if !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("check runtime Artifact size: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
