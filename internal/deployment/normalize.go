package deployment

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/text/cases"
)

const (
	managerArchiveExpansionLimit  = int64(200)
	managerZIPLocalSignature      = uint32(0x04034b50)
	managerZIPCentralSignature    = uint32(0x02014b50)
	managerZIPEndSignature        = uint32(0x06054b50)
	managerZIPDescriptorSignature = uint32(0x08074b50)
)

var (
	errManagerArchiveLayout = errors.New("manager archive layout is unsupported")
	errManagerArchiveLimit  = errors.New("manager archive limit exceeded")
)

type managerNormalizedEntry struct {
	kind   ManagerAcquireEntryKind
	mode   string
	path   string
	size   int64
	offset int64
}

type managerNormalizedTree struct {
	entries map[string]managerNormalizedEntry
	spool   *os.File
}

func NormalizeManagerArchive(
	destination io.Writer,
	request ManagerAcquireRequest,
	archive *os.File,
	scratchDir string,
) error {
	writer, err := NewManagerAcquireResponseWriter(destination, request)
	if err != nil {
		return err
	}
	if err := validateManagerArchive(archive, request); err != nil {
		return writeManagerNormalizeFailure(writer, err)
	}
	var tree managerNormalizedTree
	switch request.PackageManager.Name {
	case PackageManagerBun:
		tree, err = normalizeBunArchive(archive, request, scratchDir)
	case PackageManagerNPM:
		tree, err = normalizeNPMArchive(archive, request, scratchDir)
	default:
		err = fmt.Errorf(
			"%w: package manager %q",
			errManagerArchiveLayout,
			request.PackageManager.Name,
		)
	}
	if tree.spool != nil {
		defer tree.spool.Close()
	}
	if err != nil {
		return writeManagerNormalizeFailure(writer, err)
	}
	names := make([]string, 0, len(tree.entries))
	for name := range tree.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry := tree.entries[name]
		switch entry.kind {
		case ManagerAcquireEntryDirectory:
			if err := writer.WriteDirectory(entry.path); err != nil {
				return err
			}
		case ManagerAcquireEntryRegular:
			var content io.Reader
			if tree.spool == nil {
				return errors.New("manager normalized regular has no content spool")
			}
			content = io.NewSectionReader(tree.spool, entry.offset, entry.size)
			if err := writer.WriteRegular(
				entry.path,
				entry.mode,
				entry.size,
				content,
			); err != nil {
				return err
			}
		default:
			return fmt.Errorf(
				"manager normalized entry %q has unsupported kind %q",
				entry.path,
				entry.kind,
			)
		}
	}
	return writer.WriteTerminal(ManagerAcquireStatusOK)
}

func writeManagerNormalizeFailure(
	writer *ManagerAcquireResponseWriter,
	err error,
) error {
	status := ManagerAcquireStatusInternalError
	switch {
	case errors.Is(err, errManagerArchiveLimit):
		status = ManagerAcquireStatusLimitExceeded
	case errors.Is(err, errManagerArchiveLayout):
		status = ManagerAcquireStatusUnsupportedLayout
	}
	if terminalErr := writer.WriteTerminal(status); terminalErr != nil {
		return errors.Join(err, terminalErr)
	}
	return nil
}

