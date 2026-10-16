package disk

import "context"

// CapturedVersion owns an exact local disk cut until Release. Publication does
// not transfer remote recovery ownership: the caller must commit that separately.
// Release joins active publication and is idempotent. It does not stop the VM.
type CapturedVersion interface {
	Root() VersionRoot
	Changes() VersionChanges
	Publish(context.Context, ContinuationPublication) error
	Release()
}

// RetainedVersion owns the local operations needed by the physical save owner.
// BackRemotely establishes object backing, while Adopt additionally requires a
// committed published recovery receipt. Neither operation transfers key custody.
type RetainedVersion interface {
	CapturedVersion
	Adopt(context.Context) error
	BackRemotely(context.Context, int) error
	ReadyForRetirement() bool
	Collect(context.Context, int) (int64, error)
}
