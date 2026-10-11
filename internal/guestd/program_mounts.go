package guestd

import (
	"errors"
	"path/filepath"
)

// Each executable owner passes its immutable sources to its private namespace.
// Ordinary Computer commands pass the zero value and require neither source.
type programMounts struct {
	Runtime  string
	Artifact string
}

func (p programMounts) validate() error {
	if p == (programMounts{}) {
		return nil
	}
	if !filepath.IsAbs(p.Runtime) || !filepath.IsAbs(p.Artifact) || filepath.Clean(p.Runtime) != p.Runtime || filepath.Clean(p.Artifact) != p.Artifact {
		return errors.New("program sources must be canonical absolute directories")
	}
	return nil
}

// Boot drives are used by the existing Computer preparation runtime. Session
// processes supply their own materialized sources instead.
func bootProgramMounts() programMounts {
	return programMounts{Runtime: "/var/lib/helmr/program/runtime", Artifact: "/var/lib/helmr/program/artifact"}
}
