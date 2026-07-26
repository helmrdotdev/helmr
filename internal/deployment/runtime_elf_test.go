package deployment

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"strings"
	"testing"
)

func TestRuntimeELFFixturesAcceptSupportedArchitectures(t *testing.T) {
	for _, architecture := range []RuntimeArchitecture{
		ArchitectureX8664,
	} {
		t.Run(string(architecture), func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, architecture)
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				architecture,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeELFRequiresExactRUNPATH(t *testing.T) {
	machine, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	tests := map[string]testELF64Spec{
		"nested RUNPATH": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader},
			needed:       []string{"libnode.so"},
			runpath:      []string{runtimeMountPath + "/lib/nested"},
		},
		"RPATH": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader},
			needed:       []string{"libnode.so"},
			rpath:        []string{runtimeMountPath + "/lib"},
		},
		"multiple RUNPATHs": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader},
			needed:       []string{"libnode.so"},
			runpath: []string{
				runtimeMountPath + "/lib",
				runtimeMountPath + "/lib",
			},
		},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
			replaceMemoryArtifactFile(
				t,
				artifact,
				runtimeNodePath,
				buildTestELF64(t, spec),
			)
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("runtime verifier accepted a non-canonical search path")
			}
		})
	}
}

func TestRuntimeELFRejectsInvalidDynamicDependencyFiles(t *testing.T) {
	machine, _ := testRuntimeELFTarget(t, ArchitectureX8664)
	tests := map[string][]byte{
		"non-ELF": []byte("not an ELF shared object"),
		"ET_EXEC": buildTestELF64(t, testELF64Spec{
			machine:  machine,
			fileType: elf.ET_EXEC,
		}),
	}
	for name, dependency := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
			replaceMemoryArtifactFile(t, artifact, "lib/libnode.so", dependency)
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("runtime verifier accepted an invalid dynamic dependency")
			}
		})
	}
}

func TestRuntimeELFChecksTransitiveClosureForEveryLibrary(t *testing.T) {
	artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
	machine, _ := testRuntimeELFTarget(t, ArchitectureX8664)
	replaceMemoryArtifactFile(t, artifact, "lib/libnode.so", buildTestELF64(t, testELF64Spec{
		machine:  machine,
		fileType: elf.ET_DYN,
		needed:   []string{"libmissing.so"},
		runpath:  []string{runtimeMountPath + "/lib"},
	}))

	if err := verifyRuntimeExecutables(
		context.Background(),
		inspectRuntimeELFArtifact(t, artifact),
		ArchitectureX8664,
	); err == nil {
		t.Fatal("runtime verifier accepted a library with an unresolved transitive dependency")
	}
}

