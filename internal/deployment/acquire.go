package deployment

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"golang.org/x/text/cases"
)

const (
	ManagerAcquireFormatVersion = 0

	ManagerAcquireEntryDirectory = ManagerAcquireEntryKind("directory")
	ManagerAcquireEntryRegular   = ManagerAcquireEntryKind("regular")

	ManagerAcquireModeReadOnly            = "0444"
	ManagerAcquireModeExecutable          = "0555"
	ManagerAcquireStatusOK                = ManagerAcquireStatus("ok")
	ManagerAcquireStatusUnsupportedLayout = ManagerAcquireStatus("unsupported_layout")
	ManagerAcquireStatusLimitExceeded     = ManagerAcquireStatus("limit_exceeded")
	ManagerAcquireStatusInternalError     = ManagerAcquireStatus("internal_error")
	ManagerAcquireMaxFrameBytes           = 64 << 10
	ManagerAcquireMaxEntries              = 32768
	ManagerAcquireMaxLogicalBytes         = int64(512 << 20)
	ManagerAcquireMaxPathBytes            = 4095
	ManagerAcquireMaxComponentBytes       = 255
	ManagerAcquireMaxInputBytes           = uint64(4 + ManagerAcquireMaxFrameBytes + maxManagerDistributionBytes)

	managerAcquireEntryType    = "entry"
	managerAcquireTerminalType = "terminal"
)

type ManagerAcquireEntryKind string
type ManagerAcquireStatus string

type ManagerAcquireSource struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ManagerAcquireRequest struct {
	Architecture   RuntimeArchitecture  `json:"architecture"`
	FormatVersion  int                  `json:"formatVersion"`
	PackageManager PackageManager       `json:"packageManager"`
	Source         ManagerAcquireSource `json:"source"`
}

type ManagerAcquireEntry struct {
	Kind      ManagerAcquireEntryKind `json:"kind"`
	Mode      string                  `json:"mode"`
	Path      string                  `json:"path"`
	SizeBytes int64                   `json:"sizeBytes"`
	Type      string                  `json:"type"`
}

type ManagerAcquireTerminal struct {
	EntryCount   int64                `json:"entryCount"`
	LogicalBytes int64                `json:"logicalBytes"`
	Status       ManagerAcquireStatus `json:"status"`
	Type         string               `json:"type"`
}

func CanonicalManagerAcquireRequest(request ManagerAcquireRequest) ([]byte, error) {
	if err := validateManagerAcquireRequest(request); err != nil {
		return nil, err
	}
	return canonicalManagerAcquireDocument(request, "manager acquisition request")
}

func ParseManagerAcquireRequest(raw []byte) (ManagerAcquireRequest, error) {
	var request ManagerAcquireRequest
	if err := parseManagerAcquireDocument(
		raw,
		&request,
		"manager acquisition request",
	); err != nil {
		return ManagerAcquireRequest{}, err
	}
	if err := validateManagerAcquireRequest(request); err != nil {
		return ManagerAcquireRequest{}, err
	}
	complete, err := CanonicalManagerAcquireRequest(request)
	if err != nil {
		return ManagerAcquireRequest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerAcquireRequest{}, errors.New(
			"manager acquisition request does not match the complete canonical v0 shape",
		)
	}
	return request, nil
}

func WriteManagerAcquireRequest(
	destination io.Writer,
	request ManagerAcquireRequest,
	archive io.Reader,
) error {
	if destination == nil {
		return errors.New("manager acquisition request destination is nil")
	}
	if archive == nil {
		return errors.New("manager acquisition archive is nil")
	}
	body, err := CanonicalManagerAcquireRequest(request)
	if err != nil {
		return err
	}
	if err := writeManagerAcquireFrame(destination, body); err != nil {
		return fmt.Errorf("write manager acquisition request record: %w", err)
	}
	digest, err := copyManagerAcquireExact(destination, archive, request.Source.SizeBytes)
	if err != nil {
		return fmt.Errorf("write manager acquisition archive: %w", err)
	}
	if digest != request.Source.Digest {
		return fmt.Errorf(
			"manager acquisition archive digest = %q, want %q",
			digest,
			request.Source.Digest,
		)
	}
	return nil
}

