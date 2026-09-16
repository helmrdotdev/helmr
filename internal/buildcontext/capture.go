// Package buildcontext owns private, selected directory captures for local builds.
package buildcontext

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

// Directory contains independent copied bytes. Close removes the private tree.
// Capture rejects observed changes; it does not create an atomic snapshot.
type Directory struct {
	Path   string
	parent *os.Root
	name   string
}

func (directory *Directory) Close() error {
	if directory.parent == nil {
		return nil
	}
	err := errors.Join(directory.parent.RemoveAll(directory.name), directory.parent.Close())
	directory.parent = nil
	return err
}

// Capture selects source with .helmrignore and materializes it outside source.
// tempDir must exist; empty selects os.TempDir. Ownership stays with the caller
// until Close. No entry is overwritten, merged, renamed or hard linked.
func Capture(ctx context.Context, source, tempDir string) (_ *Directory, returnErr error) {
	if ctx == nil {
		return nil, errors.New("build source context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	tempDir, err = filepath.Abs(tempDir)
	if err != nil {
		return nil, err
	}
	tempDir, err = filepath.EvalSymlinks(tempDir)
	if err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(tempDir)
	if err != nil {
		return nil, err
	}
	captured := &Directory{Path: "", parent: parent}
	defer func() {
		if returnErr != nil {
			if captured.name == "" {
				returnErr = errors.Join(returnErr, parent.Close())
			} else {
				returnErr = errors.Join(returnErr, captured.Close())
			}
		}
	}()
	if err := validatePlacement(root, parent, source, tempDir); err != nil {
		return nil, err
	}
	name := "helmr-context-" + rand.Text()
	if err := parent.Mkdir(name, 0700); err != nil {
		return nil, err
	}
	captured.name = name
	captured.Path = filepath.Join(tempDir, name)
	destination, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer destination.Close()
	snapshot := &sourceSnapshot{
		root: root, observed: make(map[string]sourceObserved), directories: make(map[string][]string),
		prefixes: []string{source, captured.Path},
	}
	if err := snapshot.collect(ctx); err != nil {
		return nil, err
	}
	if err := snapshot.materialize(ctx, destination); err != nil {
		return nil, err
	}
	if err := snapshot.verify(ctx); err != nil {
		return nil, err
	}
	if err := validatePlacement(root, parent, source, tempDir); err != nil {
		return nil, err
	}
	if err := sameRootPath(destination, captured.Path); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return captured, nil
}

func sameRootPath(root *os.Root, name string) error {
	held, err := root.Stat(".")
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil || resolved != name {
		return fmt.Errorf("build capture directory placement changed: %q", name)
	}
	actual, err := os.Stat(name)
	if err != nil || !os.SameFile(held, actual) {
		return fmt.Errorf("build capture directory identity changed: %q", name)
	}
	return nil
}

func validatePlacement(source, parent *os.Root, sourcePath, parentPath string) error {
	if err := sameRootPath(source, sourcePath); err != nil {
		return err
	}
	if err := sameRootPath(parent, parentPath); err != nil {
		return err
	}
	sourceInfo, err := source.Stat(".")
	if err != nil {
		return err
	}
	// Compare actual ancestor identities as well as resolved names: lexical
	// prefix checks alone miss case aliases and symlinked temporary directories.
	for current := parentPath; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return err
		}
		if os.SameFile(sourceInfo, info) {
			return errors.New("build capture temp dir must be outside the source root")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return nil
}

func (snapshot *sourceSnapshot) materialize(ctx context.Context, destination *os.Root) error {
	sort.Slice(snapshot.entries, func(i, j int) bool { return snapshot.entries[i].name < snapshot.entries[j].name })
	selected := make(map[string]sourceEntry, len(snapshot.entries))
	for _, entry := range snapshot.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		selected[entry.name] = entry
		var err error
		switch {
		case entry.info.IsDir():
			err = destination.Mkdir(entry.name, 0755)
		case entry.info.Mode()&os.ModeSymlink != 0:
			err = destination.Symlink(entry.linkname, entry.name)
		default:
			err = snapshot.copyFile(ctx, destination, entry)
		}
		if err != nil {
			return fmt.Errorf("create build context entry %q exclusively: %w", entry.name, err)
		}
	}
	// Read names back from the filesystem. Successful creates alone do not prove
	// spelling fidelity on filesystems that normalize Unicode names.
	count := 0
	directories := []string{"."}
	for _, entry := range snapshot.entries {
		if entry.info.IsDir() {
			directories = append(directories, entry.name)
		}
	}
	for _, name := range directories {
		file, err := destination.Open(name)
		if err != nil {
			return err
		}
		readErr := func() error {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				children, err := file.ReadDir(128)
				for _, child := range children {
					full := child.Name()
					if name != "." {
						full = name + "/" + full
					}
					if _, ok := selected[full]; !ok {
						return fmt.Errorf("destination did not preserve exact source name %q", full)
					}
					count++
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
			}
		}()
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
	}
	if count != len(selected) {
		return errors.New("destination did not preserve every source entry")
	}
	if err := snapshot.validateLinks(ctx, selected); err != nil {
		return err
	}

	// Normalize after children have been created. Link timestamps are set without
	// following links; ownership belongs to the local caller and is never chowned.
	for _, entry := range snapshot.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := normalizeEntry(destination, entry.name, entry.info); err != nil {
			return err
		}
	}
	return normalizeEntry(destination, ".", nil)
}

