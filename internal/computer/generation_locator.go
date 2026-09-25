package computer

import (
	"encoding/hex"
	"strings"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// NewGenerationRoot converts a byte verifier's locator and authenticated capacity
// into the persisted descriptor. This conversion validates framing only; callers
// must obtain both inputs from verified bytes before publishing them.
func NewGenerationRoot(locator blockformat.Locator, capacity int64) (GenerationRoot, error) {
	root := GenerationRoot{
		FormatVersion: 1, LogicalBytes: capacity, Offset: locator.Offset,
		Pack: GenerationPack{Digest: "sha256:" + hex.EncodeToString(locator.Pack.Digest[:]), SizeBytes: locator.Pack.Size, Rank: locator.Pack.Rank},
		Page: GenerationPage{Digest: "sha256:" + hex.EncodeToString(locator.Page.Digest[:]), Salt: hex.EncodeToString(locator.Page.Salt[:]),
			KeyID: locator.Page.Key, Kind: int(locator.Page.Kind), Count: int(locator.Page.Count), SizeBytes: locator.Page.Size},
	}
	if err := root.Validate(capacity); err != nil {
		return GenerationRoot{}, err
	}
	return root, nil
}

// Locator validates the persisted descriptor before converting it for byte reads.
// The reader must authenticate membership and compare the decrypted capacity with
// LogicalBytes. A valid descriptor alone does not prove either property.
func (r GenerationRoot) Locator(capacity int64) (blockformat.Locator, error) {
	if err := r.Validate(capacity); err != nil {
		return blockformat.Locator{}, err
	}
	// Validate has established canonical 32-byte hex fields and integer bounds.
	pack, _ := hex.DecodeString(strings.TrimPrefix(r.Pack.Digest, "sha256:"))
	page, _ := hex.DecodeString(strings.TrimPrefix(r.Page.Digest, "sha256:"))
	salt, _ := hex.DecodeString(r.Page.Salt)
	out := blockformat.Locator{
		Pack:   blockformat.PackRef{Size: r.Pack.SizeBytes, Rank: r.Pack.Rank},
		Page:   blockformat.Ref{Key: r.Page.KeyID, Kind: byte(r.Page.Kind), Count: uint32(r.Page.Count), Size: r.Page.SizeBytes},
		Offset: r.Offset,
	}
	copy(out.Pack.Digest[:], pack)
	copy(out.Page.Digest[:], page)
	copy(out.Page.Salt[:], salt)
	return out, nil
}
