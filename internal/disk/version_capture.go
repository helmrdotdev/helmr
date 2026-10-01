package disk

import "context"

// CapturedVersion owns an exact local disk cut until Release. Publication does
// not transfer remote recovery ownership: the caller must commit that separately.
// Release joins active publication and is idempotent. It does not stop the VM.
type CapturedVersion interface {
	Root() VersionRoot
	Publish(context.Context, ContinuationPublication) error
	Release()
}