func (snapshot *sourceSnapshot) copyFile(ctx context.Context, destination *os.Root, entry sourceEntry) error {
	file, err := destination.OpenFile(entry.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// .helmrignore is copied from the exact bytes used to select this tree.
	var copyErr error
	if entry.body != nil {
		_, copyErr = io.Copy(file, strings.NewReader(string(entry.body)))
	} else {
		copyErr = snapshot.writeFile(ctx, file, entry)
	}
	return errors.Join(copyErr, file.Close())
}

func normalizedMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0700
	}
	if info.IsDir() || info.Mode().Perm()&0111 != 0 {
		return 0755
	}
	return 0644
}

var captureEpoch = time.Unix(0, 0)

// Kept here so platform timestamp code needs only a confined parent descriptor.
func entryParent(root *os.Root, name string) (*os.File, string, error) {
	parent, err := root.Open(path.Dir(name))
	return parent, path.Base(name), err
}

// Resolve the captured inventory, without following links through os.Root.Stat
// (which has a lower link-hop limit than the mounted Program). Missing targets
// remain confined dangling links. Never open file contents through a link.
func (snapshot *sourceSnapshot) validateLinks(ctx context.Context, selected map[string]sourceEntry) error {
	for _, link := range snapshot.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if link.info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		pending := append(strings.Split(path.Dir(link.name), "/"), strings.Split(link.linkname, "/")...)
		var resolved []string
		hops := 1 // Include the original link being resolved.
		for len(pending) != 0 {
			component := pending[0]
			pending = pending[1:]
			switch component {
			case "", ".":
				continue
			case "..":
				if len(resolved) == 0 {
					return fmt.Errorf("build context symlink %q escapes the project root", link.name)
				}
				resolved = resolved[:len(resolved)-1]
				continue
			}
			candidate := strings.Join(append(resolved, component), "/")
			if err := validateSourcePath(candidate); err != nil {
				return err
			}
			for _, prefix := range snapshot.prefixes {
				if err := safepath.ValidateHostTreePath(prefix, candidate); err != nil {
					return err
				}
			}
			entry, exists := selected[candidate]
			if !exists {
				break
			}
			if entry.info.Mode()&os.ModeSymlink != 0 {
				hops++
				if hops > safepath.TreeLinkHops {
					return fmt.Errorf("build context symlink %q exceeds %d hops", link.name, safepath.TreeLinkHops)
				}
				pending = append(strings.Split(entry.linkname, "/"), pending...)
				continue
			}
			if !entry.info.IsDir() && len(pending) != 0 {
				break
			}
			resolved = append(resolved, component)
		}
	}
	return nil
}
