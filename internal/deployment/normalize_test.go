package deployment

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeBunArchive(t *testing.T) {
	executable := managerTestELF(ArchitectureX8664, 256)
	archive := managerTestZIP(t, []managerTestZIPEntry{
		{name: "bun-linux-x64-baseline/", directory: true},
		{name: "bun-linux-x64-baseline/bun", content: executable},
	})
	request, source := managerNormalizeRequest(
		t,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
		ArchitectureX8664,
		archive,
	)
	response := new(bytes.Buffer)
	if err := NormalizeManagerArchive(response, request, source, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	entries, terminal := managerReadNormalizedResponse(t, response.Bytes(), request)
	if terminal.Status != ManagerAcquireStatusOK {
		t.Fatalf("status = %q", terminal.Status)
	}
	if !slices.Equal(managerNormalizedNames(entries), []string{"bin", "bin/bun"}) {
		t.Fatalf("entries = %#v", entries)
	}
	if !bytes.Equal(entries["bin/bun"].content, executable) ||
		entries["bin/bun"].mode != 0555 {
		t.Fatalf("Bun executable = %#v", entries["bin/bun"])
	}
}

func TestNormalizeNPMArchive(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{"name":"npm"}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("#!/usr/bin/env node\n")},
		{name: "package/docs/readme.md", mode: 0644, content: []byte("npm")},
	})
	request, source := managerNormalizeRequest(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
	)
	scratch := t.TempDir()
	response := new(bytes.Buffer)
	if err := NormalizeManagerArchive(response, request, source, scratch); err != nil {
		t.Fatal(err)
	}
	entries, terminal := managerReadNormalizedResponse(t, response.Bytes(), request)
	if terminal.Status != ManagerAcquireStatusOK {
		t.Fatalf("status = %q", terminal.Status)
	}
	want := []string{
		"lib",
		"lib/npm",
		"lib/npm/bin",
		"lib/npm/bin/npm-cli.js",
		"lib/npm/docs",
		"lib/npm/docs/readme.md",
		"lib/npm/package.json",
	}
	if !slices.Equal(managerNormalizedNames(entries), want) {
		t.Fatalf("entry names = %#v, want %#v", managerNormalizedNames(entries), want)
	}
	if entries["lib/npm/bin/npm-cli.js"].mode != 0555 ||
		entries["lib/npm/docs/readme.md"].mode != 0444 {
		t.Fatalf("normalized modes = %#v", entries)
	}
	files, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("normalization scratch retains files: %#v", files)
	}
}

