package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// ComputerSource retrieves the retained source and host-only read/write keys.
// The caller owns Clear and must compare the version/capacity with its reservation.
func (c *Client) ComputerSource(ctx context.Context, request workerapi.ComputerSourceRequest) (workerapi.ComputerSourceMaterial, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := c.preparationRequest(ctx, "/worker/v1/run/computer-instances/computer-source", request, true, 4<<20)
	if err != nil {
		return workerapi.ComputerSourceMaterial{}, err
	}
	defer clear(body)
	var material workerapi.ComputerSourceMaterial
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&material)
	trailingErr := decoder.Decode(new(any))
	if decodeErr != nil || trailingErr != io.EOF || !validComputerSource(material) {
		material.Clear()
		return workerapi.ComputerSourceMaterial{}, errors.New("invalid computer source response")
	}
	return material, nil

}

func validComputerSource(m workerapi.ComputerSourceMaterial) bool {
	if _, err := ids.Parse(m.VersionID); err != nil {
		return false
	}
	if err := m.Root.Validate(m.Root.LogicalBytes); err != nil {
		return false
	}
	if len(m.Keys) == 0 {
		return false
	}
	seen := make(map[string]bool, len(m.Keys))
	scope := m.Keys[0].Scope
	if scope == "" || len(scope) > 256 || !utf8.ValidString(scope) {
		return false
	}
	for _, k := range m.Keys {
		if _, err := ids.Parse(k.ID); err != nil {
			return false
		}
		if seen[k.ID] || k.Scope != scope || len(k.Key) != 32 {
			return false
		}
		seen[k.ID] = true
	}
	return seen[m.Root.Page.KeyID] && seen[m.WriteKeyID]
}

// PrepareComputerSeed claims or adopts the admitted seed; plaintext stays on the host.
func (c *Client) PrepareComputerSeed(ctx context.Context, request workerapi.PrepareComputerSeedRequest) (workerapi.ComputerSeedPreparation, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := c.preparationRequest(ctx, "/worker/v1/run/computer-instances/initialization/seed", request, true, 1024)
	if err != nil {
		return workerapi.ComputerSeedPreparation{}, err
	}
	defer clear(body)
	var material workerapi.ComputerSeedPreparation
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&material)
	trailingErr := decoder.Decode(new(any))
	valid := decodeErr == nil && trailingErr == io.EOF
	if material.Status == "convert" && material.Key != nil {
		key := material.Key
		valid = valid && ids.Validate(key.ID) == nil && len(key.Key) == 32 && key.Scope != "" && len(key.Scope) <= 256 && utf8.ValidString(key.Scope)
	} else {
		valid = valid && (material.Status == "ready" || material.Status == "waiting") && material.Key == nil
	}
	if !valid {
		if material.Key != nil {
			clear(material.Key.Key)
		}
		return workerapi.ComputerSeedPreparation{}, errors.New("invalid computer seed preparation response")
	}
	return material, nil
}
