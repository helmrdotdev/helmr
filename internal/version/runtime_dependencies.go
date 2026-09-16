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
	Node       dependencyVersion `json:"node"`
	TypeScript dependencyVersion `json:"typescript"`
}

var runtimeVersions = func() dependencyVersions {
	var dependencies dependencyVersions
	if err := json.Unmarshal(runtimeDependencies, &dependencies); err != nil {
		panic(err)
	}
	if dependencies.Node.Version == "" || dependencies.TypeScript.Version == "" {
		panic("missing Product Runtime dependency version")
	}
	return dependencies
}()

// Node is the exact Product-owned Node execution version.
func Node() string { return runtimeVersions.Node.Version }

// RuntimeTypeScript is the Runtime's exact parser/emitter version, not the authoring toolchain.
func RuntimeTypeScript() string { return runtimeVersions.TypeScript.Version }
