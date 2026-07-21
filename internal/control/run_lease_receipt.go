package control

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func equalRunLeaseReceipt(left, right api.WorkerRunLeaseReceipt) bool {
	leftDeadline, rightDeadline := left.StartDeadlineAt, right.StartDeadlineAt
	leftExpiry, rightExpiry := left.ExpiresAt, right.ExpiresAt
	left.StartDeadlineAt, right.StartDeadlineAt = time.Time{}, time.Time{}
	left.ExpiresAt, right.ExpiresAt = time.Time{}, time.Time{}
	return left == right &&
		leftDeadline.Equal(rightDeadline) &&
		leftExpiry.Equal(rightExpiry)
}
