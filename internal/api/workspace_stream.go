package api

import "encoding/json"

type WorkspaceStreamTerminalResponse struct {
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Stream       string          `json:"stream"`
	State        string          `json:"state"`
	Cursor       int64           `json:"cursor"`
	Error        json.RawMessage `json:"error,omitempty"`
}

type WorkspaceStreamErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Cursor  int64  `json:"cursor,omitempty"`
}
