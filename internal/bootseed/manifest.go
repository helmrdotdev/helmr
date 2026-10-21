// Package bootseed describes reusable pre-mount boot artifacts. These artifacts
// carry no Computer lease or continuation authority.
package bootseed

import (
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type Manifest struct {
	EnvironmentID      uuid.UUID      `json:"environment_id"`
	BuildID            uuid.UUID      `json:"build_id"`
	Config             []byte         `json:"config"`
	MemoryBytes        int64          `json:"memory_bytes"`
	ScratchBytes       int64          `json:"scratch_bytes"`
	MemoryHeaderDigest string         `json:"memory_header_digest"`
	VMState            cas.Descriptor `json:"vm_state"`
	Memory             cas.Descriptor `json:"memory"`
	ScratchDisk        cas.Descriptor `json:"scratch_disk"`
}

type Object struct {
	Role string
	cas.Descriptor
}

func (m Manifest) Objects() []Object {
	return []Object{{"vm_state", m.VMState}, {"memory", m.Memory}, {"scratch_disk", m.ScratchDisk}}
}

// Content reads the authority-relevant subset of the backend's exact
// configuration. The backend still compares all configuration bytes, including
// network and device layout, before launching a clone.
type Content struct {
	Contract      string                      `json:"contract"`
	RuntimeID     string                      `json:"runtime_id"`
	CPUConfig     string                      `json:"cpu_config"`
	VCPUs         int64                       `json:"vcpus"`
	MemoryMiB     int64                       `json:"memory_mib"`
	ScratchMiB    int64                       `json:"scratch_mib"`
	ComputerRoot  string                      `json:"computer_root"`
	ComputerBytes int64                       `json:"computer_bytes"`
	Program       *computercheckpoint.Program `json:"program"`
}

func (m Manifest) Content() (Content, error) {
	var result Content
	if err := json.Unmarshal(m.Config, &result); err != nil {
		return result, err
	}
	if result.Contract != "helmr.boot-seed.v1" || !sha256sum.ValidDigest(result.RuntimeID) ||
		!sha256sum.ValidDigest(result.CPUConfig) || !sha256sum.ValidDigest(result.ComputerRoot) ||
		result.VCPUs <= 0 || result.ComputerBytes <= 0 || result.MemoryMiB != m.MemoryBytes/(1<<20) ||
		m.MemoryBytes%(1<<20) != 0 || result.ScratchMiB != m.ScratchBytes/(1<<20) || m.ScratchBytes%(1<<20) != 0 {
		return result, errors.New("boot seed content does not match its resource manifest")
	}
	return result, result.Program.Validate()
}

type Qualification struct {
	Clones [2]vm.BootSeedIdentity `json:"clones"`
}

func (q Qualification) Validate() error {
	for _, clone := range q.Clones {
		id, err := uuid.Parse(clone.BootID)
		if err != nil || id == uuid.Nil() || !sha256sum.ValidDigest(clone.RandomDigest) {
			return errors.New("boot seed clone freshness evidence is incomplete")
		}
	}
	if q.Clones[0].BootID == q.Clones[1].BootID || q.Clones[0].RandomDigest == q.Clones[1].RandomDigest {
		return errors.New("boot seed clones share boot identity or randomness")
	}
	return nil
}

// Purpose binds every encrypted object to its build owner and exact boot
// configuration. A customer checkpoint cannot authenticate in this domain.
func (m Manifest) Purpose() string {
	return "boot-seed/v1/" + m.EnvironmentID.String() + "/" + m.BuildID.String() + "/" + sha256sum.DigestBytes(m.Config)
}

func (m Manifest) Validate() error {
	if m.EnvironmentID == uuid.Nil() || m.BuildID == uuid.Nil() ||
		len(m.Config) == 0 || int64(len(m.Config)) > vm.SnapshotConfigLimit || !json.Valid(m.Config) ||
		m.MemoryBytes <= 0 || m.ScratchBytes <= 0 || !sha256sum.ValidDigest(m.MemoryHeaderDigest) {
		return errors.New("incomplete boot seed manifest")
	}
	for _, item := range []struct {
		object cas.Descriptor
		media  string
	}{{m.VMState, cas.BootSeedVMStateMediaType}, {m.Memory, cas.BootSeedMemoryMediaType}, {m.ScratchDisk, cas.BootSeedScratchMediaType}} {
		if err := cas.ValidateDescriptor(item.object); err != nil {
			return err
		}
		if item.object.MediaType != item.media {
			return errors.New("boot seed object has the wrong media type")
		}
	}
	return nil
}
