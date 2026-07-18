package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	tarBlockBytes             int64 = 512
	maxCodeArchiveBytes             = 3 << 30
	maxDependencyArchiveBytes       = 9 << 30
)

type treeEntry struct {
	Path       string
	Kind       artifactEntryKind
	Mode       uint32
	SizeBytes  int64
	LinkTarget string
	Content    io.Reader
}

func writeProgramArchive(
	ctx context.Context,
	destination io.Writer,
	role artifactRole,
	entries []treeEntry,
) error {
	if ctx == nil {
		return errors.New("program archive context is nil")
	}
	if destination == nil {
		return errors.New("program archive destination is nil")
	}
	maxBytes, err := programArchiveLimit(role)
	if err != nil {
		return err
	}
	if err := validateProgramArchive(entries, role, maxBytes); err != nil {
		return err
	}

	for position := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("write program archive: %w", err)
		}
		entry := &entries[position]
		digest := sha256.Sum256([]byte(entry.Path))
		suffix := hex.EncodeToString(digest[:])
		pax := paxRecord("path", entry.Path)
		if entry.Kind == artifactEntrySymlink {
			pax = append(pax, paxRecord("linkpath", entry.LinkTarget)...)
		}
		if err := writeTarMember(
			destination,
			"PaxHeaders/"+suffix,
			0644,
			int64(len(pax)),
			'x',
			pax,
		); err != nil {
			return fmt.Errorf("write program archive PAX header for %q: %w", entry.Path, err)
		}

		typeFlag := byte('0')
		switch entry.Kind {
		case artifactEntryDirectory:
			typeFlag = '5'
		case artifactEntrySymlink:
			typeFlag = '2'
		}
		size := entry.SizeBytes
		if entry.Kind != artifactEntryRegular {
			size = 0
		}
		header, err := tarHeader("Entries/"+suffix, entry.Mode, size, typeFlag)
		if err != nil {
			return fmt.Errorf("encode program archive header for %q: %w", entry.Path, err)
		}
		if _, err := destination.Write(header[:]); err != nil {
			return fmt.Errorf("write program archive header for %q: %w", entry.Path, err)
		}
		if entry.Kind == artifactEntryRegular {
			if err := copyTreeContent(ctx, destination, entry.Content, entry.SizeBytes); err != nil {
				return fmt.Errorf("write program archive file %q: %w", entry.Path, err)
			}
			if err := writeTarPadding(destination, entry.SizeBytes); err != nil {
				return fmt.Errorf("pad program archive file %q: %w", entry.Path, err)
			}
		}
	}
	var end [2 * tarBlockBytes]byte
	if _, err := destination.Write(end[:]); err != nil {
		return fmt.Errorf("write program archive end marker: %w", err)
	}
	return nil
}

func validateProgramArchive(entries []treeEntry, role artifactRole, maxBytes int64) error {
	if len(entries) == 0 || len(entries) >= maxArtifactEntries {
		return fmt.Errorf("program archive entry count is outside [1,%d]", maxArtifactEntries-1)
	}
	directories := map[string]struct{}{".": {}}
	var previous string
	var nameBytes int64 = 1
	var logicalBytes int64
	var archiveBytes int64 = 2 * tarBlockBytes
	logicalLimit := maxCodeLogicalBytes
	if role == dependencyArtifact {
		logicalLimit = maxDependencyLogicalBytes
	}

	for position := range entries {
		entry := entries[position]
		if entry.Path == "." {
			return fmt.Errorf("program archive entry %d explicitly represents the root", position)
		}
		if position > 0 && bytes.Compare([]byte(previous), []byte(entry.Path)) >= 0 {
			return fmt.Errorf("program archive entries are duplicate or out of order at %q", entry.Path)
		}
		previous = entry.Path
		if err := validateTreeEntry(entry, role); err != nil {
			return fmt.Errorf("program archive entry %d %q: %w", position, entry.Path, err)
		}
		parent := pathDir(entry.Path)
		if _, exists := directories[parent]; !exists {
			return fmt.Errorf("program archive entry %q has no preceding directory parent %q", entry.Path, parent)
		}
		if entry.Kind == artifactEntryDirectory {
			directories[entry.Path] = struct{}{}
		}

		entryNameBytes := int64(len(entry.Path) + len(entry.LinkTarget))
		if nameBytes > maxArtifactNameBytes-entryNameBytes {
			return fmt.Errorf(
				"program archive raw path and symbolic-link-target bytes exceed %d",
				maxArtifactNameBytes,
			)
		}
		nameBytes += entryNameBytes
		if entry.Kind == artifactEntryRegular {
			if logicalBytes > logicalLimit-entry.SizeBytes {
				return fmt.Errorf(
					"program archive logical regular-file bytes exceed %d",
					logicalLimit,
				)
			}
			logicalBytes += entry.SizeBytes
		}

		paxBytes := int64(len(paxRecord("path", entry.Path)))
		if entry.Kind == artifactEntrySymlink {
			paxBytes += int64(len(paxRecord("linkpath", entry.LinkTarget)))
		}
		increment := 2*tarBlockBytes + roundTarBytes(paxBytes)
		if entry.Kind == artifactEntryRegular {
			increment += roundTarBytes(entry.SizeBytes)
		}
		if archiveBytes > maxBytes-increment {
			return fmt.Errorf("program archive exceeds %d bytes", maxBytes)
		}
		archiveBytes += increment
	}
	return nil
}