func validateManagerArchive(
	archive *os.File,
	request ManagerAcquireRequest,
) error {
	if archive == nil {
		return errors.New("manager archive file is nil")
	}
	info, err := archive.Stat()
	if err != nil {
		return fmt.Errorf("inspect manager archive: %w", err)
	}
	if !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0600 ||
		info.Size() != request.Source.SizeBytes {
		return fmt.Errorf(
			"%w: manager archive does not match the request size and file contract",
			errManagerArchiveLayout,
		)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind manager archive: %w", err)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, archive)
	if err != nil {
		return fmt.Errorf("hash manager archive: %w", err)
	}
	if written != request.Source.SizeBytes ||
		"sha256:"+hex.EncodeToString(hash.Sum(nil)) != request.Source.Digest {
		return fmt.Errorf("%w: manager archive digest mismatch", errManagerArchiveLayout)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind manager archive: %w", err)
	}
	return nil
}

func normalizeBunArchive(
	archive *os.File,
	request ManagerAcquireRequest,
	scratchDir string,
) (managerNormalizedTree, error) {
	if err := validateZIPEnvelope(archive, request.Source.SizeBytes); err != nil {
		return managerNormalizedTree{}, err
	}
	reader, err := zip.NewReader(archive, request.Source.SizeBytes)
	if err != nil {
		return managerNormalizedTree{}, fmt.Errorf("%w: open Bun ZIP: %v", errManagerArchiveLayout, err)
	}
	_, _, origin, err := managerDistribution(
		request.PackageManager,
		request.Architecture,
	)
	if err != nil {
		return managerNormalizedTree{}, fmt.Errorf("%w: %v", errManagerArchiveLayout, err)
	}
	asset := origin[strings.LastIndexByte(origin, '/')+1:]
	root := strings.TrimSuffix(asset, ".zip")
	wantFile := root + "/bun"
	seen := newManagerSourcePaths()
	var executable *zip.File
	rootSeen := false
	var logicalBytes int64
	for _, file := range reader.File {
		name, err := managerArchivePath(file.Name, file.FileInfo().IsDir())
		if err != nil {
			return managerNormalizedTree{}, err
		}
		if err := seen.accept(name); err != nil {
			return managerNormalizedTree{}, err
		}
		if file.NonUTF8 ||
			file.Flags&1 != 0 ||
			file.Flags&^uint16(0x080e) != 0 ||
			(file.Method != zip.Store && file.Method != zip.Deflate) {
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: Bun ZIP entry %q uses unsupported flags or compression",
				errManagerArchiveLayout,
				name,
			)
		}
		if file.UncompressedSize64 > math.MaxInt64 ||
			file.CompressedSize64 > math.MaxInt64 {
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: Bun ZIP entry %q size is unsupported",
				errManagerArchiveLimit,
				name,
			)
		}
		size := int64(file.UncompressedSize64)
		compressed := int64(file.CompressedSize64)
		if size > managerArchiveExpansionLimit*max(compressed, 1) {
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: Bun ZIP entry %q exceeds the expansion ratio",
				errManagerArchiveLimit,
				name,
			)
		}
		if size > ManagerAcquireMaxLogicalBytes-logicalBytes {
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: Bun ZIP exceeds the logical byte limit",
				errManagerArchiveLimit,
			)
		}
		logicalBytes += size
		switch name {
		case root:
			if !file.FileInfo().IsDir() || size != 0 {
				return managerNormalizedTree{}, fmt.Errorf(
					"%w: Bun root is not a directory",
					errManagerArchiveLayout,
				)
			}
			rootSeen = true
		case wantFile:
			if !file.Mode().IsRegular() || file.FileInfo().IsDir() {
				return managerNormalizedTree{}, fmt.Errorf(
					"%w: Bun executable is not regular",
					errManagerArchiveLayout,
				)
			}
			executable = file
		default:
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: Bun ZIP contains path %q",
				errManagerArchiveLayout,
				name,
			)
		}
	}
	if executable == nil || !rootSeen || len(reader.File) != 2 {
		return managerNormalizedTree{}, fmt.Errorf(
			"%w: Bun ZIP does not have the closed distribution layout",
			errManagerArchiveLayout,
		)
	}
	if logicalBytes > managerArchiveExpansionLimit*max(request.Source.SizeBytes, 1) {
		return managerNormalizedTree{}, fmt.Errorf(
			"%w: Bun ZIP exceeds the aggregate expansion ratio",
			errManagerArchiveLimit,
		)
	}
	spool, err := managerNormalizeSpool(scratchDir)
	if err != nil {
		return managerNormalizedTree{}, err
	}
	committed := false
	defer func() {
		if !committed {
			spool.Close()
		}
	}()
	content, err := executable.Open()
	if err != nil {
		return managerNormalizedTree{}, fmt.Errorf("%w: open Bun executable: %v", errManagerArchiveLayout, err)
	}
	header := make([]byte, 64)
	if _, err := io.ReadFull(content, header); err != nil {
		content.Close()
		return managerNormalizedTree{}, fmt.Errorf("%w: read Bun ELF header: %v", errManagerArchiveLayout, err)
	}
	if err := validateManagerELF(header, request.Architecture); err != nil {
		content.Close()
		return managerNormalizedTree{}, err
	}
	if _, err := spool.Write(header); err != nil {
		content.Close()
		return managerNormalizedTree{}, fmt.Errorf("spool Bun executable: %w", err)
	}
	if err := copyManagerNormalizedContent(
		spool,
		content,
		int64(executable.UncompressedSize64)-int64(len(header)),
		"Bun executable",
	); err != nil {
		content.Close()
		return managerNormalizedTree{}, err
	}
	var extra [1]byte
	if count, err := content.Read(extra[:]); count != 0 || err != io.EOF {
		content.Close()
		return managerNormalizedTree{}, fmt.Errorf(
			"%w: Bun executable payload is malformed",
			errManagerArchiveLayout,
		)
	}
	if err := content.Close(); err != nil {
		return managerNormalizedTree{}, fmt.Errorf("%w: close Bun executable: %v", errManagerArchiveLayout, err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return managerNormalizedTree{}, fmt.Errorf("rewind Bun executable spool: %w", err)
	}
	committed = true
	return managerNormalizedTree{
		spool: spool,
		entries: map[string]managerNormalizedEntry{
			"bin": {
				kind: ManagerAcquireEntryDirectory,
				mode: ManagerAcquireModeExecutable,
				path: "bin",
			},
			"bin/bun": {
				kind: ManagerAcquireEntryRegular,
				mode: ManagerAcquireModeExecutable,
				path: "bin/bun",
				size: int64(executable.UncompressedSize64),
			},
		},
	}, nil
}

