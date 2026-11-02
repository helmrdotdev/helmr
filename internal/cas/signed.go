package cas

import (
	"net/http"
	"time"
)

// SignedRequest is one provider request authorized by the storage owner for a
// bounded time. Its holder needs no storage credential and cannot change the
// method, object key or any signed header, including the content checksum and
// the create-only precondition of a write.
type SignedRequest struct {
	Method    string      `json:"method"`
	URL       string      `json:"url"`
	Header    http.Header `json:"header,omitempty"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// CompletedPart identifies one uploaded part of a multipart publication.
type CompletedPart struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
}

// Upload is a provider multipart publication owned by the storage owner. Parts
// cover the descriptor in order with PartSize bytes each except the last.
type Upload struct {
	ID string `json:"id"`
	// Owner names the staging namespace the storage owner assigned; it is never
	// taken from the holder.
	Owner    string `json:"-"`
	PartSize int64  `json:"part_size"`
	Parts    int32  `json:"parts"`
}
