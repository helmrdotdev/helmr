package buildcontext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCapturePreservesLongUnicodeDeepNamesAndIndependentInodes(t *testing.T) {
	source, temp := t.TempDir(), t.TempDir()
	names := []string{
		".npm-cache/_cacache/content-v2/sha512/0c/08/" + strings.Repeat("a", 124),
		"日本語.txt", strings.Repeat("d", 80) + "/" + strings.Repeat("e", 80) + "/file.txt",
		strings.Repeat("f", 255), "a-/file", "a/child", "original",
	}
	for _, name := range names {
		writeTestFile(t, filepath.Join(source, name), name)
	}
	if err := os.Link(filepath.Join(source, "original"), filepath.Join(source, "copy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "original"), 0711); err != nil {
		t.Fatal(err)
	}
	longTarget := names[2]
	for link, target := range map[string]string{"link": longTarget, "dangling": "missing", "directory": "a"} {
		if err := os.Symlink(target, filepath.Join(source, link)); err != nil {
			t.Fatal(err)
		}
	}
	captured, err := Capture(t.Context(), source, temp)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(names, "copy") {
		original, err := os.Stat(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.Stat(filepath.Join(captured.Path, name))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(original, copied) {
			t.Fatalf("shared inode %q", name)
		}
		if !copied.ModTime().Equal(captureEpoch) {
			t.Fatalf("mtime %q = %v", name, copied.ModTime())
		}
	}
	first, _ := os.Stat(filepath.Join(captured.Path, "original"))
	second, _ := os.Stat(filepath.Join(captured.Path, "copy"))
	if os.SameFile(first, second) || first.Mode().Perm() != 0755 {
		t.Fatal("hardlinks or executable mode not normalized")
	}
	for link, target := range map[string]string{"link": longTarget, "dangling": "missing"} {
		got, err := os.Readlink(filepath.Join(captured.Path, link))
		if err != nil || got != target {
			t.Fatalf("link %q = %q, %v", link, got, err)
		}
	}
	for _, name := range names {
		writeTestFile(t, filepath.Join(source, name), "changed")
		got, err := os.ReadFile(filepath.Join(captured.Path, name))
		if err != nil || string(got) != name {
			t.Fatalf("captured %q changed: %v", name, err)
		}
	}
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(captured.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup: %v", err)
	}
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureRejectsPlacementAliases(t *testing.T) {
	source, outside := t.TempDir(), t.TempDir()
	nested := filepath.Join(source, "temp")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(outside, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	for _, temp := range []string{source, nested, alias} {
		if result, err := Capture(t.Context(), source, temp); err == nil {
			result.Close()
			t.Fatalf("accepted %q", temp)
		}
	}
	children, err := os.ReadDir(nested)
	if err != nil || len(children) != 0 {
		t.Fatalf("placement failure left entries: %v %v", children, err)
	}
	sourceAlias := filepath.Join(outside, "source")
	if err := os.Symlink(source, sourceAlias); err != nil {
		t.Fatal(err)
	}
	if result, err := Capture(t.Context(), sourceAlias, alias); err == nil {
		result.Close()
		t.Fatal("accepted source symlink alias")
	}
}

func TestCaptureRejectsEscapesAndCleansPartialTree(t *testing.T) {
	for _, target := range []string{"../outside", "/etc/passwd", "a/../../outside"} {
		t.Run(target, func(t *testing.T) {
			source, temp := t.TempDir(), t.TempDir()
			writeTestFile(t, filepath.Join(source, "first"), "first")
			if err := os.Symlink(target, filepath.Join(source, "z")); err != nil {
				t.Fatal(err)
			}
			if result, err := Capture(t.Context(), source, temp); err == nil {
				result.Close()
				t.Fatal("accepted escape")
			}
			entries, err := os.ReadDir(temp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failure cleanup: %v %v", entries, err)
			}
		})
	}
	// Each link is lexically confined; their combined traversal escapes.
	source, temp := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "dir"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"dir/up": "..", "escape": "dir/up/../outside"} {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := Capture(t.Context(), source, temp); err == nil {
		result.Close()
		t.Fatal("accepted chain escape")
	}
	entries, _ := os.ReadDir(temp)
	if len(entries) != 0 {
		t.Fatal("partial context retained")
	}
}

func TestCaptureDestinationEntriesAreExclusive(t *testing.T) {
	for _, kind := range []string{"file", "directory", "link"} {
		t.Run(kind, func(t *testing.T) {
			source, dest := t.TempDir(), t.TempDir()
			switch kind {
			case "file":
				writeTestFile(t, filepath.Join(source, "entry"), "source")
			case "directory":
				writeTestFile(t, filepath.Join(source, "entry", "child"), "source")
			case "link":
				if err := os.Symlink("missing", filepath.Join(source, "entry")); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := collectTestSourceSnapshot(t, source)
			writeTestFile(t, filepath.Join(dest, "entry"), "existing")
			root, err := os.OpenRoot(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := snapshot.materialize(t.Context(), root); err == nil {
				t.Fatal("overwrote destination")
			}
			body, err := os.ReadFile(filepath.Join(dest, "entry"))
			if err != nil || string(body) != "existing" {
				t.Fatal("destination changed")
			}
		})
	}
}

func TestCaptureRejectsDestinationSymlinkParent(t *testing.T) {
	source, dest, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "dir", "child"), "source")
	snapshot := collectTestSourceSnapshot(t, source)
	if err := os.Symlink(outside, filepath.Join(dest, "dir")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := snapshot.materialize(t.Context(), root); err == nil {
		t.Fatal("merged symlink parent")
	}
	children, _ := os.ReadDir(outside)
	if len(children) != 0 {
		t.Fatal("wrote outside root")
	}
}

func TestCaptureDiscoveryBoundsIncludeIgnoredChildren(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, ".helmrignore"), "ignored*\n")
	for i := 0; i < 400; i++ {
		writeTestFile(t, filepath.Join(source, fmt.Sprintf("ignored-%04d", i)), "")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot := &sourceSnapshot{root: root, observed: map[string]sourceObserved{}, directories: map[string][]string{}, metadataLimit: 4096}
	if err := snapshot.collect(t.Context()); err == nil || !strings.Contains(err.Error(), "observation/name budget") {
		t.Fatalf("discovery bound: %v", err)
	}
	if snapshot.metadataBytes > snapshot.metadataLimit || len(snapshot.observed) > 10 {
		t.Fatal("retained unchecked observations")
	}
}

func TestCapturePayloadAndEntryBoundsDuringDiscovery(t *testing.T) {
	for _, limit := range []string{"payload", "entries"} {
		t.Run(limit, func(t *testing.T) {
			source := t.TempDir()
			writeTestFile(t, filepath.Join(source, "a"), "abc")
			writeTestFile(t, filepath.Join(source, "b"), "def")
			root, err := os.OpenRoot(source)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			snapshot := &sourceSnapshot{root: root, observed: map[string]sourceObserved{}, directories: map[string][]string{}}
			if limit == "payload" {
				snapshot.payloadLimit = 4
			} else {
				snapshot.entryLimit = 1
			}
			if err := snapshot.collect(t.Context()); err == nil {
				t.Fatal("accepted over limit")
			}
			if len(snapshot.entries) > 1 {
				t.Fatal("retained excess entry")
			}
		})
	}
}

func TestCaptureObservedMutationAndCancellationWhileCopying(t *testing.T) {
	source := t.TempDir()
	name := filepath.Join(source, "file")
	writeTestFile(t, name, strings.Repeat("a", 128<<10))
	snapshot := collectTestSourceSnapshot(t, source)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &callbackWriter{callback: cancel}
	if err := snapshot.writeFile(ctx, sink, snapshot.entries[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy cancellation: %v", err)
	}
	sink = &callbackWriter{callback: func() { writeTestFile(t, name, strings.Repeat("b", 128<<10)) }}
	if err := snapshot.writeFile(t.Context(), sink, snapshot.entries[0]); !errors.Is(err, errSourceChanged) {
		t.Fatalf("copy mutation: %v", err)
	}
}

type callbackWriter struct {
	buffer   bytes.Buffer
	callback func()
}

func (w *callbackWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if w.callback != nil {
		f := w.callback
		w.callback = nil
		f()
	}
	return n, err
}

func TestCaptureCancellationCleansDirectory(t *testing.T) {
	source, temp := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "file"), "x")
	// Cancellation after admission is covered without a race or production hook.
	ctx := &cancelAfterContext{Context: t.Context(), remaining: 8}
	if result, err := Capture(ctx, source, temp); !errors.Is(err, context.Canceled) {
		if result != nil {
			result.Close()
		}
		t.Fatalf("cancel: %v", err)
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancel cleanup: %v %v", entries, err)
	}
}

type cancelAfterContext struct {
	context.Context
	remaining int
}

func (c *cancelAfterContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestCaptureIgnoredObservationMutation(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, ".helmrignore"), "ignored\n")
	writeTestFile(t, filepath.Join(source, "ignored"), "before")
	snapshot := collectTestSourceSnapshot(t, source)
	writeTestFile(t, filepath.Join(source, "ignored"), "after!")
	if err := snapshot.verify(t.Context()); !errors.Is(err, errSourceChanged) {
		t.Fatalf("ignored observation: %v", err)
	}
}

func TestCaptureSourceParentSwapIsConfined(t *testing.T) {
	source, outside := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "dir", "file"), "safe")
	writeTestFile(t, filepath.Join(outside, "file"), "outside")
	snapshot := collectTestSourceSnapshot(t, source)
	if err := os.Rename(filepath.Join(source, "dir"), filepath.Join(source, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "dir")); err != nil {
		t.Fatal(err)
	}
	var copied bytes.Buffer
	for _, entry := range snapshot.entries {
		if entry.name == "dir/file" {
			if err := snapshot.writeFile(t.Context(), &copied, entry); err == nil {
				t.Fatal("accepted changed parent")
			}
		}
	}
	if copied.Len() != 0 {
		t.Fatal("read outside source")
	}
}