func normalizeNPMArchive(
	archive *os.File,
	request ManagerAcquireRequest,
	scratchDir string,
) (managerNormalizedTree, error) {
	spool, err := managerNormalizeSpool(scratchDir)
	if err != nil {
		return managerNormalizedTree{}, err
	}
	committed := false
	defer func() {
		if !committed {
			spool.Close()
		}
	}()
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return managerNormalizedTree{}, fmt.Errorf("rewind npm archive: %w", err)
	}
	compressed := bufio.NewReader(archive)
	gzipReader, err := gzip.NewReader(compressed)
	if err != nil {
		return managerNormalizedTree{}, fmt.Errorf("%w: open npm gzip: %v", errManagerArchiveLayout, err)
	}
	gzipReader.Multistream(false)
	streamLimit := ManagerAcquireMaxLogicalBytes +
		int64(ManagerAcquireMaxEntries)*1024 +
		1024
	if ratioLimit := managerArchiveExpansionLimit * max(request.Source.SizeBytes, 1); ratioLimit < streamLimit {
		streamLimit = ratioLimit
	}
	stream := newManagerTarStream(gzipReader, streamLimit)
	tarReader := tar.NewReader(stream)
	tree := managerNormalizedTree{
		entries: make(map[string]managerNormalizedEntry),
		spool:   spool,
	}
	seen := newManagerSourcePaths()
	var sourceEntries int
	var logicalBytes int64
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf("%w: read npm tar: %v", errManagerArchiveLayout, err)
		}
		sourceEntries++
		if sourceEntries > ManagerAcquireMaxEntries {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm tar exceeds the entry limit",
				errManagerArchiveLimit,
			)
		}
		isDirectory := header.Typeflag == tar.TypeDir
		isRegular := header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
		if !isDirectory && !isRegular ||
			header.Linkname != "" ||
			len(header.Xattrs) != 0 ||
			managerSparsePAX(header.PAXRecords) {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm tar entry %q has an unsupported type or metadata",
				errManagerArchiveLayout,
				header.Name,
			)
		}
		if header.Mode != 0644 && header.Mode != 0755 {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm tar entry %q has unsupported mode %#o",
				errManagerArchiveLayout,
				header.Name,
				header.Mode,
			)
		}
		if isDirectory && header.Size != 0 {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm tar directory %q has content",
				errManagerArchiveLayout,
				header.Name,
			)
		}
		name, err := managerArchivePath(header.Name, isDirectory)
		if err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, err
		}
		if err := seen.accept(name); err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, err
		}
		mapped, err := mapNPMArchivePath(name)
		if err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, err
		}
		if isDirectory {
			if err := tree.addDirectory(mapped); err != nil {
				gzipReader.Close()
				return managerNormalizedTree{}, err
			}
			continue
		}
		if header.Size < 0 ||
			header.Size > ManagerAcquireMaxLogicalBytes-logicalBytes {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm tar exceeds the logical byte limit",
				errManagerArchiveLimit,
			)
		}
		logicalBytes += header.Size
		if logicalBytes > managerArchiveExpansionLimit*max(request.Source.SizeBytes, 1) {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm tar exceeds the aggregate expansion ratio",
				errManagerArchiveLimit,
			)
		}
		offset, err := spool.Seek(0, io.SeekCurrent)
		if err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, fmt.Errorf("inspect npm spool offset: %w", err)
		}
		if err := copyManagerNormalizedContent(
			spool,
			tarReader,
			header.Size,
			"npm entry "+name,
		); err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, err
		}
		mode := ManagerAcquireModeReadOnly
		if header.Mode == 0755 {
			mode = ManagerAcquireModeExecutable
		}
		if err := tree.addRegular(mapped, mode, header.Size, offset); err != nil {
			gzipReader.Close()
			return managerNormalizedTree{}, err
		}
	}
	if _, err := io.Copy(managerZeroWriter{}, stream); err != nil {
		gzipReader.Close()
		if errors.Is(err, errManagerArchiveLimit) {
			return managerNormalizedTree{}, err
		}
		return managerNormalizedTree{}, fmt.Errorf("%w: npm tar trailing data: %v", errManagerArchiveLayout, err)
	}
	if err := stream.validate(); err != nil {
		gzipReader.Close()
		return managerNormalizedTree{}, err
	}
	if err := gzipReader.Close(); err != nil {
		return managerNormalizedTree{}, fmt.Errorf("%w: close npm gzip: %v", errManagerArchiveLayout, err)
	}
	if extra, err := compressed.ReadByte(); err != io.EOF {
		if err == nil {
			return managerNormalizedTree{}, fmt.Errorf(
				"%w: npm gzip has trailing compressed data %#x",
				errManagerArchiveLayout,
				extra,
			)
		}
		return managerNormalizedTree{}, fmt.Errorf("read npm gzip EOF: %w", err)
	}
	if err := tree.addDirectory("lib"); err != nil {
		return managerNormalizedTree{}, err
	}
	if err := tree.addDirectory("lib/npm"); err != nil {
		return managerNormalizedTree{}, err
	}
	if len(tree.entries) > ManagerAcquireMaxEntries {
		return managerNormalizedTree{}, fmt.Errorf(
			"%w: normalized npm tree exceeds the entry limit",
			errManagerArchiveLimit,
		)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return managerNormalizedTree{}, fmt.Errorf("rewind npm spool: %w", err)
	}
	if err := validateManagerNormalizedFamily(request.PackageManager, tree.entries); err != nil {
		return managerNormalizedTree{}, err
	}
	committed = true
	return tree, nil
}

