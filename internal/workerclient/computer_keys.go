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

// InitialComputerKey retrieves the instance's pinned initial write key. It is
// host-only; the caller owns clearing the returned Key after use.
func (c *Client) InitialComputerKey(ctx context.Context, request workerapi.InitialComputerKeyRequest) (workerapi.ComputerKeyMaterial, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := c.preparationRequest(ctx, "/worker/v1/run/computer-instances/initialization/key", request, true, 1024)
	if err != nil {
		return workerapi.ComputerKeyMaterial{}, err
	}
	defer clear(body)
	var material workerapi.ComputerKeyMaterial
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&material)
	trailingErr := decoder.Decode(new(any))
	_, idErr := ids.Parse(material.ID)
	if decodeErr != nil || trailingErr != io.EOF || idErr != nil || len(material.Key) != 32 || material.Scope == "" || len(material.Scope) > 256 || !utf8.ValidString(material.Scope) {
		clear(material.Key)
		return workerapi.ComputerKeyMaterial{}, errors.New("invalid computer key response")
	}
	return material, nil

}

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
