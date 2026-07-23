package deployment

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestManagerAcquireRequestRoundTrip(t *testing.T) {
	t.Parallel()
	archive := []byte("manager archive")
	request := managerAcquireTestRequest(PackageManagerBun, archive)

	var wire bytes.Buffer
	if err := WriteManagerAcquireRequest(&wire, request, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	copied := managerAcquireTestFile(t)
	defer copied.Close()
	got, err := ReadManagerAcquireRequest(&wire, copied)
	if err != nil {
		t.Fatal(err)
	}
	if got != request {
		t.Fatalf("request = %#v, want %#v", got, request)
	}
	raw, err := io.ReadAll(copied)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, archive) {
		t.Fatalf("archive = %q, want %q", raw, archive)
	}
}

func TestManagerAcquireRequestRejectsAmbiguousDocuments(t *testing.T) {
	t.Parallel()
	request := managerAcquireTestRequest(PackageManagerBun, []byte("archive"))
	canonical, err := CanonicalManagerAcquireRequest(request)
	if err != nil {
		t.Fatal(err)
	}

	var object map[string]any
	if err := json.Unmarshal(canonical, &object); err != nil {
		t.Fatal(err)
	}
	object["unknown"] = true
	rawUnknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := jsoncanon.Transform(rawUnknown)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append(
		[]byte(`{"architecture":"aarch64",`),
		canonical[1:]...,
	)
	for name, raw := range map[string][]byte{
		"unknown":       unknown,
		"non-canonical": append([]byte{'\n'}, canonical...),
		"duplicate":     duplicate,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseManagerAcquireRequest(raw); err == nil {
				t.Fatal("ParseManagerAcquireRequest returned nil error")
			}
		})
	}
}

func TestManagerAcquireRequestRejectsContentMismatch(t *testing.T) {
	t.Parallel()
	archive := []byte("archive")
	request := managerAcquireTestRequest(PackageManagerBun, archive)

	t.Run("writer trailing bytes", func(t *testing.T) {
		var wire bytes.Buffer
		trailing := append(append([]byte(nil), archive...), 0)
		if err := WriteManagerAcquireRequest(
			&wire,
			request,
			bytes.NewReader(trailing),
		); err == nil {
			t.Fatal("WriteManagerAcquireRequest returned nil error")
		}
	})

	t.Run("reader trailing bytes", func(t *testing.T) {
		var wire bytes.Buffer
		if err := WriteManagerAcquireRequest(
			&wire,
			request,
			bytes.NewReader(archive),
		); err != nil {
			t.Fatal(err)
		}
		wire.WriteByte(0)
		copied := managerAcquireTestFile(t)
		defer copied.Close()
		if _, err := ReadManagerAcquireRequest(
			&wire,
			copied,
		); err == nil {
			t.Fatal("ReadManagerAcquireRequest returned nil error")
		}
		info, err := copied.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 {
			t.Fatalf("provisional archive size = %d, want zero", info.Size())
		}
	})

	t.Run("digest", func(t *testing.T) {
		bad := request
		bad.Source.Digest = managerAcquireDigest([]byte("different"))
		if err := WriteManagerAcquireRequest(
			io.Discard,
			bad,
			bytes.NewReader(archive),
		); err == nil {
			t.Fatal("WriteManagerAcquireRequest returned nil error")
		}
	})

	t.Run("reader digest", func(t *testing.T) {
		bad := request
		bad.Source.Digest = managerAcquireDigest([]byte("different"))
		body, err := CanonicalManagerAcquireRequest(bad)
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if err := writeManagerAcquireFrame(&wire, body); err != nil {
			t.Fatal(err)
		}
		wire.Write(archive)
		copied := managerAcquireTestFile(t)
		defer copied.Close()
		if _, err := ReadManagerAcquireRequest(&wire, copied); err == nil {
			t.Fatal("ReadManagerAcquireRequest returned nil error")
		}
		info, err := copied.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 {
			t.Fatalf("provisional archive size = %d, want zero", info.Size())
		}
	})
}