func TestCaptureMetadataIgnoresHostTimes(t *testing.T) {
	source, temp := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "file"), "content")
	if err := os.Symlink("file", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	capture, err := Capture(t.Context(), source, temp)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	for _, name := range []string{".", "file", "link"} {
		info, err := os.Lstat(filepath.Join(capture.Path, name))
		if err != nil || !info.ModTime().Equal(time.Unix(0, 0)) {
			t.Fatalf("%s mtime: %v %v", name, info, err)
		}
	}
}

var _ io.Writer = (*callbackWriter)(nil)

func TestCaptureCrossFilesystemAliases(t *testing.T) {
	for _, pair := range [][2]string{{"Case", "case"}, {"caf\u00e9", "cafe\u0301"}} {
		for _, kind := range []string{"file", "directory", "link"} {
			t.Run(pair[0]+"/"+kind, func(t *testing.T) {
				source := t.TempDir()
				create := func(root, name string) error {
					switch kind {
					case "directory":
						return os.Mkdir(filepath.Join(root, name), 0755)
					case "link":
						return os.Symlink("missing", filepath.Join(root, name))
					default:
						f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
						if err != nil {
							return err
						}
						return f.Close()
					}
				}
				if err := create(source, pair[0]); err != nil {
					t.Fatal(err)
				}
				if err := create(source, pair[1]); errors.Is(err, os.ErrExist) {
					t.Skip("source filesystem aliases these names")
				} else if err != nil {
					t.Fatal(err)
				}
				t.Logf("source %s: exclusively created distinct %s names %q and %q", source, kind, pair[0], pair[1])
				temp := t.TempDir()
				if parent := os.Getenv("HELMR_CAPTURE_ALIAS_DESTINATION"); parent != "" {
					var err error
					temp, err = os.MkdirTemp(parent, "capture-alias-")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.RemoveAll(temp) })
				}
				probe := filepath.Join(temp, "probe")
				if err := os.Mkdir(probe, 0700); err != nil {
					t.Fatal(err)
				}
				if err := create(probe, pair[0]); err != nil {
					t.Fatal(err)
				}
				aliasErr := create(probe, pair[1])
				if aliasErr != nil && !errors.Is(aliasErr, os.ErrExist) {
					t.Fatal(aliasErr)
				}
				t.Logf("destination %s: alias probe for %q and %q: aliases=%t, second create=%v", temp, pair[0], pair[1], aliasErr != nil, aliasErr)
				if err := os.RemoveAll(probe); err != nil {
					t.Fatal(err)
				}
				result, err := Capture(t.Context(), source, temp)
				if aliasErr != nil {
					if err == nil {
						result.Close()
						t.Fatal("merged filesystem aliases")
					}
					t.Logf("Capture rejected %s aliases: %v", kind, err)
					children, readErr := os.ReadDir(temp)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if len(children) != 0 {
						t.Fatal("alias failure retained context")
					}
					t.Logf("rejected %s capture cleanup: destination has %d entries", kind, len(children))
				} else {
					if err != nil {
						t.Fatal(err)
					}
					result.Close()
				}
			})
		}
	}
}

