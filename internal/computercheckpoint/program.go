package computercheckpoint

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// Program records immutable mounted backings, independently of Session lifetime.
// Their stores retain them through deployment membership and platform ownership;
// they are not encrypted checkpoint payloads.
type Program struct {
	Runtime  Object `json:"runtime"`
	Artifact Object `json:"artifact"`
}

func (p *Program) Validate() error {
	if p == nil {
		return nil
	}
	for _, item := range []struct {
		object Object
		media  string
		limit  int64
	}{{p.Runtime, artifact.RuntimeArtifactMediaType, artifact.MaxRuntimePhysicalBytes}, {p.Artifact, artifact.ProgramArtifactMediaType, artifact.MaxProgramPhysicalBytes}} {
		if err := cas.ValidateDescriptor(cas.Descriptor(item.object)); err != nil {
			return err
		}
		if item.object.MediaType != item.media || item.object.SizeBytes > item.limit {
			return errors.New("invalid boot Program descriptor")
		}
	}
	return nil
}
func (p *Program) Size() int64 {
	if p == nil {
		return 0
	}
	return p.Runtime.SizeBytes + p.Artifact.SizeBytes
}