func ReadManagerAcquireRequest(
	source io.Reader,
	archiveDestination *os.File,
) (request ManagerAcquireRequest, returnErr error) {
	if source == nil {
		return ManagerAcquireRequest{}, errors.New("manager acquisition request source is nil")
	}
	if archiveDestination == nil {
		return ManagerAcquireRequest{}, errors.New(
			"manager acquisition archive destination is nil",
		)
	}
	if err := validateManagerAcquireFile(
		archiveDestination,
		"archive destination",
	); err != nil {
		return ManagerAcquireRequest{}, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := resetManagerAcquireFile(archiveDestination); err != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("discard manager acquisition archive: %w", err),
			)
		}
	}()
	body, err := readManagerAcquireFrame(source)
	if err != nil {
		return ManagerAcquireRequest{}, fmt.Errorf(
			"read manager acquisition request record: %w",
			err,
		)
	}
	request, err = ParseManagerAcquireRequest(body)
	if err != nil {
		return ManagerAcquireRequest{}, err
	}
	digest, err := copyManagerAcquireExact(
		archiveDestination,
		source,
		request.Source.SizeBytes,
	)
	if err != nil {
		return ManagerAcquireRequest{}, fmt.Errorf(
			"read manager acquisition archive: %w",
			err,
		)
	}
	if digest != request.Source.Digest {
		return ManagerAcquireRequest{}, fmt.Errorf(
			"manager acquisition archive digest = %q, want %q",
			digest,
			request.Source.Digest,
		)
	}
	if _, err := archiveDestination.Seek(0, io.SeekStart); err != nil {
		return ManagerAcquireRequest{}, fmt.Errorf(
			"rewind manager acquisition archive: %w",
			err,
		)
	}
	committed = true
	return request, nil
}

func CanonicalManagerAcquireEntry(entry ManagerAcquireEntry) ([]byte, error) {
	if err := validateManagerAcquireEntry(entry); err != nil {
		return nil, err
	}
	return canonicalManagerAcquireDocument(entry, "manager acquisition entry")
}

