package control

import (
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/stablejson"
)

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := stablejson.Encode(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}
