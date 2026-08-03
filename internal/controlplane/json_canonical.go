package controlplane

import (
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}
