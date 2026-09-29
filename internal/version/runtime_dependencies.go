package version

import (
	_ "embed"
	"encoding/json"
)

//go:embed runtime-dependencies.json
var runtimeDependencies []byte

type dependencyVersion struct {
	Version string `json:"version"`
}

type dependencyVersions struct {
	Node dependencyVersion `json:"node"`
}

var runtimeVersions = func() dependencyVersions {
	var dependencies dependencyVersions
	if err := json.Unmarshal(runtimeDependencies, &dependencies); err != nil {
		panic(err)
	}
	if dependencies.Node.Version == "" {
		panic("missing Product Runtime dependency version")
	}
	return dependencies
}()

// Node is the exact Product-owned Node execution version.
func Node() string { return runtimeVersions.Node.Version }