func TestRuntimeELFResolvesDirectorySymlinkComponents(t *testing.T) {
	artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
	machine, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	artifact.addDirectory("lib/real")
	artifact.addFile(
		"lib/real/libnode.so",
		buildTestELF64(t, testELF64Spec{
			machine:  machine,
			fileType: elf.ET_DYN,
			runpath:  []string{runtimeMountPath + "/lib"},
		}),
		0644,
	)
	artifact.addLink("lib/alias", "real")
	delete(artifact.files, "lib/libnode.so")
	artifact.mutate("lib/libnode.so", func(entry *artifactEntry) {
		entry.Kind = artifactEntrySymlink
		entry.Form = squashFSBasicSymlinkForm
		entry.Mode = 0777
		entry.SizeBytes = int64(len("alias/libnode.so"))
		entry.LinkTarget = "alias/libnode.so"
	})
	replaceMemoryArtifactFile(t, artifact, runtimeNodePath, buildTestELF64(t, testELF64Spec{
		machine:      machine,
		fileType:     elf.ET_DYN,
		interpreters: []string{loader},
		needed:       []string{"libnode.so"},
		runpath:      []string{runtimeMountPath + "/lib"},
	}))

	if err := verifyRuntimeExecutables(
		context.Background(),
		inspectRuntimeELFArtifact(t, artifact),
		ArchitectureX8664,
	); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeELFRejectsDuplicateInterpreter(t *testing.T) {
	artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
	machine, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	replaceMemoryArtifactFile(t, artifact, runtimeNodePath, buildTestELF64(t, testELF64Spec{
		machine:      machine,
		fileType:     elf.ET_DYN,
		interpreters: []string{loader, loader},
		needed:       []string{"libnode.so"},
		runpath:      []string{runtimeMountPath + "/lib"},
	}))

	if err := verifyRuntimeExecutables(
		context.Background(),
		inspectRuntimeELFArtifact(t, artifact),
		ArchitectureX8664,
	); err == nil {
		t.Fatal("runtime verifier accepted multiple PT_INTERP headers")
	}
}

func TestRuntimeELFRequiresConfinedSharedObjectIdentity(t *testing.T) {
	machine, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	otherMachine := elf.EM_AARCH64
	tests := map[string]struct {
		path string
		spec testELF64Spec
	}{
		"loader architecture": {
			path: strings.TrimPrefix(loader, runtimeMountPath+"/"),
			spec: testELF64Spec{machine: otherMachine, fileType: elf.ET_DYN},
		},
		"loader type": {
			path: strings.TrimPrefix(loader, runtimeMountPath+"/"),
			spec: testELF64Spec{machine: machine, fileType: elf.ET_EXEC},
		},
		"loader interpreter": {
			path: strings.TrimPrefix(loader, runtimeMountPath+"/"),
			spec: testELF64Spec{
				machine:      machine,
				fileType:     elf.ET_DYN,
				interpreters: []string{loader},
			},
		},
		"dependency architecture": {
			path: "lib/libnode.so",
			spec: testELF64Spec{machine: otherMachine, fileType: elf.ET_DYN},
		},
		"dependency type": {
			path: "lib/libnode.so",
			spec: testELF64Spec{machine: machine, fileType: elf.ET_EXEC},
		},
		"dependency interpreter": {
			path: "lib/libnode.so",
			spec: testELF64Spec{
				machine:      machine,
				fileType:     elf.ET_DYN,
				interpreters: []string{loader},
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
			replaceMemoryArtifactFile(t, artifact, test.path, buildTestELF64(t, test.spec))
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("runtime verifier accepted a shared object outside the identity contract")
			}
		})
	}
}

func TestRuntimeELFRequiresLoaderBootstrapShape(t *testing.T) {
	machine, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	loaderPath := strings.TrimPrefix(loader, runtimeMountPath+"/")
	tests := map[string]testELF64Spec{
		"dynamic dependency": {
			machine:  machine,
			fileType: elf.ET_DYN,
			needed:   []string{"libnode.so"},
		},
		"RPATH": {
			machine:  machine,
			fileType: elf.ET_DYN,
			rpath:    []string{runtimeMountPath + "/lib"},
		},
		"RUNPATH": {
			machine:  machine,
			fileType: elf.ET_DYN,
			runpath:  []string{runtimeMountPath + "/lib"},
		},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
			replaceMemoryArtifactFile(t, artifact, loaderPath, buildTestELF64(t, spec))
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("runtime verifier accepted a patched loader bootstrap")
			}
		})
	}
}

