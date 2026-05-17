//go:build !embed_console && !embed_web

package console

import "io/fs"

type missingFS struct{}

func (missingFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// FS returns the embedded console application when built with -tags embed_console.
func FS() fs.FS {
	return missingFS{}
}