func validateManagerNormalizedFamily(
	manager PackageManager,
	entries map[string]managerNormalizedEntry,
) error {
	wireEntries := make(map[string]ManagerAcquireEntry, len(entries))
	for name, entry := range entries {
		wireEntries[name] = ManagerAcquireEntry{
			Kind:      entry.kind,
			Mode:      entry.mode,
			Path:      entry.path,
			SizeBytes: entry.size,
			Type:      managerAcquireEntryType,
		}
	}
	if err := validateManagerAcquireFamily(manager, wireEntries); err != nil {
		return fmt.Errorf("%w: %v", errManagerArchiveLayout, err)
	}
	return nil
}

func (tree *managerNormalizedTree) addDirectory(name string) error {
	if name == "" {
		return fmt.Errorf("%w: normalized directory is empty", errManagerArchiveLayout)
	}
	if existing, ok := tree.entries[name]; ok {
		if existing.kind != ManagerAcquireEntryDirectory {
			return fmt.Errorf(
				"%w: normalized path %q is both a file and directory",
				errManagerArchiveLayout,
				name,
			)
		}
		return nil
	}
	if err := validateManagerAcquirePath(name); err != nil {
		return fmt.Errorf("%w: %v", errManagerArchiveLayout, err)
	}
	if len(tree.entries) >= ManagerAcquireMaxEntries {
		return fmt.Errorf(
			"%w: normalized tree exceeds the entry limit",
			errManagerArchiveLimit,
		)
	}
	tree.entries[name] = managerNormalizedEntry{
		kind: ManagerAcquireEntryDirectory,
		mode: ManagerAcquireModeExecutable,
		path: name,
	}
	if parent := path.Dir(name); parent != "." {
		return tree.addDirectory(parent)
	}
	return nil
}

