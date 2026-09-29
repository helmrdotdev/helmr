// Package blockformat owns immutable Computer page and pack identities.
// It provides framing, not storage access or publication authority.
package blockformat

// Ref binds both the random encryption identity and immutable ciphertext identity.
type Ref struct {
	Digest [32]byte
	Salt   [32]byte
	Key    string
	Kind   byte
	Count  uint32
	Size   int64
}

type PackRef struct {
	Digest [32]byte
	Size   int64
	Rank   int
}
type Locator struct {
	Pack   PackRef
	Page   Ref
	Offset int64
}
