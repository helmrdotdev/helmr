package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func (d ProgramDefinition) MarshalJSON() ([]byte, error) {
	var manifest any
	switch {
	case d.Kind == definition.KindAgent && d.Agent != nil && d.Computer == nil:
		manifest = d.Agent
	case d.Kind == definition.KindComputer && d.Computer != nil && d.Agent == nil:
		manifest = d.Computer
	default:
		return nil, errors.New("program definition requires exactly its kind's manifest")
	}
	return json.Marshal(struct {
		DeclaredID string          `json:"declaredId"`
		Kind       definition.Kind `json:"kind"`
		Manifest   any             `json:"manifest"`
	}{d.DeclaredID, d.Kind, manifest})
}
func (d *ProgramDefinition) UnmarshalJSON(raw []byte) error {
	var wire struct {
		DeclaredID string          `json:"declaredId"`
		Kind       definition.Kind `json:"kind"`
		Manifest   json.RawMessage `json:"manifest"`
	}
	if err := decodeClosedDefinition(raw, &wire); err != nil {
		return err
	}
	*d = ProgramDefinition{Kind: wire.Kind, DeclaredID: wire.DeclaredID}
	switch wire.Kind {
	case definition.KindAgent:
		return decodeClosedDefinition(wire.Manifest, &d.Agent)
	case definition.KindComputer:
		return decodeClosedDefinition(wire.Manifest, &d.Computer)
	default:
		return fmt.Errorf("unsupported program definition kind %q", wire.Kind)
	}
}
func ValidateProgramDefinition(d ProgramDefinition) error {
	if !definition.ValidDeclaredID(d.DeclaredID) {
		return errors.New("invalid program definition id")
	}
	switch {
	case d.Kind == definition.KindAgent && d.Agent != nil && d.Computer == nil:
		return definition.ValidateAgentManifest(*d.Agent)
	case d.Kind == definition.KindComputer && d.Computer != nil && d.Agent == nil:
		return definition.ValidateComputerManifest(*d.Computer)
	default:
		return errors.New("program definition requires exactly its kind's manifest")
	}
}
func CompareProgramDefinitions(left, right ProgramDefinition) int {
	if compared := strings.Compare(string(left.Kind), string(right.Kind)); compared != 0 {
		return compared
	}
	return strings.Compare(left.DeclaredID, right.DeclaredID)
}
func (d ProgramDefinition) Clone() ProgramDefinition {
	return cloneProgramDefinition(d)
}
func cloneProgramDefinition(d ProgramDefinition) ProgramDefinition {
	if d.Agent != nil {
		value := *d.Agent
		value.CloseAfterIdleMs = cloneInt64(value.CloseAfterIdleMs)
		value.MaxTurnDurationMs = cloneInt64(value.MaxTurnDurationMs)
		value.Triggers = maps.Clone(value.Triggers)
		for id, trigger := range value.Triggers {
			trigger.Input = slices.Clone(trigger.Input)
			value.Triggers[id] = trigger
		}
		d.Agent = &value
	}
	if d.Computer != nil {
		value := *d.Computer
		value.Seed.Config.Env = slices.Clone(value.Seed.Config.Env)
		value.Seed.Config.Entrypoint = slices.Clone(value.Seed.Config.Entrypoint)
		value.Seed.Config.Cmd = slices.Clone(value.Seed.Config.Cmd)
		value.Resources.DiskMiB = cloneInt64(value.Resources.DiskMiB)
		if value.Refresh != nil {
			refresh := *value.Refresh
			refresh.MaxAgeMs = cloneInt64(refresh.MaxAgeMs)
			value.Refresh = &refresh
		}
		value.Secrets = secretbinding.CloneReferences(value.Secrets)
		value.BuildSecrets = secretbinding.CloneReferences(value.BuildSecrets)
		d.Computer = &value
	}
	return d
}
func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
func decodeClosedDefinition(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return jsoncanon.RequireEOF(decoder, "definition input")
}