func (tree *managerNormalizedTree) addRegular(
	name string,
	mode string,
	size int64,
	offset int64,
) error {
	if _, ok := tree.entries[name]; ok {
		return fmt.Errorf("%w: duplicate normalized path %q", errManagerArchiveLayout, name)
	}
	if err := validateManagerAcquirePath(name); err != nil {
		return fmt.Errorf("%w: %v", errManagerArchiveLayout, err)
	}
	if parent := path.Dir(name); parent != "." {
		if err := tree.addDirectory(parent); err != nil {
			return err
		}
	}
	if len(tree.entries) >= ManagerAcquireMaxEntries {
		return fmt.Errorf(
			"%w: normalized tree exceeds the entry limit",
			errManagerArchiveLimit,
		)
	}
	tree.entries[name] = managerNormalizedEntry{
		kind:   ManagerAcquireEntryRegular,
		mode:   mode,
		path:   name,
		size:   size,
		offset: offset,
	}
	return nil
}

type managerSourcePaths struct {
	exact  map[string]struct{}
	folded map[string]string
}

func newManagerSourcePaths() managerSourcePaths {
	return managerSourcePaths{
		exact:  make(map[string]struct{}),
		folded: make(map[string]string),
	}
}

func (paths managerSourcePaths) accept(name string) error {
	if _, ok := paths.exact[name]; ok {
		return fmt.Errorf("%w: duplicate archive path %q", errManagerArchiveLayout, name)
	}
	folded := cases.Fold().String(name)
	if existing, ok := paths.folded[folded]; ok {
		return fmt.Errorf(
			"%w: archive paths %q and %q collide under case folding",
			errManagerArchiveLayout,
			existing,
			name,
		)
	}
	paths.exact[name] = struct{}{}
	paths.folded[folded] = name
	return nil
}

func managerArchivePath(name string, directory bool) (string, error) {
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	if err := validateManagerAcquirePath(name); err != nil {
		return "", fmt.Errorf("%w: archive path: %v", errManagerArchiveLayout, err)
	}
	return name, nil
}

func mapNPMArchivePath(name string) (string, error) {
	if name == "package" {
		return "lib/npm", nil
	}
	if !strings.HasPrefix(name, "package/") {
		return "", fmt.Errorf(
			"%w: npm tar path %q is outside package",
			errManagerArchiveLayout,
			name,
		)
	}
	return "lib/npm/" + strings.TrimPrefix(name, "package/"), nil
}