func ParseManagerAcquireEntry(raw []byte) (ManagerAcquireEntry, error) {
	var entry ManagerAcquireEntry
	if err := parseManagerAcquireDocument(
		raw,
		&entry,
		"manager acquisition entry",
	); err != nil {
		return ManagerAcquireEntry{}, err
	}
	if err := validateManagerAcquireEntry(entry); err != nil {
		return ManagerAcquireEntry{}, err
	}
	complete, err := CanonicalManagerAcquireEntry(entry)
	if err != nil {
		return ManagerAcquireEntry{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerAcquireEntry{}, errors.New(
			"manager acquisition entry does not match the complete canonical v0 shape",
		)
	}
	return entry, nil
}

func CanonicalManagerAcquireTerminal(terminal ManagerAcquireTerminal) ([]byte, error) {
	if err := validateManagerAcquireTerminal(terminal); err != nil {
		return nil, err
	}
	return canonicalManagerAcquireDocument(terminal, "manager acquisition terminal")
}

func ParseManagerAcquireTerminal(raw []byte) (ManagerAcquireTerminal, error) {
	var terminal ManagerAcquireTerminal
	if err := parseManagerAcquireDocument(
		raw,
		&terminal,
		"manager acquisition terminal",
	); err != nil {
		return ManagerAcquireTerminal{}, err
	}
	if err := validateManagerAcquireTerminal(terminal); err != nil {
		return ManagerAcquireTerminal{}, err
	}
	complete, err := CanonicalManagerAcquireTerminal(terminal)
	if err != nil {
		return ManagerAcquireTerminal{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerAcquireTerminal{}, errors.New(
			"manager acquisition terminal does not match the complete canonical v0 shape",
		)
	}
	return terminal, nil
}

type ManagerAcquireResponseWriter struct {
	destination io.Writer
	request     ManagerAcquireRequest
	state       managerAcquireEntryState
	finished    bool
	failed      error
}

func NewManagerAcquireResponseWriter(
	destination io.Writer,
	request ManagerAcquireRequest,
) (*ManagerAcquireResponseWriter, error) {
	if destination == nil {
		return nil, errors.New("manager acquisition response destination is nil")
	}
	if err := validateManagerAcquireRequest(request); err != nil {
		return nil, err
	}
	return &ManagerAcquireResponseWriter{
		destination: destination,
		request:     request,
		state:       newManagerAcquireEntryState(),
	}, nil
}

func (writer *ManagerAcquireResponseWriter) WriteDirectory(path string) error {
	return writer.writeEntry(ManagerAcquireEntry{
		Kind:      ManagerAcquireEntryDirectory,
		Mode:      ManagerAcquireModeExecutable,
		Path:      path,
		SizeBytes: 0,
		Type:      managerAcquireEntryType,
	}, nil)
}

func (writer *ManagerAcquireResponseWriter) WriteRegular(
	path string,
	mode string,
	sizeBytes int64,
	content io.Reader,
) error {
	if content == nil {
		return writer.fail(errors.New("manager acquisition regular content is nil"))
	}
	return writer.writeEntry(ManagerAcquireEntry{
		Kind:      ManagerAcquireEntryRegular,
		Mode:      mode,
		Path:      path,
		SizeBytes: sizeBytes,
		Type:      managerAcquireEntryType,
	}, content)
}

func (writer *ManagerAcquireResponseWriter) WriteTerminal(
	status ManagerAcquireStatus,
) error {
	if writer == nil {
		return errors.New("manager acquisition response writer is nil")
	}
	if writer.failed != nil {
		return writer.failed
	}
	if writer.finished {
		return errors.New("manager acquisition response is already terminal")
	}
	terminal := ManagerAcquireTerminal{
		EntryCount:   int64(writer.state.count),
		LogicalBytes: writer.state.logicalBytes,
		Status:       status,
		Type:         managerAcquireTerminalType,
	}
	if err := validateManagerAcquireTerminal(terminal); err != nil {
		return writer.fail(err)
	}
	if status == ManagerAcquireStatusOK {
		if err := validateManagerAcquireFamily(
			writer.request.PackageManager,
			writer.state.entries,
		); err != nil {
			return writer.fail(err)
		}
	}
	body, err := CanonicalManagerAcquireTerminal(terminal)
	if err != nil {
		return writer.fail(err)
	}
	if err := writeManagerAcquireFrame(writer.destination, body); err != nil {
		return writer.fail(fmt.Errorf("write manager acquisition terminal: %w", err))
	}
	writer.finished = true
	return nil
}

func (writer *ManagerAcquireResponseWriter) writeEntry(
	entry ManagerAcquireEntry,
	content io.Reader,
) error {
	if writer == nil {
		return errors.New("manager acquisition response writer is nil")
	}
	if writer.failed != nil {
		return writer.failed
	}
	if writer.finished {
		return errors.New("manager acquisition response is already terminal")
	}
	if err := writer.state.accept(entry); err != nil {
		return writer.fail(err)
	}
	body, err := CanonicalManagerAcquireEntry(entry)
	if err != nil {
		return writer.fail(err)
	}
	if err := writeManagerAcquireFrame(writer.destination, body); err != nil {
		return writer.fail(fmt.Errorf("write manager acquisition entry: %w", err))
	}
	if entry.Kind == ManagerAcquireEntryRegular {
		if err := copyManagerAcquireContentExact(
			writer.destination,
			content,
			entry.SizeBytes,
		); err != nil {
			return writer.fail(fmt.Errorf(
				"write manager acquisition entry %q payload: %w",
				entry.Path,
				err,
			))
		}
	}
	writer.state.commit(entry)
	return nil
}

func (writer *ManagerAcquireResponseWriter) fail(err error) error {
	if writer != nil && writer.failed == nil {
		writer.failed = err
	}
	return err
}

func ReadManagerAcquireResponse(
	source io.Reader,
	provisional *os.File,
	request ManagerAcquireRequest,
) (terminal ManagerAcquireTerminal, returnErr error) {
	if source == nil {
		return ManagerAcquireTerminal{}, errors.New(
			"manager acquisition response source is nil",
		)
	}
	if provisional == nil {
		return ManagerAcquireTerminal{}, errors.New(
			"manager acquisition provisional writer is nil",
		)
	}
	if err := validateManagerAcquireRequest(request); err != nil {
		return ManagerAcquireTerminal{}, err
	}
	if err := validateManagerAcquireFile(
		provisional,
		"provisional output",
	); err != nil {
		return ManagerAcquireTerminal{}, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := resetManagerAcquireFile(provisional); err != nil {
			returnErr = errors.Join(
				returnErr,
				fmt.Errorf("discard manager acquisition provisional output: %w", err),
			)
		}
	}()

	state := newManagerAcquireEntryState()
	archive := tar.NewWriter(provisional)
	for {
		body, err := readManagerAcquireFrame(source)
		if err != nil {
			return ManagerAcquireTerminal{}, fmt.Errorf(
				"read manager acquisition response record: %w",
				err,
			)
		}
		recordType, err := managerAcquireRecordType(body)
		if err != nil {
			return ManagerAcquireTerminal{}, err
		}
		switch recordType {
		case managerAcquireEntryType:
			entry, err := ParseManagerAcquireEntry(body)
			if err != nil {
				return ManagerAcquireTerminal{}, err
			}
			if err := state.accept(entry); err != nil {
				return ManagerAcquireTerminal{}, err
			}
			if err := writeManagerAcquireTarEntry(archive, source, entry); err != nil {
				return ManagerAcquireTerminal{}, err
			}
			state.commit(entry)
		case managerAcquireTerminalType:
			terminal, err = ParseManagerAcquireTerminal(body)
			if err != nil {
				return ManagerAcquireTerminal{}, err
			}
			if terminal.EntryCount != int64(state.count) {
				return ManagerAcquireTerminal{}, fmt.Errorf(
					"manager acquisition terminal entryCount = %d, want %d",
					terminal.EntryCount,
					state.count,
				)
			}
			if terminal.LogicalBytes != state.logicalBytes {
				return ManagerAcquireTerminal{}, fmt.Errorf(
					"manager acquisition terminal logicalBytes = %d, want %d",
					terminal.LogicalBytes,
					state.logicalBytes,
				)
			}
			if err := requireManagerAcquireEOF(source); err != nil {
				return ManagerAcquireTerminal{}, fmt.Errorf(
					"read manager acquisition response EOF: %w",
					err,
				)
			}
			if terminal.Status != ManagerAcquireStatusOK {
				return terminal, nil
			}
			if err := validateManagerAcquireFamily(
				request.PackageManager,
				state.entries,
			); err != nil {
				return ManagerAcquireTerminal{}, err
			}
			if err := archive.Close(); err != nil {
				return ManagerAcquireTerminal{}, fmt.Errorf(
					"finish manager acquisition tar: %w",
					err,
				)
			}
			if _, err := provisional.Seek(0, io.SeekStart); err != nil {
				return ManagerAcquireTerminal{}, fmt.Errorf(
					"rewind manager acquisition provisional output: %w",
					err,
				)
			}
			committed = true
			return terminal, nil
		default:
			return ManagerAcquireTerminal{}, fmt.Errorf(
				"manager acquisition response record type %q is unsupported",
				recordType,
			)
		}
	}
}

func validateManagerAcquireFile(file *os.File, label string) error {
	if file == nil {
		return fmt.Errorf("manager acquisition %s file is nil", label)
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect manager acquisition %s file: %w", label, err)
	}
	if !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0600 ||
		info.Size() != 0 {
		return fmt.Errorf(
			"manager acquisition %s must be an empty regular mode 0600 file",
			label,
		)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf(
			"inspect manager acquisition %s offset: %w",
			label,
			err,
		)
	}
	if offset != 0 {
		return fmt.Errorf("manager acquisition %s is not at offset zero", label)
	}
	return nil
}

func resetManagerAcquireFile(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return nil
}

type managerAcquireEntryState struct {
	entries      map[string]ManagerAcquireEntry
	folded       map[string]string
	directories  map[string]struct{}
	previous     string
	count        int
	logicalBytes int64
}

func newManagerAcquireEntryState() managerAcquireEntryState {
	return managerAcquireEntryState{
		entries:     make(map[string]ManagerAcquireEntry),
		folded:      make(map[string]string),
		directories: map[string]struct{}{".": {}},
	}
}

func (state *managerAcquireEntryState) accept(entry ManagerAcquireEntry) error {
	if err := validateManagerAcquireEntry(entry); err != nil {
		return err
	}
	if state.count >= ManagerAcquireMaxEntries {
		return fmt.Errorf(
			"manager acquisition entry count exceeds %d",
			ManagerAcquireMaxEntries,
		)
	}
	if state.count > 0 && bytes.Compare(
		[]byte(state.previous),
		[]byte(entry.Path),
	) >= 0 {
		return fmt.Errorf(
			"manager acquisition entries are duplicate or out of order at %q",
			entry.Path,
		)
	}
	folded := cases.Fold().String(entry.Path)
	if previous, exists := state.folded[folded]; exists && previous != entry.Path {
		return fmt.Errorf(
			"manager acquisition paths %q and %q have a case-fold collision",
			previous,
			entry.Path,
		)
	}
	parent := path.Dir(entry.Path)
	if _, exists := state.directories[parent]; !exists {
		return fmt.Errorf(
			"manager acquisition entry %q has no preceding directory parent %q",
			entry.Path,
			parent,
		)
	}
	if entry.Kind == ManagerAcquireEntryRegular &&
		state.logicalBytes > ManagerAcquireMaxLogicalBytes-entry.SizeBytes {
		return fmt.Errorf(
			"manager acquisition logical bytes exceed %d",
			ManagerAcquireMaxLogicalBytes,
		)
	}
	return nil
}

func (state *managerAcquireEntryState) commit(entry ManagerAcquireEntry) {
	state.entries[entry.Path] = entry
	state.folded[cases.Fold().String(entry.Path)] = entry.Path
	state.previous = entry.Path
	state.count++
	if entry.Kind == ManagerAcquireEntryDirectory {
		state.directories[entry.Path] = struct{}{}
	} else {
		state.logicalBytes += entry.SizeBytes
	}
}

func validateManagerAcquireRequest(request ManagerAcquireRequest) error {
	if request.FormatVersion != ManagerAcquireFormatVersion {
		return fmt.Errorf(
			"manager acquisition request formatVersion = %d, want %d",
			request.FormatVersion,
			ManagerAcquireFormatVersion,
		)
	}
	if !validArchitecture(request.Architecture) {
		return fmt.Errorf(
			"manager acquisition request architecture %q is unsupported",
			request.Architecture,
		)
	}
	if err := validateManagerPackage(request.PackageManager); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(request.Source.Digest) {
		return errors.New(
			"manager acquisition source digest is not a lowercase SHA-256 digest",
		)
	}
	if request.Source.SizeBytes < 1 ||
		request.Source.SizeBytes > maxManagerDistributionBytes {
		return fmt.Errorf(
			"manager acquisition source sizeBytes is outside [1,%d]",
			maxManagerDistributionBytes,
		)
	}
	return nil
}

func validateManagerAcquireEntry(entry ManagerAcquireEntry) error {
	if entry.Type != managerAcquireEntryType {
		return fmt.Errorf(
			"manager acquisition entry type = %q, want %q",
			entry.Type,
			managerAcquireEntryType,
		)
	}
	if err := validateManagerAcquirePath(entry.Path); err != nil {
		return err
	}
	if entry.SizeBytes < 0 || entry.SizeBytes > ManagerAcquireMaxLogicalBytes {
		return fmt.Errorf(
			"manager acquisition entry %q sizeBytes is outside [0,%d]",
			entry.Path,
			ManagerAcquireMaxLogicalBytes,
		)
	}
	switch entry.Kind {
	case ManagerAcquireEntryDirectory:
		if entry.Mode != ManagerAcquireModeExecutable || entry.SizeBytes != 0 {
			return fmt.Errorf(
				"manager acquisition directory %q must have mode %s and size zero",
				entry.Path,
				ManagerAcquireModeExecutable,
			)
		}
	case ManagerAcquireEntryRegular:
		if entry.Mode != ManagerAcquireModeReadOnly &&
			entry.Mode != ManagerAcquireModeExecutable {
			return fmt.Errorf(
				"manager acquisition regular %q has unsupported mode %q",
				entry.Path,
				entry.Mode,
			)
		}
	default:
		return fmt.Errorf(
			"manager acquisition entry kind %q is unsupported",
			entry.Kind,
		)
	}
	return nil
}

func validateManagerAcquireTerminal(terminal ManagerAcquireTerminal) error {
	if terminal.Type != managerAcquireTerminalType {
		return fmt.Errorf(
			"manager acquisition terminal type = %q, want %q",
			terminal.Type,
			managerAcquireTerminalType,
		)
	}
	if terminal.EntryCount < 0 ||
		terminal.EntryCount > ManagerAcquireMaxEntries {
		return fmt.Errorf(
			"manager acquisition terminal entryCount is outside [0,%d]",
			ManagerAcquireMaxEntries,
		)
	}
	if terminal.LogicalBytes < 0 ||
		terminal.LogicalBytes > ManagerAcquireMaxLogicalBytes {
		return fmt.Errorf(
			"manager acquisition terminal logicalBytes is outside [0,%d]",
			ManagerAcquireMaxLogicalBytes,
		)
	}
	switch terminal.Status {
	case ManagerAcquireStatusOK,
		ManagerAcquireStatusUnsupportedLayout,
		ManagerAcquireStatusLimitExceeded,
		ManagerAcquireStatusInternalError:
		return nil
	default:
		return fmt.Errorf(
			"manager acquisition terminal status %q is unsupported",
			terminal.Status,
		)
	}
}

func validateManagerAcquirePath(value string) error {
	if value == "" ||
		len(value) > ManagerAcquireMaxPathBytes ||
		!utf8.ValidString(value) ||
		strings.ContainsRune(value, 0) ||
		strings.ContainsRune(value, '\\') ||
		path.IsAbs(value) ||
		path.Clean(value) != value ||
		value == "." {
		return fmt.Errorf("manager acquisition path %q is not normalized relative UTF-8", value)
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == "" ||
			component == "." ||
			component == ".." ||
			len(component) > ManagerAcquireMaxComponentBytes {
			return fmt.Errorf(
				"manager acquisition path %q has an invalid component",
				value,
			)
		}
	}
	return nil
}

func validateManagerAcquireFamily(
	manager PackageManager,
	entries map[string]ManagerAcquireEntry,
) error {
	switch manager.Name {
	case PackageManagerBun:
		if len(entries) != 2 ||
			!managerAcquireEntryMatches(
				entries["bin"],
				ManagerAcquireEntryDirectory,
				ManagerAcquireModeExecutable,
			) ||
			!managerAcquireEntryMatches(
				entries["bin/bun"],
				ManagerAcquireEntryRegular,
				ManagerAcquireModeExecutable,
			) {
			return errors.New(
				"manager acquisition Bun response does not have the exact bin/bin-bun layout",
			)
		}
	case PackageManagerNPM:
		if !managerAcquireEntryMatches(
			entries["lib"],
			ManagerAcquireEntryDirectory,
			ManagerAcquireModeExecutable,
		) ||
			!managerAcquireEntryMatches(
				entries["lib/npm"],
				ManagerAcquireEntryDirectory,
				ManagerAcquireModeExecutable,
			) {
			return errors.New(
				"manager acquisition npm response is missing its lib/npm roots",
			)
		}
		for name := range entries {
			if name != "lib" &&
				name != "lib/npm" &&
				!strings.HasPrefix(name, "lib/npm/") {
				return fmt.Errorf(
					"manager acquisition npm response path %q is outside lib/npm",
					name,
				)
			}
		}
		if entries["lib/npm/package.json"].Kind != ManagerAcquireEntryRegular {
			return errors.New(
				"manager acquisition npm response requires regular lib/npm/package.json",
			)
		}
		if !managerAcquireEntryMatches(
			entries["lib/npm/bin/npm-cli.js"],
			ManagerAcquireEntryRegular,
			ManagerAcquireModeExecutable,
		) {
			return errors.New(
				"manager acquisition npm response requires executable regular lib/npm/bin/npm-cli.js",
			)
		}
	default:
		return fmt.Errorf(
			"manager acquisition package manager %q is unsupported",
			manager.Name,
		)
	}
	return nil
}

func managerAcquireEntryMatches(
	entry ManagerAcquireEntry,
	kind ManagerAcquireEntryKind,
	mode string,
) bool {
	return entry.Type == managerAcquireEntryType &&
		entry.Kind == kind &&
		entry.Mode == mode
}

func canonicalManagerAcquireDocument(value any, label string) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", label, err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if len(canonical) == 0 || len(canonical) > ManagerAcquireMaxFrameBytes {
		return nil, fmt.Errorf(
			"%s size is outside [1,%d]",
			label,
			ManagerAcquireMaxFrameBytes,
		)
	}
	return canonical, nil
}

