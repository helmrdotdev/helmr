// Package lifetime defines the optional resource-retention duration shared by
// admission APIs. It does not apply to execution or observation timeouts.
package lifetime

import (
	"bytes"
	"encoding/json"
	"errors"
)

const DefaultMilliseconds int64 = 7 * 24 * 60 * 60 * 1000
const MaxMilliseconds int64 = 9007199254740991

// Policy distinguishes omission (inherit), explicit null (never), and a positive
// integer number of milliseconds. After resolution only finite/never remains.
type Policy struct {
	Set          bool
	Milliseconds *int64
}

func (p Policy) IsZero() bool { return !p.Set }
func (p Policy) Valid() bool {
	return (p.Set || p.Milliseconds == nil) && (p.Milliseconds == nil || (*p.Milliseconds > 0 && *p.Milliseconds <= MaxMilliseconds))
}
func (p *Policy) UnmarshalJSON(raw []byte) error {
	p.Set = true
	p.Milliseconds = nil
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return errors.New("retention must be null or positive safe integer milliseconds")
	}
	p.Milliseconds = &value
	if !p.Valid() {
		return errors.New("retention must be null or positive safe integer milliseconds")
	}
	return nil
}
func (p Policy) MarshalJSON() ([]byte, error) { return json.Marshal(p.Milliseconds) }
func Resolve(override Policy, duration *int64, never bool) *int64 {
	if override.Set {
		return override.Milliseconds
	}
	if never {
		return nil
	}
	if duration != nil {
		return duration
	}
	value := DefaultMilliseconds
	return &value
}
func Same(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