func managerSparsePAX(records map[string]string) bool {
	for name := range records {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "sparse") ||
			strings.Contains(lower, "xattr") {
			return true
		}
	}
	return false
}

func managerNormalizeSpool(directory string) (*os.File, error) {
	if directory == "" {
		directory = os.TempDir()
	}
	file, err := os.CreateTemp(directory, ".helmr-manager-*")
	if err != nil {
		return nil, fmt.Errorf("create manager normalization spool: %w", err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return nil, fmt.Errorf("unlink manager normalization spool: %w", err)
	}
	return file, nil
}

func validateManagerELF(header []byte, architecture RuntimeArchitecture) error {
	if len(header) < 64 ||
		!bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) ||
		header[4] != 2 ||
		header[5] != 1 ||
		header[6] != 1 {
		return fmt.Errorf("%w: Bun executable is not a little-endian ELF64 file", errManagerArchiveLayout)
	}
	fileType := binary.LittleEndian.Uint16(header[16:18])
	if fileType != 2 && fileType != 3 {
		return fmt.Errorf("%w: Bun executable ELF type is unsupported", errManagerArchiveLayout)
	}
	machine := binary.LittleEndian.Uint16(header[18:20])
	var want uint16
	switch architecture {
	case ArchitectureAArch64:
		want = 183
	case ArchitectureX8664:
		want = 62
	default:
		return fmt.Errorf("%w: architecture %q is unsupported", errManagerArchiveLayout, architecture)
	}
	if machine != want {
		return fmt.Errorf(
			"%w: Bun executable machine = %d, want %d",
			errManagerArchiveLayout,
			machine,
			want,
		)
	}
	return nil
}

type managerTarStream struct {
	reader io.Reader
	tail   [1024]byte
	total  int64
	limit  int64
}

func newManagerTarStream(reader io.Reader, limit int64) *managerTarStream {
	return &managerTarStream{reader: reader, limit: limit}
}

func (reader *managerTarStream) Read(destination []byte) (int, error) {
	count, err := reader.reader.Read(destination)
	if int64(count) > reader.limit-reader.total {
		return 0, errManagerArchiveLimit
	}
	for _, value := range destination[:count] {
		reader.tail[reader.total%int64(len(reader.tail))] = value
		reader.total++
	}
	return count, err
}

func copyManagerNormalizedContent(
	destination io.Writer,
	source io.Reader,
	size int64,
	label string,
) error {
	buffer := make([]byte, 64<<10)
	remaining := size
	for remaining > 0 {
		next := int64(len(buffer))
		if remaining < next {
			next = remaining
		}
		count, err := io.ReadFull(source, buffer[:next])
		if err != nil {
			return fmt.Errorf("%w: read %s: %v", errManagerArchiveLayout, label, err)
		}
		if written, err := destination.Write(buffer[:count]); err != nil {
			return fmt.Errorf("write %s spool: %w", label, err)
		} else if written != count {
			return fmt.Errorf("write %s spool: %w", label, io.ErrShortWrite)
		}
		remaining -= int64(count)
	}
	return nil
}

func (reader *managerTarStream) validate() error {
	if reader.total < int64(len(reader.tail)) || reader.total%512 != 0 {
		return fmt.Errorf("%w: npm tar has no canonical end marker", errManagerArchiveLayout)
	}
	for _, value := range reader.tail {
		if value != 0 {
			return fmt.Errorf("%w: npm tar has no canonical end marker", errManagerArchiveLayout)
		}
	}
	return nil
}

type managerZeroWriter struct{}

func (managerZeroWriter) Write(content []byte) (int, error) {
	for _, value := range content {
		if value != 0 {
			return 0, errors.New("non-zero data follows the tar end marker")
		}
	}
	return len(content), nil
}

