package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"testing"
)

func TestProgramArchiveMatchesGoldenStream(t *testing.T) {
	t.Parallel()
	entries := programArchiveFixture()
	var first bytes.Buffer
	if err := writeTreeArchive(
		context.Background(),
		&first,
		programArtifact,
		treeEntrySequence(entries),
		false,
	); err != nil {
		t.Fatalf("writeTreeArchive: %v", err)
	}
	var second bytes.Buffer
	if err := writeTreeArchive(
		context.Background(),
		&second,
		programArtifact,
		treeEntrySequence(programArchiveFixture()),
		false,
	); err != nil {
		t.Fatalf("writeTreeArchive second: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("program archive changed across identical writes")
	}
	digest := sha256.Sum256(first.Bytes())
	const wantDigest = "80eed1512d12df062fadc27e44b0d270eb9341b915211fd9cab002d8da4d3e99"
	if got := fmt.Sprintf("%x", digest); got != wantDigest {
		t.Fatalf("program archive digest = %s, want %s; size = %d", got, wantDigest, first.Len())
	}
	if first.Len() != 9728 {
		t.Fatalf("program archive size = %d, want 9728", first.Len())
	}

	reader := tar.NewReader(bytes.NewReader(first.Bytes()))
	want := programArchiveFixture()
	for position := range want {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("read entry %d: %v", position, err)
		}
		entry := want[position]
		if header.Name != entry.Path ||
			header.Mode != int64(entry.Mode) ||
			header.Typeflag != treeEntryTarType(entry.Kind) ||
			header.Linkname != entry.LinkTarget {
			t.Fatalf("entry %d header = %#v", position, header)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read entry %d content: %v", position, err)
		}
		if entry.Kind == artifactEntryRegular {
			expected, err := io.ReadAll(entry.Content)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(content, expected) {
				t.Fatalf("entry %d content = %q, want %q", position, content, expected)
			}
		} else if len(content) != 0 {
			t.Fatalf("entry %d has %d content bytes", position, len(content))
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("archive end = %v, want EOF", err)
	}
}

func TestProgramArchiveRejectsInvalidTrees(t *testing.T) {
	t.Parallel()
	valid := func() []treeEntry {
		return []treeEntry{
			{Path: "dir", Kind: artifactEntryDirectory, Mode: 0755},
			{
				Path:      "dir/file",
				Kind:      artifactEntryRegular,
				Mode:      0644,
				SizeBytes: 1,
				Content:   strings.NewReader("x"),
			},
		}
	}
	tests := map[string]func([]treeEntry) []treeEntry{
		"explicit root": func(entries []treeEntry) []treeEntry {
			entries[0].Path = "."
			return entries
		},
		"out of order": func(entries []treeEntry) []treeEntry {
			return []treeEntry{entries[1], entries[0]}
		},
		"duplicate": func(entries []treeEntry) []treeEntry {
			return append(entries, entries[1])
		},
		"missing parent": func(entries []treeEntry) []treeEntry {
			return entries[1:]
		},
		"bad file mode": func(entries []treeEntry) []treeEntry {
			entries[1].Mode = 0600
			return entries
		},
		"file without content": func(entries []treeEntry) []treeEntry {
			entries[1].Content = nil
			return entries
		},
		"directory with content": func(entries []treeEntry) []treeEntry {
			entries[0].Content = strings.NewReader("")
			return entries
		},
		"absolute link": func(entries []treeEntry) []treeEntry {
			entries[1] = treeEntry{
				Path:       "dir/link",
				Kind:       artifactEntrySymlink,
				Mode:       0777,
				LinkTarget: "/escape",
			}
			return entries
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeTreeArchive(
				context.Background(),
				&output,
				programArtifact,
				treeEntrySequence(mutate(valid())),
				false,
			); err == nil {
				t.Fatal("writeTreeArchive returned nil error")
			}
		})
	}
}

func TestProgramArchiveRejectsContentLengthMismatch(t *testing.T) {
	t.Parallel()
	tests := []treeEntry{
		{
			Path:      "short",
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: 2,
			Content:   strings.NewReader("x"),
		},
		{
			Path:      "long",
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: 1,
			Content:   strings.NewReader("xx"),
		},
	}
	for _, entry := range tests {
		t.Run(entry.Path, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeTreeArchive(
				context.Background(),
				&output,
				programArtifact,
				treeEntrySequence([]treeEntry{entry}),
				false,
			); err == nil {
				t.Fatal("writeTreeArchive returned nil error")
			}
		})
	}
}

func TestProgramArchiveRejectsCancellation(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeTreeArchive(
		ctx,
		&output,
		programArtifact,
		treeEntrySequence(programArchiveFixture()),
		false,
	); err == nil {
		t.Fatal("writeTreeArchive ignored cancellation")
	}
}

func TestProgramArchiveStreamsEntriesAndPropagatesSourceFailure(t *testing.T) {
	t.Parallel()
	sourceErr := errors.New("source failed")
	sequence := func(yield func(treeEntry, error) bool) {
		if !yield(treeEntry{Path: "dir", Kind: artifactEntryDirectory, Mode: 0755}, nil) {
			return
		}
		yield(treeEntry{}, sourceErr)
	}
	var output bytes.Buffer
	err := writeTreeArchive(context.Background(), &output, programArtifact, sequence, false)
	if !errors.Is(err, sourceErr) {
		t.Fatalf("writeTreeArchive error = %v, want %v", err, sourceErr)
	}
	if output.Len() == 0 {
		t.Fatal("writeTreeArchive did not stream the accepted entry")
	}
}

func TestPAXRecordLengthIncludesItsDigits(t *testing.T) {
	t.Parallel()
	record := paxRecord("path", strings.Repeat("x", 100))
	length, _, ok := strings.Cut(string(record), " ")
	if !ok {
		t.Fatalf("record = %q", record)
	}
	if length != fmt.Sprint(len(record)) {
		t.Fatalf("record length prefix = %q, want %d", length, len(record))
	}
}

func programArchiveFixture() []treeEntry {
	return []treeEntry{
		{Path: "bin", Kind: artifactEntryDirectory, Mode: 0755},
		{
			Path:      "bin/tool",
			Kind:      artifactEntryRegular,
			Mode:      0755,
			SizeBytes: int64(len("#!/usr/bin/env node\n")),
			Content:   strings.NewReader("#!/usr/bin/env node\n"),
		},
		{Path: "empty", Kind: artifactEntryDirectory, Mode: 0755},
		{
			Path:       "tool",
			Kind:       artifactEntrySymlink,
			Mode:       0777,
			LinkTarget: "bin/tool",
		},
		{
			Path:      "資料",
			Kind:      artifactEntryRegular,
			Mode:      0644,
			SizeBytes: 3,
			Content:   strings.NewReader("abc"),
		},
	}
}

func treeEntryTarType(kind artifactEntryKind) byte {
	switch kind {
	case artifactEntryRegular:
		return tar.TypeReg
	case artifactEntryDirectory:
		return tar.TypeDir
	case artifactEntrySymlink:
		return tar.TypeSymlink
	default:
		panic("unsupported test tree entry")
	}
}

func treeEntrySequence(entries []treeEntry) iter.Seq2[treeEntry, error] {
	return func(yield func(treeEntry, error) bool) {
		for _, entry := range entries {
			if !yield(entry, nil) {
				return
			}
		}
	}
}