func TestRuntimeELFRequiresExactLibcIdentity(t *testing.T) {
	machine, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	otherLoader := runtimeMountPath + "/lib/ld-linux-aarch64.so.1"
	tests := map[string]testELF64Spec{
		"missing interpreter": {
			machine:  machine,
			fileType: elf.ET_DYN,
			sonames:  []string{"libc.so.6"},
			runpath:  []string{runtimeMountPath + "/lib"},
		},
		"empty interpreter": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{""},
			sonames:      []string{"libc.so.6"},
			runpath:      []string{runtimeMountPath + "/lib"},
		},
		"wrong interpreter": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{otherLoader},
			sonames:      []string{"libc.so.6"},
			runpath:      []string{runtimeMountPath + "/lib"},
		},
		"multiple interpreters": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader, loader},
			sonames:      []string{"libc.so.6"},
			runpath:      []string{runtimeMountPath + "/lib"},
		},
		"missing SONAME": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader},
			runpath:      []string{runtimeMountPath + "/lib"},
		},
		"wrong SONAME": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader},
			sonames:      []string{"libc.so.7"},
			runpath:      []string{runtimeMountPath + "/lib"},
		},
		"multiple SONAMEs": {
			machine:      machine,
			fileType:     elf.ET_DYN,
			interpreters: []string{loader},
			sonames:      []string{"libc.so.6", "libc.so.6"},
			runpath:      []string{runtimeMountPath + "/lib"},
		},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
			replaceMemoryArtifactFile(
				t,
				artifact,
				runtimeLibcPath,
				buildTestELF64(t, spec),
			)
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("runtime verifier accepted libc outside the exact identity contract")
			}
		})
	}
}

func TestRuntimeELFRequiresExactLibraryModes(t *testing.T) {
	_, loader := testRuntimeELFTarget(t, ArchitectureX8664)
	tests := map[string]string{
		"loader is not executable": strings.TrimPrefix(loader, runtimeMountPath+"/"),
		"libc is executable":       runtimeLibcPath,
		"dependency is executable": "lib/libnode.so",
	}
	for name, filePath := range tests {
		t.Run(name, func(t *testing.T) {
			artifact := newValidRuntimeELFArtifact(t, ArchitectureX8664)
			artifact.mutate(filePath, func(entry *artifactEntry) {
				if filePath == strings.TrimPrefix(loader, runtimeMountPath+"/") {
					entry.Mode = 0644
				} else {
					entry.Mode = 0755
				}
			})
			if err := verifyRuntimeExecutables(
				context.Background(),
				inspectRuntimeELFArtifact(t, artifact),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("runtime verifier accepted a library outside the mode contract")
			}
		})
	}
}

type testELF64Spec struct {
	machine      elf.Machine
	fileType     elf.Type
	interpreters []string
	needed       []string
	sonames      []string
	rpath        []string
	runpath      []string
}

