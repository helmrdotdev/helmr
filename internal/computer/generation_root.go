package computer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// GenerationRoot is the persisted/wire identity of one exact disk generation.
// Framing validation is not byte authentication or permission to restore it.
type GenerationRoot struct {
	FormatVersion int            `json:"format_version"`
	LogicalBytes  int64          `json:"logical_bytes"`
	Pack          GenerationPack `json:"pack"`
	Page          GenerationPage `json:"page"`
	Offset        int64          `json:"offset"`
}

type GenerationPack struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	Rank      int    `json:"rank"`
}

type GenerationPage struct {
	Digest    string `json:"digest"`
	Salt      string `json:"salt"`
	KeyID     string `json:"key_id"`
	Kind      int    `json:"kind"`
	Count     int    `json:"count"`
	SizeBytes int64  `json:"size_bytes"`
}

func (r GenerationRoot) Validate(capacity int64) error {
	if r.FormatVersion != 1 || capacity <= 0 || capacity%4096 != 0 || r.LogicalBytes != capacity {
		return errors.New("invalid computer generation format or capacity")
	}
	if !sha256sum.ValidDigest(r.Pack.Digest) {
		return errors.New("invalid generation pack digest")
	}
	if !sha256sum.ValidDigest(r.Page.Digest) {
		return errors.New("invalid generation page digest")
	}
	// A root is a single encrypted metadata record in a rank-separated pack.
	// Incoming page membership and disk geometry still require authenticated bytes.
	if r.Pack.SizeBytes < 8 || r.Pack.SizeBytes > 4<<20 || r.Pack.Rank < 2 || r.Pack.Rank > 6 ||
		r.Page.Kind != 3 || r.Page.Count != 1 || r.Page.SizeBytes <= 20 || r.Page.SizeBytes > r.Pack.SizeBytes ||
		r.Offset < 8 || r.Offset > r.Pack.SizeBytes-r.Page.SizeBytes {
		return errors.New("invalid generation root locator bounds")
	}
	id, err := ids.Parse(r.Page.KeyID)
	if err != nil || id.String() != r.Page.KeyID {
		return errors.New("invalid generation root key identity")
	}
	salt, err := hex.DecodeString(r.Page.Salt)
	if err != nil || len(salt) != 32 || strings.ToLower(r.Page.Salt) != r.Page.Salt {
		return errors.New("invalid generation page salt")
	}
	return nil
}

func ParseGenerationRoot(raw []byte, capacity int64) (GenerationRoot, error) {
	var root GenerationRoot
	if len(raw) == 0 || len(raw) > 2048 {
		return root, errors.New("invalid generation root encoding size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&root); err != nil {
		return GenerationRoot{}, errors.New("invalid generation root encoding")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return GenerationRoot{}, errors.New("trailing generation root data")
	}
	if err := root.Validate(capacity); err != nil {
		return GenerationRoot{}, err
	}
	return root, nil
}
