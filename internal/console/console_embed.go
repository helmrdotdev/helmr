//go:build embed_console

package console

import (
	"embed"
	"io/fs"
)

//go:embed out
var out embed.FS

// FS returns the embedded console application.
func FS() fs.FS {
	sub, err := fs.Sub(out, "out")
	if err != nil {
		panic(err)
	}
	return sub
}