func validateTreeEntry(entry treeEntry, role artifactRole) error {
	if err := validateArtifactPath(entry.Path, role); err != nil {
		return err
	}
	if entry.SizeBytes < 0 {
		return errors.New("logical size is negative")
	}
	switch entry.Kind {
	case artifactEntryRegular:
		if entry.Mode != 0644 && entry.Mode != 0755 {
			return fmt.Errorf("regular-file mode %#o is unsupported", entry.Mode)
		}
		if entry.SizeBytes > maxArtifactFileSize {
			return fmt.Errorf("regular file exceeds %d bytes", maxArtifactFileSize)
		}
		if entry.LinkTarget != "" || entry.Content == nil {
			return errors.New("regular-file content metadata is invalid")
		}
	case artifactEntryDirectory:
		if entry.Mode != 0755 || entry.SizeBytes != 0 ||
			entry.LinkTarget != "" || entry.Content != nil {
			return errors.New("directory metadata is invalid")
		}
	case artifactEntrySymlink:
		if entry.Mode != 0777 || entry.SizeBytes != 0 || entry.Content != nil {
			return errors.New("symbolic-link metadata is invalid")
		}
		if err := validateSymlinkTarget(entry.LinkTarget); err != nil {
			return err
		}
	default:
		return fmt.Errorf("entry kind %q is unsupported", entry.Kind)
	}
	return nil
}

func programArchiveLimit(role artifactRole) (int64, error) {
	switch role {
	case codeArtifact:
		return maxCodeArchiveBytes, nil
	case dependencyArtifact:
		return maxDependencyArchiveBytes, nil
	default:
		return 0, fmt.Errorf("program archive artifact role = %d", role)
	}
}

func paxRecord(key, value string) []byte {
	payload := key + "=" + value + "\n"
	length := len(payload) + 2
	for {
		next := len(strconv.Itoa(length)) + 1 + len(payload)
		if next == length {
			return []byte(strconv.Itoa(length) + " " + payload)
		}
		length = next
	}
}

func writeTarMember(
	destination io.Writer,
	name string,
	mode uint32,
	size int64,
	typeFlag byte,
	body []byte,
) error {
	header, err := tarHeader(name, mode, size, typeFlag)
	if err != nil {
		return err
	}
	if _, err := destination.Write(header[:]); err != nil {
		return err
	}
	if _, err := destination.Write(body); err != nil {
		return err
	}
	return writeTarPadding(destination, size)
}

func tarHeader(
	name string,
	mode uint32,
	size int64,
	typeFlag byte,
) ([tarBlockBytes]byte, error) {
	var header [tarBlockBytes]byte
	if len(name) == 0 || len(name) > 100 {
		return header, fmt.Errorf("tar header name length = %d", len(name))
	}
	if size < 0 {
		return header, errors.New("tar header size is negative")
	}
	copy(header[0:100], name)
	if err := writeTarOctal(header[100:108], uint64(mode)); err != nil {
		return header, fmt.Errorf("encode tar mode: %w", err)
	}
	for _, field := range [][]byte{
		header[108:116],
		header[116:124],
		header[136:148],
		header[329:337],
		header[337:345],
	} {
		if err := writeTarOctal(field, 0); err != nil {
			return header, err
		}
	}
	if err := writeTarOctal(header[124:136], uint64(size)); err != nil {
		return header, fmt.Errorf("encode tar size: %w", err)
	}
	for index := 148; index < 156; index++ {
		header[index] = ' '
	}
	header[156] = typeFlag
	copy(header[257:263], "ustar\x00")
	copy(header[263:265], "00")
	var checksum uint64
	for _, value := range header {
		checksum += uint64(value)
	}
	if checksum > 0777777 {
		return header, fmt.Errorf("tar checksum %o exceeds six octal digits", checksum)
	}
	for index := 148; index < 154; index++ {
		header[index] = '0'
	}
	checksumValue := strconv.FormatUint(checksum, 8)
	copy(header[154-len(checksumValue):154], checksumValue)
	header[154] = 0
	header[155] = ' '
	return header, nil
}

func writeTarOctal(field []byte, value uint64) error {
	if len(field) < 2 {
		return errors.New("tar octal field is too short")
	}
	encoded := strconv.FormatUint(value, 8)
	if len(encoded) > len(field)-1 {
		return fmt.Errorf("octal value %o does not fit %d bytes", value, len(field))
	}
	for index := range len(field) - 1 {
		field[index] = '0'
	}
	copy(field[len(field)-1-len(encoded):len(field)-1], encoded)
	field[len(field)-1] = 0
	return nil
}

func copyTreeContent(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
	size int64,
) error {
	var buffer [128 << 10]byte
	remaining := size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		count, readErr := source.Read(buffer[:want])
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
			remaining -= int64(count)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if remaining == 0 {
					return nil
				}
				return fmt.Errorf("content is %d bytes shorter than declared", remaining)
			}
			return readErr
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	var extra [1]byte
	count, err := source.Read(extra[:])
	if count != 0 {
		return errors.New("content is longer than declared")
	}
	if err == nil {
		return io.ErrNoProgress
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writeTarPadding(destination io.Writer, size int64) error {
	padding := roundTarBytes(size) - size
	if padding == 0 {
		return nil
	}
	var zeros [tarBlockBytes]byte
	_, err := destination.Write(zeros[:padding])
	return err
}

func roundTarBytes(size int64) int64 {
	return (size + tarBlockBytes - 1) / tarBlockBytes * tarBlockBytes
}

func pathDir(value string) string {
	for index := len(value) - 1; index >= 0; index-- {
		if value[index] == '/' {
			return value[:index]
		}
	}
	return "."
}