func TestCaptureRejectsIgnoreFilenameAlias(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, ".HELMRIGNORE"), "file\n")
	if _, err := os.Stat(filepath.Join(source, ".helmrignore")); errors.Is(err, os.ErrNotExist) {
		t.Skip("case-sensitive filesystem")
	}
	writeTestFile(t, filepath.Join(source, "file"), "included")
	if result, err := Capture(t.Context(), source, t.TempDir()); err == nil {
		result.Close()
		t.Fatal("read aliased ignore authority")
	}
}

func TestCaptureRejectsOversizedSparseFileWithoutCopying(t *testing.T) {
	source, temp := t.TempDir(), t.TempDir()
	file, err := os.Create(filepath.Join(source, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Truncate(maxSourceBytes+1), file.Close()); err != nil {
		t.Fatal(err)
	}
	if result, err := Capture(t.Context(), source, temp); err == nil {
		result.Close()
		t.Fatal("accepted payload beyond 512 MiB")
	}
	children, err := os.ReadDir(temp)
	if err != nil || len(children) != 0 {
		t.Fatal("oversize cleanup failed")
	}
}

func TestCaptureDetectsParentMovedInsideSource(t *testing.T) {
	source, outer := t.TempDir(), t.TempDir()
	temp := filepath.Join(outer, "temp")
	if err := os.Mkdir(temp, 0700); err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	parentRoot, err := os.OpenRoot(temp)
	if err != nil {
		t.Fatal(err)
	}
	defer parentRoot.Close()
	inside := filepath.Join(source, "moved")
	if err := os.Rename(temp, inside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, temp); err != nil {
		t.Fatal(err)
	}
	if err := validatePlacement(sourceRoot, parentRoot, source, temp); err == nil {
		t.Fatal("accepted moved destination through replacement alias")
	}
}

func TestCaptureLinkHopBound(t *testing.T) {
	for _, count := range []int{40, 41} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			source := t.TempDir()
			writeTestFile(t, filepath.Join(source, "file"), "content")
			for i := 0; i < count; i++ {
				target := "file"
				if i+1 < count {
					target = fmt.Sprintf("link-%02d", i+1)
				}
				if err := os.Symlink(target, filepath.Join(source, fmt.Sprintf("link-%02d", i))); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Capture(t.Context(), source, t.TempDir())
			if count == 40 {
				if err != nil {
					t.Fatal(err)
				}
				result.Close()
			} else if err == nil {
				result.Close()
				t.Fatal("accepted more than 40 link hops")
			}
		})
	}
}