func buildTestELF64(t *testing.T, spec testELF64Spec) []byte {
	t.Helper()
	if spec.machine == elf.EM_NONE {
		t.Fatal("test ELF machine is unspecified")
	}
	if spec.fileType == elf.ET_NONE {
		t.Fatal("test ELF type is unspecified")
	}

	type dynamicEntry struct {
		tag   elf.DynTag
		value uint64
	}
	dynamicEntries := make(
		[]dynamicEntry,
		0,
		len(spec.needed)+len(spec.sonames)+len(spec.rpath)+len(spec.runpath)+1,
	)
	dynamicStrings := []byte{0}
	addDynamicStrings := func(tag elf.DynTag, values []string) {
		for _, value := range values {
			offset := len(dynamicStrings)
			dynamicStrings = append(dynamicStrings, value...)
			dynamicStrings = append(dynamicStrings, 0)
			dynamicEntries = append(dynamicEntries, dynamicEntry{
				tag:   tag,
				value: uint64(offset),
			})
		}
	}
	addDynamicStrings(elf.DT_NEEDED, spec.needed)
	addDynamicStrings(elf.DT_SONAME, spec.sonames)
	addDynamicStrings(elf.DT_RPATH, spec.rpath)
	addDynamicStrings(elf.DT_RUNPATH, spec.runpath)
	hasDynamic := spec.fileType == elf.ET_DYN || len(dynamicEntries) != 0
	var dynamicRaw []byte
	if hasDynamic {
		dynamicEntries = append(dynamicEntries, dynamicEntry{tag: elf.DT_NULL})
		dynamicRaw = make([]byte, len(dynamicEntries)*16)
		for index, entry := range dynamicEntries {
			offset := index * 16
			binary.LittleEndian.PutUint64(dynamicRaw[offset:], uint64(entry.tag))
			binary.LittleEndian.PutUint64(dynamicRaw[offset+8:], entry.value)
		}
	}

	const (
		elfHeaderSize     = 64
		programHeaderSize = 56
		sectionHeaderSize = 64
	)
	programCount := len(spec.interpreters)
	if hasDynamic {
		programCount++
	}
	offset := alignTestELFOffset(elfHeaderSize+programCount*programHeaderSize, 8)
	interpreterOffsets := make([]int, len(spec.interpreters))
	interpreterRaw := make([][]byte, len(spec.interpreters))
	for index, interpreter := range spec.interpreters {
		interpreterOffsets[index] = offset
		interpreterRaw[index] = append([]byte(interpreter), 0)
		offset += len(interpreterRaw[index])
	}
	offset = alignTestELFOffset(offset, 8)
	dynamicStringsOffset := offset
	if hasDynamic {
		offset += len(dynamicStrings)
	}
	offset = alignTestELFOffset(offset, 8)
	dynamicOffset := offset
	if hasDynamic {
		offset += len(dynamicRaw)
	}
	sectionOffset := alignTestELFOffset(offset, 8)
	sectionCount := 1
	if hasDynamic {
		sectionCount += 2
	}
	raw := make([]byte, sectionOffset+sectionCount*sectionHeaderSize)

	copy(raw[0:4], []byte(elf.ELFMAG))
	raw[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	raw[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	raw[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(raw[16:], uint16(spec.fileType))
	binary.LittleEndian.PutUint16(raw[18:], uint16(spec.machine))
	binary.LittleEndian.PutUint32(raw[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint64(raw[32:], uint64(elfHeaderSize))
	binary.LittleEndian.PutUint64(raw[40:], uint64(sectionOffset))
	binary.LittleEndian.PutUint16(raw[52:], elfHeaderSize)
	binary.LittleEndian.PutUint16(raw[54:], programHeaderSize)
	binary.LittleEndian.PutUint16(raw[56:], uint16(programCount))
	binary.LittleEndian.PutUint16(raw[58:], sectionHeaderSize)
	binary.LittleEndian.PutUint16(raw[60:], uint16(sectionCount))

	programIndex := 0
	for index, interpreter := range interpreterRaw {
		writeTestELFProgramHeader(
			raw[elfHeaderSize+programIndex*programHeaderSize:],
			elf.PT_INTERP,
			interpreterOffsets[index],
			len(interpreter),
			1,
		)
		copy(raw[interpreterOffsets[index]:], interpreter)
		programIndex++
	}
	if hasDynamic {
		writeTestELFProgramHeader(
			raw[elfHeaderSize+programIndex*programHeaderSize:],
			elf.PT_DYNAMIC,
			dynamicOffset,
			len(dynamicRaw),
			8,
		)
		copy(raw[dynamicStringsOffset:], dynamicStrings)
		copy(raw[dynamicOffset:], dynamicRaw)

		writeTestELFSectionHeader(
			raw[sectionOffset+sectionHeaderSize:],
			elf.SHT_STRTAB,
			dynamicStringsOffset,
			len(dynamicStrings),
			0,
			1,
			0,
		)
		writeTestELFSectionHeader(
			raw[sectionOffset+2*sectionHeaderSize:],
			elf.SHT_DYNAMIC,
			dynamicOffset,
			len(dynamicRaw),
			1,
			8,
			16,
		)
	}
	return raw
}

func writeTestELFProgramHeader(
	raw []byte,
	programType elf.ProgType,
	offset int,
	size int,
	alignment uint64,
) {
	binary.LittleEndian.PutUint32(raw, uint32(programType))
	binary.LittleEndian.PutUint32(raw[4:], uint32(elf.PF_R))
	binary.LittleEndian.PutUint64(raw[8:], uint64(offset))
	binary.LittleEndian.PutUint64(raw[32:], uint64(size))
	binary.LittleEndian.PutUint64(raw[40:], uint64(size))
	binary.LittleEndian.PutUint64(raw[48:], alignment)
}

func writeTestELFSectionHeader(
	raw []byte,
	sectionType elf.SectionType,
	offset int,
	size int,
	link uint32,
	alignment uint64,
	entrySize uint64,
) {
	binary.LittleEndian.PutUint32(raw[4:], uint32(sectionType))
	binary.LittleEndian.PutUint64(raw[8:], uint64(elf.SHF_ALLOC))
	binary.LittleEndian.PutUint64(raw[24:], uint64(offset))
	binary.LittleEndian.PutUint64(raw[32:], uint64(size))
	binary.LittleEndian.PutUint32(raw[40:], link)
	binary.LittleEndian.PutUint64(raw[48:], alignment)
	binary.LittleEndian.PutUint64(raw[56:], entrySize)
}

func alignTestELFOffset(value, alignment int) int {
	return (value + alignment - 1) &^ (alignment - 1)
}

func newValidRuntimeELFArtifact(
	t *testing.T,
	architecture RuntimeArchitecture,
) *memoryArtifact {
	t.Helper()
	machine, loader := testRuntimeELFTarget(t, architecture)
	loaderPath := strings.TrimPrefix(loader, runtimeMountPath+"/")
	artifact := newMemoryArtifact()
	artifact.addDirectory("bin")
	artifact.addDirectory("helmr")
	artifact.addDirectory("lib")
	artifact.addFile(runtimeNodePath, buildTestELF64(t, testELF64Spec{
		machine:      machine,
		fileType:     elf.ET_DYN,
		interpreters: []string{loader},
		needed:       []string{"libnode.so"},
		runpath:      []string{runtimeMountPath + "/lib"},
	}), 0755)
	artifact.addFile(loaderPath, buildTestELF64(t, testELF64Spec{
		machine:  machine,
		fileType: elf.ET_DYN,
	}), 0755)
	artifact.addFile("lib/libnode.so", buildTestELF64(t, testELF64Spec{
		machine:  machine,
		fileType: elf.ET_DYN,
		runpath:  []string{runtimeMountPath + "/lib"},
	}), 0644)
	artifact.addFile(runtimeLibcPath, buildTestELF64(t, testELF64Spec{
		machine:      machine,
		fileType:     elf.ET_DYN,
		interpreters: []string{loader},
		sonames:      []string{"libc.so.6"},
		runpath:      []string{runtimeMountPath + "/lib"},
	}), 0644)
	return artifact
}

func testRuntimeELFTarget(
	t *testing.T,
	architecture RuntimeArchitecture,
) (elf.Machine, string) {
	t.Helper()
	machine, loader, err := runtimeELFTarget(architecture)
	if err != nil {
		t.Fatal(err)
	}
	return machine, loader
}

func replaceMemoryArtifactFile(
	t *testing.T,
	artifact *memoryArtifact,
	filePath string,
	raw []byte,
) {
	t.Helper()
	if _, exists := artifact.files[filePath]; !exists {
		t.Fatalf("test artifact file %q is absent", filePath)
	}
	artifact.files[filePath] = append([]byte(nil), raw...)
	artifact.mutate(filePath, func(entry *artifactEntry) {
		entry.SizeBytes = int64(len(raw))
	})
}

func inspectRuntimeELFArtifact(t *testing.T, artifact *memoryArtifact) *inspectedArtifact {
	t.Helper()
	inspected, err := inspectArtifact(
		context.Background(),
		artifact,
		runtimeArtifact,
		maxRuntimeLogicalBytes,
		squashFSPhysicalAlign,
	)
	if err != nil {
		t.Fatalf("inspect runtime ELF test artifact: %v", err)
	}
	return inspected
}
