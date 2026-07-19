package deployment

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"slices"
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

func managerTestELF(architecture RuntimeArchitecture, size int) []byte {
	if size < 64 {
		size = 64
	}
	content := make([]byte, size)
	copy(content, []byte{0x7f, 'E', 'L', 'F'})
	content[4] = 2
	content[5] = 1
	content[6] = 1
	binary.LittleEndian.PutUint16(content[16:18], 3)
	machine := uint16(62)
	if architecture == ArchitectureAArch64 {
		machine = 183
	}
	binary.LittleEndian.PutUint16(content[18:20], machine)
	return content
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