func TestCaptureRejectsLinkCycles(t *testing.T) {
	for _, targets := range [][]string{{"link-0"}, {"link-1", "link-0"}} {
		t.Run(fmt.Sprintf("%d-link-cycle", len(targets)), func(t *testing.T) {
			source, temp := t.TempDir(), t.TempDir()
			for i, target := range targets {
				if err := os.Symlink(target, filepath.Join(source, fmt.Sprintf("link-%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Capture(t.Context(), source, temp)
			if err == nil {
				result.Close()
				t.Fatal("accepted link cycle")
			}
			if !strings.Contains(err.Error(), "exceeds 40 hops") {
				t.Fatalf("unexpected cycle rejection: %v", err)
			}
			if children, err := os.ReadDir(temp); err != nil || len(children) != 0 {
				t.Fatalf("cycle cleanup: %v, %d entries", err, len(children))
			}
		})
	}
}

func TestCaptureDanglingLinkHopBound(t *testing.T) {
	for _, count := range []int{40, 41} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			source, temp := t.TempDir(), t.TempDir()
			for i := 0; i < count; i++ {
				target := "missing"
				if i+1 < count {
					target = fmt.Sprintf("link-%02d", i+1)
				}
				if err := os.Symlink(target, filepath.Join(source, fmt.Sprintf("link-%02d", i))); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Capture(t.Context(), source, temp)
			if count == 40 {
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < count; i++ {
					want := "missing"
					if i+1 < count {
						want = fmt.Sprintf("link-%02d", i+1)
					}
					if got, err := os.Readlink(filepath.Join(result.Path, fmt.Sprintf("link-%02d", i))); err != nil || got != want {
						t.Fatalf("captured dangling link %d = %q, %v", i, got, err)
					}
				}
				if err := result.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					result.Close()
					t.Fatal("accepted dangling chain beyond 40 hops")
				}
				if !strings.Contains(err.Error(), "exceeds 40 hops") {
					t.Fatalf("unexpected hop rejection: %v", err)
				}
			}
			if children, err := os.ReadDir(temp); err != nil || len(children) != 0 {
				t.Fatalf("dangling chain cleanup: %v, %d entries", err, len(children))
			}
		})
	}
}

func TestCaptureActualDepthBoundary(t *testing.T) {
	for _, depth := range []int{128, 129} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			source := t.TempDir()
			name := strings.Repeat("d/", depth-1) + "file"
			writeTestFile(t, filepath.Join(source, name), "content")
			captured, err := Capture(t.Context(), source, t.TempDir())
			if depth == 128 {
				if err != nil {
					t.Fatal(err)
				}
				defer captured.Close()
				if _, err := os.ReadFile(filepath.Join(captured.Path, name)); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				captured.Close()
				t.Fatal("accepted depth 129")
			}
		})
	}
}

func TestCaptureAccountsForActualDestinationPrefix(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	limit := 4096
	if runtime.GOOS == "darwin" {
		limit = 1024
	}
	remaining := limit - len(source) - 2
	var parts []string
	for remaining > 255 {
		parts = append(parts, strings.Repeat("d", 255))
		remaining -= 256
	}
	parts = append(parts, strings.Repeat("f", remaining))
	name := strings.Join(parts, "/")
	writeTestFile(t, filepath.Join(source, name), "fits source")
	temp := t.TempDir()
	if result, err := Capture(t.Context(), source, temp); err == nil {
		result.Close()
		t.Fatal("accepted context whose actual prefix exceeds host path limit")
	} else if !strings.Contains(err.Error(), "host path exceeds") {
		t.Fatal(err)
	}
	children, err := os.ReadDir(temp)
	if err != nil || len(children) != 0 {
		t.Fatal("host prefix failure retained context")
	}
}
