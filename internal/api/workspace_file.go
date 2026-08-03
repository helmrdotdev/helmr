package api

type WorkspaceFileEntry struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Mode       uint32 `json:"mode"`
	SizeBytes  *int64 `json:"size_bytes,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
}

type WorkspaceFileContent struct {
	DataBase64 string `json:"data_base64"`
}

type WorkspaceFilePage struct {
	Items      []WorkspaceFileEntry `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}
