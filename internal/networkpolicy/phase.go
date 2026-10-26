package networkpolicy

import "errors"

// Phase identifies the trusted host operation installing outbound authority.
type Phase string

const (
	PhaseRuntime       Phase = "runtime"
	PhasePreparation   Phase = "preparation"
	PhaseBootSeed      Phase = "boot_seed"
	PhaseQualification Phase = "qualification"
)

func (p Phase) Validate() error {
	switch p {
	case PhaseRuntime, PhasePreparation, PhaseBootSeed, PhaseQualification:
		return nil
	default:
		return errors.New("network execution phase is missing or unsupported")
	}
}