func TestManagerAcquireResponseRoundTripsBun(t *testing.T) {
	t.Parallel()
	request := managerAcquireTestRequest(PackageManagerBun, []byte("source"))
	var response bytes.Buffer
	writer, err := NewManagerAcquireResponseWriter(&response, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteDirectory("bin"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRegular(
		"bin/bun",
		ManagerAcquireModeExecutable,
		3,
		bytes.NewReader([]byte("bun")),
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTerminal(ManagerAcquireStatusOK); err != nil {
		t.Fatal(err)
	}

	terminal, provisional, err := readManagerAcquireTestResponse(
		t,
		&response,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != ManagerAcquireStatusOK ||
		terminal.EntryCount != 2 ||
		terminal.LogicalBytes != 3 {
		t.Fatalf("terminal = %#v", terminal)
	}
	assertManagerAcquireTar(t, provisional, []managerAcquireTarEntry{
		{path: "bin", mode: 0555, directory: true},
		{path: "bin/bun", mode: 0555, content: "bun"},
	})
}

func TestManagerAcquireResponseRoundTripsNPM(t *testing.T) {
	t.Parallel()
	request := managerAcquireTestRequest(PackageManagerNPM, []byte("source"))
	var response bytes.Buffer
	writer, err := NewManagerAcquireResponseWriter(&response, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"lib", "lib/npm", "lib/npm/bin"} {
		if err := writer.WriteDirectory(directory); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteRegular(
		"lib/npm/bin/npm-cli.js",
		ManagerAcquireModeExecutable,
		3,
		bytes.NewReader([]byte("cli")),
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRegular(
		"lib/npm/package.json",
		ManagerAcquireModeReadOnly,
		2,
		bytes.NewReader([]byte("{}")),
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTerminal(ManagerAcquireStatusOK); err != nil {
		t.Fatal(err)
	}

	terminal, provisional, err := readManagerAcquireTestResponse(
		t,
		&response,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != ManagerAcquireStatusOK ||
		terminal.EntryCount != 5 ||
		terminal.LogicalBytes != 5 {
		t.Fatalf("terminal = %#v", terminal)
	}
	assertManagerAcquireTar(t, provisional, []managerAcquireTarEntry{
		{path: "lib", mode: 0555, directory: true},
		{path: "lib/npm", mode: 0555, directory: true},
		{path: "lib/npm/bin", mode: 0555, directory: true},
		{path: "lib/npm/bin/npm-cli.js", mode: 0555, content: "cli"},
		{path: "lib/npm/package.json", mode: 0444, content: "{}"},
	})
}

func TestManagerAcquireResponseWriterRejectsInvalidEntries(t *testing.T) {
	t.Parallel()
	request := managerAcquireTestRequest(PackageManagerBun, []byte("source"))
	tests := map[string]func(*ManagerAcquireResponseWriter) error{
		"missing parent": func(writer *ManagerAcquireResponseWriter) error {
			return writer.WriteDirectory("bin/nested")
		},
		"non-canonical path": func(writer *ManagerAcquireResponseWriter) error {
			return writer.WriteDirectory("bin/../other")
		},
		"component too long": func(writer *ManagerAcquireResponseWriter) error {
			return writer.WriteDirectory(strings.Repeat("a", ManagerAcquireMaxComponentBytes+1))
		},
		"unsupported mode": func(writer *ManagerAcquireResponseWriter) error {
			return writer.WriteRegular("file", "0755", 0, bytes.NewReader(nil))
		},
		"case collision": func(writer *ManagerAcquireResponseWriter) error {
			if err := writer.WriteDirectory("Foo"); err != nil {
				return err
			}
			return writer.WriteDirectory("foo")
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer, err := NewManagerAcquireResponseWriter(io.Discard, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := test(writer); err == nil {
				t.Fatal("entry was accepted")
			}
		})
	}
}

func TestManagerAcquireResponseReaderRejectsMalformedStreams(t *testing.T) {
	t.Parallel()
	request := managerAcquireTestRequest(PackageManagerBun, []byte("source"))

	t.Run("count mismatch", func(t *testing.T) {
		var response bytes.Buffer
		writeManagerAcquireTestEntry(t, &response, ManagerAcquireEntry{
			Kind:      ManagerAcquireEntryDirectory,
			Mode:      ManagerAcquireModeExecutable,
			Path:      "bin",
			SizeBytes: 0,
			Type:      managerAcquireEntryType,
		}, nil)
		writeManagerAcquireTestTerminal(t, &response, ManagerAcquireTerminal{
			EntryCount:   0,
			LogicalBytes: 0,
			Status:       ManagerAcquireStatusUnsupportedLayout,
			Type:         managerAcquireTerminalType,
		})
		if _, output, err := readManagerAcquireTestResponse(
			t,
			&response,
			request,
		); err == nil {
			t.Fatal("ReadManagerAcquireResponse returned nil error")
		} else if len(output) != 0 {
			t.Fatalf("provisional output = %d bytes, want zero", len(output))
		}
	})

	t.Run("truncated payload", func(t *testing.T) {
		var response bytes.Buffer
		writeManagerAcquireTestEntry(t, &response, ManagerAcquireEntry{
			Kind:      ManagerAcquireEntryDirectory,
			Mode:      ManagerAcquireModeExecutable,
			Path:      "bin",
			SizeBytes: 0,
			Type:      managerAcquireEntryType,
		}, nil)
		writeManagerAcquireTestEntry(t, &response, ManagerAcquireEntry{
			Kind:      ManagerAcquireEntryRegular,
			Mode:      ManagerAcquireModeExecutable,
			Path:      "bin/bun",
			SizeBytes: 3,
			Type:      managerAcquireEntryType,
		}, []byte("b"))
		if _, output, err := readManagerAcquireTestResponse(
			t,
			&response,
			request,
		); err == nil {
			t.Fatal("ReadManagerAcquireResponse returned nil error")
		} else if len(output) != 0 {
			t.Fatalf("provisional output = %d bytes, want zero", len(output))
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		var response bytes.Buffer
		writer, err := NewManagerAcquireResponseWriter(&response, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteTerminal(ManagerAcquireStatusUnsupportedLayout); err != nil {
			t.Fatal(err)
		}
		response.WriteByte(0)
		if _, output, err := readManagerAcquireTestResponse(
			t,
			&response,
			request,
		); err == nil {
			t.Fatal("ReadManagerAcquireResponse returned nil error")
		} else if len(output) != 0 {
			t.Fatalf("provisional output = %d bytes, want zero", len(output))
		}
	})

	t.Run("incomplete successful family", func(t *testing.T) {
		var response bytes.Buffer
		writeManagerAcquireTestEntry(t, &response, ManagerAcquireEntry{
			Kind:      ManagerAcquireEntryDirectory,
			Mode:      ManagerAcquireModeExecutable,
			Path:      "bin",
			SizeBytes: 0,
			Type:      managerAcquireEntryType,
		}, nil)
		writeManagerAcquireTestTerminal(t, &response, ManagerAcquireTerminal{
			EntryCount:   1,
			LogicalBytes: 0,
			Status:       ManagerAcquireStatusOK,
			Type:         managerAcquireTerminalType,
		})
		if _, output, err := readManagerAcquireTestResponse(
			t,
			&response,
			request,
		); err == nil {
			t.Fatal("ReadManagerAcquireResponse returned nil error")
		} else if len(output) != 0 {
			t.Fatalf("provisional output = %d bytes, want zero", len(output))
		}
	})
}

func TestManagerAcquireResponseReturnsNonOKTerminal(t *testing.T) {
	t.Parallel()
	request := managerAcquireTestRequest(PackageManagerBun, []byte("source"))
	var response bytes.Buffer
	writer, err := NewManagerAcquireResponseWriter(&response, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteDirectory("bin"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTerminal(ManagerAcquireStatusLimitExceeded); err != nil {
		t.Fatal(err)
	}
	terminal, output, err := readManagerAcquireTestResponse(
		t,
		&response,
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != ManagerAcquireStatusLimitExceeded ||
		terminal.EntryCount != 1 ||
		terminal.LogicalBytes != 0 {
		t.Fatalf("terminal = %#v", terminal)
	}
	if len(output) != 0 {
		t.Fatalf("provisional output = %d bytes, want zero", len(output))
	}
}

type managerAcquireTarEntry struct {
	path      string
	mode      int64
	content   string
	directory bool
}

func assertManagerAcquireTar(
	t *testing.T,
	raw []byte,
	want []managerAcquireTarEntry,
) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(raw))
	for index, expected := range want {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		if header.Name != expected.path ||
			header.Mode != expected.mode ||
			(header.Typeflag == tar.TypeDir) != expected.directory {
			t.Fatalf("entry %d header = %#v, want %#v", index, header, expected)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != expected.content {
			t.Fatalf("entry %d content = %q, want %q", index, content, expected.content)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("tar trailing entry error = %v, want EOF", err)
	}
}

func managerAcquireTestRequest(
	name PackageManagerName,
	archive []byte,
) ManagerAcquireRequest {
	return ManagerAcquireRequest{
		Architecture:  ArchitectureAArch64,
		FormatVersion: ManagerAcquireFormatVersion,
		PackageManager: PackageManager{
			Name:    name,
			Version: "1.3.10",
		},
		Source: ManagerAcquireSource{
			Digest:    managerAcquireDigest(archive),
			SizeBytes: int64(len(archive)),
		},
	}
}

func managerAcquireDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeManagerAcquireTestEntry(
	t *testing.T,
	destination io.Writer,
	entry ManagerAcquireEntry,
	payload []byte,
) {
	t.Helper()
	raw, err := CanonicalManagerAcquireEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManagerAcquireFrame(destination, raw); err != nil {
		t.Fatal(err)
	}
	if len(payload) > 0 {
		if _, err := destination.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
}

func writeManagerAcquireTestTerminal(
	t *testing.T,
	destination io.Writer,
	terminal ManagerAcquireTerminal,
) {
	t.Helper()
	raw, err := CanonicalManagerAcquireTerminal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManagerAcquireFrame(destination, raw); err != nil {
		t.Fatal(err)
	}
}

func readManagerAcquireTestResponse(
	t *testing.T,
	source io.Reader,
	request ManagerAcquireRequest,
) (ManagerAcquireTerminal, []byte, error) {
	t.Helper()
	file := managerAcquireTestFile(t)
	defer file.Close()
	terminal, readErr := ReadManagerAcquireResponse(source, file, request)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return terminal, raw, readErr
}

func managerAcquireTestFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "manager-acquire-*")
	if err != nil {
		t.Fatal(err)
	}
	return file
}