type managerZIPCentral struct {
	name       string
	flags      uint16
	method     uint16
	crc        uint32
	compressed uint32
	logical    uint32
	offset     uint32
}

func validateZIPEnvelope(file *os.File, size int64) error {
	if size < 22 {
		return fmt.Errorf("%w: ZIP is shorter than its end record", errManagerArchiveLayout)
	}
	end := make([]byte, 22)
	if _, err := file.ReadAt(end, size-int64(len(end))); err != nil {
		return fmt.Errorf("read ZIP end record: %w", err)
	}
	if binary.LittleEndian.Uint32(end[:4]) != managerZIPEndSignature ||
		binary.LittleEndian.Uint16(end[4:6]) != 0 ||
		binary.LittleEndian.Uint16(end[6:8]) != 0 ||
		binary.LittleEndian.Uint16(end[8:10]) != binary.LittleEndian.Uint16(end[10:12]) ||
		binary.LittleEndian.Uint16(end[20:22]) != 0 {
		return fmt.Errorf("%w: ZIP end record is unsupported", errManagerArchiveLayout)
	}
	entryCount := int(binary.LittleEndian.Uint16(end[10:12]))
	if entryCount < 1 {
		return fmt.Errorf("%w: ZIP is empty", errManagerArchiveLayout)
	}
	if entryCount > ManagerAcquireMaxEntries {
		return fmt.Errorf("%w: ZIP entry count is outside the limit", errManagerArchiveLimit)
	}
	centralSize := int64(binary.LittleEndian.Uint32(end[12:16]))
	centralOffset := int64(binary.LittleEndian.Uint32(end[16:20]))
	if centralOffset < 0 ||
		centralSize < 0 ||
		centralOffset+centralSize != size-22 {
		return fmt.Errorf("%w: ZIP central directory does not fill the archive", errManagerArchiveLayout)
	}
	central, err := readZIPCentral(file, centralOffset, centralSize, entryCount)
	if err != nil {
		return err
	}
	slices.SortFunc(central, func(left, right managerZIPCentral) int {
		return int(left.offset) - int(right.offset)
	})
	var next int64
	for index, entry := range central {
		if int64(entry.offset) != next {
			return fmt.Errorf("%w: ZIP local entries are not contiguous", errManagerArchiveLayout)
		}
		next, err = validateZIPLocal(file, entry)
		if err != nil {
			return err
		}
		if index+1 < len(central) && next != int64(central[index+1].offset) {
			return fmt.Errorf("%w: ZIP local entry has trailing data", errManagerArchiveLayout)
		}
	}
	if next != centralOffset {
		return fmt.Errorf("%w: ZIP local data does not end at the central directory", errManagerArchiveLayout)
	}
	return nil
}