func TestNormalizeManagerArchiveRejectsWrongBunArchitecture(t *testing.T) {
	archive := managerTestZIP(t, []managerTestZIPEntry{
		{name: "bun-linux-aarch64/", directory: true},
		{
			name:    "bun-linux-aarch64/bun",
			content: managerTestELF(ArchitectureX8664, 128),
		},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestValidateManagerELFRequiresClosedLoaderContract(t *testing.T) {
	for _, architecture := range []RuntimeArchitecture{
		ArchitectureAArch64,
		ArchitectureX8664,
	} {
		content := managerTestELF(architecture, 0)
		if err := validateManagerELF(
			bytes.NewReader(content),
			int64(len(content)),
			architecture,
		); err != nil {
			t.Fatalf("%s: %v", architecture, err)
		}
	}

	tests := []struct {
		name   string
		mutate func(*managerTestELFConfig)
	}{
		{
			name: "interpreter",
			mutate: func(config *managerTestELFConfig) {
				config.interpreter = "/lib64/ld-linux-x86-64.so.3"
			},
		},
		{
			name: "missing interpreter",
			mutate: func(config *managerTestELFConfig) {
				config.interpreters = 0
			},
		},
		{
			name: "duplicate interpreter",
			mutate: func(config *managerTestELFConfig) {
				config.interpreters = 2
			},
		},
		{
			name: "missing dynamic",
			mutate: func(config *managerTestELFConfig) {
				config.dynamics = 0
			},
		},
		{
			name: "duplicate dynamic",
			mutate: func(config *managerTestELFConfig) {
				config.dynamics = 2
			},
		},
		{
			name: "rpath",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_RPATH},
				}
			},
		},
		{
			name: "runpath",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_RUNPATH},
				}
			},
		},
		{
			name: "audit",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_AUDIT},
				}
			},
		},
		{
			name: "dependency audit",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_DEPAUDIT},
				}
			},
		},
		{
			name: "filter",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_FILTER},
				}
			},
		},
		{
			name: "auxiliary",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_AUXILIARY},
				}
			},
		},
		{
			name: "missing library",
			mutate: func(config *managerTestELFConfig) {
				config.needed = config.needed[:len(config.needed)-1]
			},
		},
		{
			name: "extra library",
			mutate: func(config *managerTestELFConfig) {
				config.needed = append(config.needed, "libz.so.1")
			},
		},
		{
			name: "duplicate library",
			mutate: func(config *managerTestELFConfig) {
				config.needed = append(config.needed, config.needed[0])
			},
		},
		{
			name: "duplicate string table",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_STRTAB, value: 1},
				}
			},
		},
		{
			name: "duplicate string size",
			mutate: func(config *managerTestELFConfig) {
				config.extra = []managerTestELFDynamic{
					{tag: elf.DT_STRSZ, value: 1},
				}
			},
		},
		{
			name: "missing terminator",
			mutate: func(config *managerTestELFConfig) {
				config.includeNull = false
			},
		},
		{
			name: "unterminated library",
			mutate: func(config *managerTestELFConfig) {
				config.terminateStrings = false
			},
		},
		{
			name: "divergent dynamic mapping",
			mutate: func(config *managerTestELFConfig) {
				config.dynamicAddressDelta = 16
			},
		},
		{
			name: "ambiguous load mapping",
			mutate: func(config *managerTestELFConfig) {
				config.loads = 2
			},
		},
		{
			name: "partial load mapping",
			mutate: func(config *managerTestELFConfig) {
				config.partialLoad = true
			},
		},
		{
			name: "invalid needed offset",
			mutate: func(config *managerTestELFConfig) {
				config.invalidNeededOffset = true
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := managerTestELFDefaults(ArchitectureX8664)
			test.mutate(&config)
			content := managerTestELFDocument(config, 0)
			if err := validateManagerELF(
				bytes.NewReader(content),
				int64(len(content)),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("validateManagerELF accepted an open loader contract")
			}
		})
	}

	header := managerTestELFHeader(ArchitectureX8664, 64, 0)
	if err := validateManagerELF(
		bytes.NewReader(header),
		int64(len(header)),
		ArchitectureX8664,
	); err == nil {
		t.Fatal("validateManagerELF accepted a header-only ELF")
	}

	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "program count",
			mutate: func(content []byte) {
				binary.LittleEndian.PutUint16(content[56:58], ^uint16(0))
			},
		},
		{
			name: "program offset",
			mutate: func(content []byte) {
				binary.LittleEndian.PutUint64(content[32:40], ^uint64(0))
			},
		},
		{
			name: "interpreter offset",
			mutate: func(content []byte) {
				offset := managerTestELFProgramHeader(content, elf.PT_INTERP)
				binary.LittleEndian.PutUint64(content[offset+8:offset+16], ^uint64(0))
			},
		},
		{
			name: "interpreter size",
			mutate: func(content []byte) {
				offset := managerTestELFProgramHeader(content, elf.PT_INTERP)
				binary.LittleEndian.PutUint64(content[offset+32:offset+40], ^uint64(0))
			},
		},
		{
			name: "dynamic offset",
			mutate: func(content []byte) {
				offset := managerTestELFProgramHeader(content, elf.PT_DYNAMIC)
				binary.LittleEndian.PutUint64(content[offset+8:offset+16], ^uint64(0))
			},
		},
		{
			name: "dynamic size",
			mutate: func(content []byte) {
				offset := managerTestELFProgramHeader(content, elf.PT_DYNAMIC)
				binary.LittleEndian.PutUint64(content[offset+32:offset+40], ^uint64(0))
				binary.LittleEndian.PutUint64(content[offset+40:offset+48], ^uint64(0))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := managerTestELF(ArchitectureX8664, 0)
			test.mutate(content)
			if err := validateManagerELF(
				bytes.NewReader(content),
				int64(len(content)),
				ArchitectureX8664,
			); err == nil {
				t.Fatal("validateManagerELF accepted invalid program headers")
			}
		})
	}

	content := managerTestELF(ArchitectureX8664, 0)
	binary.LittleEndian.PutUint64(content[40:48], ^uint64(0))
	binary.LittleEndian.PutUint16(content[58:60], ^uint16(0))
	binary.LittleEndian.PutUint16(content[60:62], ^uint16(0))
	binary.LittleEndian.PutUint16(content[62:64], ^uint16(0))
	if err := validateManagerELF(
		bytes.NewReader(content),
		int64(len(content)),
		ArchitectureX8664,
	); err != nil {
		t.Fatalf("irrelevant section metadata changed admission: %v", err)
	}
}