func parseManagerAcquireDocument(raw []byte, value any, label string) error {
	if len(raw) == 0 || len(raw) > ManagerAcquireMaxFrameBytes {
		return fmt.Errorf(
			"%s size is outside [1,%d]",
			label,
			ManagerAcquireMaxFrameBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%s is not RFC 8785 canonical JSON", label)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	if err := ensureEOF(decoder, label); err != nil {
		return err
	}
	return nil
}

func managerAcquireRecordType(raw []byte) (string, error) {
	if len(raw) == 0 || len(raw) > ManagerAcquireMaxFrameBytes {
		return "", fmt.Errorf(
			"manager acquisition response record size is outside [1,%d]",
			ManagerAcquireMaxFrameBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return "", fmt.Errorf(
			"canonicalize manager acquisition response record: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return "", errors.New(
			"manager acquisition response record is not RFC 8785 canonical JSON",
		)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf(
			"decode manager acquisition response record type: %w",
			err,
		)
	}
	return envelope.Type, nil
}

func writeManagerAcquireFrame(destination io.Writer, body []byte) error {
	if len(body) == 0 || len(body) > ManagerAcquireMaxFrameBytes {
		return fmt.Errorf(
			"manager acquisition frame size is outside [1,%d]",
			ManagerAcquireMaxFrameBytes,
		)
	}
	if err := frameio.WriteMessageFrame(destination, body); err != nil {
		return err
	}
	return nil
}

func readManagerAcquireFrame(source io.Reader) ([]byte, error) {
	body, err := frameio.ReadMessageFrameBounded(
		source,
		uint32(ManagerAcquireMaxFrameBytes),
	)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, errors.New("manager acquisition frame is empty")
	}
	return body, nil
}

func copyManagerAcquireExact(
	destination io.Writer,
	source io.Reader,
	sizeBytes int64,
) (string, error) {
	hash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(destination, hash), source, sizeBytes)
	if err != nil {
		return "", err
	}
	if written != sizeBytes {
		return "", fmt.Errorf(
			"manager acquisition content is %d bytes shorter than declared",
			sizeBytes-written,
		)
	}
	if err := requireManagerAcquireEOF(source); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func copyManagerAcquireContentExact(
	destination io.Writer,
	source io.Reader,
	sizeBytes int64,
) error {
	written, err := io.CopyN(destination, source, sizeBytes)
	if err != nil {
		return err
	}
	if written != sizeBytes {
		return fmt.Errorf(
			"manager acquisition content is %d bytes shorter than declared",
			sizeBytes-written,
		)
	}
	return requireManagerAcquireEOF(source)
}

func requireManagerAcquireEOF(source io.Reader) error {
	var extra [1]byte
	count, err := source.Read(extra[:])
	if count != 0 {
		return errors.New("manager acquisition input has trailing bytes")
	}
	if err == nil {
		return io.ErrNoProgress
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writeManagerAcquireTarEntry(
	writer *tar.Writer,
	source io.Reader,
	entry ManagerAcquireEntry,
) error {
	mode := int64(0444)
	if entry.Mode == ManagerAcquireModeExecutable {
		mode = 0555
	}
	header := &tar.Header{
		Name:   entry.Path,
		Mode:   mode,
		Size:   entry.SizeBytes,
		Uid:    0,
		Gid:    0,
		Format: tar.FormatPAX,
	}
	if entry.Kind == ManagerAcquireEntryDirectory {
		header.Typeflag = tar.TypeDir
		header.Size = 0
	} else {
		header.Typeflag = tar.TypeReg
	}
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf(
			"write manager acquisition tar header %q: %w",
			entry.Path,
			err,
		)
	}
	if entry.Kind == ManagerAcquireEntryRegular {
		written, err := io.CopyN(writer, source, entry.SizeBytes)
		if err != nil {
			return fmt.Errorf(
				"write manager acquisition tar payload %q: %w",
				entry.Path,
				err,
			)
		}
		if written != entry.SizeBytes {
			return fmt.Errorf(
				"manager acquisition payload %q is %d bytes shorter than declared",
				entry.Path,
				entry.SizeBytes-written,
			)
		}
	}
	return nil
}