func readZIPCentral(
	file *os.File,
	offset int64,
	size int64,
	count int,
) ([]managerZIPCentral, error) {
	reader := io.NewSectionReader(file, offset, size)
	entries := make([]managerZIPCentral, 0, count)
	for range count {
		fixed := make([]byte, 46)
		if _, err := io.ReadFull(reader, fixed); err != nil {
			return nil, fmt.Errorf("%w: read ZIP central entry: %v", errManagerArchiveLayout, err)
		}
		if binary.LittleEndian.Uint32(fixed[:4]) != managerZIPCentralSignature {
			return nil, fmt.Errorf("%w: ZIP central signature is invalid", errManagerArchiveLayout)
		}
		nameLength := int(binary.LittleEndian.Uint16(fixed[28:30]))
		extraLength := int(binary.LittleEndian.Uint16(fixed[30:32]))
		commentLength := int(binary.LittleEndian.Uint16(fixed[32:34]))
		if binary.LittleEndian.Uint16(fixed[34:36]) != 0 ||
			binary.LittleEndian.Uint32(fixed[20:24]) == math.MaxUint32 ||
			binary.LittleEndian.Uint32(fixed[24:28]) == math.MaxUint32 ||
			binary.LittleEndian.Uint32(fixed[42:46]) == math.MaxUint32 {
			return nil, fmt.Errorf("%w: ZIP64 or multi-disk entry is unsupported", errManagerArchiveLayout)
		}
		variable := make([]byte, nameLength+extraLength+commentLength)
		if _, err := io.ReadFull(reader, variable); err != nil {
			return nil, fmt.Errorf("%w: read ZIP central fields: %v", errManagerArchiveLayout, err)
		}
		entries = append(entries, managerZIPCentral{
			name:       string(variable[:nameLength]),
			flags:      binary.LittleEndian.Uint16(fixed[8:10]),
			method:     binary.LittleEndian.Uint16(fixed[10:12]),
			crc:        binary.LittleEndian.Uint32(fixed[16:20]),
			compressed: binary.LittleEndian.Uint32(fixed[20:24]),
			logical:    binary.LittleEndian.Uint32(fixed[24:28]),
			offset:     binary.LittleEndian.Uint32(fixed[42:46]),
		})
	}
	var extra [1]byte
	if count, err := reader.Read(extra[:]); count != 0 || err != io.EOF {
		return nil, fmt.Errorf("%w: ZIP central directory has trailing data", errManagerArchiveLayout)
	}
	return entries, nil
}

func validateZIPLocal(
	file *os.File,
	entry managerZIPCentral,
) (int64, error) {
	fixed := make([]byte, 30)
	if _, err := file.ReadAt(fixed, int64(entry.offset)); err != nil {
		return 0, fmt.Errorf("%w: read ZIP local entry: %v", errManagerArchiveLayout, err)
	}
	if binary.LittleEndian.Uint32(fixed[:4]) != managerZIPLocalSignature ||
		binary.LittleEndian.Uint16(fixed[6:8]) != entry.flags ||
		binary.LittleEndian.Uint16(fixed[8:10]) != entry.method {
		return 0, fmt.Errorf("%w: ZIP local entry disagrees with its central entry", errManagerArchiveLayout)
	}
	nameLength := int64(binary.LittleEndian.Uint16(fixed[26:28]))
	extraLength := int64(binary.LittleEndian.Uint16(fixed[28:30]))
	name := make([]byte, nameLength)
	if _, err := file.ReadAt(name, int64(entry.offset)+30); err != nil ||
		string(name) != entry.name {
		return 0, fmt.Errorf("%w: ZIP local entry name is invalid", errManagerArchiveLayout)
	}
	dataEnd := int64(entry.offset) + 30 + nameLength + extraLength + int64(entry.compressed)
	if entry.flags&8 == 0 {
		if binary.LittleEndian.Uint32(fixed[14:18]) != entry.crc ||
			binary.LittleEndian.Uint32(fixed[18:22]) != entry.compressed ||
			binary.LittleEndian.Uint32(fixed[22:26]) != entry.logical {
			return 0, fmt.Errorf("%w: ZIP local sizes disagree with central authority", errManagerArchiveLayout)
		}
		return dataEnd, nil
	}
	descriptor := make([]byte, 16)
	if _, err := file.ReadAt(descriptor, dataEnd); err != nil {
		return 0, fmt.Errorf("%w: read ZIP data descriptor: %v", errManagerArchiveLayout, err)
	}
	start := 0
	length := int64(12)
	if binary.LittleEndian.Uint32(descriptor[:4]) == managerZIPDescriptorSignature {
		start = 4
		length = 16
	}
	if binary.LittleEndian.Uint32(descriptor[start:start+4]) != entry.crc ||
		binary.LittleEndian.Uint32(descriptor[start+4:start+8]) != entry.compressed ||
		binary.LittleEndian.Uint32(descriptor[start+8:start+12]) != entry.logical {
		return 0, fmt.Errorf("%w: ZIP data descriptor disagrees with central authority", errManagerArchiveLayout)
	}
	return dataEnd + length, nil
}
