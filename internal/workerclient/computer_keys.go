package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// InitialComputerKey retrieves the runtime's pinned initial write key. It is
// host-only; the caller owns clearing the returned Key after use.
func (c *Client) InitialComputerKey(ctx context.Context, request workerapi.InitialComputerKeyRequest) (workerapi.ComputerKeyMaterial, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, err := json.Marshal(request)
	if err != nil {
		return workerapi.ComputerKeyMaterial{}, errors.New("invalid computer key request")
	}
	for attempt := range 2 {
		token, err := c.token(ctx)
		if err != nil {
			return workerapi.ComputerKeyMaterial{}, errors.New("computer key authentication failed")
		}
		req, err := c.transport.Request(ctx, http.MethodPost, "/worker/v1/run/runtime-instances/initialization/key", bytes.NewReader(payload), token)
		if err != nil {
			return workerapi.ComputerKeyMaterial{}, errors.New("invalid computer key endpoint")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.transport.DoSensitive(req)
		if attempt == 0 && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateToken(token)
			continue
		}
		if err != nil {
			return workerapi.ComputerKeyMaterial{}, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1025))
		resp.Body.Close()
		if readErr != nil || len(body) > 1024 {
			clear(body)
			return workerapi.ComputerKeyMaterial{}, errors.New("invalid computer key response")
		}
		var material workerapi.ComputerKeyMaterial
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&material)
		trailingErr := decoder.Decode(new(any))
		clear(body)
		_, idErr := ids.Parse(material.ID)
		if decodeErr != nil || trailingErr != io.EOF || idErr != nil || len(material.Key) != 32 || material.Scope == "" || len(material.Scope) > 256 || !utf8.ValidString(material.Scope) {
			clear(material.Key)
			return workerapi.ComputerKeyMaterial{}, errors.New("invalid computer key response")
		}
		return material, nil
	}
	return workerapi.ComputerKeyMaterial{}, errors.New("computer key authentication retry exhausted")
}

// ComputerSource retrieves the retained source and host-only read/write keys.
// The caller owns Clear and must compare the version/capacity with its reservation.
func (c *Client) ComputerSource(ctx context.Context, request workerapi.ComputerSourceRequest) (workerapi.ComputerSourceMaterial, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, err := json.Marshal(request)
	if err != nil {
		return workerapi.ComputerSourceMaterial{}, errors.New("invalid computer key request")
	}
	for attempt := range 2 {
		token, err := c.token(ctx)
		if err != nil {
			return workerapi.ComputerSourceMaterial{}, errors.New("computer key authentication failed")
		}
		req, err := c.transport.Request(ctx, http.MethodPost, "/worker/v1/run/runtime-instances/computer-source", bytes.NewReader(payload), token)
		if err != nil {
			return workerapi.ComputerSourceMaterial{}, errors.New("invalid computer key endpoint")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.transport.DoSensitive(req)
		if attempt == 0 && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateToken(token)
			continue
		}
		if err != nil {
			return workerapi.ComputerSourceMaterial{}, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		resp.Body.Close()
		if readErr != nil || len(body) > 4<<20 {
			clear(body)
			return workerapi.ComputerSourceMaterial{}, errors.New("invalid computer key response")
		}
		var material workerapi.ComputerSourceMaterial
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&material)
		trailingErr := decoder.Decode(new(any))
		clear(body)
		if decodeErr != nil || trailingErr != io.EOF || !validComputerSource(material) {
			material.Clear()
			return workerapi.ComputerSourceMaterial{}, errors.New("invalid computer source response")
		}

		return material, nil
	}
	return workerapi.ComputerSourceMaterial{}, errors.New("computer key authentication retry exhausted")
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
