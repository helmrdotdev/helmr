package auditpolicy

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Policy is supplied by the process owner. A zero value never disables capture
// or selects an implicit retention period.
type Policy struct{ RetentionSeconds int64 }

func (p Policy) Validate() error {
	if p.RetentionSeconds <= 0 || p.RetentionSeconds > math.MaxInt64/int64(time.Second) {
		return errors.New("audit retention must be positive and representable in seconds")
	}
	return nil
}

func Parse(value string) (Policy, error) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return Policy{}, errors.New("AUDIT_RETENTION_SECONDS must be explicitly configured in seconds")
	}
	p := Policy{RetentionSeconds: seconds}
	return p, p.Validate()
}
