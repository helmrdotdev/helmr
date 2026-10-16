package vm

import "fmt"

// ComputerCaptureError reports an online cut that did not yield a usable receipt.
// The caller must retain the admitted save request. Ambiguity means local effects
// may exist; it does not establish corrupt data or authorize deleting staging.
type ComputerCaptureError struct {
	Phase string
	Cause error
}

func (e *ComputerCaptureError) Error() string {
	return fmt.Sprintf("Computer %s failed before capture receipt: %v", e.Phase, e.Cause)
}
func (e *ComputerCaptureError) Unwrap() error { return e.Cause }
