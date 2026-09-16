package version

import (
	_ "embed"
	"encoding/json"
)

//go:embed node-release.json
var nodeRelease []byte

var nodeVersion = func() string {
	var release struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(nodeRelease, &release); err != nil {
		panic(err)
	}
	if release.Version == "" {
		panic("missing Product Node version")
	}
	return release.Version
}()

// Node is the exact Product-owned Node execution version.
func Node() string { return nodeVersion }