func TestOfficialBunLoaderContract(t *testing.T) {
	path := os.Getenv("HELMR_BUN_ARCHIVE")
	if path == "" {
		t.Skip("HELMR_BUN_ARCHIVE is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var executable []byte
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() ||
			!strings.HasSuffix(entry.Name, "/bun") {
			continue
		}
		if executable != nil {
			t.Fatal("Bun archive contains multiple executables")
		}
		source, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		executable, err = io.ReadAll(source)
		if closeErr := source.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if executable == nil {
		t.Fatal("Bun archive contains no executable")
	}
	architecture := RuntimeArchitecture(os.Getenv("HELMR_BUN_ARCHITECTURE"))
	request, source := managerNormalizeRequest(
		t,
		PackageManager{Name: PackageManagerBun, Version: "1.3.10"},
		architecture,
		raw,
	)
	response := new(bytes.Buffer)
	if err := NormalizeManagerArchive(
		response,
		request,
		source,
		t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	entries, terminal := managerReadNormalizedResponse(
		t,
		response.Bytes(),
		request,
	)
	if terminal.Status != ManagerAcquireStatusOK {
		t.Fatalf("status = %q", terminal.Status)
	}
	entry, ok := entries["bin/bun"]
	if !ok || entry.mode != 0555 ||
		!bytes.Equal(entry.content, executable) {
		t.Fatal("normalized Bun executable differs from the official payload")
	}
}

func TestNormalizeManagerArchiveRequiresBunRootEntry(t *testing.T) {
	archive := managerTestZIP(t, []managerTestZIPEntry{
		{
			name:    "bun-linux-x64-baseline/bun",
			content: managerTestELF(ArchitectureX8664, 128),
		},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
		ArchitectureX8664,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRejectsZIPTrailingData(t *testing.T) {
	archive := managerTestZIP(t, []managerTestZIPEntry{
		{name: "bun-linux-x64-baseline/", directory: true},
		{
			name:    "bun-linux-x64-baseline/bun",
			content: managerTestELF(ArchitectureX8664, 128),
		},
	})
	archive = append(archive, []byte("trailing")...)
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
		ArchitectureX8664,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRejectsZIPBomb(t *testing.T) {
	archive := managerTestZIP(t, []managerTestZIPEntry{
		{name: "bun-linux-x64-baseline/", directory: true},
		{
			name:    "bun-linux-x64-baseline/bun",
			content: managerTestELF(ArchitectureX8664, 1<<20),
		},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
		ArchitectureX8664,
		archive,
		ManagerAcquireStatusLimitExceeded,
	)
}

func TestNormalizeManagerArchiveRejectsNPMTraversal(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("npm")},
		{name: "package/../escape", mode: 0644, content: []byte("escape")},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRejectsNPMCaseFoldCollision(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("npm")},
		{name: "package/LICENSE", mode: 0644, content: []byte("upper")},
		{name: "package/license", mode: 0644, content: []byte("lower")},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRejectsNPMLink(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("npm")},
		{
			name:     "package/link",
			mode:     0755,
			typeflag: tar.TypeSymlink,
			linkname: "package.json",
		},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRejectsNPMTrailingGzip(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("npm")},
	})
	archive = append(archive, 1)
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRejectsNPMMode(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0600, content: []byte(`{}`)},
		{name: "package/bin/npm-cli.js", mode: 0755, content: []byte("npm")},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRequiresNPMEntrypoint(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

func TestNormalizeManagerArchiveRequiresExecutableNPMEntrypoint(t *testing.T) {
	archive := managerTestTGZ(t, []managerTestTarEntry{
		{name: "package/package.json", mode: 0644, content: []byte(`{}`)},
		{name: "package/bin/npm-cli.js", mode: 0644, content: []byte("npm")},
	})
	managerAssertNormalizeStatus(
		t,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
		archive,
		ManagerAcquireStatusUnsupportedLayout,
	)
}

type managerTestZIPEntry struct {
	name      string
	content   []byte
	directory bool
}

func managerTestZIP(t *testing.T, entries []managerTestZIPEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.directory {
			header.Method = zip.Store
			header.SetMode(os.ModeDir | 0755)
		} else {
			header.SetMode(0755)
		}
		destination, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := destination.Write(entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type managerTestTarEntry struct {
	name     string
	mode     int64
	content  []byte
	typeflag byte
	linkname string
}

func managerTestTGZ(t *testing.T, entries []managerTestTarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	compressor := gzip.NewWriter(&output)
	writer := tar.NewWriter(compressor)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		if err := writer.WriteHeader(&tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Size:     int64(len(entry.content)),
			Typeflag: typeflag,
			Linkname: entry.linkname,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type managerTestELFDynamic struct {
	tag   elf.DynTag
	value uint64
}

type managerTestELFConfig struct {
	architecture        RuntimeArchitecture
	dynamicAddressDelta uint64
	dynamics            int
	extra               []managerTestELFDynamic
	includeNull         bool
	invalidNeededOffset bool
	interpreter         string
	interpreters        int
	loads               int
	needed              []string
	partialLoad         bool
	terminateStrings    bool
}

func managerTestELF(architecture RuntimeArchitecture, size int) []byte {
	return managerTestELFDocument(managerTestELFDefaults(architecture), size)
}

func managerTestELFDefaults(architecture RuntimeArchitecture) managerTestELFConfig {
	interpreter := "/lib64/ld-linux-x86-64.so.2"
	loader := "ld-linux-x86-64.so.2"
	if architecture == ArchitectureAArch64 {
		interpreter = "/lib/ld-linux-aarch64.so.1"
		loader = "ld-linux-aarch64.so.1"
	}
	return managerTestELFConfig{
		architecture: architecture,
		dynamics:     1,
		includeNull:  true,
		interpreter:  interpreter,
		interpreters: 1,
		loads:        1,
		needed: []string{
			"libc.so.6",
			loader,
			"libpthread.so.0",
			"libdl.so.2",
			"libm.so.6",
		},
		terminateStrings: true,
	}
}

func managerTestELFDocument(config managerTestELFConfig, size int) []byte {
	const (
		headerSize  = 64
		programSize = 56
		baseAddress = uint64(0x400000)
	)
	programs := config.loads + config.interpreters + config.dynamics
	if config.partialLoad {
		programs++
	}
	interpreter := append([]byte(config.interpreter), 0)
	interpreterOffset := headerSize + programs*programSize

	stringTable := []byte{0}
	needed := make([]managerTestELFDynamic, 0, len(config.needed))
	for _, library := range config.needed {
		needed = append(needed, managerTestELFDynamic{
			tag:   elf.DT_NEEDED,
			value: uint64(len(stringTable)),
		})
		stringTable = append(stringTable, library...)
		stringTable = append(stringTable, 0)
	}
	if !config.terminateStrings {
		stringTable[len(stringTable)-1] = 'x'
	}
	if config.invalidNeededOffset {
		needed[0].value = uint64(len(stringTable))
	}
	stringOffset := interpreterOffset + len(interpreter)
	dynamicOffset := (stringOffset + len(stringTable) + 7) &^ 7
	dynamic := append(needed, config.extra...)
	dynamic = append(
		dynamic,
		managerTestELFDynamic{
			tag:   elf.DT_STRTAB,
			value: baseAddress + uint64(stringOffset),
		},
		managerTestELFDynamic{
			tag:   elf.DT_STRSZ,
			value: uint64(len(stringTable)),
		},
	)
	if config.includeNull {
		dynamic = append(dynamic, managerTestELFDynamic{tag: elf.DT_NULL})
	}
	minimum := dynamicOffset + len(dynamic)*16
	if size < minimum {
		size = minimum
	}
	content := managerTestELFHeader(config.architecture, size, programs)
	copy(content[interpreterOffset:], interpreter)
	copy(content[stringOffset:], stringTable)
	for index, entry := range dynamic {
		offset := dynamicOffset + index*16
		binary.LittleEndian.PutUint64(
			content[offset:offset+8],
			uint64(entry.tag),
		)
		binary.LittleEndian.PutUint64(
			content[offset+8:offset+16],
			entry.value,
		)
	}

	program := 0
	for range config.loads {
		offset := headerSize + program*programSize
		writeManagerTestELFProgram(
			content[offset:],
			elf.PT_LOAD,
			elf.PF_R|elf.PF_X,
			0,
			baseAddress,
			uint64(size),
		)
		program++
	}
	if config.partialLoad {
		offset := headerSize + program*programSize
		writeManagerTestELFProgram(
			content[offset:],
			elf.PT_LOAD,
			elf.PF_R,
			uint64(dynamicOffset+8),
			baseAddress+uint64(dynamicOffset+8),
			16,
		)
		program++
	}
	for range config.interpreters {
		offset := headerSize + program*programSize
		writeManagerTestELFProgram(
			content[offset:],
			elf.PT_INTERP,
			elf.PF_R,
			uint64(interpreterOffset),
			baseAddress+uint64(interpreterOffset),
			uint64(len(interpreter)),
		)
		program++
	}
	for range config.dynamics {
		offset := headerSize + program*programSize
		writeManagerTestELFProgram(
			content[offset:],
			elf.PT_DYNAMIC,
			elf.PF_R|elf.PF_W,
			uint64(dynamicOffset),
			baseAddress+uint64(dynamicOffset)+config.dynamicAddressDelta,
			uint64(len(dynamic)*16),
		)
		program++
	}
	return content
}

func managerTestELFHeader(
	architecture RuntimeArchitecture,
	size int,
	programs int,
) []byte {
	content := make([]byte, size)
	copy(content, []byte{0x7f, 'E', 'L', 'F'})
	content[4] = byte(elf.ELFCLASS64)
	content[5] = byte(elf.ELFDATA2LSB)
	content[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(content[16:18], uint16(elf.ET_DYN))
	machine := elf.EM_X86_64
	if architecture == ArchitectureAArch64 {
		machine = elf.EM_AARCH64
	}
	binary.LittleEndian.PutUint16(content[18:20], uint16(machine))
	binary.LittleEndian.PutUint32(content[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint64(content[32:40], 64)
	binary.LittleEndian.PutUint16(content[52:54], 64)
	binary.LittleEndian.PutUint16(content[54:56], 56)
	binary.LittleEndian.PutUint16(content[56:58], uint16(programs))
	binary.LittleEndian.PutUint16(content[58:60], 64)
	return content
}

func writeManagerTestELFProgram(
	destination []byte,
	programType elf.ProgType,
	flags elf.ProgFlag,
	offset uint64,
	address uint64,
	size uint64,
) {
	binary.LittleEndian.PutUint32(destination[0:4], uint32(programType))
	binary.LittleEndian.PutUint32(destination[4:8], uint32(flags))
	binary.LittleEndian.PutUint64(destination[8:16], offset)
	binary.LittleEndian.PutUint64(destination[16:24], address)
	binary.LittleEndian.PutUint64(destination[24:32], address)
	binary.LittleEndian.PutUint64(destination[32:40], size)
	binary.LittleEndian.PutUint64(destination[40:48], size)
	binary.LittleEndian.PutUint64(destination[48:56], 4096)
}

func managerTestELFProgramHeader(
	content []byte,
	programType elf.ProgType,
) int {
	offset := int(binary.LittleEndian.Uint64(content[32:40]))
	count := int(binary.LittleEndian.Uint16(content[56:58]))
	for range count {
		if elf.ProgType(binary.LittleEndian.Uint32(content[offset:offset+4])) == programType {
			return offset
		}
		offset += 56
	}
	panic("manager test ELF program is missing")
}

func managerNormalizeRequest(
	t *testing.T,
	manager PackageManager,
	architecture RuntimeArchitecture,
	content []byte,
) (ManagerAcquireRequest, *os.File) {
	t.Helper()
	sum := sha256.Sum256(content)
	request := ManagerAcquireRequest{
		Architecture:   architecture,
		FormatVersion:  ManagerAcquireFormatVersion,
		PackageManager: manager,
		Source: ManagerAcquireSource{
			Digest:    "sha256:" + hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(content)),
		},
	}
	file, err := os.OpenFile(
		t.TempDir()+"/archive",
		os.O_CREATE|os.O_EXCL|os.O_RDWR,
		0600,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		file.Close()
	})
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return request, file
}

type managerNormalizedTestEntry struct {
	mode    int64
	content []byte
}

func managerReadNormalizedResponse(
	t *testing.T,
	raw []byte,
	request ManagerAcquireRequest,
) (map[string]managerNormalizedTestEntry, ManagerAcquireTerminal) {
	t.Helper()
	provisional := managerDownloadFile(t)
	terminal, err := ReadManagerAcquireResponse(
		bytes.NewReader(raw),
		provisional,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != ManagerAcquireStatusOK {
		return nil, terminal
	}
	reader := tar.NewReader(provisional)
	entries := make(map[string]managerNormalizedTestEntry)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = managerNormalizedTestEntry{
			mode:    header.Mode,
			content: content,
		}
	}
	return entries, terminal
}

func managerNormalizedNames(entries map[string]managerNormalizedTestEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func managerAssertNormalizeStatus(
	t *testing.T,
	manager PackageManager,
	architecture RuntimeArchitecture,
	archive []byte,
	want ManagerAcquireStatus,
) {
	t.Helper()
	request, source := managerNormalizeRequest(t, manager, architecture, archive)
	response := new(bytes.Buffer)
	if err := NormalizeManagerArchive(response, request, source, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	_, terminal := managerReadNormalizedResponse(t, response.Bytes(), request)
	if terminal.Status != want {
		t.Fatalf("status = %q, want %q", terminal.Status, want)
	}
}
