package definition

import (
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// InputSchemaDiscovery is build-time documentation, never an execution validator.
// Consumers must not fetch external references in this document.
type InputSchemaDiscovery struct {
	Status string          `json:"status"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

func ValidateInputContract(kind string, schema *InputSchemaDiscovery) error {
	if kind == "text" && schema == nil {
		return nil
	}
	if kind != "json" || schema == nil {
		return errors.New("invalid agent input contract")
	}
	switch schema.Status {
	case "available":
		if schema.Reason != "" {
			return errors.New("available input schema cannot have an unavailable reason")
		}
		raw, err := jsoncanon.Transform(schema.Schema)
		if err != nil || len(raw) > 64*1024 || !(len(raw) > 0 && raw[0] == '{' || string(raw) == "true" || string(raw) == "false") {
			return errors.New("invalid or oversized input JSON Schema")
		}
	case "unavailable":
		if len(schema.Schema) != 0 {
			return errors.New("unavailable input schema cannot contain a schema")
		}
		switch schema.Reason {
		case "missing_converter", "conversion_failed", "invalid_schema", "schema_too_large":
		default:
			return errors.New("invalid input schema unavailable reason")
		}
	default:
		return errors.New("invalid input schema discovery status")
	}
	return nil
}
