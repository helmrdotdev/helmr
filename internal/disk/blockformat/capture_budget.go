package blockformat

import (
	"encoding/json"
	"math"
	"strings"
)

// CaptureBudget bounds ciphertext for the specified changed paths, including
// full existing leaf references, node encryption, pack directories and the root.
// It needs no source reads. Keys must be the complete retained tree key closure.
type CaptureBudget struct {
	shape                          Root
	dataHeader, leaf, branch, root int64
}

func NewCaptureBudget(shape Root, scope string, keys map[string][]byte) (CaptureBudget, error) {

	if err := validateRoot(shape); err != nil {
		return CaptureBudget{}, err
	}
	longest := ""
	longestJSON := 0
	maxKeyBytes := 0
	for key := range keys {
		maxKeyBytes = max(maxKeyBytes, len(key))
		raw, err := json.Marshal(key)
		if err != nil {
			return CaptureBudget{}, err
		}
		if len(raw) > longestJSON {
			longest, longestJSON = key, len(raw)
		}
	}
	ref := Ref{Key: longest, Kind: RootKind, Count: MaxRecords, Size: math.MaxInt64}
	for i := range ref.Digest {
		ref.Digest[i], ref.Salt[i] = 255, 255
	}
	locatorRef := ref
	locatorRef.Key = strings.Repeat("x", maxKeyBytes)
	locator := Locator{Pack: PackRef{Digest: ref.Digest, Size: math.MaxInt64, Rank: 255}, Page: locatorRef, Offset: math.MaxInt64}
	header, err := Header(scope, Ref{Key: strings.Repeat("x", maxKeyBytes), Kind: RootKind, Count: 1})
	if err != nil {
		return CaptureBudget{}, err
	}
	descriptor, err := json.Marshal(ref)
	if err != nil {
		return CaptureBudget{}, err
	}
	directory, _, err := encodePack(255, nil)
	if err != nil {
		return CaptureBudget{}, err
	}
	objectOverhead := int64(len(header) + 20 + len(descriptor) + len(directory) + 1)
	leaf := Node{Capacity: shape.Capacity, Fanout: shape.Fanout, Level: shape.Level, Start: MaxBlocks}
	branch := leaf
	for i := 0; i < shape.Fanout; i++ {
		leaf.Segments = append(leaf.Segments, ref)
		leaf.Entries = append(leaf.Entries, Entry{Slot: i, Segment: shape.Fanout, Record: MaxRecords})
		child := locator
		branch.Entries = append(branch.Entries, Entry{Slot: i, Child: &child})
	}
	leafRaw, err := json.Marshal(leaf)
	if err != nil {
		return CaptureBudget{}, err
	}
	branchRaw, err := json.Marshal(branch)
	if err != nil {
		return CaptureBudget{}, err
	}
	rootRaw, err := json.Marshal(Root{Capacity: shape.Capacity, Fanout: shape.Fanout, Level: shape.Level, Index: &locator})
	if err != nil {
		return CaptureBudget{}, err
	}
	return CaptureBudget{shape: shape, dataHeader: int64(len(header)), leaf: int64(len(leafRaw)) + objectOverhead, branch: int64(len(branchRaw)) + objectOverhead, root: int64(len(rootRaw)) + objectOverhead}, nil
}

func (b CaptureBudget) Bytes(blocks []uint64) int64 {
	if len(blocks) == 0 {
		return 0
	}
	// At most one data segment for MaxChangedBlocks. Per-record framing is20bytes.
	budget := b.dataHeader + int64(len(blocks))*(BlockSize+20) + b.root
	span := uint64(b.shape.Fanout)
	for level := 0; level <= b.shape.Level; level++ {
		touched := make(map[uint64]struct{})
		for _, block := range blocks {
			touched[block/span] = struct{}{}
		}
		size := b.branch
		if level == 0 {
			size = b.leaf
		}
		budget += int64(len(touched)) * size
		span *= uint64(b.shape.Fanout)
	}
	return budget
}
